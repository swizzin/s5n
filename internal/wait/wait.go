// Package wait is box's one deadline-and-poll loop. A caller keeps the budget,
// the poll interval and the sentence box says at the deadline; an error ends
// the wait, so a caller that retries a refusal keeps the last one itself.
package wait

import (
	"context"
	"errors"
	"time"
)

// Clock is the loop's two time calls. The zero value is the system clock and a
// timer that a cancelled context cuts short; tests inject a fake.
type Clock struct {
	// Now is what time.Now is in the loop.
	Now func() time.Time
	// Sleep advances the clock by one interval; nil sleeps on a timer.
	Sleep func(time.Duration)
}

// Waited is what one Until did: whether it happened or the budget ran out.
type Waited struct {
	// Done is whether fn reported the thing happened.
	Done bool
	// Tries is how many times fn was called, the deadline attempt included.
	Tries int
}

// Until polls fn until it reports done or the budget expires; fn gets the
// context so one attempt can be cancelled. The last sleep never runs past the
// deadline, but the attempt at the deadline still happens.
func Until(ctx context.Context, budget, poll time.Duration, fn func(context.Context) (bool, error)) (Waited, error) {
	return UntilClock(ctx, Clock{}, budget, poll, fn)
}

// UntilClock is Until with an injected clock.
func UntilClock(ctx context.Context, clock Clock, budget, poll time.Duration, fn func(context.Context) (bool, error)) (Waited, error) {
	if poll <= 0 {
		// A zero poll would call fn in a busy loop; callers default it.
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

// sleep waits one interval on the caller's fake clock, or on a timer the
// context can cut short.
func sleep(ctx context.Context, clock Clock, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if clock.Sleep != nil {
		clock.Sleep(d)
		// A fake Sleep returns at once, so catch a cancellation here.
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
