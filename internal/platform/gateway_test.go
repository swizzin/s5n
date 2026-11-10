package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// gatewayObject builds the part of a Gateway object the probe reads: status
// addresses and conditions.
func gatewayObject(addresses []string, conditions ...map[string]any) *unstructured.Unstructured {
	status := map[string]any{}
	if len(addresses) > 0 {
		list := make([]any, 0, len(addresses))
		for _, a := range addresses {
			list = append(list, map[string]any{"type": "IPAddress", "value": a})
		}
		status["addresses"] = list
	}
	if len(conditions) > 0 {
		list := make([]any, 0, len(conditions))
		for _, c := range conditions {
			list = append(list, c)
		}
		status["conditions"] = list
	}
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	if len(status) > 0 {
		obj.Object["status"] = status
	}
	return obj
}

// fakeReader is the Gateway read Address polls.
type fakeReader struct {
	answers []GatewayAnswer
	err     error
	calls   int
}

func (f *fakeReader) read(context.Context) (GatewayAnswer, error) {
	f.calls++
	if f.err != nil {
		return GatewayAnswer{}, f.err
	}
	if len(f.answers) == 0 {
		return GatewayAnswer{}, nil
	}
	answer := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return answer, nil
}

// An address and Programmed=True is the answer that skips MetalLB.
func TestAnswerOfReportsTheAddress(t *testing.T) {
	answer := answerOf(gatewayObject(
		[]string{"10.0.0.50"},
		map[string]any{"type": "Programmed", "status": "True", "reason": "Programmed", "message": "Address assigned"},
	))
	if answer.Address != "10.0.0.50" {
		t.Fatalf("address = %q, want 10.0.0.50", answer.Address)
	}
	if !answer.Programmed() {
		t.Fatal("Programmed() = false for a Programmed=True Gateway")
	}
}

// AddressNotAssigned is the Gateway saying "my Service has no address": the
// one reason MetalLB exists to fix. The answer carries it; deciding is the
// deadline's job, not the read's.
func TestAnswerOfAddressNotAssigned(t *testing.T) {
	answer := answerOf(gatewayObject(nil,
		map[string]any{"type": "Programmed", "status": "False", "reason": gatewayAddressNotAssigned, "message": "No addresses have been assigned to the Gateway"},
	))
	if answer.Address != "" {
		t.Fatalf("address = %q, want none", answer.Address)
	}
	if answer.Reason != gatewayAddressNotAssigned || answer.Programmed() {
		t.Fatalf("answer = %+v", answer)
	}
}

// A failure reason is reported as it is, with the message: the Decision is
// what stops on it, and it can only do that with the words the Gateway used.
func TestAnswerOfAnotherFailureReason(t *testing.T) {
	answer := answerOf(gatewayObject([]string{"172.18.255.239"},
		map[string]any{"type": "Programmed", "status": "False", "reason": "NoResources", "message": "Envoy replicas unavailable"},
	))
	if answer.Reason != "NoResources" || answer.Message != "Envoy replicas unavailable" {
		t.Fatalf("answer = %+v", answer)
	}
	if answer.Address != "172.18.255.239" {
		t.Fatalf("address = %q, want the one the Gateway has", answer.Address)
	}
	if answer.Programmed() {
		t.Fatal("Programmed() = true for Programmed=False")
	}
}

// A Gateway that has not been reconciled yet has no conditions at all; that is
// "no address yet", not a failure.
func TestAnswerOfUnreconciled(t *testing.T) {
	answer := answerOf(gatewayObject(nil))
	if answer.Address != "" || answer.Reason != "" || answer.Programmed() {
		t.Fatalf("answer = %+v, want an empty one", answer)
	}
}

func TestAddressReturnsImmediatelyWhenTheGatewayHasOne(t *testing.T) {
	g := &Gateway{reader: &fakeReader{answers: []GatewayAnswer{{Address: "10.0.0.50", Status: "True"}}}, Poll: time.Millisecond}
	answer, err := g.Address(context.Background(), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Address != "10.0.0.50" {
		t.Fatalf("address = %q", answer.Address)
	}
}

// The reasons are transient while Envoy's data plane rolls out: an address
// with NoResources is not an answer, and the probe keeps polling until the
// Gateway is Programmed -- which is the case the pre-installed-MetalLB e2e hit.
func TestAddressKeepsPollingThroughNoResources(t *testing.T) {
	reader := &fakeReader{answers: []GatewayAnswer{
		{Address: "172.18.255.239", Status: "False", Reason: "NoResources", Message: "Envoy replicas unavailable"},
		{Address: "172.18.255.239", Status: "False", Reason: "NoResources", Message: "Envoy replicas unavailable"},
		{Address: "172.18.255.239", Status: "True", Reason: "Programmed"},
	}}
	g := &Gateway{reader: reader, Poll: time.Millisecond}
	answer, err := g.Address(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Address != "172.18.255.239" || !answer.Programmed() {
		t.Fatalf("answer = %+v, want the address Programmed", answer)
	}
	if reader.calls != 3 {
		t.Fatalf("reads = %d, want three: two NoResources then Programmed", reader.calls)
	}
}

// No address within the bound is an empty answer that keeps the Gateway's own
// reason, and it stops reading rather than hanging.
func TestAddressTimesOutWithTheGatewaysReason(t *testing.T) {
	reader := &fakeReader{answers: []GatewayAnswer{{Reason: gatewayAddressNotAssigned}}}
	g := &Gateway{reader: reader, Poll: time.Millisecond}
	start := time.Now()
	answer, err := g.Address(context.Background(), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the bound was not respected: %s", elapsed)
	}
	if answer.Address != "" || answer.Reason != gatewayAddressNotAssigned {
		t.Fatalf("answer = %+v", answer)
	}
	if reader.calls < 2 {
		t.Fatalf("reads = %d, want it to keep asking until the bound", reader.calls)
	}
}

func TestAddressReturnsTheReadError(t *testing.T) {
	g := &Gateway{reader: &fakeReader{err: errors.New("AddressNotUsable")}, Poll: time.Millisecond}
	if _, err := g.Address(context.Background(), 20*time.Millisecond); err == nil {
		t.Fatal("a failed read was read as an answer")
	}
}

func TestAddressStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &Gateway{reader: &fakeReader{}, Poll: time.Millisecond}
	if _, err := g.Address(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Any HTTP status is an answer: an empty Envoy answers 404, and that proves
// the address reaches the data plane. The request goes to port 80 of the
// Gateway's own address.
func TestReachableAcceptsAnyHTTPStatus(t *testing.T) {
	var got string
	g := &Gateway{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("no route")), Header: http.Header{}}, nil
	})}}
	code, err := g.Reachable(context.Background(), "10.0.0.50", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", code)
	}
	if got != "http://10.0.0.50:80/" {
		t.Fatalf("requested %q, want http://10.0.0.50:80/", got)
	}
}

// A refused connection is not: the address does not reach anything.
// TestReachableAnswersAfterARefusal: a Gateway whose proxy is still binding
// port 80 refuses the first request and answers the next. The answer wins; the
// earlier refusal is reported only when nothing ever answered.
func TestReachableAnswersAfterARefusal(t *testing.T) {
	attempts := 0
	g := &Gateway{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("dial tcp 10.0.0.50:80: connect: connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})}}
	code, err := g.Reachable(context.Background(), "10.0.0.50", 5*time.Second)
	if err != nil {
		t.Fatalf("answered on attempt %d, but Reachable failed: %v", attempts, err)
	}
	if code != http.StatusOK || attempts != 2 {
		t.Fatalf("code = %d after %d attempts, want 200 after 2", code, attempts)
	}
}

func TestReachableFailsWhenNothingConnects(t *testing.T) {
	attempts := 0
	g := &Gateway{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("dial tcp 10.0.0.50:80: connect: connection refused")
	})}}
	code, err := g.Reachable(context.Background(), "10.0.0.50", 20*time.Millisecond)
	if err == nil {
		t.Fatalf("a refused connection was accepted (code %d)", code)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v", err)
	}
	if attempts < 1 {
		t.Fatal("no attempt was made")
	}
}

// The address may be a hostname (a cloud provider's ELB reports one), so the
// URL must not assume an IP.
func TestReachableAcceptsAHostname(t *testing.T) {
	var got string
	g := &Gateway{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
	})}}
	if _, err := g.Reachable(context.Background(), "lb.example.com", 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got != "http://lb.example.com:80/" {
		t.Fatalf("requested %q", got)
	}
}
