// Package fetch is the one network policy `box bootstrap` fetches through. An
// upstream 5xx during bootstrap has nothing to do with the host, so GET and
// HEAD are retried on 5xx, 429, or a refused, reset or timed-out connection.
// A 404, 403 or certificate error is the answer on the first try, and the
// attempt count is bounded so a real outage fails the run rather than hangs it.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultAttempts is how many times one fetch is tried in total.
	DefaultAttempts = 4
	// DefaultDelay is the wait before the second attempt; each later one
	// doubles it (1s, 2s, 4s).
	DefaultDelay = time.Second
	// DefaultMaxDelay caps one wait, Retry-After included: longer is hanging.
	DefaultMaxDelay = 10 * time.Second
	// jitterShare is the fraction added at random, so simultaneous failures do
	// not all return together.
	jitterShare = 10
)

// Policy is the retry rule. The zero value is box's policy; Sleep and Now let
// a test run the backoff without spending the time.
type Policy struct {
	// Attempts is the total number of tries, the first included. Zero means
	// DefaultAttempts.
	Attempts int
	// Delay is the wait before the second attempt. Zero means DefaultDelay.
	Delay time.Duration
	// MaxDelay caps one wait. Zero means DefaultMaxDelay.
	MaxDelay time.Duration
	// Sleep waits for d, returning ctx's error as soon as ctx is done. Nil
	// means a timer. Tests replace it to record the waits and take no time.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now reads the clock, which a Retry-After given as an HTTP date needs.
	// Nil means time.Now.
	Now func() time.Time
}

// Attempt is one try's report to Do.
type Attempt struct {
	// Err is the failure. Nil means the fetch succeeded.
	Err error
	// Retry says a later attempt may succeed.
	Retry bool
	// RetryAfter is the wait the server asked for, when it asked for one.
	RetryAfter time.Duration
	// Status names the failure the way box says it, and the final sentence
	// quotes it.
	Status string
}

// Failure is what Do returns when it gives up, as box's sentence: "fetching
// <url>: 504 Gateway Timeout after 4 attempts". The type lets a caller tell
// box's finished sentence from a failure the retry never touched.
type Failure struct {
	URL      string
	Status   string
	Attempts int
}

func (f *Failure) Error() string {
	noun := "attempts"
	if f.Attempts == 1 {
		noun = "attempt"
	}
	return fmt.Sprintf("fetching %s: %s after %d %s", f.URL, f.Status, f.Attempts, noun)
}

// Do runs try until it succeeds, fails in a way no retry can fix, or runs out
// of attempts. A fixable failure is returned as given; an exhausted retry is a
// *Failure.
func (p Policy) Do(ctx context.Context, url string, try func() Attempt) error {
	attempts := p.attempts()
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		a := try()
		if a.Err == nil {
			return nil
		}
		if !a.Retry {
			return a.Err
		}
		if attempt >= attempts {
			status := a.Status
			if status == "" {
				status = a.Err.Error()
			}
			return &Failure{URL: url, Status: status, Attempts: attempts}
		}
		if err := p.wait(ctx, attempt, a.RetryAfter); err != nil {
			return err
		}
	}
}

func (p Policy) attempts() int {
	if p.Attempts > 0 {
		return p.Attempts
	}
	return DefaultAttempts
}

func (p Policy) delay() time.Duration {
	if p.Delay > 0 {
		return p.Delay
	}
	return DefaultDelay
}

func (p Policy) maxDelay() time.Duration {
	if p.MaxDelay > 0 {
		return p.MaxDelay
	}
	return DefaultMaxDelay
}

func (p Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// wait is the pause after a failed attempt: the server's Retry-After when it
// sent one, box's doubling backoff otherwise, capped either way.
func (p Policy) wait(ctx context.Context, attempt int, retryAfter time.Duration) error {
	d, fromServer := retryAfter, retryAfter > 0
	if !fromServer {
		d = p.delay()
		for i := 1; i < attempt && d < p.maxDelay(); i++ {
			d *= 2
		}
	}
	if max := p.maxDelay(); d > max {
		d = max
	}
	// A server's Retry-After is taken at its word (capped above); box's own
	// backoff gets jitter.
	if !fromServer && d > 0 {
		d += time.Duration(rand.Int64N(int64(d)/jitterShare + 1))
	}
	return p.sleep(ctx, d)
}

func (p Policy) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Retryable reports whether an HTTP status is worth another attempt: a server
// briefly unwell (5xx) or asking box to slow down (429). Other answers stand.
func Retryable(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

// IsTransient reports whether a transport error is one a second attempt can
// fix: a timeout, a refused, reset or closed connection, or a response that
// ended before its headers. A certificate, an unparseable URL or a missing
// name is not made better by asking again.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	return false
}

// retryAfterAt reads a Retry-After header, seconds or HTTP date; zero means no
// wait was asked for.
func retryAfterAt(h http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(h.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// discard drains and closes a body box will not read, so the connection is
// reused rather than torn down.
func discard(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}
