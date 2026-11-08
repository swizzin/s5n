package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Kubernetes is the cluster client: client-go for the typed reads and the
// access reviews, the dynamic client with server-side apply for manifests.
type Kubernetes struct {
	cs     kubernetes.Interface
	dyn    dynamic.Interface
	mapper meta.RESTMapper
	// Timeout bounds one readiness wait; zero means five minutes.
	Timeout time.Duration
	// Poll is how often readiness is re-read; zero means two seconds.
	Poll time.Duration
}

// NewCluster builds the client for one kubeconfig. The path is required: box
// resolves a cluster through bootstrap, and an empty path would silently fall
// back to ~/.kube/config, which is never box's cluster.
func NewCluster(kubeconfig string) (*Kubernetes, error) {
	if kubeconfig == "" {
		return nil, errors.New("no kubeconfig: box bootstrap always has one from its cluster decision")
	}
	// client-go's deprecation warnings go through klog and would land in the
	// middle of box's output; box is the whole interface.
	rest.SetDefaultWarningHandler(rest.NoWarnings{})

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	return &Kubernetes{
		cs:     cs,
		dyn:    dyn,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
	}, nil
}

// Allowed answers one SelfSubjectAccessReview.
func (k *Kubernetes) Allowed(ctx context.Context, a Access) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:      a.Verb,
				Group:     a.Group,
				Resource:  a.Resource,
				Namespace: a.Namespace,
			},
		},
	}
	out, err := k.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

// PrimaryInternalIP is the address MetalLB advertises: the node's own primary
// InternalIP, as a single /32 pool.
func (k *Kubernetes) PrimaryInternalIP(ctx context.Context) (string, error) {
	nodes, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return "", errors.New("the cluster reports no nodes")
	}
	names := make([]string, 0, len(nodes.Items))
	byName := map[string]corev1.Node{}
	for _, n := range nodes.Items {
		names = append(names, n.Name)
		byName[n.Name] = n
	}
	sort.Strings(names)
	for _, name := range names {
		for _, a := range byName[name].Status.Addresses {
			if a.Type == corev1.NodeInternalIP && a.Address != "" {
				return a.Address, nil
			}
		}
	}
	return "", errors.New("no node reports an InternalIP address")
}

// Apply server-side-applies each document with field manager box. Force is set
// because box owns the platform layer: a field another manager once touched
// must not make `box bootstrap` fail forever.
func (k *Kubernetes) Apply(ctx context.Context, docs []map[string]any) error {
	for _, doc := range docs {
		obj := &unstructured.Unstructured{Object: doc}
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" || obj.GetName() == "" {
			return errors.New("manifest: an object has no kind or name")
		}
		mapping, err := k.mapping(gvk)
		if err != nil {
			return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
		}
		var ri dynamic.ResourceInterface = k.dyn.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := obj.GetNamespace()
			if ns == "" {
				return fmt.Errorf("%s %s: no namespace", gvk.Kind, obj.GetName())
			}
			ri = k.dyn.Resource(mapping.Resource).Namespace(ns)
		}
		if _, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
			return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
		}
	}
	return nil
}

// mapping resolves a kind to its resource. A CRD that was installed moments
// ago is not in the discovery cache yet, so a miss resets the cache and tries
// once more; the API server serves the new kind well before that gives up.
func (k *Kubernetes) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	m, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return m, nil
	}
	if r, ok := k.mapper.(interface{ Reset() }); ok {
		r.Reset()
	}
	m, err = k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("the cluster does not serve this kind: %w", err)
	}
	return m, nil
}

// Await blocks until the object is ready or the bound expires.
func (k *Kubernetes) Await(ctx context.Context, w Wait) error {
	timeout := k.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	poll := k.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		ready, err := k.ready(ctx, w)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (k *Kubernetes) ready(ctx context.Context, w Wait) (bool, error) {
	switch w.Kind {
	case WaitDeployment:
		d, err := k.cs.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return deploymentReady(d), nil
	case WaitDaemonSet:
		d, err := k.cs.AppsV1().DaemonSets(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return daemonSetReady(d), nil
	case WaitGateway:
		gvr, err := k.mapping(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
		if err != nil {
			return false, err
		}
		obj, err := k.dyn.Resource(gvr.Resource).Namespace(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, c := range conditions {
			cond, _ := c.(map[string]any)
			if cond["type"] == "Programmed" && cond["status"] == "True" {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("unknown wait kind %q", w.Kind)
	}
}

func deploymentReady(d *appsv1.Deployment) bool {
	if d.Generation > d.Status.ObservedGeneration {
		return false
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return d.Status.UpdatedReplicas == desired &&
		d.Status.AvailableReplicas >= desired &&
		d.Status.UnavailableReplicas == 0
}

func daemonSetReady(d *appsv1.DaemonSet) bool {
	if d.Generation > d.Status.ObservedGeneration {
		return false
	}
	return d.Status.DesiredNumberScheduled > 0 &&
		d.Status.NumberReady == d.Status.DesiredNumberScheduled &&
		d.Status.UpdatedNumberScheduled == d.Status.DesiredNumberScheduled
}
