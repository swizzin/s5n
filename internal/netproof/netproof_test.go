package netproof

// netproof_test.go scripts the pods' answers, so every branch is tested without a
// cluster; the pods themselves are asserted on the objects the proof creates.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scripted is one created pod's answer.
type scripted struct {
	// exit is the container's exit code, when it ends.
	exit int32
	// listens makes the pod a target: running, never ending. addrAfter is how
	// many status reads it takes to report an address (0: the create answer has one).
	listens   bool
	addrAfter int
	// waiting is a container that cannot start.
	waiting string
}

// fakePods answers one created pod per scripted answer, and records what was
// created and deleted.
type fakePods struct {
	script  []scripted
	created []*corev1.Pod
	deleted []string
	// gets counts each pod's status reads, for the listeners' addresses.
	gets    map[string]int
	cniPods []string
	// dnsServices are kube-system's Services, for DNSTarget.
	dnsServices []corev1.Service
	next        int
}

func (f *fakePods) Services(_ context.Context, _ string) (*corev1.ServiceList, error) {
	return &corev1.ServiceList{Items: f.dnsServices}, nil
}

func (f *fakePods) Create(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if f.next >= len(f.script) {
		return nil, fmt.Errorf("unexpected pod %q: the test scripted %d", pod.GenerateName, len(f.script))
	}
	s := f.script[f.next]
	f.next++
	p := pod.DeepCopy()
	p.Name = fmt.Sprintf("%s%d", pod.GenerateName, len(f.created)+1)
	switch {
	case s.listens:
		p.Status.Phase = corev1.PodRunning
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "probe",
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		if s.addrAfter == 0 {
			p.Status.PodIP = podIP(len(f.created) + 1)
		}
	case s.waiting != "":
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "probe",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: s.waiting}},
		}}
	default:
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "probe",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: s.exit}},
		}}
	}
	f.created = append(f.created, p)
	return p.DeepCopy(), nil
}

// podIP is one target's address, so a client dials the pod it was meant to.
func podIP(n int) string { return fmt.Sprintf("10.244.0.%d", n) }

func (f *fakePods) Get(_ context.Context, _, name string) (*corev1.Pod, error) {
	for i, p := range f.created {
		if p.Name != name {
			continue
		}
		if f.gets == nil {
			f.gets = map[string]int{}
		}
		f.gets[name]++
		if p.Status.PodIP == "" && f.gets[name] >= f.script[i].addrAfter {
			p.Status.PodIP = podIP(i + 1)
		}
		return p, nil
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

// exits scripts pods that end with the given codes.
func exits(codes ...int32) []scripted {
	out := make([]scripted, 0, len(codes))
	for _, code := range codes {
		out = append(out, scripted{exit: code})
	}
	return out
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

// TestEgressHoldsWhenTheLabelledPodIsDenied: the control reaches the address and
// the labelled pod does not.
func TestEgressHoldsWhenTheLabelledPodIsDenied(t *testing.T) {
	f := &fakePods{script: exits(exitAsAsked, exitAsAsked), cniPods: []string{"kindnet-abc"}}
	rep, err := ProveEgress(context.Background(), f, options())
	if err != nil {
		t.Fatalf("ProveEgress: %v", err)
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

// TestEgressRefusesAClusterThatDoesNotEnforce: the labelled pod reached what the
// policy denies, so a rehearsal would run live.
func TestEgressRefusesAClusterThatDoesNotEnforce(t *testing.T) {
	f := &fakePods{script: exits(exitAsAsked, exitOpposite), cniPods: []string{"kindnet-abc"}}
	rep, err := ProveEgress(context.Background(), f, options())
	if err == nil {
		t.Fatal("ProveEgress accepted a cluster whose labelled pod reached the target")
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

// TestEgressSaysTheProofCouldNotRun: a control that cannot reach the address
// means the probe's failure proves nothing, and the refusal must say that.
func TestEgressSaysTheProofCouldNotRun(t *testing.T) {
	f := &fakePods{script: exits(exitOpposite)}
	_, err := ProveEgress(context.Background(), f, options())
	if err == nil {
		t.Fatal("ProveEgress accepted a control pod that could not reach the target")
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

// TestEgressRefusesWhenTheProbeCannotRun: a pod that never ran is not a lock.
func TestEgressRefusesWhenTheProbeCannotRun(t *testing.T) {
	f := &fakePods{script: exits(exitAsAsked, exitBroken)}
	_, err := ProveEgress(context.Background(), f, options())
	if err == nil {
		t.Fatal("ProveEgress accepted a probe pod that never ran")
	}
	for _, want := range []string{"could not be proven", "the probe pod exited"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to contain %q", err, want)
		}
	}
}

// TestEgressPodsAreTheOnesTheClusterWillAdmit: the control carries no release
// label, the probe carries it, and both pass restricted and the image policy.
func TestEgressPodsAreTheOnesTheClusterWillAdmit(t *testing.T) {
	f := &fakePods{script: exits(exitAsAsked, exitAsAsked)}
	if _, err := ProveEgress(context.Background(), f, options()); err != nil {
		t.Fatalf("ProveEgress: %v", err)
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
		assertRestricted(t, pod)
	}
	if got := control.Spec.Containers[0].Command[6]; got != "open" {
		t.Errorf("control --want = %q, want open", got)
	}
	if got := probe.Spec.Containers[0].Command[6]; got != "closed" {
		t.Errorf("probe --want = %q, want closed", got)
	}
}

// assertRestricted checks a probe pod against restricted and the image rule.
func assertRestricted(t *testing.T, pod *corev1.Pod) {
	t.Helper()
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
	if !strings.HasPrefix(c.Command[0], "/") {
		t.Errorf("%s command = %v, want the probe's own", pod.Name, c.Command)
	}
}

// TestEgressChecksAPublicAddress: the labelled pod reaching a public address means
// the policy is absent; failing to is what both a locked box and no internet look
// like, so it is only a note.
func TestEgressChecksAPublicAddress(t *testing.T) {
	o := options()
	o.Internet = Target{Host: "192.0.2.53", Port: 443}

	f := &fakePods{script: exits(exitAsAsked, exitAsAsked, exitOpposite)}
	rep, err := ProveEgress(context.Background(), f, o)
	if err == nil {
		t.Fatal("ProveEgress accepted a labelled pod that reached the public address")
	}
	if !rep.InternetReached || !strings.Contains(err.Error(), "192.0.2.53:443") {
		t.Errorf("refusal = %v, report = %+v", err, rep)
	}
	if len(f.deleted) != 3 {
		t.Errorf("deleted %v, want all three pods", f.deleted)
	}

	f = &fakePods{script: exits(exitAsAsked, exitAsAsked, exitAsAsked)}
	rep, err = ProveEgress(context.Background(), f, o)
	if err != nil {
		t.Fatalf("ProveEgress with the public check closed: %v", err)
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

// TestDNSTargetIsTheDNSServiceMetricsPort: a Service address outlives any CoreDNS
// pod, whose departure would read as a closed connection. No metrics port, or no
// DNS Service, is a refusal rather than a proof with nothing to dial.
func TestDNSTargetIsTheDNSServiceMetricsPort(t *testing.T) {
	svc := func(name, ip string, ports ...int32) corev1.Service {
		s := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}},
			Spec:       corev1.ServiceSpec{ClusterIP: ip},
		}
		for _, p := range ports {
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Port: p})
		}
		return s
	}
	f := &fakePods{dnsServices: []corev1.Service{svc("kube-dns", "10.96.0.10", 53, 53, 9153)}}
	got, err := DNSTarget(context.Background(), f)
	if err != nil {
		t.Fatalf("DNSTarget: %v", err)
	}
	if got.String() != "10.96.0.10:9153" {
		t.Errorf("target = %s, want the DNS Service's metrics port", got)
	}

	noMetrics := &fakePods{dnsServices: []corev1.Service{svc("kube-dns", "10.96.0.10", 53)}}
	if _, err := DNSTarget(context.Background(), noMetrics); err == nil {
		t.Error("DNSTarget accepted a DNS Service with no metrics port")
	} else if !strings.Contains(err.Error(), "9153") {
		t.Errorf("refusal = %v, want it to name the port it looked for", err)
	}

	if _, err := DNSTarget(context.Background(), &fakePods{}); err == nil {
		t.Error("DNSTarget accepted a cluster with no DNS Service")
	} else if !strings.Contains(err.Error(), "cannot prove the lock") {
		t.Errorf("refusal = %v, want it to say the lock cannot be proven", err)
	}
}

// TestEgressPodsSettle: every probe pod dials with --settle, so a policy the CNI
// has not programmed yet is not read as a cluster that ignores it.
func TestEgressPodsSettle(t *testing.T) {
	o := Options{Namespace: "s5n", Release: "s5n", Image: "s5n:test", Target: Target{Host: "10.96.0.10", Port: 9153}, Settle: 20 * time.Second}
	cmd := strings.Join(egressPod(o, "probe", nil, o.Target, "closed").Spec.Containers[0].Command, " ")
	for _, want := range []string{"--settle 20s", "--want closed", "--address 10.96.0.10:9153", "--timeout 2s"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("probe command %q has no %q", cmd, want)
		}
	}
}

// TestParseTargetReadsWhatStringWrites: the grant's string and the proof's
// address must round-trip.
func TestParseTargetReadsWhatStringWrites(t *testing.T) {
	want := Target{Host: "10.96.0.10", Port: 9153}
	got, err := ParseTarget(want.String())
	if err != nil || got != want {
		t.Errorf("ParseTarget(%q) = %v, %v; want %v", want.String(), got, err, want)
	}
	for _, bad := range []string{"", "10.96.0.10", ":9153", "10.96.0.10:0", "10.96.0.10:x"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted it", bad)
		}
	}
}
