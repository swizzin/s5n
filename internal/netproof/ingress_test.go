package netproof

// ingress_test.go proves the ingress proof's four answers (enforced, ignored,
// the pods cannot reach each other, a pod that never ran) and that nothing it
// wrote outlives the run.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func ingressOptions() IngressOptions {
	return IngressOptions{
		Namespace: "seedbox",
		Image:     "ghcr.io/swizzin/s5n:0.5.0@sha256:" + strings.Repeat("0", 64),
		Timeout:   0,
		Poll:      time.Millisecond,
	}
}

// listeners is two targets that come up with addresses.
func listeners() []scripted {
	return []scripted{{listens: true}, {listens: true}}
}

// TestIngressHoldsWhenTheDeniedTargetIsUnreachable: a pod reaches the target no
// policy selects and cannot reach the one the policy denies.
func TestIngressHoldsWhenTheDeniedTargetIsUnreachable(t *testing.T) {
	script := append(listeners(), exits(exitAsAsked, exitAsAsked)...)
	f := &fakePods{script: script, cniPods: []string{"kindnet-abc"}}
	rep, err := ProveIngress(context.Background(), f, ingressOptions())
	if err != nil {
		t.Fatalf("ProveIngress: %v", err)
	}
	if !rep.ControlReached || rep.ProbeReached {
		t.Errorf("report = %+v, want the control reachable and the denied target unreachable", rep)
	}
	if rep.CNI != "kindnet" || rep.Port != IngressPort {
		t.Errorf("report = %+v, want kindnet on the proof's port", rep)
	}
	if len(f.deleted) != 4 {
		t.Errorf("deleted %v, want both targets and both clients", f.deleted)
	}
	if len(f.deletedPolicies) != 1 {
		t.Errorf("deleted %v, want the deny policy too", f.deletedPolicies)
	}
}

// TestIngressRefusesAClusterThatDoesNotEnforce: a pod reached a target a deny-all
// policy selects, and every app behind such a policy is then open to the cluster.
func TestIngressRefusesAClusterThatDoesNotEnforce(t *testing.T) {
	script := append(listeners(), exits(exitAsAsked, exitOpposite)...)
	f := &fakePods{script: script, cniPods: []string{"kindnet-abc"}}
	rep, err := ProveIngress(context.Background(), f, ingressOptions())
	if err == nil {
		t.Fatal("ProveIngress accepted a cluster that reached a target through a deny-all policy")
	}
	for _, want := range []string{
		"does not enforce NetworkPolicy",
		"kindnet",
		"seedbox",
		"10.244.0.2:" + fmt.Sprint(IngressPort),
		"Calico",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to contain %q", err, want)
		}
	}
	if !rep.ProbeReached || !rep.ControlReached {
		t.Errorf("report = %+v, want both dials reported", rep)
	}
	if len(f.deleted) != 4 || len(f.deletedPolicies) != 1 {
		t.Errorf("deleted %v and policies %v, want everything the proof wrote removed", f.deleted, f.deletedPolicies)
	}
}

// TestIngressSaysThePodsCouldNotReachEachOther: the control's failure is this
// network's silence, never a finding about the CNI.
func TestIngressSaysThePodsCouldNotReachEachOther(t *testing.T) {
	script := append(listeners(), exits(exitOpposite)...)
	f := &fakePods{script: script, cniPods: []string{"kindnet-abc"}}
	_, err := ProveIngress(context.Background(), f, ingressOptions())
	if err == nil {
		t.Fatal("ProveIngress accepted a control that reached nothing")
	}
	for _, want := range []string{"could not run", "pods could not reach each other", "10.244.0.1:18080"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "does not enforce") {
		t.Errorf("refusal = %v, and the control failed: that must not read as a finding about the CNI", err)
	}
	if len(f.deleted) != 3 {
		t.Errorf("deleted %v, want both targets and the control client", f.deleted)
	}
}

// TestIngressCleansUpWhatItWrote: every path, a failure included, removes the
// pods and the policy.
func TestIngressCleansUpWhatItWrote(t *testing.T) {
	// The target cannot start.
	f := &fakePods{script: []scripted{{waiting: "ErrImagePull"}}}
	_, err := ProveIngress(context.Background(), f, ingressOptions())
	if err == nil {
		t.Fatal("ProveIngress accepted a target that never listened")
	}
	if !strings.Contains(err.Error(), "could not run") {
		t.Errorf("refusal = %v, want it to say the proof could not run", err)
	}
	if len(f.deleted) != 1 {
		t.Errorf("deleted %v, want the target that was created", f.deleted)
	}

	// A cancelled run: the target never reports an address, and box gives up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f = &fakePods{script: []scripted{{listens: true, addrAfter: 50}}}
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	_, err = ProveIngress(ctx, f, ingressOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("refusal = %v, want the cancelled context", err)
	}
	if len(f.deleted) != 1 {
		t.Errorf("deleted %v, want the target deleted when box was interrupted", f.deleted)
	}
	if len(f.policies) != 0 || len(f.deletedPolicies) != 0 {
		t.Errorf("policies %v written, %v deleted, want none written before the targets came up", f.policies, f.deletedPolicies)
	}

	// The second target cannot be written: the first is still removed.
	f = &fakePods{script: listeners(), createErr: errors.New("forbidden"), createErrOn: "s5n-probe-denied-target-"}
	if _, err := ProveIngress(context.Background(), f, ingressOptions()); err == nil {
		t.Fatal("ProveIngress accepted a target it could not create")
	}
	if len(f.deleted) != 1 || !strings.HasPrefix(f.deleted[0], "s5n-probe-control-target-") {
		t.Errorf("deleted %v, want the control target", f.deleted)
	}

	// The policy cannot be written.
	f = &fakePods{script: listeners(), policyErr: errors.New("forbidden")}
	if _, err := ProveIngress(context.Background(), f, ingressOptions()); err == nil {
		t.Fatal("ProveIngress accepted a policy it could not write")
	}
	if len(f.deleted) != 2 {
		t.Errorf("deleted %v, want both targets", f.deleted)
	}
}

// TestIngressPolicyDeniesEveryIngressToItsTarget: the object the proof writes is
// a deny-all to the one pod it selects, in the namespace the apps will run in.
func TestIngressPolicyDeniesEveryIngressToItsTarget(t *testing.T) {
	script := append(listeners(), exits(exitAsAsked, exitAsAsked)...)
	f := &fakePods{script: script}
	if _, err := ProveIngress(context.Background(), f, ingressOptions()); err != nil {
		t.Fatalf("ProveIngress: %v", err)
	}
	if len(f.policies) != 1 {
		t.Fatalf("wrote %d policies, want 1", len(f.policies))
	}
	p := f.policies[0]
	if p.Namespace != "seedbox" {
		t.Errorf("policy namespace = %q, want the seedbox's", p.Namespace)
	}
	if got := p.Spec.PodSelector.MatchLabels[proofLabel]; got != roleDeniedTarget {
		t.Errorf("policy selects %q, want the denied target only", got)
	}
	if len(p.Spec.Ingress) != 0 {
		t.Errorf("policy allows %d ingress rules, want none", len(p.Spec.Ingress))
	}
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policy types = %v, want Ingress", p.Spec.PolicyTypes)
	}
}

// TestIngressPodsListenAndDialTheTargets: the targets listen on the proof's port,
// each client dials its own target with the answer it expects, and every pod
// passes restricted and the image rule.
func TestIngressPodsListenAndDialTheTargets(t *testing.T) {
	// The first target takes two status reads to report an address: a client must
	// never be written with an empty one.
	script := []scripted{{listens: true, addrAfter: 2}, {listens: true}}
	script = append(script, exits(exitAsAsked, exitAsAsked)...)
	f := &fakePods{script: script}
	rep, err := ProveIngress(context.Background(), f, ingressOptions())
	if err != nil {
		t.Fatalf("ProveIngress: %v", err)
	}
	if rep.Port != IngressPort {
		t.Errorf("report port = %d, want %d", rep.Port, IngressPort)
	}
	if len(f.created) != 4 {
		t.Fatalf("created %d pods, want 2 targets and 2 clients", len(f.created))
	}
	controlTarget, deniedTarget, controlClient, deniedClient := f.created[0], f.created[1], f.created[2], f.created[3]
	for _, target := range []*corev1.Pod{controlTarget, deniedTarget} {
		cmd := strings.Join(target.Spec.Containers[0].Command, " ")
		if !strings.Contains(cmd, "probe listen") || !strings.Contains(cmd, "--port 18080") {
			t.Errorf("target command = %q, want the listener on the proof's port", cmd)
		}
	}
	if got := strings.Join(controlClient.Spec.Containers[0].Command, " "); !strings.Contains(got, "--address 10.244.0.1:18080") || !strings.Contains(got, "--want open") {
		t.Errorf("control client command = %q, want the first target and --want open", got)
	}
	if got := strings.Join(deniedClient.Spec.Containers[0].Command, " "); !strings.Contains(got, "--address 10.244.0.2:18080") || !strings.Contains(got, "--want closed") {
		t.Errorf("denied client command = %q, want the second target and --want closed", got)
	}
	if controlTarget.Labels[proofLabel] == deniedTarget.Labels[proofLabel] {
		t.Errorf("both targets carry %q: the policy selects both", controlTarget.Labels[proofLabel])
	}
	for _, pod := range f.created {
		assertRestricted(t, pod)
	}
}

// TestIngressRefusesOptionsItCannotUse: no namespace or no image is refused
// before any pod exists.
func TestIngressRefusesOptionsItCannotUse(t *testing.T) {
	for _, o := range []IngressOptions{
		{Image: "s5n:test"},
		{Namespace: "seedbox"},
	} {
		f := &fakePods{}
		if _, err := ProveIngress(context.Background(), f, o); err == nil {
			t.Errorf("ProveIngress accepted %+v", o)
		}
		if len(f.created) != 0 {
			t.Errorf("created %v before refusing %+v", f.created, o)
		}
	}
}
