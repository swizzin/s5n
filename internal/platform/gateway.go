package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The Gateway probe. MetalLB exists in s5n only to give the Gateway's Service
// an address -- the torrent port is a NodePort -- so the capability to probe
// is the Gateway's own: it either reports an address or it does not. That
// question can only be asked after the Gateway is applied, which is why the
// plan defers it past the confirm instead of answering it up front.
const (
	// ProbeTimeout is how long the Gateway gets to report an address before
	// box stops waiting for one. Decision.ProbeTimeout overrides it in tests.
	ProbeTimeout = 60 * time.Second
	// ReachTimeout is how long the address gets to answer HTTP from this
	// host. Decision.ReachTimeout overrides it in tests.
	ReachTimeout = 30 * time.Second
	// probePoll is how often the Gateway's status is re-read.
	probePoll = 2 * time.Second
	// reachPoll is how often the HTTP check is retried.
	reachPoll = time.Second
	// reachRequestTimeout bounds one HTTP attempt.
	reachRequestTimeout = 3 * time.Second

	// gatewayAddressNotAssigned is the Gateway API's standard Programmed
	// reason for a Gateway whose address has not been assigned: the one
	// condition MetalLB fixes. Any other reason is a failure to stop on.
	gatewayAddressNotAssigned = "AddressNotAssigned"
)

// GatewayAnswer is what the Gateway says about its address, at the moment it
// was last read. Every field is evidence for the deadline decision: the
// Programmed reasons are transient while the data plane rolls out, so box
// never acts on one until the wait is over.
type GatewayAnswer struct {
	// Address is the first address the Gateway reports; empty when it has
	// none yet.
	Address string
	// Reason, Status and Message are the Programmed condition: Reason is
	// AddressNotAssigned when the Gateway's Service has no address yet, and
	// something else (NoResources, AddressNotUsable) when it is failing for
	// another reason.
	Reason  string
	Status  string
	Message string
	// missing says there is no Gateway object to ask. Nothing will change by
	// waiting, so the probe answers at once instead of polling for a minute.
	missing bool
}

// Programmed reports whether the Gateway says it is ready.
func (a GatewayAnswer) Programmed() bool { return a.Status == "True" }

// ReasonNoGateway is the answer's reason when there is no Gateway at all.
const ReasonNoGateway = "the Gateway does not exist"

// GatewayProbe answers the two questions box asks about the Gateway it
// applies: has it an address, and does that address answer HTTP from this
// host. Both need the Gateway to exist, so neither can be asked before the
// confirm.
type GatewayProbe interface {
	// Address waits up to timeout for the Gateway to report an address and be
	// Programmed, and returns the last answer either way. It never judges an
	// intermediate reason: NoResources and friends are normal while Envoy's
	// data plane is rolling out, and only the answer at the deadline means
	// anything.
	Address(ctx context.Context, timeout time.Duration) (GatewayAnswer, error)
	// Reachable asks address for HTTP on port 80, retrying for up to retry.
	// Any HTTP status is an answer (an empty Envoy answers 404); a refused or
	// timed-out connection is not.
	Reachable(ctx context.Context, address string, retry time.Duration) (int, error)
}

// Gateway is the real GatewayProbe: the Gateway API object through the same
// client box applies it with, and a plain HTTP GET from this host.
type Gateway struct {
	k *Kubernetes
	// Client is the reachability check's HTTP client. Nil means a direct
	// client, never through a proxy, that does not follow redirects.
	Client *http.Client
	// Poll is how often the status is re-read; zero means two seconds.
	Poll time.Duration

	// reader replaces the cluster read in tests.
	reader gatewayReader
}

// gatewayReader is the one read Address polls.
type gatewayReader interface {
	read(ctx context.Context) (GatewayAnswer, error)
}

// NewGatewayProbe builds the probe over the cluster client box uses. It does
// not contact the cluster: the client is lazy.
func NewGatewayProbe(kubeconfig string) (*Gateway, error) {
	k, err := NewCluster(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &Gateway{k: k}, nil
}

// Address waits up to timeout for the Gateway to report an address *and* be
// Programmed. Both are required: Envoy Gateway has the address before its
// proxy pod is ready, and reports Programmed=False with NoResources until it
// is, which is not a reason to install anything.
func (g *Gateway) Address(ctx context.Context, timeout time.Duration) (GatewayAnswer, error) {
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		answer, err := g.current(ctx)
		if err != nil {
			return GatewayAnswer{}, err
		}
		if (answer.Address != "" && answer.Programmed()) || answer.missing {
			return answer, nil
		}
		if time.Now().After(deadline) {
			return answer, nil
		}
		if err := pause(ctx, sleepFor(g.poll(), deadline)); err != nil {
			return answer, err
		}
	}
}

// Reachable asks address for HTTP on port 80, retrying until retry expires.
func (g *Gateway) Reachable(ctx context.Context, address string, retry time.Duration) (int, error) {
	if retry <= 0 {
		retry = ReachTimeout
	}
	url := "http://" + net.JoinHostPort(address, "80") + "/"
	deadline := time.Now().Add(retry)
	for {
		code, err := g.get(ctx, url)
		if err == nil {
			return code, nil
		}
		if time.Now().After(deadline) {
			return 0, err
		}
		if err := pause(ctx, sleepFor(reachPoll, deadline)); err != nil {
			return 0, err
		}
	}
}

func (g *Gateway) current(ctx context.Context) (GatewayAnswer, error) {
	if g.reader != nil {
		return g.reader.read(ctx)
	}
	return g.read(ctx)
}

// read is the current state of the Gateway: its address and the Programmed
// condition, whatever they say.
func (g *Gateway) read(ctx context.Context) (GatewayAnswer, error) {
	gvr, err := g.k.mapping(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	if err != nil {
		return GatewayAnswer{}, err
	}
	obj, err := g.k.dyn.Resource(gvr.Resource).Namespace(GatewayNamespace).Get(ctx, GatewayName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Not applied by this run and not there from an earlier one: there is
		// nothing to wait for.
		return GatewayAnswer{Reason: ReasonNoGateway, missing: true}, nil
	}
	if err != nil {
		return GatewayAnswer{}, fmt.Errorf("read Gateway %s/%s: %w", GatewayNamespace, GatewayName, err)
	}
	return answerOf(obj), nil
}

// answerOf is what one Gateway object says about its address. It is separate
// from the read so the deadline decision -- AddressNotAssigned against a
// reason MetalLB would only hide, an address without Programmed=True against
// neither -- is testable without a cluster.
func answerOf(obj *unstructured.Unstructured) GatewayAnswer {
	reason, status, message := programmed(obj)
	return GatewayAnswer{
		Address: firstAddress(obj),
		Reason:  reason,
		Status:  status,
		Message: message,
	}
}

// programmed is the Gateway's Programmed condition as (reason, status, message).
func programmed(gw *unstructured.Unstructured) (reason, status, message string) {
	conditions, _, _ := unstructured.NestedSlice(gw.Object, "status", "conditions")
	for _, c := range conditions {
		cond, _ := c.(map[string]any)
		if cond["type"] != "Programmed" {
			continue
		}
		reason, _ = cond["reason"].(string)
		status, _ = cond["status"].(string)
		message, _ = cond["message"].(string)
	}
	return reason, status, message
}

// firstAddress is the first address the Gateway reports, if any. Envoy Gateway
// copies its proxy Service's LoadBalancer addresses into status.addresses, so
// an empty list means the Service has none.
func firstAddress(gw *unstructured.Unstructured) string {
	addresses, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	for _, a := range addresses {
		entry, _ := a.(map[string]any)
		if value, _ := entry["value"].(string); value != "" {
			return value
		}
	}
	return ""
}

// get is one HTTP attempt. Any status code is an answer.
func (g *Gateway) get(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

func (g *Gateway) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{
		// Direct, never through a proxy: the question is whether *this host*
		// reaches the address box just published, and a proxy would answer
		// instead of the Gateway. A redirect is already an answer.
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: reachRequestTimeout}).DialContext,
			ResponseHeaderTimeout: reachRequestTimeout,
		},
		Timeout:       reachRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (g *Gateway) poll() time.Duration {
	if g.Poll > 0 {
		return g.Poll
	}
	return probePoll
}

// sleepFor is how long one wait may take: never past the deadline, so a short
// timeout is a short wait rather than one poll interval longer.
func sleepFor(poll time.Duration, deadline time.Time) time.Duration {
	if left := time.Until(deadline); left < poll {
		return left
	}
	return poll
}

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// interface check: the real probe is what cmd/box hands the platform decision.
var _ GatewayProbe = (*Gateway)(nil)

// errNoProbe is what a Decision without a probe answers: a binary built
// without one is a programming mistake, not a cluster state.
var errNoProbe = errors.New("the Gateway probe is not available in this build")
