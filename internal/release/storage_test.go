package release

import (
	"context"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/swizzin/s5n/internal/kube"
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
	if got := pv.Labels[kube.ManagedByLabel]; got != "box" {
		t.Errorf("label %s = %q, want box", kube.ManagedByLabel, got)
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
	if got := pvc.Labels[kube.ManagedByLabel]; got != "box" {
		t.Errorf("label %s = %q, want box", kube.ManagedByLabel, got)
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

// TestKubeStorageDeletesOnlyAVolumeItWouldRemove: undo is narrow on purpose. A
// volume goes; a claim is refused rather than guessed at.
func TestKubeStorageDeletesOnlyAVolumeItWouldRemove(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	s := NewStorageFor("testbox", cs)
	if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "testbox-s5n-data"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, StorageObject{Kind: KindPersistentVolumeClaim, Name: "testbox-s5n-data"}); err == nil {
		t.Fatal("Delete accepted a claim")
	}
	if _, err := cs.CoreV1().PersistentVolumes().Get(ctx, "testbox-s5n-data", metav1.GetOptions{}); err != nil {
		t.Fatalf("a refused delete removed the volume: %v", err)
	}

	if err := s.Delete(ctx, StorageObject{Kind: KindPersistentVolume, Name: "testbox-s5n-data"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumes().Get(ctx, "testbox-s5n-data", metav1.GetOptions{}); err == nil {
		t.Fatal("the volume is still there")
	}
}

// TestKubeStorageDeletingAGoneVolumeIsNotAnError: a volume someone removed
// between the write and the failure is already the state box wants.
func TestKubeStorageDeletingAGoneVolumeIsNotAnError(t *testing.T) {
	ctx := context.Background()
	if err := NewStorageFor("testbox", fake.NewClientset()).Delete(ctx,
		StorageObject{Kind: KindPersistentVolume, Name: "testbox-s5n-data"}); err != nil {
		t.Fatalf("Delete of a missing volume: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// reviewAnswers answers the access review with one verdict. The TypeMeta is
// what the API server sends back; the fake tracker needs it to decode the
// object it returns.
func reviewAnswers(cs *fake.Clientset, allowed bool) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"},
			Status:   authv1.SubjectAccessReviewStatus{Allowed: allowed},
		}, nil
	})
}

// allowNamespaceCreate answers the access review the way root's kubeconfig
// does.
func allowNamespaceCreate(cs *fake.Clientset) { reviewAnswers(cs, true) }

// TestKubeStorageCreatesTheNamespaceItNeeds: a fresh cluster has no namespace,
// so box asks whether it may create one, creates it with the restricted labels
// and its own managed-by label, and then plans the namespace as present.
func TestKubeStorageCreatesTheNamespaceItNeeds(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	allowNamespaceCreate(cs)
	s := NewStorageFor("testbox", cs)

	plan, err := s.PlanNamespace(ctx)
	if err != nil {
		t.Fatalf("PlanNamespace: %v", err)
	}
	if plan.Exists || !plan.CanCreate {
		t.Fatalf("plan = %+v, want a missing namespace this identity may create", plan)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "testbox", metav1.GetOptions{}); err == nil {
		t.Fatal("planning the namespace created it")
	}

	if err := s.CreateNamespace(ctx); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	ns, err := cs.CoreV1().Namespaces().Get(ctx, "testbox", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	for label, want := range map[string]string{
		podSecurityEnforce:  "restricted",
		podSecurityAudit:    "restricted",
		podSecurityWarn:     "restricted",
		kube.ManagedByLabel: kube.FieldManager,
	} {
		if got := ns.Labels[label]; got != want {
			t.Errorf("label %s = %q, want %q", label, got, want)
		}
	}

	plan, err = s.PlanNamespace(ctx)
	if err != nil {
		t.Fatalf("second PlanNamespace: %v", err)
	}
	if !plan.Exists || plan.Enforce != podSecurityRestricted {
		t.Fatalf("plan = %+v, want an existing namespace at restricted", plan)
	}
}

// TestKubeStorageWillNotCreateANamespaceWithoutPermission: the access review is
// the gate, and a denial is a plain answer -- no error, nothing created. This
// is a user's kubeconfig, which is namespace-scoped.
func TestKubeStorageWillNotCreateANamespaceWithoutPermission(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	reviewAnswers(cs, false)
	s := NewStorageFor("testbox", cs)

	plan, err := s.PlanNamespace(ctx)
	if err != nil {
		t.Fatalf("PlanNamespace: %v", err)
	}
	if plan.Exists || plan.CanCreate {
		t.Fatalf("plan = %+v, want a missing namespace this identity may not create", plan)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "testbox", metav1.GetOptions{}); err == nil {
		t.Fatal("a refused plan created the namespace")
	}
}

// TestKubeStorageReportsAWeakNamespaceAndLeavesItAlone: enforce is what admits
// a pod, so a namespace enforcing baseline is weaker than the chart assumes.
// Planning it changes nothing; the labels are still the admin's.
func TestKubeStorageReportsAWeakNamespaceAndLeavesItAlone(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "testbox",
			Labels: map[string]string{podSecurityEnforce: "baseline"},
		},
	}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, existing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	plan, err := NewStorageFor("testbox", cs).PlanNamespace(ctx)
	if err != nil {
		t.Fatalf("PlanNamespace: %v", err)
	}
	if !plan.Exists || plan.Enforce != "baseline" {
		t.Fatalf("plan = %+v, want an existing namespace enforcing baseline", plan)
	}
	ns, err := cs.CoreV1().Namespaces().Get(ctx, "testbox", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ns.Labels); got != 1 || ns.Labels[podSecurityEnforce] != "baseline" {
		t.Fatalf("labels = %v, want the admin's one label untouched", ns.Labels)
	}
}

// TestKubeStorageReportsANamespaceWithNoEnforceLabel: an unlabelled namespace
// admits anything, so it is weaker too and apply warns.
func TestKubeStorageReportsANamespaceWithNoEnforceLabel(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "testbox"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	plan, err := NewStorageFor("testbox", cs).PlanNamespace(ctx)
	if err != nil {
		t.Fatalf("PlanNamespace: %v", err)
	}
	if !plan.Exists || plan.Enforce != "" {
		t.Fatalf("plan = %+v, want an existing namespace with no enforce label", plan)
	}
}
