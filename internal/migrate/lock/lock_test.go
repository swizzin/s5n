package lock

// lock_test.go drives the proof with a fake clientset: the two pods' answers
// are scripted, so every branch — the lock holds, the cluster does not enforce
// it, the control could not run, the public check — is tested without a
// cluster. What the pods are is asserted on the objects the proof creates.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakePods hands back one scripted exit code per created pod, in order, already
// terminated; it records what was created and deleted, and names a CNI from
// kube-system.
type fakePods struct {
	exits   []int32
	created []*corev1.Pod
	deleted []string
	cniPods []string
	next    int
}

func (f *fakePods) Create(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if f.next >= len(f.exits) {
		return nil, fmt.Errorf("unexpected pod %q: the test scripted %d", pod.GenerateName, len(f.exits))
	}
	code := f.exits[f.next]
	f.next++
	p := pod.DeepCopy()
	p.Name = fmt.Sprintf("%s%d", pod.GenerateName, f.next)
	p.Status.Phase = corev1.PodSucceeded
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "probe",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
	}}
	f.created = append(f.created, p)
	return p, nil
}

func (f *fakePods) Get(_ context.Context, _, name string) (*corev1.Pod, error) {
	for _, p := range f.created {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("pod %q not found", name)
}

func (f *fakePods) Delete(_ context.Context, _, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakePods) List(_ context.Context, namespace string) (*corev1.PodList, error) {
	list := &corev1.PodList{}
	if namespace != "kube-system" {
		return list, nil
	}
	for _, name := range f.cniPods {
		list.Items = append(list.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	}
	return list, nil
}

func options() Options {
	return Options{
		Namespace: "seedbox",
		Release:   "s5n",
		Image:     "ghcr.io/swizzin/s5n:0.5.0@sha256:" + strings.Repeat("0", 64),
		Target:    Target{Host: "10.0.0.2", Port: 9153},
		Timeout:   0,
	}
}

// TestProveHoldsWhenTheLabelledPodIsDenied is the happy path: the control
// reaches the address and the pod carrying the release's label does not, so the
// policy selected it and denied it.
func TestProveHoldsWhenTheLabelledPodIsDenied(t *testing.T) {
	f := &fakePods{exits: []int32{exitAsAsked, exitAsAsked}, cniPods: []string{"kindnet-abc"}}
	rep, err := Prove(context.Background(), f, options())
	if err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if !rep.ControlReached || rep.ProbeReached {
		t.Errorf("report = %+v, want the control reachable and the probe denied", rep)
	}
	if rep.CNI != "kindnet" {
		t.Errorf("CNI = %q, want kindnet", rep.CNI)
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted %v, want both probe pods", f.deleted)
	}
}

// TestProveRefusesAClusterThatDoesNotEnforce is the refusal this whole package
// exists for: the labelled pod reached the address the policy denies, so a
// rehearsal would run live. It names the CNI when it can.
func TestProveRefusesAClusterThatDoesNotEnforce(t *testing.T) {
	f := &fakePods{exits: []int32{exitAsAsked, exitOpposite}, cniPods: []string{"kindnet-abc"}}
	rep, err := Prove(context.Background(), f, options())
	if err == nil {
		t.Fatal("Prove accepted a cluster whose labelled pod reached the target")
	}
	for _, want := range []string{
		"does not enforce NetworkPolicy",
		"kindnet",
		"app.kubernetes.io/instance=s5n",
		"10.0.0.2:9153",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to contain %q", err, want)
		}
	}
	if !rep.ProbeReached {
		t.Error("the report does not say the probe reached the target")
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted %v, want both pods deleted on the refusal too", f.deleted)
	}
}

// TestProveSaysTheProofCouldNotRun: a control that cannot reach the address
// means the probe's failure proves nothing — a namespace policy of its own, or
// a network that does not route, looks exactly like this. The refusal must say
// that, never that the lock works.
func TestProveSaysTheProofCouldNotRun(t *testing.T) {
	f := &fakePods{exits: []int32{exitOpposite}}
	_, err := Prove(context.Background(), f, options())
	if err == nil {
		t.Fatal("Prove accepted a control pod that could not reach the target")
	}
	for _, want := range []string{"could not be proven", "without app.kubernetes.io/instance=s5n", "could not reach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "does not enforce") {
		t.Errorf("refusal = %v, and the control failed: that must not read as a finding about the CNI", err)
	}
}

// TestProveRefusesWhenTheProbeCannotRun: an image that will not pull or a pod
// the namespace refuses is not "the lock held".
func TestProveRefusesWhenTheProbeCannotRun(t *testing.T) {
	f := &fakePods{exits: []int32{exitAsAsked, exitBroken}}
	_, err := Prove(context.Background(), f, options())
	if err == nil {
		t.Fatal("Prove accepted a probe pod that never ran")
	}
	for _, want := range []string{"could not be proven", "the probe pod exited"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to contain %q", err, want)
		}
	}
}

// TestProbePodsAreTheOnesTheClusterWillAdmit: the control carries no release
// label (so only a namespace-wide policy can select it), the probe carries the
// instance label and nothing that changes a selection, and both are the pods
// Pod Security "restricted" and the blessed-image policy admit.
func TestProbePodsAreTheOnesTheClusterWillAdmit(t *testing.T) {
	f := &fakePods{exits: []int32{exitAsAsked, exitAsAsked}}
	if _, err := Prove(context.Background(), f, options()); err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if len(f.created) != 2 {
		t.Fatalf("created %d pods, want 2", len(f.created))
	}
	control, probe := f.created[0], f.created[1]
	if _, ok := control.Labels["app.kubernetes.io/instance"]; ok {
		t.Errorf("the control pod carries the release's instance label: %v", control.Labels)
	}
	if probe.Labels["app.kubernetes.io/instance"] != "s5n" {
		t.Errorf("the probe pod does not carry the release's instance label: %v", probe.Labels)
	}
	if probe.Labels["s5n.io/role"] == "wiring" {
		t.Error("the probe pod looks like the wiring Job, whose egress policy admits the API server")
	}
	for _, pod := range f.created {
		spec := pod.Spec
		if spec.HostNetwork || spec.HostPID || spec.HostIPC {
			t.Errorf("%s uses a host namespace", pod.Name)
		}
		if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
			t.Errorf("%s mounts a service account token", pod.Name)
		}
		if spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("%s restarts: %s", pod.Name, spec.RestartPolicy)
		}
		if len(spec.Containers) != 1 {
			t.Fatalf("%s runs %d containers", pod.Name, len(spec.Containers))
		}
		c := spec.Containers[0]
		if c.Image != options().Image {
			t.Errorf("%s image = %q, want the release's own", pod.Name, c.Image)
		}
		sc := c.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s container security context = %+v", pod.Name, sc)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s drops %v, want ALL", pod.Name, sc.Capabilities)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s seccomp = %+v", pod.Name, sc.SeccompProfile)
		}
		if !contains(c.Command, "--want") {
			t.Errorf("%s command = %v, want the probe's own", pod.Name, c.Command)
		}
	}
	if control.Spec.Containers[0].Command[6] != "open" {
		t.Errorf("control --want = %q, want open", control.Spec.Containers[0].Command[6])
	}
	if probe.Spec.Containers[0].Command[6] != "closed" {
		t.Errorf("probe --want = %q, want closed", probe.Spec.Containers[0].Command[6])
	}
}

// TestProveChecksAPublicAddress: the second check only ever adds a failure.
// A public address the labelled pod reaches means the policy is absent; one it
// cannot reach is what a locked box and a host with no internet both look like,
// so the note says so and the run goes on.
func TestProveChecksAPublicAddress(t *testing.T) {
	o := options()
	o.Internet = Target{Host: "192.0.2.53", Port: 443}

	f := &fakePods{exits: []int32{exitAsAsked, exitAsAsked, exitOpposite}}
	rep, err := Prove(context.Background(), f, o)
	if err == nil {
		t.Fatal("Prove accepted a labelled pod that reached the public address")
	}
	if !rep.InternetReached || !strings.Contains(err.Error(), "192.0.2.53:443") {
		t.Errorf("refusal = %v, report = %+v", err, rep)
	}
	if len(f.deleted) != 3 {
		t.Errorf("deleted %v, want all three pods", f.deleted)
	}

	f = &fakePods{exits: []int32{exitAsAsked, exitAsAsked, exitAsAsked}}
	rep, err = Prove(context.Background(), f, o)
	if err != nil {
		t.Fatalf("Prove with the public check closed: %v", err)
	}
	if !rep.InternetChecked || rep.InternetReached {
		t.Errorf("report = %+v, want the public check run and closed", rep)
	}
	if !strings.Contains(rep.Note, "no internet") {
		t.Errorf("note = %q, want it to say a host without internet looks the same", rep.Note)
	}
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}
