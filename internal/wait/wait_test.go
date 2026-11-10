package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock is the installer's seam: Now never moves on its own, and Sleep
// moves it, so a wait that would take a minute runs at once and still proves
// how many attempts it made.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (f *fakeClock) clock() Clock {
	return Clock{
		Now:   func() time.Time { return f.now },
		Sleep: func(d time.Duration) { f.sleeps = append(f.sleeps, d); f.now = f.now.Add(d) },
	}
}

func TestUntilReturnsAsSoonAsItIsDone(t *testing.T) {
	calls := 0
	got, err := Until(context.Background(), time.Minute, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return calls == 1, nil
	})
	if err != nil {
		t.Fatalf("Until: %v", err)
	}
	if !got.Done || got.Tries != 1 || calls != 1 {
		t.Fatalf("waited = %+v after %d calls, want done on the first", got, calls)
	}
}

// A later try is the case every wait exists for: a webhook that refuses once,
// a Certificate that is written a moment after it is asked for.
func TestUntilSucceedsOnALaterTry(t *testing.T) {
	f := &fakeClock{now: time.Unix(0, 0)}
	calls := 0
	got, err := UntilClock(context.Background(), f.clock(), time.Second, 100*time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil {
		t.Fatalf("Until: %v", err)
	}
	if !got.Done || got.Tries != 3 {
		t.Fatalf("waited = %+v, want done on the third try", got)
	}
	if len(f.sleeps) != 2 {
		t.Fatalf("slept %d times, want two (between the three tries): %v", len(f.sleeps), f.sleeps)
	}
}

// The budget is a deadline, not an endless poll: the attempt at the deadline
// happens, and the wait gives up without sleeping past it.
func TestUntilStopsAtTheDeadline(t *testing.T) {
	f := &fakeClock{now: time.Unix(0, 0)}
	got, err := UntilClock(context.Background(), f.clock(), 5*time.Second, time.Second, func(context.Context) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("Until: %v", err)
	}
	if got.Done {
		t.Fatal("a wait that never succeeded reported done")
	}
	// t=0 through t=5s: the attempt at the deadline is included.
	if got.Tries != 6 {
		t.Fatalf("tries = %d, want 6", got.Tries)
	}
	for _, d := range f.sleeps {
		if d != time.Second {
			t.Fatalf("slept %s, want one second each time: %v", d, f.sleeps)
		}
	}
}

// The last sleep never runs past the deadline: a 50ms budget with a 2m poll
// waits 50ms, not two minutes. This is the behaviour the Gateway probe had and
// the readiness Await did not, and the reason two waits with one timeout could
// take different times to give up.
func TestUntilNeverSleepsPastTheDeadline(t *testing.T) {
	f := &fakeClock{now: time.Unix(0, 0)}
	got, err := UntilClock(context.Background(), f.clock(), 50*time.Millisecond, 2*time.Minute, func(context.Context) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("Until: %v", err)
	}
	if got.Done || got.Tries != 2 {
		t.Fatalf("waited = %+v, want one attempt and one at the deadline", got)
	}
	if len(f.sleeps) != 1 || f.sleeps[0] != 50*time.Millisecond {
		t.Fatalf("slept %v, want exactly the 50ms that is left", f.sleeps)
	}
}

// An error ends the wait, and it is the error the caller returns: a failed read
// is not a state to keep polling.
func TestUntilStopsOnAnError(t *testing.T) {
	refused := errors.New("connection refused")
	got, err := Until(context.Background(), time.Minute, time.Millisecond, func(context.Context) (bool, error) {
		return false, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the attempt's own error", err)
	}
	if got.Done || got.Tries != 1 {
		t.Fatalf("waited = %+v, want one try that failed", got)
	}
}

// A cancelled context stops the wait between attempts, and one cancelled
// before the first attempt never asks at all: nothing box waits for should be
// read after an interrupt.
func TestUntilStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := Until(ctx, time.Minute, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, a cancelled wait asked anyway", calls)
	}
}

// The real clock's sleep is cut short by the context, so a cancelled wait does
// not sit out its poll interval.
func TestUntilWakesOnCancellationDuringTheSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	_, err := Until(ctx, time.Minute, 30*time.Second, func(context.Context) (bool, error) {
		cancel()
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the wait slept %s after a cancellation", elapsed)
	}
}

// A zero poll is a programming error said out loud, not a busy loop for the
// whole budget.
func TestUntilRefusesAZeroPoll(t *testing.T) {
	_, err := Until(context.Background(), time.Minute, 0, func(context.Context) (bool, error) {
		t.Fatal("a zero poll still called the function")
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "poll interval") {
		t.Fatalf("err = %v, want a refusal that names the poll interval", err)
	}
}
