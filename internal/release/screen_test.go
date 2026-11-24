package release

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/say"
)

// These are the screens the wiring wait and its receipt produce, from fakes: a
// receipt whose checks all passed, one with checks a person still has to clear,
// one that failed, and a wait that ran out of time. docs/output.md is the
// rulebook behind them.

// checkSteps is a receipt with n checks that all passed.
func checkSteps(n int) []Step {
	steps := make([]Step, 0, n)
	for i := 0; i < n; i++ {
		steps = append(steps, Step{Name: "check" + string(rune('a'+i%26)), Result: "ok"})
	}
	return steps
}

// receiptScreen runs one wait and then renders its receipt, which is what an
// apply does: the wait says nothing itself, and the receipt is the row. It
// returns the screen and the run log separately.
func receiptScreen(t *testing.T, rec Receipt, setUp func(*fakes)) (string, string) {
	t.Helper()
	f := newFakes(t)
	f.rel.receipt = rec
	if setUp != nil {
		setUp(f)
	}
	var screen, logf bytes.Buffer
	plain := &say.Plain{W: &screen, Log: &logf, Sample: time.Millisecond}
	f.app.Say = plain
	got, err := f.app.waitReceipt(context.Background(), f.rel, time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("wait: %v\n%s", err, screen.String())
	}
	ReportReceipt(plain, got, rehearsalOfValue(f))
	return screen.String(), logf.String()
}

// rehearsalOfValue is the rehearsal flag the fake values carry, which no test
// sets: a scene about a rehearsal says so itself.
func rehearsalOfValue(*fakes) bool { return false }

// TestEveryCheckPassedIsOneRow: the wiring that worked is one row -- what it
// checked and how long the Job took -- and each check is a line in the run log.
func TestEveryCheckPassedIsOneRow(t *testing.T) {
	screen, logf := receiptScreen(t, Receipt{
		Status:    "ok",
		StartedAt: time.Now(),
		Duration:  time.Minute + 2*time.Second,
		Steps:     checkSteps(34),
	}, func(f *fakes) {
		f.rel.job = []string{
			"s5n wiring: 2 Arr instance(s), qBittorrent=true, Prowlarr=true",
			"sonarr/downloadclient: created",
		}
	})

	want := "✓ wiring         34 checks · 1m2s\n"
	if screen != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", screen, want)
	}
	// The Job's own log, and every check it folded away, are in the log.
	for _, want := range []string{
		"wiring Job log:",
		"  s5n wiring: 2 Arr instance(s), qBittorrent=true, Prowlarr=true",
		"  sonarr/downloadclient: created",
		"wiring checka: ok",
	} {
		if !strings.Contains(logf, want) {
			t.Fatalf("the run log is missing %q:\n%s", want, logf)
		}
	}
	if strings.Contains(screen, "sonarr/downloadclient") {
		t.Fatalf("a check reached the screen:\n%s", screen)
	}
}

// TestAPendingRowWaitsOnAPerson: a check a person still has to clear is a row
// of its own, marked !, with the state in its column and the reason -- the URL
// or the command -- hanging under it.
func TestAPendingRowWaitsOnAPerson(t *testing.T) {
	screen, logf := receiptScreen(t, Receipt{
		Status:    "ok",
		StartedAt: time.Now(),
		Duration:  12 * time.Second,
		Steps: []Step{
			{Name: "sonarr/downloadclient", Result: "ok"},
			{Name: "tautulli/plex", Result: "pending",
				Detail: "Plex is not claimed yet: claim it at https://plex.tv/claim"},
		},
	}, nil)

	want := strings.Join([]string{
		"✓ wiring         2 checks · 12s",
		"! tautulli/plex  pending",
		"                           Plex is not claimed yet: claim it at https://plex.tv/claim",
		"",
	}, "\n")
	if screen != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", screen, want)
	}
	if !strings.Contains(logf, "wiring tautulli/plex: pending (Plex is not claimed yet: claim it at https://plex.tv/claim)") {
		t.Fatalf("the run log does not carry the pending check:\n%s", logf)
	}
	if strings.Contains(logf, "34 checks") {
		t.Fatalf("a receipt with a pending row folded to a count:\n%s", logf)
	}
}

// TestAFailedCheckIsOneCrossRow: the check that failed is the row, and the
// reason hangs under it. The command's own error says nothing more, so the
// apply's caller prints the run log's path and not a second copy.
func TestAFailedCheckIsOneCrossRow(t *testing.T) {
	f := newFakes(t)
	f.rel.receipt = Receipt{
		Status:    "failed",
		StartedAt: time.Now(),
		Error:     "sonarr/downloadclient: the API refused the client",
		Steps: []Step{
			{Name: "sonarr/downloadclient", Result: "failed", Detail: "the API refused the client"},
			{Name: "plex/claim", Result: "pending", Detail: "no Plex token"},
		},
	}
	if _, err := f.apply(t); err == nil {
		t.Fatal("a failed wiring reported success")
	} else if !say.ReportedBy(err) {
		t.Fatalf("error = %v, want the rows the flow already drew", err)
	}
	out := f.out.String()
	for _, want := range []string{
		"✗ sonarr/downloadclient  failed",
		"the API refused the client",
		"! plex/claim     pending",
		"no Plex token",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the screen is missing %q:\n%s", want, out)
		}
	}
}

// TestARehearsalSaysItsPendingRowsAreTheRealRuns: a rehearsal box has no
// internet, so a claim or a sign-in is not for that box to clear. The rows say
// where they are cleared instead of pointing at a URL on a disposable box.
func TestARehearsalSaysItsPendingRowsAreTheRealRuns(t *testing.T) {
	var out bytes.Buffer
	r := &say.Plain{W: &out}
	ReportReceipt(r, Receipt{
		Status: "ok",
		Steps: []Step{
			{Name: "sonarr/downloadclient", Result: "ok"},
			{Name: "tautulli/plex", Result: "pending", Detail: "claim it at https://plex.tv/claim"},
		},
	}, true)

	want := strings.Join([]string{
		"✓ wiring         2 checks",
		"! tautulli/plex  pending",
		"                           claim it at https://plex.tv/claim",
		"· rehearsal · pending rows clear on the real run",
		"",
	}, "\n")
	if got := out.String(); got != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", got, want)
	}
}

// TestATimedOutWaitIsOneCrossRowAndTheCommand: a Job that never reports is one
// ✗ row saying what box was waiting for -- the Job's newest line -- and the
// command that runs it again. The Job's whole log is in the run log.
func TestATimedOutWaitIsOneCrossRowAndTheCommand(t *testing.T) {
	f := newFakes(t)
	var screen, logf bytes.Buffer
	plain := &say.Plain{W: &screen, Log: &logf, Sample: time.Millisecond}
	f.app.Say = plain
	f.app.ReceiptWait = 1100 * time.Millisecond
	f.app.Poll = 10 * time.Millisecond
	f.rel.receipt = Receipt{}
	f.rel.receiptErr = ErrNoReceipt
	f.rel.job = []string{
		"s5n wiring: 1 Arr instance(s), qBittorrent=true, Prowlarr=false",
		"pbh/ready: waiting at http://pbh:8080",
	}

	_, err := f.app.waitReceipt(context.Background(), f.rel, time.Now())
	if err == nil {
		t.Fatal("a wait that never got its receipt reported success")
	}
	if !say.ReportedBy(err) {
		t.Fatalf("error = %v, want the row the wait drew itself", err)
	}
	want := strings.Join([]string{
		"✗ wiring         no receipt after 1s",
		"                 pbh/ready: waiting at http://pbh:8080",
		"· your apps are running · only the wiring stopped",
		"  $ box apply",
		"",
	}, "\n")
	if got := screen.String(); got != want {
		t.Fatalf("screen:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(logf.String(), "  pbh/ready: waiting at http://pbh:8080") {
		t.Fatalf("the Job's log is not in the run log:\n%s", logf.String())
	}
}

// TestTheWaitIsNotHeldByAJobWithNoLog: the read is a pod's log, and a pod that
// is not there -- or one that is up and quiet -- is not an error the wait
// reports. The budget alone ends the wait, so a cluster that cannot answer
// `pods/log` costs the wait nothing.
func TestTheWaitIsNotHeldByAJobWithNoLog(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(*fakes)
	}{
		{"no pod", func(f *fakes) { f.rel.jobErr = errors.New("no s5n-wiring pod yet") }},
		{"a quiet pod", func(f *fakes) { f.rel.jobHang = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakes(t)
			var screen bytes.Buffer
			f.app.Say = &say.Plain{W: &screen, Sample: time.Millisecond}
			f.app.ReceiptWait = 300 * time.Millisecond
			f.app.Poll = 20 * time.Millisecond
			f.rel.receiptErr = ErrNoReceipt
			tc.setUp(f)

			start := time.Now()
			_, err := f.app.waitReceipt(context.Background(), f.rel, time.Now())
			if err == nil {
				t.Fatal("the wait reported success with no receipt")
			}
			if spent := time.Since(start); spent > 3*time.Second {
				t.Fatalf("the wait spent %s on a Job with no log; its budget was 300ms", spent)
			}
			if !strings.Contains(screen.String(), "✗ wiring") {
				t.Fatalf("the wait drew no row:\n%s", screen.String())
			}
		})
	}
}
