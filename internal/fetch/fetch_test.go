package fetch

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// waits records the backoff a retrying fetch asked for, so a test can assert
// it without spending it.
type waits struct{ seen []time.Duration }

func (w *waits) sleep(_ context.Context, d time.Duration) error {
	w.seen = append(w.seen, d)
	return nil
}

func fastPolicy(w *waits) Policy {
	return Policy{Sleep: w.sleep}
}

// statusServer answers with one status per request, in order, then the last of
// them for any further request. It counts the requests it saw.
type statusServer struct {
	codes []int
	body  string
	// headers are set on the answer at the same index as codes.
	headers []http.Header
	hits    atomic.Int32
}

func (s *statusServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(s.hits.Add(1)) - 1
	if n >= len(s.codes) {
		n = len(s.codes) - 1
	}
	if n < len(s.headers) {
		for k, values := range s.headers[n] {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
	}
	w.WriteHeader(s.codes[n])
	_, _ = io.WriteString(w, s.body)
}

func TestTransportRetriesAServerError(t *testing.T) {
	handler := &statusServer{
		codes: []int{http.StatusGatewayTimeout, http.StatusBadGateway, http.StatusOK},
		body:  "the chart",
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	w := &waits{}
	client := &http.Client{Transport: Transport{Policy: fastPolicy(w)}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("the third attempt should have succeeded: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if string(body) != "the chart" {
		t.Errorf("body = %q, want %q", body, "the chart")
	}
	if got := handler.hits.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (two failures then a success)", got)
	}
	if len(w.seen) != 2 {
		t.Fatalf("waits = %v, want two", w.seen)
	}
	// 1s then 2s, each plus up to a tenth of jitter.
	for i, base := range []time.Duration{time.Second, 2 * time.Second} {
		if w.seen[i] < base || w.seen[i] > base+base/jitterShare {
			t.Errorf("wait %d = %s, want %s plus jitter", i, w.seen[i], base)
		}
	}
}

func TestTransportDoesNotRetryAClientError(t *testing.T) {
	handler := &statusServer{codes: []int{http.StatusNotFound}, body: "no such chart"}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	w := &waits{}
	client := &http.Client{Transport: Transport{Policy: fastPolicy(w)}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("a 404 is an answer, not a fetch failure: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 passed through", resp.StatusCode)
	}
	if got := handler.hits.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: a 404 is not retried", got)
	}
	if len(w.seen) != 0 {
		t.Errorf("waits = %v, want none", w.seen)
	}
}

func TestTransportHonoursRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "7")
	srv := httptest.NewServer(&statusServer{
		codes:   []int{http.StatusTooManyRequests, http.StatusOK},
		headers: []http.Header{header, {}},
	})
	defer srv.Close()

	w := &waits{}
	client := &http.Client{Transport: Transport{Policy: fastPolicy(w)}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	_ = resp.Body.Close()
	if len(w.seen) != 1 {
		t.Fatalf("waits = %v, want one", w.seen)
	}
	if w.seen[0] != 7*time.Second {
		t.Errorf("wait = %s, want the server's 7s Retry-After", w.seen[0])
	}
}

func TestTransportCapsAReturnAfterTheServerAsksFor(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "600")
	srv := httptest.NewServer(&statusServer{
		codes:   []int{http.StatusServiceUnavailable, http.StatusOK},
		headers: []http.Header{header, {}},
	})
	defer srv.Close()

	w := &waits{}
	client := &http.Client{Transport: Transport{Policy: fastPolicy(w)}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	_ = resp.Body.Close()
	if len(w.seen) != 1 || w.seen[0] != DefaultMaxDelay {
		t.Errorf("waits = %v, want one capped at %s", w.seen, DefaultMaxDelay)
	}
}

func TestTransportNeverRetriesAPost(t *testing.T) {
	handler := &statusServer{codes: []int{http.StatusInternalServerError}, body: "boom"}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	w := &waits{}
	client := &http.Client{Transport: Transport{Policy: fastPolicy(w)}}
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want the 500 passed through", resp.StatusCode)
	}
	if got := handler.hits.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: a POST may already have been acted on", got)
	}
}

func TestTransportRetriesATransientTransportError(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, &netOpError{err: syscall.ECONNRESET}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("after the reset")),
			Header:     http.Header{},
		}, nil
	})

	w := &waits{}
	client := &http.Client{Transport: Transport{Base: base, Policy: fastPolicy(w)}}
	resp, err := client.Get("https://example.com/chart.tgz")
	if err != nil {
		t.Fatalf("a reset connection should be retried: %v", err)
	}
	defer resp.Body.Close()
	if got := calls.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestTransportDoesNotRetryAPermanentTransportError(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("x509: certificate signed by unknown authority")
	})

	client := &http.Client{Transport: Transport{Base: base, Policy: fastPolicy(&waits{})}}
	if _, err := client.Get("https://example.com/chart.tgz"); err == nil {
		t.Fatal("a certificate error must fail the fetch")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1: a certificate is not a transient failure", got)
	}
}

func TestTransportExhaustingAttemptsNamesTheURLAndTheCount(t *testing.T) {
	handler := &statusServer{codes: []int{http.StatusGatewayTimeout}, body: "upstream took too long"}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{Transport: Transport{Policy: fastPolicy(&waits{})}}
	_, err := client.Get(srv.URL + "/chart.tgz")
	if err == nil {
		t.Fatal("four 504s must fail the fetch")
	}
	want := "fetching " + srv.URL + "/chart.tgz: 504 Gateway Timeout after 4 attempts"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %T, want a *fetch.Failure the caller can recognise", err)
	}
	if failure.URL != srv.URL+"/chart.tgz" || failure.Status != "504 Gateway Timeout" || failure.Attempts != 4 {
		t.Errorf("failure = %+v, want the URL, the last status and four attempts", failure)
	}
	if got := handler.hits.Load(); got != 4 {
		t.Errorf("requests = %d, want 4", got)
	}
}

func TestTransportStopsWhenTheContextIsCancelledDuringBackoff(t *testing.T) {
	handler := &statusServer{codes: []int{http.StatusServiceUnavailable}}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// The real sleeper: the point is that a cancel during the 1s wait does
	// not wait out the 1s, nor the remaining attempts.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := &http.Client{Transport: Transport{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	start := time.Now()
	//nolint:bodyclose // a cancelled fetch returns no response to close
	_, err = client.Do(req)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the context's deadline", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("returned after %s, want promptly on cancel", elapsed)
	}
	if got := handler.hits.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: nothing is retried after a cancel", got)
	}
}

func TestDoReturnsANonRetryableFailureUnchanged(t *testing.T) {
	refused := errors.New("404 Not Found: no chart by that name")
	err := Policy{Sleep: (&waits{}).sleep}.Do(context.Background(), "https://example.com", func() Attempt {
		return Attempt{Err: refused, Status: "404 Not Found"}
	})
	if !errors.Is(err, refused) {
		t.Errorf("error = %v, want the caller's own error, untouched", err)
	}
}

func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"connection reset", syscall.ECONNRESET, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"closed under the request", net.ErrClosed, true},
		{"end of body", io.ErrUnexpectedEOF, true},
		{"timeout", &netOpError{err: context.DeadlineExceeded, timeout: true}, true},
		{"cancelled", context.Canceled, false},
		{"certificate", errors.New("x509: certificate signed by unknown authority"), false},
	} {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("%s: IsTransient = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// netOpError is a net.Error with a chosen cause, which the real network stack
// produces for a reset or a timed-out connection.
type netOpError struct {
	err     error
	timeout bool
}

func (e *netOpError) Error() string   { return e.err.Error() }
func (e *netOpError) Unwrap() error   { return e.err }
func (e *netOpError) Timeout() bool   { return e.timeout }
func (e *netOpError) Temporary() bool { return e.timeout }
