package release

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreapply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/seedbox"
)

// NominalCapacity is the size box writes on every PV and claim. A hostPath
// volume has no size, but the API requires a number and nothing enforces it.
const NominalCapacity = "1Ti"

// Pod Security labels on the namespace box creates: enforce admits or refuses a
// pod, and audit and warn say the same level for legibility and to match
// `adduser`. Anything weaker lets in a pod the chart's security context assumes
// it will not get.
const (
	podSecurityEnforce    = "pod-security.kubernetes.io/enforce"
	podSecurityAudit      = "pod-security.kubernetes.io/audit"
	podSecurityWarn       = "pod-security.kubernetes.io/warn"
	podSecurityRestricted = "restricted"
)

// NamespacePlan is what box found of the seedbox's namespace before any write.
type NamespacePlan struct {
	Name   string
	Exists bool
	// CanCreate is asked only when the namespace is missing: cluster-scoped, so
	// root's kubeconfig can and a user's cannot.
	CanCreate bool
	// Enforce is the existing namespace's Pod Security level, or "" for no
	// label: only enforce decides admission.
	Enforce string
}

// PlanNamespace reads one namespace; a missing one also asks the access
// question, so a refusal comes before the PV is written.
func (s *KubeStorage) PlanNamespace(ctx context.Context) (NamespacePlan, error) {
	plan := NamespacePlan{Name: s.namespace}
	live, err := s.cs.CoreV1().Namespaces().Get(ctx, s.namespace, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		allowed, err := s.canCreateNamespaces(ctx)
		if err != nil {
			return plan, err
		}
		plan.CanCreate = allowed
		return plan, nil
	case err != nil:
		return plan, fmt.Errorf("read namespace %s: %w", s.namespace, err)
	}
	plan.Exists = true
	plan.Enforce = live.Labels[podSecurityEnforce]
	return plan, nil
}

// canCreateNamespaces asks the API server as this identity; a denial is an
// answer, not an error. It asks apply, not create: CreateNamespace
// server-side-applies, which is create when missing and patch when present.
func (s *KubeStorage) canCreateNamespaces(ctx context.Context) (bool, error) {
	allowed, err := kube.Allowed(ctx, kube.SSAR(s.cs), kube.Access{Verb: kube.VerbApply, Resource: "namespaces", What: "namespaces"})
	if err != nil {
		return false, fmt.Errorf("ask whether this kubeconfig may create namespaces: %w", err)
	}
	return allowed, nil
}

// CreateNamespace server-side-applies the namespace with restricted labels and
// box's managed-by, under the same field manager as the volumes. An existing
// namespace is never applied.
func (s *KubeStorage) CreateNamespace(ctx context.Context) error {
	ns := coreapply.Namespace(s.namespace).WithLabels(map[string]string{
		kube.ManagedByLabel: kube.FieldManager,
		podSecurityEnforce:  podSecurityRestricted,
		podSecurityAudit:    podSecurityRestricted,
		podSecurityWarn:     podSecurityRestricted,
	})
	if _, err := s.cs.CoreV1().Namespaces().Apply(ctx, ns, metav1.ApplyOptions{FieldManager: kube.FieldManager}); err != nil {
		return fmt.Errorf("create namespace %s: %w", s.namespace, err)
	}
	return nil
}

// StorageObject is one PV or claim to write; the fields are what box prints.
type StorageObject struct {
	Kind   string // KindPersistentVolume or KindPersistentVolumeClaim
	Name   string
	Detail string // e.g. "hostPath /srv/s5n/data"
	apply  func(ctx context.Context) error
}

// The two kinds Plan returns; undo deletes only a volume this run created.
const (
	KindPersistentVolume      = "PersistentVolume"
	KindPersistentVolumeClaim = "PersistentVolumeClaim"
)

// StorageClient is box's handle on the namespace and storage. Plan and
// PlanNamespace only read, so every conflict is refused before a write; a claim
// written into a missing namespace would fail after its cluster-scoped PV
// exists. Apply and CreateNamespace write one planned object each.
type StorageClient interface {
	Plan(ctx context.Context, namespace string, vols []seedbox.Volume) ([]StorageObject, error)
	PlanNamespace(ctx context.Context) (NamespacePlan, error)
	CreateNamespace(ctx context.Context) error
	Apply(ctx context.Context, obj StorageObject) error
	// Delete removes a volume this run created when it cannot finish.
	Delete(ctx context.Context, obj StorageObject) error
}

// KubeStorage is the client-go implementation: cluster-scoped PVs and the
// namespace's claims.
type KubeStorage struct {
	namespace string
	cs        kubernetes.Interface
}

// NewStorage builds the storage client; no cluster call happens until Plan.
func NewStorage(kubeconfig, namespace string) (*KubeStorage, error) {
	if namespace == "" {
		return nil, fmt.Errorf("no namespace: the grant names the release namespace")
	}
	cfg, err := kube.Config(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("storage client for %s: %w", namespace, err)
	}
	return NewStorageFor(namespace, cs), nil
}

// NewStorageFor is NewStorage over an existing clientset, which is what the
// tests drive.
func NewStorageFor(namespace string, cs kubernetes.Interface) *KubeStorage {
	return &KubeStorage{namespace: namespace, cs: cs}
}

// Plan returns only what must be created, in data-then-config order, and never
// writes: a conflict on the second must not leave the first applied.
func (s *KubeStorage) Plan(ctx context.Context, namespace string, vols []seedbox.Volume) ([]StorageObject, error) {
	var out []StorageObject
	for _, v := range vols {
		objs, err := s.planVolume(ctx, namespace, v)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

// planVolume is one volume's PV and claim. An existing PV is never mutated: it
// must match the path and single claim, or box refuses.
func (s *KubeStorage) planVolume(ctx context.Context, namespace string, v seedbox.Volume) ([]StorageObject, error) {
	var out []StorageObject

	pv := s.persistentVolume(namespace, v)
	live, err := s.cs.CoreV1().PersistentVolumes().Get(ctx, v.Claim, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out = append(out, StorageObject{
			Kind:   KindPersistentVolume,
			Name:   v.Claim,
			Detail: "hostPath " + v.Path,
			apply: func(ctx context.Context) error {
				_, err := s.cs.CoreV1().PersistentVolumes().Apply(ctx, pv, metav1.ApplyOptions{FieldManager: kube.FieldManager})
				return err
			},
		})
	case err != nil:
		return nil, fmt.Errorf("read PersistentVolume %s: %w", v.Claim, err)
	default:
		if err := s.checkPersistentVolume(live, v); err != nil {
			return nil, err
		}
	}

	pvc := s.persistentVolumeClaim(namespace, v)
	liveClaim, err := s.cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, v.Claim, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out = append(out, StorageObject{
			Kind:   KindPersistentVolumeClaim,
			Name:   v.Claim,
			Detail: "volume " + v.Claim,
			apply: func(ctx context.Context) error {
				_, err := s.cs.CoreV1().PersistentVolumeClaims(namespace).Apply(ctx, pvc, metav1.ApplyOptions{FieldManager: kube.FieldManager})
				return err
			},
		})
	case err != nil:
		return nil, fmt.Errorf("read PersistentVolumeClaim %s/%s: %w", namespace, v.Claim, err)
	default:
		if liveClaim.Spec.VolumeName != v.Claim {
			return nil, fmt.Errorf("PersistentVolumeClaim %s/%s already binds volume %q, not %q: box will not rebind a claim; delete or rename it first",
				namespace, v.Claim, liveClaim.Spec.VolumeName, v.Claim)
		}
	}
	return out, nil
}

// checkPersistentVolume refuses a PV of box's name that backs another path or
// claim; a static volume's path is never changed in place. The error names both
// paths.
func (s *KubeStorage) checkPersistentVolume(live *corev1.PersistentVolume, v seedbox.Volume) error {
	if live.Spec.HostPath == nil {
		return fmt.Errorf("PersistentVolume %s already exists and is not a hostPath volume: box will not take it over; delete or rename it first", live.Name)
	}
	if live.Spec.HostPath.Path != v.Path {
		return fmt.Errorf("PersistentVolume %s already backs %s, but storage.%s.path is %s: box never changes a volume's path in place; delete or rename the PV first",
			live.Name, live.Spec.HostPath.Path, v.Kind, v.Path)
	}
	ref := live.Spec.ClaimRef
	if ref == nil {
		return fmt.Errorf("PersistentVolume %s backs %s but names no claim (storage.%s.path is %s): box binds it to one claim only; delete or rename the PV first",
			live.Name, v.Path, v.Kind, v.Path)
	}
	if ref.Namespace != s.namespace || ref.Name != v.Claim {
		return fmt.Errorf("PersistentVolume %s backs %s for claim %s/%s, but this box's claim is %s/%s: box names the claim it creates and never shares a volume between boxes; delete or rename the PV first",
			live.Name, v.Path, ref.Namespace, ref.Name, s.namespace, v.Claim)
	}
	return nil
}

// Apply server-side-applies one planned object.
func (s *KubeStorage) Apply(ctx context.Context, obj StorageObject) error {
	if obj.apply == nil {
		return fmt.Errorf("nothing to apply for %s %s", obj.Kind, obj.Name)
	}
	if err := obj.apply(ctx); err != nil {
		return fmt.Errorf("apply %s %s: %w", obj.Kind, obj.Name, err)
	}
	return nil
}

// Delete removes a volume this run created. Only volumes: a claim is harmless
// and rebound by the next apply, while a PV would be left backing a box that
// never came up.
func (s *KubeStorage) Delete(ctx context.Context, obj StorageObject) error {
	if obj.Kind != KindPersistentVolume {
		return fmt.Errorf("refusing to delete %s %s: box only removes a volume it created in this run", obj.Kind, obj.Name)
	}
	if err := s.cs.CoreV1().PersistentVolumes().Delete(ctx, obj.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PersistentVolume %s: %w", obj.Name, err)
	}
	return nil
}

// checkHostPaths refuses missing host directories: box does not create them
// (`box init` does), and an empty volume under a path meant to be filled is
// worse. Skipped under --paths-in-node.
func checkHostPaths(vols []seedbox.Volume) error {
	for _, v := range vols {
		info, err := os.Stat(v.Path)
		if err != nil {
			return fmt.Errorf("storage.%s.path %s: %w; run `box init` to create the host directories, then apply again", v.Kind, v.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("storage.%s.path %s is not a directory", v.Kind, v.Path)
		}
	}
	return nil
}

// persistentVolume is one host directory, one claim, Retain, no storage class.
// The apply configuration claims exactly the fields box writes.
func (s *KubeStorage) persistentVolume(namespace string, v seedbox.Volume) *coreapply.PersistentVolumeApplyConfiguration {
	return coreapply.PersistentVolume(v.Claim).
		WithLabels(map[string]string{kube.ManagedByLabel: kube.FieldManager}).
		WithSpec(coreapply.PersistentVolumeSpec().
			WithCapacity(capacity()).
			WithAccessModes(corev1.ReadWriteMany).
			WithPersistentVolumeReclaimPolicy(corev1.PersistentVolumeReclaimRetain).
			WithStorageClassName("").
			WithVolumeMode(corev1.PersistentVolumeFilesystem).
			WithClaimRef(coreapply.ObjectReference().WithNamespace(namespace).WithName(v.Claim)).
			WithHostPath(coreapply.HostPathVolumeSource().WithPath(v.Path).WithType(corev1.HostPathDirectory)))
}

// persistentVolumeClaim pins the claim to its PV by name.
func (s *KubeStorage) persistentVolumeClaim(namespace string, v seedbox.Volume) *coreapply.PersistentVolumeClaimApplyConfiguration {
	return coreapply.PersistentVolumeClaim(v.Claim, namespace).
		WithLabels(map[string]string{kube.ManagedByLabel: kube.FieldManager}).
		WithSpec(coreapply.PersistentVolumeClaimSpec().
			WithAccessModes(corev1.ReadWriteMany).
			WithStorageClassName("").
			WithVolumeName(v.Claim).
			WithResources(coreapply.VolumeResourceRequirements().WithRequests(capacity())))
}

// capacity is the nominal size both objects carry. See NominalCapacity.
func capacity() corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(NominalCapacity)}
}

var _ StorageClient = (*KubeStorage)(nil)
