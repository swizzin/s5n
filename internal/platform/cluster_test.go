package platform

import (
	"os"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/swizzin/s5n/internal/bootstrap"
)

// capturedStatus is the status Envoy Gateway 1.9.3 wrote for the login's
// SecurityPolicy, read from the cluster into testdata rather than built here:
// the point of these tests is the document a real controller writes.
func capturedStatus(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/securitypolicy-accepted.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := utilyaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	status, _ := doc["status"].(map[string]any)
	if status == nil {
		t.Fatalf("fixture carries no status: %v", doc)
	}
	return status
}

func gatewayTarget() PolicyTarget {
	return PolicyTarget{
		Group:     "gateway.networking.k8s.io",
		Kind:      "Gateway",
		Name:      GatewayName,
		Namespace: GatewayNamespace,
	}
}

// The captured document is the one that matters: acceptance lives under
// status.ancestors, and status.conditions is not set at all. A wait that read
// the top-level conditions would never see this policy accepted.
func TestPolicyAcceptedReadsTheCapturedStatus(t *testing.T) {
	status := capturedStatus(t)
	ok, why := PolicyAccepted(status, gatewayTarget())
	if !ok || why != "" {
		t.Fatalf("PolicyAccepted(captured) = %v, %q, want accepted", ok, why)
	}
	if _, set := status["conditions"]; set {
		t.Fatal("the fixture has top-level conditions: it is not the captured shape any more")
	}
}

// A status with only the top-level conditions is not an accepted policy:
// Envoy Gateway never writes one, and reading it would make the wait pass on a
// document that means nothing.
func TestPolicyAcceptedRefusesATopLevelCondition(t *testing.T) {
	status := map[string]any{"conditions": []any{map[string]any{
		"type": "Accepted", "status": "True", "reason": "Accepted",
	}}}
	if ok, _ := PolicyAccepted(status, gatewayTarget()); ok {
		t.Fatal("a policy with only status.conditions was read as accepted")
	}
	if ok, _ := PolicyAccepted(nil, gatewayTarget()); ok {
		t.Fatal("an empty status was read as accepted")
	}
}

// Transient then accepted, on the real shape: the ancestor is there with the
// condition False and a reason while the controller has not translated the
// policy, and the same document with True is the answer the wait stops on.
func TestPolicyAcceptedTransientThenAccepted(t *testing.T) {
	transient := capturedStatus(t)
	condition := ancestorCondition(t, transient)
	condition["status"] = "False"
	condition["reason"] = "Waiting for controller"
	condition["message"] = "The SecurityPolicy has not been picked up yet"

	ok, why := PolicyAccepted(transient, gatewayTarget())
	if ok {
		t.Fatal("a transient status was read as accepted")
	}
	if !strings.Contains(why, "Waiting for controller") || !strings.Contains(why, "has not been picked up yet") {
		t.Fatalf("reason = %q, want Envoy Gateway's reason and message", why)
	}
	if ok, _ := PolicyAccepted(capturedStatus(t), gatewayTarget()); !ok {
		t.Fatal("the captured Accepted status was not read as accepted")
	}
}

// The ancestorRef is the match, so the same policy is not accepted for another
// target: a listener-scoped policy is never satisfied by the Gateway's own
// ancestor entry, and another controller's entry never counts.
func TestPolicyAcceptedMatchesTheAncestorRef(t *testing.T) {
	listener := gatewayTarget()
	listener.SectionName = "http-box-example-test"
	if ok, _ := PolicyAccepted(capturedStatus(t), listener); ok {
		t.Fatal("the Gateway's ancestor entry was read as accepting a listener-scoped policy")
	}

	other := gatewayTarget()
	other.Name = "other"
	if ok, _ := PolicyAccepted(capturedStatus(t), other); ok {
		t.Fatal("another Gateway's ancestor entry was read as accepting this policy")
	}

	foreign := capturedStatus(t)
	foreignAncestor := ancestor(t, foreign)
	foreignAncestor["controllerName"] = "example.com/other-controller"
	if ok, _ := PolicyAccepted(foreign, gatewayTarget()); ok {
		t.Fatal("a foreign controller's ancestor entry was read as Envoy Gateway accepting the policy")
	}

	// The same, with a sectionName on both sides: a listener policy is
	// accepted by the entry for its own listener.
	listenerStatus := capturedStatus(t)
	ref, _ := ancestor(t, listenerStatus)["ancestorRef"].(map[string]any)
	ref["sectionName"] = "http-box-example-test"
	if ok, _ := PolicyAccepted(listenerStatus, listener); !ok {
		t.Fatal("a listener-scoped ancestor entry did not accept the listener's policy")
	}
}

// An ancestor entry with no Accepted condition yet is still transient, not a
// refusal: the controller has written the entry but not the verdict.
func TestPolicyAcceptedWithoutAConditionIsTransient(t *testing.T) {
	status := capturedStatus(t)
	ancestor(t, status)["conditions"] = []any{}
	if ok, why := PolicyAccepted(status, gatewayTarget()); ok || why == "" {
		t.Fatalf("PolicyAccepted = %v, %q, want a transient not-yet answer", ok, why)
	}
}

func ancestor(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	ancestors, _ := status["ancestors"].([]any)
	if len(ancestors) == 0 {
		t.Fatalf("no ancestors in %v", status)
	}
	a, _ := ancestors[0].(map[string]any)
	return a
}

func ancestorCondition(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	conditions, _ := ancestor(t, status)["conditions"].([]any)
	if len(conditions) == 0 {
		t.Fatalf("no conditions in %v", status)
	}
	c, _ := conditions[0].(map[string]any)
	return c
}

// The controller name the decoder trusts is the one box installs Envoy Gateway
// as: a policy accepted by anything else is a policy in front of a Gateway box
// does not serve.
func TestPolicyAcceptedUsesTheEnvoyGatewayControllerName(t *testing.T) {
	status := capturedStatus(t)
	if got, _ := ancestor(t, status)["controllerName"].(string); got != bootstrap.EnvoyGatewayController {
		t.Fatalf("fixture controller %q, want %q", got, bootstrap.EnvoyGatewayController)
	}
}
