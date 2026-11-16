package discover

import (
	"context"
	"fmt"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
)

// Cluster is a Source over a live source cluster. It only ever lists: no object
// it reads is written, no Secret is read at all, and the client is built the way
// every other host-side box client is (internal/kube), so a source kubeconfig
// with an exec credential or a token file works as it does under kubectl.
type Cluster struct {
	cs         kubernetes.Interface
	namespaces []string

	mu     sync.Mutex
	loaded bool
	wls    []Workload
	svcs   []Service
	raw    map[string][]byte
	err    error
}

// NewCluster builds the source client for one kubeconfig.
//
// namespaces limits every list to those namespaces, one call each; empty lists
// across all namespaces, which needs cluster-wide read access. A namespace-
// scoped kubeconfig -- a user's, where root's is not -- can only read the
// namespaces it names, so the same list is Options.Namespaces.
func NewCluster(kubeconfig string, namespaces []string) (*Cluster, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig: discovery reads a source cluster through one")
	}
	cfg, err := kube.Config(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	return NewClusterFor(cs, namespaces), nil
}

// NewClusterFor is NewCluster over an existing clientset, which is what the
// tests drive.
func NewClusterFor(cs kubernetes.Interface, namespaces []string) *Cluster {
	return &Cluster{cs: cs, namespaces: namespaces}
}

// Workloads is the source's Deployments and StatefulSets.
func (c *Cluster) Workloads(ctx context.Context) ([]Workload, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.wls, nil
}

// Services is the source's Services.
func (c *Cluster) Services(ctx context.Context) ([]Service, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.svcs, nil
}

// Raw is every object read, as the plan directory keeps it.
func (c *Cluster) Raw(ctx context.Context) (map[string][]byte, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	return c.raw, nil
}

// load reads the source once, whichever of the three questions comes first.
func (c *Cluster) load(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return c.err
	}
	c.loaded = true
	c.err = c.read(ctx)
	return c.err
}

func (c *Cluster) read(ctx context.Context) error {
	raw := map[string][]byte{}
	var deploys []*appsv1.Deployment
	var stss []*appsv1.StatefulSet
	var svcs []*corev1.Service
	for _, ns := range c.scope() {
		list, err := c.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list deployments in %s: %w", where(ns), err)
		}
		for i := range list.Items {
			deploys = append(deploys, &list.Items[i])
		}
		sets, err := c.cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list statefulsets in %s: %w", where(ns), err)
		}
		for i := range sets.Items {
			stss = append(stss, &sets.Items[i])
		}
		services, err := c.cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list services in %s: %w", where(ns), err)
		}
		for i := range services.Items {
			svcs = append(svcs, &services.Items[i])
		}
	}
	pvcs, pvs, err := c.claims(ctx, deploys, stss)
	if err != nil {
		return err
	}
	for _, o := range []struct {
		apiVersion, kind string
		objs             []any
	}{
		{appsV1, "Deployment", anySlice(deploys)},
		{appsV1, "StatefulSet", anySlice(stss)},
		{"v1", "Service", anySlice(svcs)},
		{"v1", "PersistentVolumeClaim", anySlice(pvcs)},
		{"v1", "PersistentVolume", anySlice(pvs)},
	} {
		if err := record(raw, o.apiVersion, o.kind, o.objs); err != nil {
			return err
		}
	}

	claims := indexClaims(pvcs, pvs)
	for _, d := range deploys {
		c.wls = append(c.wls, workloadOf("Deployment", d.Namespace, d.Name, d.Spec.Template.Labels, d.Spec.Template.Spec, claims))
	}
	for _, s := range stss {
		c.wls = append(c.wls, workloadOf("StatefulSet", s.Namespace, s.Name, s.Spec.Template.Labels, s.Spec.Template.Spec, claims))
	}
	for _, svc := range svcs {
		c.svcs = append(c.svcs, serviceOf(svc))
	}
	c.raw = raw
	return nil
}

// scope is what every list is asked for: the namespaces discovery was told
// about, or the empty string, which client-go reads as all of them.
func (c *Cluster) scope() []string {
	if len(c.namespaces) == 0 {
		return []string{""}
	}
	return c.namespaces
}

// where names a list's scope in a refusal.
func where(namespace string) string {
	if namespace == "" {
		return "every namespace"
	}
	return namespace
}

// claims reads the claims and volumes the source's mounts go through, and only
// when a mount goes through one: a source whose apps all use hostPaths does not
// need a permission it may not have.
func (c *Cluster) claims(ctx context.Context, deploys []*appsv1.Deployment, stss []*appsv1.StatefulSet) (pvcs []*corev1.PersistentVolumeClaim, pvs []*corev1.PersistentVolume, err error) {
	specs := make([]corev1.PodSpec, 0, len(deploys)+len(stss))
	for _, d := range deploys {
		specs = append(specs, d.Spec.Template.Spec)
	}
	for _, s := range stss {
		specs = append(specs, s.Spec.Template.Spec)
	}
	wanted := false
	for _, spec := range specs {
		for _, v := range spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				wanted = true
			}
		}
	}
	if !wanted {
		return nil, nil, nil
	}
	for _, ns := range c.scope() {
		list, err := c.cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("list persistentvolumeclaims in %s: %w", where(ns), err)
		}
		for i := range list.Items {
			pvcs = append(pvcs, &list.Items[i])
		}
	}
	volumes, err := c.cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list persistentvolumes: %w", err)
	}
	for i := range volumes.Items {
		pvs = append(pvs, &volumes.Items[i])
	}
	return pvcs, pvs, nil
}
