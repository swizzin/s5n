package release

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/swizzin/s5n/internal/seedbox"
)

var testVolumes = []seedbox.Volume{
	{Kind: "data", Path: "/srv/s5n/data", Claim: "testbox-s5n-data"},
	{Kind: "config", Path: "/srv/s5n/config", Claim: "testbox-s5n-config"},
}

// TestKubeStoragePlansAndWritesTheStaticVolume is the real shape of what box
// creates: a hostPath PV bound to exactly one claim, and a claim pinned to it.
// Both carry no storage class, and the size is the nominal one.
func TestKubeStoragePlansAndWritesTheStaticVolume(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	s := NewStorageFor("testbox", cs)

	plan, err := s.Plan(ctx, "testbox", testVolumes)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan) != 4 {
		t.Fatalf("plan = %+v, want two PVs and two claims", plan)
	}
	for _, obj := range plan {
		if err := s.Apply(ctx, obj); err != nil {
			t.Fatalf("Apply %s %s: %v", obj.Kind, obj.Name, err)
		}
	}

	pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, "testbox-s5n-data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PV: %v", err)
	}
	if pv.Spec.HostPath == nil || pv.Spec.HostPath.Path != "/srv/s5n/data" {
		t.Errorf("hostPath = %+v, want /srv/s5n/data", pv.Spec.HostPath)
	}
	if pv.Spec.HostPath != nil && (pv.Spec.HostPath.Type == nil || *pv.Spec.HostPath.Type != corev1.HostPathDirectory) {
		t.Errorf("hostPath type = %v, want Directory", pv.Spec.HostPath.Type)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("reclaim = %q, want Retain", pv.Spec.PersistentVolumeReclaimPolicy)
	}
	if pv.Spec.StorageClassName != "" {
		t.Errorf("storageClassName = %q, want empty", pv.Spec.StorageClassName)
	}
	if len(pv.Spec.AccessModes) != 1 || pv.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("accessModes = %v, want ReadWriteMany", pv.Spec.AccessModes)
	}
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != "testbox" || pv.Spec.ClaimRef.Name != "testbox-s5n-data" {
		t.Errorf("claimRef = %+v, want testbox/testbox-s5n-data", pv.Spec.ClaimRef)
	}
	if got := pv.Labels[ManagedByLabel]; got != "box" {
		t.Errorf("label %s = %q, want box", ManagedByLabel, got)
	}
	if pv.Spec.Capacity.Storage().String() != NominalCapacity {
		t.Errorf("capacity = %s, want the nominal %s", pv.Spec.Capacity.Storage(), NominalCapacity)
	}

	pvc, err := cs.CoreV1().PersistentVolumeClaims("testbox").Get(ctx, "testbox-s5n-data", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	if pvc.Spec.VolumeName != "testbox-s5n-data" {
		t.Errorf("volumeName = %q, want the PV's name", pvc.Spec.VolumeName)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" {
		t.Errorf("storageClassName = %v, want an explicit empty string", pvc.Spec.StorageClassName)
	}
	if pvc.Spec.Resources.Requests.Storage().String() != NominalCapacity {
		t.Errorf("request = %s, want the nominal %s", pvc.Spec.Resources.Requests.Storage(), NominalCapacity)
	}
	if got := pvc.Labels[ManagedByLabel]; got != "box" {
		t.Errorf("label %s = %q, want box", ManagedByLabel, got)
	}

	// A second Plan over what box just wrote is empty: a re-apply is no writes.
	again, err := s.Plan(ctx, "testbox", testVolumes)
	if err != nil {
		t.Fatalf("second Plan: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second plan = %+v, want nothing to do", again)
	}
}

// TestKubeStorageRefusesAnotherPath: a PV of box's name that backs a different
// directory is never re-pointed. The error names both paths, and Plan wrote
// nothing -- including the claim for the volume it did not refuse.
func TestKubeStorageRefusesAnotherPath(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	existing := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "testbox-s5n-data"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/srv/old/data", Type: ptr(corev1.HostPathDirectory)},
			},
			ClaimRef: &corev1.ObjectReference{Namespace: "testbox", Name: "testbox-s5n-data"},
		},
	}
	if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, existing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := NewStorageFor("testbox", cs).Plan(ctx, "testbox", testVolumes)
	if err == nil {
		t.Fatal("Plan accepted a PV that backs another path")
	}
	if !strings.Contains(err.Error(), "/srv/old/data") || !strings.Contains(err.Error(), "/srv/s5n/data") {
		t.Fatalf("error does not name both paths: %v", err)
	}
	claims, err := cs.CoreV1().PersistentVolumeClaims("testbox").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 {
		t.Fatalf("a refused plan wrote %d claims", len(claims.Items))
	}
}

// TestKubeStorageRefusesAnotherClaim: the volume is bound to someone else's
// claim, so it is not box's to reuse.
func TestKubeStorageRefusesAnotherClaim(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	existing := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "testbox-s5n-data"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/srv/s5n/data", Type: ptr(corev1.HostPathDirectory)},
			},
			ClaimRef: &corev1.ObjectReference{Namespace: "other", Name: "other-data"},
		},
	}
	if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, existing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := NewStorageFor("testbox", cs).Plan(ctx, "testbox", testVolumes)
	if err == nil {
		t.Fatal("Plan accepted a PV bound to another claim")
	}
	for _, want := range []string{"other/other-data", "testbox/testbox-s5n-data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestKubeStorageRefusesABoundClaim: a claim that already names another volume
// is never rebound; box names the claim it creates and nothing else.
func TestKubeStorageRefusesABoundClaim(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "testbox-s5n-data", Namespace: "testbox"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "someone-elses-pv"},
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("testbox").Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := NewStorageFor("testbox", cs).Plan(ctx, "testbox", testVolumes)
	if err == nil {
		t.Fatal("Plan accepted a claim bound to another volume")
	}
	if !strings.Contains(err.Error(), "someone-elses-pv") || !strings.Contains(err.Error(), "testbox-s5n-data") {
		t.Fatalf("error does not name both volumes: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
