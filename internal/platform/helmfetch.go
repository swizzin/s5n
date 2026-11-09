package platform

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"helm.sh/helm/v4/pkg/getter"

	"github.com/swizzin/s5n/internal/fetch"
)

// Helm's chart-repo getter cannot be given a retrying transport. Its only
// injection point is getter.WithTransport, which takes a concrete
// *http.Transport (not an http.RoundTripper), and HTTPGetter builds its own
// *http.Client around it -- so nothing box can pass in sits between the getter
// and the wire. (The OCI path does take a client,
// registry.ClientOptHTTPClient(*http.Client), and gets fetch.Transport
// directly; see Helm.chartClient.) So a chart-repo fetch is retried one level
// up, around Get, with the same fetch.Policy: both the repository index and the
// chart tarball are fetched through this getter, and either is what an
// upstream 504 interrupts.
//
// What this seam cannot see is the response headers, so a Retry-After on a 429
// or 503 is not honoured here; the backoff is box's own. A fetch box makes
// directly (the k0s release asset) does honour it.

// chartGetters is Helm's getter list with the http/https getter wrapped so a
// transient failure is retried. Everything else -- the OCI getter, plugin
// getters -- is left exactly as Helm built it: the OCI getter pulls through the
// registry client box hands the downloader, and that already carries the
// retrying transport.
func chartGetters(providers getter.Providers, policy fetch.Policy) getter.Providers {
	out := make(getter.Providers, 0, len(providers))
	for _, provider := range providers {
		if provider.Provides("http") || provider.Provides("https") {
			build := provider.New
			provider.New = func(options ...getter.Option) (getter.Getter, error) {
				inner, err := build(options...)
				if err != nil {
					return nil, err
				}
				return retryingGetter{inner: inner, policy: policy}, nil
			}
		}
		out = append(out, provider)
	}
	return out
}

// retryingGetter retries one chart-repo fetch under box's policy.
type retryingGetter struct {
	inner  getter.Getter
	policy fetch.Policy
}

// Get implements getter.Getter. Helm's own options (the Accept header, a
// repo's credentials, a TLS config, the per-request timeout) are handed
// straight to the wrapped getter on every attempt, so the retry adds nothing
// to the request and takes nothing away.
func (g retryingGetter) Get(href string, options ...getter.Option) (*bytes.Buffer, error) {
	var body *bytes.Buffer
	err := g.policy.Do(context.Background(), href, func() fetch.Attempt {
		buf, err := g.inner.Get(href, options...)
		if err == nil {
			body = buf
			return fetch.Attempt{}
		}
		if status, text, ok := getterStatus(err, href); ok {
			return fetch.Attempt{Err: err, Retry: fetch.Retryable(status), Status: text}
		}
		// Not a status Helm reported: a connection that failed under the
		// request. Same rule as the transport for what may be tried again.
		return fetch.Attempt{Err: err, Retry: fetch.IsTransient(err), Status: err.Error()}
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// getterStatus reads the HTTP status out of the error Helm's HTTP getter
// returns for a non-200 response. The getter reports it with neither the code
// nor the headers on it -- pkg/getter/httpgetter.go builds
//
//	fmt.Errorf("failed to fetch %s : %s", href, resp.Status)
//
// -- so the code is read back out of that sentence.
// TestGetterStatusReadsTheStatusHelmReturns runs Helm's real getter against a
// 504, so an upgrade that changes the wording fails a test instead of quietly
// turning retries off.
func getterStatus(err error, href string) (code int, status string, ok bool) {
	prefix := "failed to fetch " + href + " : "
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return 0, "", false
	}
	status = strings.TrimPrefix(msg, prefix) // "504 Gateway Timeout"
	first, _, _ := strings.Cut(status, " ")
	n, convErr := strconv.Atoi(first)
	if convErr != nil {
		return 0, "", false
	}
	return n, status, true
}
