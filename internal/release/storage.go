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
	"k8s.io/client-go/tools/clientcmd"

	"github.com/swizzin/s5n/internal/seedbox"
)

// NominalCapacity is the size box writes on every PersistentVolume and claim.
//
// A hostPath volume has no size: the filesystem is the limit, and the API
// requires a number anyway. Nothing enforces this one -- there is no quota, no
// resize, and `box` never reads it back as a limit.
const NominalCapacity = "1Ti"

// StorageFieldManager is the server-side-apply field manager for the PVs and
// claims box creates, so `kubectl apply --field-manager=box` and box agree about
// who owns which field.
const StorageFieldManager = "box"

// ManagedByLabel is how an operator (and the e2e) can find the storage box
// created. It is not used to decide ownership: what a PV backs is its hostPath
// and claimRef, not a label anyone can edit.
const ManagedByLabel = "app.kubernetes.io/managed-by"

// StorageObject is one PV or claim box has to write, as Plan found it. The
// fields are what box prints; the apply is what box runs after the operator
// says yes.
type StorageObject struct {
	Kind   string // "PersistentVolume" or "PersistentVolumeClaim"
	Name   string
	Detail string // e.g. "hostPath /srv/s5n/data"
	apply  func(ctx context.Context) error
}

// StorageClient is box's handle on the storage a seedbox needs. Plan only
// reads: every refusal -- a PV that backs another path, a claim bound to
// another volume -- happens there, before anything is written. Apply writes one
// planned object with server-side apply.
type StorageClient interface {
	Plan(ctx context.Context, namespace string, vols []seedbox.Volume) ([]StorageObject, error)
	Apply(ctx context.Context, obj StorageObject) error
}

// KubeStorage is the client-go implementation: cluster-scoped PVs and the
// namespace's claims.
type KubeStorage struct {
	namespace string
	cs        kubernetes.Interface
}

// NewStorage builds the storage client for one namespace. Like NewHelm it is
// lazy: no cluster call happens until Plan.
func NewStorage(kubeconfig, namespace string) (*KubeStorage, error) {
	if namespace == "" {
		return nil, fmt.Errorf("no namespace: the grant names the release namespace")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
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

// Plan compares the volumes with the cluster. It returns only what has to be
// created, in data-then-config order, and never writes: a conflict on the
// second volume must not leave the first one applied.
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

// planVolume is one volume's PV and claim. An existing PV is never mutated: box
// either finds the one it would have made -- same path, same single claim -- or
// refuses and says what it found.
func (s *KubeStorage) planVolume(ctx context.Context, namespace string, v seedbox.Volume) ([]StorageObject, error) {
	var out []StorageObject

	pv := s.persistentVolume(namespace, v)
	live, err := s.cs.CoreV1().PersistentVolumes().Get(ctx, v.Claim, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out = append(out, StorageObject{
			Kind:   "PersistentVolume",
			Name:   v.Claim,
			Detail: "hostPath " + v.Path,
			apply: func(ctx context.Context) error {
				_, err := s.cs.CoreV1().PersistentVolumes().Apply(ctx, pv, metav1.ApplyOptions{FieldManager: StorageFieldManager})
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
			Kind:   "PersistentVolumeClaim",
			Name:   v.Claim,
			Detail: "volume " + v.Claim,
			apply: func(ctx context.Context) error {
				_, err := s.cs.CoreV1().PersistentVolumeClaims(namespace).Apply(ctx, pvc, metav1.ApplyOptions{FieldManager: StorageFieldManager})
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

// checkPersistentVolume refuses a PV of box's name that backs something else.
// The comparison is the path and the single claim, which is the whole contract
// of a static volume: a bound PV's path is never changed in place. Both paths
// are in the error, because "which one is it actually using" is the only
// question the operator has.
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

// checkHostPaths refuses a grant whose host directories are not there. box does
// not create them -- `box init` does, and it reports what it found -- so a
// missing path here means the release would come up with an empty volume under
// a path the operator meant to fill. The check is skipped only for a path that
// lives inside a test node (`--paths-in-node`), where this host cannot see it.
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

// persistentVolume is the static PV box creates for one volume: one host
// directory, one claim, reclaim Retain, no storage class. It is a server-side
// apply configuration, so the fields box claims are exactly the ones it would
// write back; nothing else on the object is touched.
func (s *KubeStorage) persistentVolume(namespace string, v seedbox.Volume) *coreapply.PersistentVolumeApplyConfiguration {
	return coreapply.PersistentVolume(v.Claim).
		WithLabels(map[string]string{ManagedByLabel: StorageFieldManager}).
		WithSpec(coreapply.PersistentVolumeSpec().
			WithCapacity(capacity()).
			WithAccessModes(corev1.ReadWriteMany).
			WithPersistentVolumeReclaimPolicy(corev1.PersistentVolumeReclaimRetain).
			WithStorageClassName("").
			WithVolumeMode(corev1.PersistentVolumeFilesystem).
			WithClaimRef(coreapply.ObjectReference().WithNamespace(namespace).WithName(v.Claim)).
			WithHostPath(coreapply.HostPathVolumeSource().WithPath(v.Path).WithType(corev1.HostPathDirectory)))
}

// persistentVolumeClaim pins the claim to its PV by name, so nothing but this
// exact volume can satisfy it.
func (s *KubeStorage) persistentVolumeClaim(namespace string, v seedbox.Volume) *coreapply.PersistentVolumeClaimApplyConfiguration {
	return coreapply.PersistentVolumeClaim(v.Claim, namespace).
		WithLabels(map[string]string{ManagedByLabel: StorageFieldManager}).
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
