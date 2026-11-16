package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"k8s.io/client-go/restmapper"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/wait"
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
	cfg, err := kube.Config(kubeconfig)
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
	return kube.SSAR(k.cs).Allowed(ctx, a)
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

// Apply server-side-applies each document with field manager box.
func (k *Kubernetes) Apply(ctx context.Context, docs []map[string]any) error {
	return k.ApplyWith(ctx, kube.FieldManager, docs)
}

// ApplyWith is the same write under another field manager. A list that is
// merged by key (the Gateway's listeners) is what needs it: the TLS flow owns
// its host's listener, so the platform's own Gateway apply never removes it.
// Force is set because box owns the platform layer: a field another manager
// once touched must not make `box bootstrap` fail forever.
func (k *Kubernetes) ApplyWith(ctx context.Context, manager string, docs []map[string]any) error {
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
		if _, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
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

// Serves reports whether the cluster's API serves a kind. It is how init tells
// a box whose platform installed cert-manager from one whose did not, before
// the first write.
func (k *Kubernetes) Serves(_ context.Context, group, version, kind string) (bool, error) {
	if _, err := k.mapping(schema.GroupVersionKind{Group: group, Version: version, Kind: kind}); err != nil {
		return false, nil
	}
	return true, nil
}

// Get reads one object as an unstructured document. A missing object is
// (nil, false, nil): a caller deciding between create and leave-alone asks
// this, and "not there" is an answer.
func (k *Kubernetes) Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error) {
	gvr, err := k.mapping(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	if err != nil {
		return nil, false, err
	}
	obj, err := k.dyn.Resource(gvr.Resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s %s/%s: %w", kind, namespace, name, err)
	}
	return obj.Object, true, nil
}

// Create makes one object and never updates one. A Secret box must not rotate
// is written with this and not with Apply: an apply would happily replace the
// field another run had set.
func (k *Kubernetes) Create(ctx context.Context, doc map[string]any) error {
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
	if _, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: kube.FieldManager}); err != nil {
		return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}

// Delete removes one object. A missing object is not an error: every caller
// removes what it may have written on a previous run, and "it is not there" is
// the state it wants.
func (k *Kubernetes) Delete(ctx context.Context, group, version, kind, namespace, name string) error {
	gvr, err := k.mapping(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	if err != nil {
		return err
	}
	err = k.dyn.Resource(gvr.Resource).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// UpdateStatus writes one object's status. box uses it for the one thing a
// status carries that is a request: cert-manager's Issuing condition, which is
// how a Certificate is asked to re-issue itself (cmctl renew does the same
// write). It is separate from Apply because a status subresource rejects an
// ordinary apply that sets it.
func (k *Kubernetes) UpdateStatus(ctx context.Context, group, version, kind, namespace string, obj map[string]any) error {
	gvr, err := k.mapping(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: obj}
	if _, err := k.dyn.Resource(gvr.Resource).Namespace(namespace).UpdateStatus(ctx, u, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %s %s/%s status: %w", kind, namespace, u.GetName(), err)
	}
	return nil
}

// NamespaceExists is the one read that decides whether a platform object can
// be written at all: s5n-system is the admission policy's namespace, so box
// creates it at bootstrap.
func (k *Kubernetes) NamespaceExists(ctx context.Context, name string) (bool, error) {
	_, err := k.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read namespace %s: %w", name, err)
	}
	return true, nil
}

// PolicyTarget is the object a policy attaches to: the same fields its
// targetRefs carries, because that is what status.ancestors echoes back. A
// Gateway-wide policy leaves SectionName empty; a listener-scoped one names the
// listener.
type PolicyTarget struct {
	Group       string
	Kind        string
	Name        string
	Namespace   string
	SectionName string
}

// PolicyAccepted reads a policy's status for one target. It is a function of
// the status document rather than of the client so the shapes Envoy Gateway
// really writes can be tested from a captured status.
//
// Envoy Gateway does not put an Accepted condition on the policy itself: a
// policy is accepted by the controller that serves the object it targets, and
// it reports that under status.ancestors, one entry per target and controller,
// with the ancestorRef naming the target. Top-level status.conditions is
// therefore not looked at -- a policy with only those has not been accepted,
// and reading them would make the wait succeed on a document EG never writes.
func PolicyAccepted(status map[string]any, target PolicyTarget) (bool, string) {
	ancestors, _ := status["ancestors"].([]any)
	for _, raw := range ancestors {
		ancestor, _ := raw.(map[string]any)
		if name, _ := ancestor["controllerName"].(string); name != bootstrap.EnvoyGatewayController {
			continue
		}
		ref, _ := ancestor["ancestorRef"].(map[string]any)
		if !sameTargetRef(ref, target) {
			continue
		}
		conditions, _ := ancestor["conditions"].([]any)
		for _, raw := range conditions {
			cond, _ := raw.(map[string]any)
			if cond["type"] != "Accepted" {
				continue
			}
			if cond["status"] == conditionTrue {
				return true, ""
			}
			return false, policyReason(cond)
		}
		// The ancestor is there, the condition is not: still transient.
		return false, "Envoy Gateway has not accepted it yet"
	}
	return false, "Envoy Gateway has not accepted it yet"
}

// sameTargetRef is the ancestorRef match. Every field the ancestorRef sets is
// compared, and sectionName is compared as written: a listener-scoped policy
// must not be satisfied by an ancestor entry for the whole Gateway (or another
// listener).
func sameTargetRef(ref map[string]any, target PolicyTarget) bool {
	for field, want := range map[string]string{
		"group":       target.Group,
		"kind":        target.Kind,
		"name":        target.Name,
		"namespace":   target.Namespace,
		"sectionName": target.SectionName,
	} {
		got, _ := ref[field].(string)
		if got != want {
			return false
		}
	}
	return true
}

// policyReason is the condition as one sentence: Envoy Gateway's reason, and
// its message when it has one.
func policyReason(cond map[string]any) string {
	reason, _ := cond["reason"].(string)
	message, _ := cond["message"].(string)
	if reason == "" {
		reason = "not Accepted"
	}
	if message != "" {
		reason += ": " + message
	}
	return reason
}

// Accepted answers whether the controller serving target has accepted the
// object, with the reason when it has not. It is the wait a policy needs:
// Envoy Gateway reports a SecurityPolicy it has not translated yet with a
// transient reason, and a policy it refuses with a permanent one.
func (k *Kubernetes) Accepted(ctx context.Context, group, version, kind, namespace, name string, target PolicyTarget) (bool, string, error) {
	gvr, err := k.mapping(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	if err != nil {
		return false, "", err
	}
	obj, err := k.dyn.Resource(gvr.Resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, "the " + kind + " has not been written yet", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("read %s %s/%s: %w", kind, namespace, name, err)
	}
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	ok, why := PolicyAccepted(status, target)
	return ok, why, nil
}

// CertificateReady answers a Certificate's Ready condition for the generation
// the object is at (CertificateStatus). A Certificate that is not there yet is
// not an error: the wait that follows is what gives cert-manager time to write
// it.
func (k *Kubernetes) CertificateReady(ctx context.Context, namespace, name string) (bool, string, error) {
	gvr, err := k.mapping(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	if err != nil {
		return false, "", err
	}
	obj, err := k.dyn.Resource(gvr.Resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, "the Certificate has not been written yet", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("read Certificate %s/%s: %w", namespace, name, err)
	}
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	generation, _, _ := unstructured.NestedInt64(obj.Object, "metadata", "generation")
	ready, reason := CertificateStatus(status, generation)
	return ready, reason, nil
}

// CertificateStatus answers whether a Certificate has issued its current
// generation, with cert-manager's reason when it has not.
//
// The generation is the input that makes a move visible. box moves a host by
// writing a new issuerRef into the Certificate's spec, and that bumps
// metadata.generation; the Ready condition keeps saying True, and keeps the
// generation it was written for, until the issuing controller reconciles the
// new spec and has a certificate for it. A wait that read the condition alone
// answers Ready for the certificate box is still serving -- the one the move
// was meant to replace -- so the run reports the move done before cert-manager
// has asked for anything, writes the host's HTTPS listener behind the old
// certificate, and the next run sits out the whole CertificateTimeout on the
// same host. That is exactly what the e2e's ACME move caught: Ready printed at
// once for a Certificate that was still on the self-signed Secret minutes
// later.
//
// A condition without an observedGeneration is read as current. cert-manager
// has stamped the field on every condition since long before the pinned
// release, so a document without it is not one box meets, and treating it as
// stale would make every wait run its full timeout against a controller that
// does not write it.
func CertificateStatus(status map[string]any, generation int64) (bool, string) {
	conditions, _, _ := unstructured.NestedSlice(status, "conditions")
	for _, raw := range conditions {
		cond, _ := raw.(map[string]any)
		if cond["type"] != "Ready" {
			continue
		}
		if cond["status"] != conditionTrue {
			reason, _ := cond["reason"].(string)
			message, _ := cond["message"].(string)
			if reason == "" {
				reason = "not Ready"
			}
			if message != "" {
				reason += ": " + message
			}
			return false, reason
		}
		if seen, ok, err := unstructured.NestedInt64(cond, "observedGeneration"); err == nil && ok && seen < generation {
			return false, fmt.Sprintf("cert-manager has not reconciled generation %d yet (its Ready condition describes generation %d)", generation, seen)
		}
		return true, ""
	}
	return false, "cert-manager has not reconciled it yet"
}

// conditionTrue is the API's literal for a condition that holds. Every
// condition box reads is a string in an unstructured object, so the spelling
// lives once, here, and the one writer that sets a condition (the Issuing
// condition box uses to ask for a re-issue) uses it too.
const conditionTrue = "True"

// ErrNotReady is the deadline answer of a read-only wait: the object is still
// not ready, which a caller may handle differently from a failed read.
var ErrNotReady = errors.New("not ready")

// Await blocks until the object is ready or the bound expires.
func (k *Kubernetes) Await(ctx context.Context, w Wait) error {
	timeout := k.Timeout
	if w.Timeout > 0 {
		timeout = w.Timeout
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	poll := k.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	waited, err := wait.Until(ctx, timeout, poll, func(ctx context.Context) (bool, error) {
		return k.ready(ctx, w)
	})
	if err != nil {
		return err
	}
	if waited.Done {
		return nil
	}
	return fmt.Errorf("%w after %s", ErrNotReady, timeout)
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
	case WaitCertificate:
		ready, _, err := k.CertificateReady(ctx, w.Namespace, w.Name)
		return ready, err
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
