package platform

import (
	"os"
	"path/filepath"
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

// capturedCertificateStatus is the status cert-manager wrote for one
// Certificate, read from a cluster into testdata rather than built here: the
// question the wait asks is about the document a real controller writes, and a
// stub written from the same guess as the reader passes both ways.
func capturedCertificateStatus(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
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

// A certificate the controller issued for the generation the object is at:
// the answer every re-run of init or apply gets for a host that already moved.
func TestCertificateStatusReadsTheCapturedReady(t *testing.T) {
	status := capturedCertificateStatus(t, "certificate-ready.yaml")
	if ready, why := CertificateStatus(status, 2); !ready || why != "" {
		t.Fatalf("CertificateStatus(captured, 2) = %v, %q, want ready", ready, why)
	}
}

// The same document, one generation later: this run's apply moved the issuer,
// so the Ready=True still describes the certificate the move replaces. Reading
// it as the answer is how a move is reported done before cert-manager has
// issued anything -- and the next run then sits out the whole timeout on it.
func TestCertificateStatusIgnoresAReadyFromAnEarlierGeneration(t *testing.T) {
	status := capturedCertificateStatus(t, "certificate-ready.yaml")
	ready, why := CertificateStatus(status, 3)
	if ready {
		t.Fatal("a Ready condition written for generation 2 was read as generation 3: the re-issue would be reported done, and the next run would wait the timeout out on it")
	}
	if !strings.Contains(why, "generation 3") {
		t.Fatalf("reason %q does not name the generation that has to be issued", why)
	}
}

// The in-flight re-issue, on the captured shape: Ready=False with the reason
// the operator is given at the deadline, Issuing=True beside it.
func TestCertificateStatusReadsTheReissueReason(t *testing.T) {
	status := capturedCertificateStatus(t, "certificate-reissuing.yaml")
	ready, why := CertificateStatus(status, 2)
	if ready {
		t.Fatal("a Certificate whose Secret was issued by another issuer was read as ready")
	}
	if !strings.HasPrefix(why, "IncorrectIssuer: Issuing certificate as Secret was previously issued by") {
		t.Fatalf("reason %q is not cert-manager's IncorrectIssuer sentence", why)
	}
}

// cert-manager has stamped observedGeneration on every condition since long
// before the pinned release, so a condition without one is not a document box
// meets. Reading it as stale would wait the full timeout on every run against
// a controller that does not write the field.
func TestCertificateStatusWithoutObservedGeneration(t *testing.T) {
	status := map[string]any{"conditions": []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Ready", "message": "Certificate is up to date and has not expired",
	}}}
	if ready, why := CertificateStatus(status, 7); !ready || why != "" {
		t.Fatalf("CertificateStatus(no observedGeneration, 7) = %v, %q, want ready", ready, why)
	}
}

// No Ready condition is not an answer: a Certificate being written for the
// first time, or one whose only condition is Issuing, is still being issued.
func TestCertificateStatusWithoutAReadyCondition(t *testing.T) {
	if ready, why := CertificateStatus(nil, 1); ready || why == "" {
		t.Fatalf("CertificateStatus(nil, 1) = %v, %q: no status was read as ready", ready, why)
	}
	only := map[string]any{"conditions": []any{map[string]any{"type": "Issuing", "status": "True", "reason": "IncorrectIssuer"}}}
	if ready, why := CertificateStatus(only, 2); ready || why == "" {
		t.Fatalf("CertificateStatus(only Issuing, 2) = %v, %q: a certificate with no Ready condition was read as ready", ready, why)
	}
}
