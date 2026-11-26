package platform

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"helm.sh/helm/v4/pkg/getter"

	"github.com/swizzin/s5n/internal/fetch"
)

// Helm's chart-repo getter cannot take a retrying transport: getter.WithTransport
// wants a concrete *http.Transport and HTTPGetter builds its own client around it, so
// nothing box passes sits between getter and wire (the OCI path does take a client, see
// Helm.chartClient). A chart-repo fetch is therefore retried one level up, around Get,
// with the same policy: both the index and the tarball go through this getter.
//
// It cannot see response headers, so a Retry-After is not honoured here; the backoff is
// box's own.

// chartGetters is Helm's list with the http/https getter wrapped for retries; the OCI
// and plugin getters are as Helm built them, the OCI one carrying the retrying
// transport through its own client.
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

// Get implements getter.Getter: Helm's options go to the wrapped getter every attempt,
// so the retry adds nothing to the request.
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
		// Not a status Helm reported: a connection that failed under the request.
		return fetch.Attempt{Err: err, Retry: fetch.IsTransient(err), Status: err.Error()}
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// getterStatus reads the HTTP status out of Helm's error for a non-200 response,
// which carries neither code nor headers (pkg/getter/httpgetter.go builds
// `fmt.Errorf("failed to fetch %s : %s", href, resp.Status)`), so it is read back out
// of the sentence. TestGetterStatusReadsTheStatusHelmReturns pins the wording, so a
// Helm bump fails a test instead of quietly disabling retries.
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
