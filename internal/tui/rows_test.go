package tui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/say"
)

// A fact too long for the terminal hangs under its own column, so the name column
// stays a column.
func TestReporterRowsWrapUnderTheText(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 60, NoColor: true, Yes: true})
	r.Rows([]say.Row{
		{Mark: say.MarkDone, Name: "box", Facts: []string{"0.6.0 → 0.6.1", "signature ok"}},
		{Mark: say.MarkFailed, Name: "chart", Facts: []string{"can't read s5n 0.2.0 from the registry"}, Why: "it has no platform.yaml"},
		{Mark: say.MarkNeeds, Facts: []string{"sonarr is held on 5.0.0.3041 until a release ships a fixed image"}, Command: "box status"},
	})
	r.Close()

	lines := strip(buf.String())
	for _, want := range []string{
		"✓ box            0.6.0 → 0.6.1 · signature ok",
		"✗ chart          can't read s5n 0.2.0 from the registry",
		"                 it has no platform.yaml",
		"  $ box status",
	} {
		if indexOf(lines, want) < 0 {
			t.Fatalf("rows are missing %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	// Every line after the first starts in the facts column, not under the mark.
	first := indexPrefix(lines, "! sonarr is held on 5.0.0.3041 until a release")
	if first < 0 {
		t.Fatalf("the note did not start at its mark:\n%s", strings.Join(lines, "\n"))
	}
	if next := lines[first+1]; !strings.HasPrefix(next, "  image") {
		t.Fatalf("the wrapped note does not hang under the text: %q\n%s", next, strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if w := lipgloss.Width(line); w > 60 {
			t.Fatalf("line is %d columns wide: %q", w, line)
		}
	}
}

// A command breaks as a shell would: at spaces, a backslash ending each line but
// the last, never inside a token.
func TestReporterCommandContinuesLikeAShell(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 44, NoColor: true, Yes: true})
	r.Rows([]say.Row{{Mark: say.MarkCommand, Facts: []string{
		"box update --values /home/example/.config/box/values.yaml --record /etc/s5n/cluster.yaml",
	}}})
	r.Close()

	lines := strip(buf.String())
	var command []string
	for _, line := range lines {
		if strings.HasPrefix(line, "$ ") || strings.HasPrefix(line, "  ") && strings.Contains(line, " ") {
			command = append(command, line)
		}
	}
	if len(command) < 2 {
		t.Fatalf("a long command was not continued:\n%s", strings.Join(lines, "\n"))
	}
	for i, line := range command {
		if w := lipgloss.Width(line); w > 44 {
			t.Fatalf("command line %d is %d columns wide: %q", i, w, line)
		}
		if i < len(command)-1 && !strings.HasSuffix(line, " \\") {
			t.Fatalf("command line %d does not continue with a backslash: %q", i, line)
		}
	}
	// Putting the lines back together gives the command the caller asked for.
	joined := strings.Join(command, " ")
	joined = strings.ReplaceAll(joined, " \\ ", " ")
	joined = strings.ReplaceAll(joined, "$ ", "")
	for _, token := range []string{"/home/example/.config/box/values.yaml", "/etc/s5n/cluster.yaml"} {
		if !strings.Contains(joined, token) {
			t.Fatalf("the command lost %q:\n%s", token, joined)
		}
	}
}

// A waiting step is one live line -- what it does, elapsed against its limit, the
// newest source line trimmed -- and one ✓ row when finished. Sampled lines go to
// the log.
func TestReporterWaitShowsElapsedLimitAndSource(t *testing.T) {
	var screen, log syncBuffer
	r := NewReporter(ReporterOptions{
		Out: &screen, Width: 80, NoColor: true, Yes: true,
		Log: &log, Sample: 2 * time.Millisecond,
	})
	waiting := []string{"pulling s5n:0.6.3", "waiting for the wiring Job", "waiting for the wiring Job"}
	var mu sync.Mutex
	i := 0
	source := func() string {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(waiting) {
			return waiting[len(waiting)-1]
		}
		line := waiting[i]
		i++
		return line
	}
	// A clock that moves on every reading, so the sampler comes due without a real
	// second passing.
	reads := 0
	r.now = func() time.Time {
		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(time.Duration(reads) * time.Second)
		reads++
		return at
	}
	h := r.Start(say.Op{Name: "0.6.2 → 0.6.3", Label: "waiting for the wiring Job", Budget: 10 * time.Minute, Source: source})
	time.Sleep(200 * time.Millisecond)
	h.Done("")
	r.Close()

	live := screen.String()
	for _, want := range []string{"of 10m", "pulling s5n:0.6.3", "waiting for the wiring Job"} {
		if !strings.Contains(live, want) {
			t.Fatalf("the live line never said %q:\n%q", want, live)
		}
	}
	final := strip(live)
	if indexPrefix(final, "✓ 0.6.2 → 0.6.3  ") < 0 {
		t.Fatalf("the finish row is not one ✓ line with its duration:\n%s", strings.Join(final, "\n"))
	}
	logged := log.String()
	if n := strings.Count(logged, "pulling s5n:0.6.3"); n != 1 {
		t.Fatalf("the log carries the first sampled line %d times, want once:\n%s", n, logged)
	}
	if n := strings.Count(logged, "waiting for the wiring Job"); n != 1 {
		t.Fatalf("the log carries a repeated line %d times, want once:\n%s", n, logged)
	}
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("a logged line is %d columns wide: %q", w, line)
		}
	}
}

// A failure is one ✗ row with no frame, and the last line names the log.
func TestReporterErrorIsOneRow(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 80, NoColor: true, Yes: true, LogPath: "/var/log/box/apply-20260101T000000Z.log"})
	r.Error(&say.Subject{
		Name:  "chart",
		Facts: []string{"can't read s5n 0.2.0 from the registry"},
		Why:   "it has no platform.yaml",
		Err:   errFake("can't read s5n 0.2.0 from the registry: it has no platform.yaml"),
	})
	r.Close()

	lines := strip(buf.String())
	for _, want := range []string{
		"✗ chart          can't read s5n 0.2.0 from the registry",
		"                 it has no platform.yaml",
		"· log            /var/log/box/apply-20260101T000000Z.log",
	} {
		if indexOf(lines, want) < 0 {
			t.Fatalf("failure is missing %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	for _, line := range lines {
		for _, box := range []string{"╭", "│", "╰"} {
			if strings.HasPrefix(line, box) {
				t.Fatalf("the failure drew a frame: %q", line)
			}
		}
	}
}

func errFake(text string) error { return &fakeError{text} }

type fakeError struct{ text string }

func (e *fakeError) Error() string { return e.text }

// syncBuffer is a bytes.Buffer safe for the sampler's goroutine and the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
