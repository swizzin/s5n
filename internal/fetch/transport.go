package fetch

import (
	"fmt"
	"net/http"
)

// Transport is the retrying http.RoundTripper box's fetches go through. It
// exists so a 5xx from an upstream during bootstrap is retried rather than
// handed to the operator as a failed install; the package comment says what is
// and is not retried.
//
// Only GET and HEAD are retried, and only when the body can be replayed: a
// POST that failed may already have done its work on the far side.
type Transport struct {
	// Base performs one attempt. Nil means http.DefaultTransport.
	Base http.RoundTripper
	// Policy is the retry rule. The zero value is box's policy.
	Policy Policy
}

// RoundTrip implements http.RoundTripper.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base()
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return base.RoundTrip(req)
	}
	if req.Body != nil && req.GetBody == nil {
		return base.RoundTrip(req)
	}

	var resp *http.Response
	err := t.Policy.Do(req.Context(), req.URL.String(), func() Attempt {
		// RoundTrip must not modify the caller's request, and a retry is a
		// new request: same URL, same method, a fresh body.
		attempt := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return Attempt{Err: err, Status: err.Error()}
			}
			attempt.Body = body
		}
		got, err := base.RoundTrip(attempt)
		if err != nil {
			// The caller's own context ending is not an upstream failure:
			// say why the fetch stopped instead of retrying a cancelled run.
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return Attempt{Err: ctxErr, Status: ctxErr.Error()}
			}
			return Attempt{Err: err, Retry: IsTransient(err), Status: err.Error()}
		}
		if !Retryable(got.StatusCode) {
			resp = got
			return Attempt{}
		}
		retryAfter := retryAfterAt(got.Header, t.Policy.now())
		discard(got.Body)
		return Attempt{
			Err:        fmt.Errorf("%s: %s", req.URL, got.Status),
			Retry:      true,
			RetryAfter: retryAfter,
			Status:     got.Status,
		}
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("fetching %s: no response", req.URL)
	}
	return resp, nil
}

func (t Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}
