// Package wait is box's one deadline-and-poll loop.
//
// Box waits for six things that are not its own: the Gateway's status, a
// workload rolling out, a Certificate, a webhook that answers a moment after
// its pod is Ready, the wiring receipt, and the cluster API k0s just started.
// Each of those was written separately, so each carried its own copy of the
// same four questions -- has it happened, is the budget gone, was the context
// cancelled while we slept, what did the last attempt say -- and two different
// sleeps: one that could overshoot the deadline by a whole poll, one that could
// not. That difference is why two waits with the same timeout could take
// different times to give up.
//
// What stays with the caller is what was never common: the budget, the poll
// interval, and the sentence box says when the budget runs out. A caller that
// retries a refusal (a webhook, a connection) keeps the last error itself,
// because at the deadline the refusal is the thing it has to report; a caller
// that reads state stops on a read error instead. Until treats a non-nil error
// as the end of the wait, which is what every read-error site wants, and the
// retrying sites hand it (false, nil) and remember the error on the side.
package wait

import (
	"context"
	"errors"
	"time"
)

// Clock is the loop's two time calls. The zero value is the system clock and a
// timer that a cancelled context cuts short.
//
// It exists for the one caller whose tests already inject a clock: the k0s
// installer proves how many attempts a five-second wait made by moving a fake
// Now forward in Sleep instead of spending the five seconds. That seam is kept
// rather than the package growing a second loop for tests.
type Clock struct {
	// Now is what time.Now is in the loop.
	Now func() time.Time
	// Sleep advances the clock by one interval. A test's Sleep moves its fake
	// Now and returns at once; nil sleeps on a timer.
	Sleep func(time.Duration)
}

// Waited is what one Until did. It is how a caller tells "it happened" from
// "the budget ran out", which are different sentences.
type Waited struct {
	// Done is whether fn reported the thing happened.
	Done bool
	// Tries is how many times fn was called, the attempt at the deadline
	// included.
	Tries int
}

// Until polls fn until it reports done or the budget expires, and returns what
// it saw. fn gets the context so one attempt can be cancelled mid-call.
//
// The sleep never runs past the deadline, so a timeout is the time the caller
// asked for rather than one poll interval more. The attempt at the deadline
// still happens: a caller that only gets an answer just before its budget runs
// out has an answer, and one that does not learns so on that attempt rather
// than on a sleep that would have run past it.
func Until(ctx context.Context, budget, poll time.Duration, fn func(context.Context) (bool, error)) (Waited, error) {
	return UntilClock(ctx, Clock{}, budget, poll, fn)
}

// UntilClock is Until with an injected clock.
func UntilClock(ctx context.Context, clock Clock, budget, poll time.Duration, fn func(context.Context) (bool, error)) (Waited, error) {
	if poll <= 0 {
		// A zero poll would call fn in a busy loop for the whole budget.
		// Every caller defaults its interval, so this is a programming error
		// said out loud rather than a hang.
		return Waited{}, errors.New("wait: the poll interval must be positive")
	}
	now := clock.Now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(budget)

	var waited Waited
	for {
		if err := ctx.Err(); err != nil {
			return waited, err
		}
		done, err := fn(ctx)
		waited.Tries++
		if err != nil {
			return waited, err
		}
		if done {
			waited.Done = true
			return waited, nil
		}
		if !now().Before(deadline) {
			return waited, nil
		}
		if err := sleep(ctx, clock, pollUntil(poll, now(), deadline)); err != nil {
			return waited, err
		}
	}
}

// pollUntil is how long one sleep may take: never past the deadline.
func pollUntil(poll time.Duration, now, deadline time.Time) time.Duration {
	if left := deadline.Sub(now); left < poll {
		return left
	}
	return poll
}

// sleep waits one interval: the caller's fake clock when it has one, a timer
// that the context can cut short otherwise.
func sleep(ctx context.Context, clock Clock, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if clock.Sleep != nil {
		clock.Sleep(d)
		// A fake Sleep returns at once, so a cancellation that arrived during
		// it is caught here rather than by the timer that does not exist.
		return ctx.Err()
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
