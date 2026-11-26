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

	"github.com/swizzin/s5n/internal/wait"
)

// The Gateway probe. MetalLB exists only to give the Gateway's Service an address (the
// torrent port is a NodePort), so the capability to probe is the Gateway's own: an
// address or not. It can only be asked after the Gateway is applied, which is why the
// plan defers it past the confirm.
const (
	// AddressTimeout is how long the Gateway gets to say it has no address before
	// MetalLB is installed: the answer comes within seconds, so a longer wait is only
	// a pause with a known ending. Decision.AddressTimeout overrides it.
	AddressTimeout = 15 * time.Second
	// ProbeTimeout is the whole wait for the Gateway to be Programmed with an address:
	// anything but AddressNotAssigned at AddressTimeout gets the rest of it, those
	// reasons being transient until the deadline. Also the wait after MetalLB.
	// Decision.ProbeTimeout overrides it.
	ProbeTimeout = 60 * time.Second
	// ReachTimeout is how long the address gets to answer HTTP; Decision.ReachTimeout
	// overrides it.
	ReachTimeout = 30 * time.Second
	// probePoll is how often the Gateway's status is re-read.
	probePoll = 2 * time.Second
	// reachPoll is how often the HTTP check is retried.
	reachPoll = time.Second
	// reachRequestTimeout bounds one HTTP attempt.
	reachRequestTimeout = 3 * time.Second

	// gatewayAddressNotAssigned is the standard Programmed reason for an unassigned
	// address: the one condition MetalLB fixes. Any other reason is a failure.
	gatewayAddressNotAssigned = "AddressNotAssigned"
)

// GatewayAnswer is what the Gateway says about its address at the last read. Every
// field is evidence for the deadline decision: Programmed reasons are transient while
// the data plane rolls out, so box acts only on the answer at the deadline.
type GatewayAnswer struct {
	// Address is the first address the Gateway reports, or empty.
	Address string
	// Reason, Status and Message are the Programmed condition: AddressNotAssigned when
	// the Service has no address, else a reason about the data plane.
	Reason  string
	Status  string
	Message string
	// missing says there is no Gateway to ask, so the probe answers at once.
	missing bool
}

// Programmed reports whether the Gateway says it is ready.
func (a GatewayAnswer) Programmed() bool { return a.Status == conditionTrue }

// ReasonNoGateway is the answer's reason when there is no Gateway at all.
const ReasonNoGateway = "the Gateway does not exist"

// GatewayProbe answers the two questions about the Gateway box applies: has it an
// address, and does that address answer HTTP from this host.
type GatewayProbe interface {
	// Address waits up to timeout for an address and Programmed, returning the last
	// answer either way: NoResources and friends are normal while Envoy rolls out.
	Address(ctx context.Context, timeout time.Duration) (GatewayAnswer, error)
	// Reachable asks address for HTTP on 80, retrying: any status is an answer (an
	// empty Envoy answers 404), a refused or timed-out connection is not.
	Reachable(ctx context.Context, address string, retry time.Duration) (int, error)
}

// Gateway is the real GatewayProbe: the Gateway API object through the same
// client box applies it with, and a plain HTTP GET from this host.
type Gateway struct {
	k *Kubernetes
	// Client is the reachability client; nil means a direct one that follows no
	// redirects.
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

// NewGatewayProbe builds the probe over box's cluster client; the client is lazy.
func NewGatewayProbe(kubeconfig string) (*Gateway, error) {
	k, err := NewCluster(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &Gateway{k: k}, nil
}

// Address waits up to timeout for an address *and* Programmed: Envoy has the address
// before its proxy is ready and reports NoResources until it is, which is no reason to
// install anything.
func (g *Gateway) Address(ctx context.Context, timeout time.Duration) (GatewayAnswer, error) {
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	var answer GatewayAnswer
	_, err := wait.Until(ctx, timeout, g.poll(), func(ctx context.Context) (bool, error) {
		current, err := g.current(ctx)
		if err != nil {
			return false, err
		}
		answer = current
		return (current.Address != "" && current.Programmed()) || current.missing, nil
	})
	if err != nil {
		// The last answer goes back with the error too: the wait still saw something.
		return answer, err
	}
	// A Gateway never Programmed is the last answer, not an error: the deadline
	// decides.
	return answer, nil
}

// Reachable asks address for HTTP on port 80, retrying until retry expires.
func (g *Gateway) Reachable(ctx context.Context, address string, retry time.Duration) (int, error) {
	if retry <= 0 {
		retry = ReachTimeout
	}
	url := "http://" + net.JoinHostPort(address, "80") + "/"
	// A refused connection is no answer: it is remembered and the wait keeps asking.
	var code int
	var lastErr error
	waited, err := wait.Until(ctx, retry, reachPoll, func(ctx context.Context) (bool, error) {
		c, err := g.get(ctx, url)
		if err != nil {
			lastErr = err
			return false, nil
		}
		code = c
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	// A later answer is the answer; an earlier refusal matters only if none came.
	if !waited.Done {
		return 0, lastErr
	}
	return code, nil
}

func (g *Gateway) current(ctx context.Context) (GatewayAnswer, error) {
	if g.reader != nil {
		return g.reader.read(ctx)
	}
	return g.read(ctx)
}

// read is the Gateway's address and Programmed condition, whatever they say.
func (g *Gateway) read(ctx context.Context) (GatewayAnswer, error) {
	gvr, err := g.k.mapping(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	if err != nil {
		return GatewayAnswer{}, err
	}
	obj, err := g.k.dyn.Resource(gvr.Resource).Namespace(GatewayNamespace).Get(ctx, GatewayName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Not applied by this run and not there earlier: nothing to wait for.
		return GatewayAnswer{Reason: ReasonNoGateway, missing: true}, nil
	}
	if err != nil {
		return GatewayAnswer{}, fmt.Errorf("read Gateway %s/%s: %w", GatewayNamespace, GatewayName, err)
	}
	return answerOf(obj), nil
}

// answerOf is what one Gateway object says about its address, separate from the read
// so the deadline decision is testable without a cluster.
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

// firstAddress is the first address the Gateway reports: Envoy copies its proxy
// Service's LoadBalancer addresses into status.addresses.
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
		// Direct, never through a proxy: the question is whether this host reaches the
		// address, and a proxy would answer instead. A redirect is already an answer.
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

// interface check: the real probe is what cmd/box hands the platform decision.
var _ GatewayProbe = (*Gateway)(nil)

// errNoProbe is what a Decision without a probe answers: a programming mistake, not a
// cluster state.
var errNoProbe = errors.New("the Gateway probe is not available in this build")
