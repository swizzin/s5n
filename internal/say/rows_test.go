package say

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The gutter and the columns: one mark, the name padded to the fixed column,
// and the facts after it. A note has no name, so its text starts in the gutter's
// own column; a refusal's reason and a row's command hang under the text.
func TestPlainRows(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Rows([]Row{
		{Mark: MarkDone, Name: "box", Facts: []string{"0.6.0 → 0.6.1", "signature ok"}},
		{Mark: MarkFailed, Name: "chart", Facts: []string{"can't read s5n 0.2.0 from the registry"}, Why: "it has no platform.yaml"},
		{Mark: MarkDone, Name: "0.6.2 → 0.6.3", Facts: []string{"41s"}},
		{Mark: MarkNeeds, Name: "sonarr", State: "held", Facts: []string{"at 5.0.0.3041"}},
		{Mark: MarkNeeds, Facts: []string{"sonarr is held on 5.0.0 until a release ships a fixed image"}, Command: "box status"},
		{Mark: MarkCommand, Facts: []string{"box update --values /home/example/.config/box/values.yaml"}},
	})
	want := strings.Join([]string{
		"✓ box            0.6.0 → 0.6.1 · signature ok",
		"✗ chart          can't read s5n 0.2.0 from the registry",
		"                 it has no platform.yaml",
		"✓ 0.6.2 → 0.6.3  41s",
		"! sonarr         held      at 5.0.0.3041",
		"! sonarr is held on 5.0.0 until a release ships a fixed image",
		"  $ box status",
		"$ box update --values /home/example/.config/box/values.yaml",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", got, want)
	}
}

// A row with no mark is a note: nothing is left blank where a mark would be, and
// nothing is left dangling where a name would be.
func TestPlainRowDefaultsTheMark(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Rows([]Row{{Facts: []string{"nothing on this box changed"}}, {Name: "log", Facts: []string{"/var/log/box/update-x.log"}}})
	want := "· nothing on this box changed\n· log            /var/log/box/update-x.log\n"
	if got := buf.String(); got != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", got, want)
	}
}

// A name wider than the column takes the column with it: a truncated name is a
// different name, and the facts still start one column past the longest one.
func TestPlainRowKeepsALongNameWhole(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Rows([]Row{{Mark: MarkNeeds, Name: "plex/metadata", Facts: []string{"2.1 GiB"}}})
	want := "! plex/metadata  2.1 GiB\n"
	if got := buf.String(); got != want {
		t.Fatalf("row = %q, want %q", got, want)
	}
}

// Durations are numbers a person reads: 41s, 6s, 4m10s, 10m -- no trailing
// zeros, and no unit a person does not need.
func TestDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{41 * time.Second, "41s"},
		{0, "0s"},
		{time.Minute, "1m"},
		{4*time.Minute + 10*time.Second, "4m10s"},
		{10 * time.Minute, "10m"},
		{2*time.Hour + 5*time.Minute, "2h5m"},
		{2 * time.Hour, "2h"},
		{90*time.Second + 400*time.Millisecond, "1m30s"},
	}
	for _, tc := range cases {
		if got := Duration(tc.d); got != tc.want {
			t.Fatalf("Duration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// A waiting step's source is sampled, and only a line it has not already given
// is new: a log that repeats itself for four minutes adds four minutes of noise
// to the transcript otherwise.
func TestWatcherSaysEachLineOnce(t *testing.T) {
	lines := []string{"pulling", " pulling ", "starting", "", "starting", "ready"}
	i := 0
	w := &Watcher{Source: func() string {
		if i >= len(lines) {
			return "ready"
		}
		line := lines[i]
		i++
		return line
	}}
	var got []string
	for n := 0; n < len(lines)+2; n++ {
		if line, ok := w.Next(); ok {
			got = append(got, line)
		}
	}
	if want := []string{"pulling", "starting", "ready"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sampled %v, want %v", got, want)
	}
}

// A named step ends as a row: the name in its column, what it did beside it, and
// how long it took. The transcript never wraps, so the row is the same bytes
// whatever the terminal was.
func TestPlainNamedStepEndsAsARow(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	reads := []time.Duration{0, 41 * time.Second}
	read := 0
	p.Now = func() time.Time {
		at := t0.Add(reads[read])
		if read < len(reads)-1 {
			read++
		}
		return at
	}
	h := p.Start(Op{Name: "0.6.2 → 0.6.3", Label: "updating s5n to 0.6.3"})
	h.Done("")
	if got, want := buf.String(), "✓ 0.6.2 → 0.6.3  41s\n"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

// A named wait puts each new line from its source in the log and nowhere else:
// a transcript has no line to redraw, so the screen only sees the finish.
func TestPlainWaitSamplesIntoTheLogOnly(t *testing.T) {
	var screen, log bytes.Buffer
	p := &Plain{W: &screen, Log: &log, Sample: 5 * time.Millisecond}
	lines := []string{"pulling layer 1", "pulling layer 2", "pulling layer 2", "starting"}
	var mu sync.Mutex
	i := 0
	source := func() string {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(lines) {
			return "starting"
		}
		line := lines[i]
		i++
		return line
	}
	h := p.Start(Op{Name: "0.6.2 → 0.6.3", Budget: time.Minute, Source: source})
	time.Sleep(40 * time.Millisecond)
	h.Done("")
	if got := screen.String(); got != "✓ 0.6.2 → 0.6.3  0s\n" {
		t.Fatalf("screen = %q, want only the finish row", got)
	}
	logged := log.String()
	for _, line := range []string{"pulling layer 1", "pulling layer 2", "starting"} {
		if n := strings.Count(logged, line); n != 1 {
			t.Fatalf("the log carries %q %d times, want once:\n%s", line, n, logged)
		}
	}
}

// A failure is the one ✗ row: the subject in its column, the refusal beside it,
// the reason under it -- and a failed run's last line is where its log is.
func TestPlainErrorIsOneRowAndTheLogPath(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf, LogPath: "/var/log/box/update-20261123T075500Z.log"}
	err := fmt.Errorf("can't read s5n 0.2.0 from the registry: it has no platform.yaml")
	p.Rows([]Row{{Mark: MarkNote, Facts: []string{"nothing on this box changed"}}})
	p.Error(&Subject{Name: "chart", Facts: []string{"can't read s5n 0.2.0 from the registry"}, Why: "it has no platform.yaml", Err: err})
	want := strings.Join([]string{
		"· nothing on this box changed",
		"✗ chart          can't read s5n 0.2.0 from the registry",
		"                 it has no platform.yaml",
		"· log            /var/log/box/update-20261123T075500Z.log",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Fatalf("failure:\n%s\nwant:\n%s", got, want)
	}
}

// An error a flow has already drawn says nothing more, and the log's path is
// still the last line: the run failed, and the transcript is where the rest is.
func TestPlainReportedErrorOnlyNamesTheLog(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf, LogPath: "/var/log/box/update-20260101T000000Z.log"}
	p.Error(Reported{Err: errors.New("the step stopped")})
	if got, want := buf.String(), "· log            /var/log/box/update-20260101T000000Z.log\n"; got != want {
		t.Fatalf("failure = %q, want %q", got, want)
	}
}

// A run with no log says nothing about one, and a run with a log keeps the
// screen and the transcript in step: every line, twice.
func TestPlainWritesTheScreenAndTheLog(t *testing.T) {
	var screen, log bytes.Buffer
	p := &Plain{W: &screen, Log: &log}
	p.Rows([]Row{{Mark: MarkDone, Name: "wiring", Facts: []string{"34 checks"}}})
	p.Detail("wiring sonarr/downloadclient: ok (created)")
	if screen.String() != "✓ wiring         34 checks\n" {
		t.Fatalf("screen = %q", screen.String())
	}
	if want := "✓ wiring         34 checks\nwiring sonarr/downloadclient: ok (created)\n"; log.String() != want {
		t.Fatalf("log = %q, want %q", log.String(), want)
	}
}

// A block is read down a column: one name wider than the built-in column
// widens the whole block, so the rows after it keep their facts in the same
// column instead of drifting two columns to the right (rule 1). It is what
// `admission policies` needed: every row of a run starts its facts in one
// place, or none of them do.
func TestPlainRowsWidenTheWholeBlock(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Rows([]Row{
		{Mark: MarkDone, Name: "Envoy Gateway", Facts: []string{"38s"}},
		{Mark: MarkDone, Name: "cert-manager", Facts: []string{"52s"}},
		{Mark: MarkDone, Name: "admission policies", Facts: []string{"2s"}},
	})
	want := strings.Join([]string{
		"✓ Envoy Gateway       38s",
		"✓ cert-manager        52s",
		"✓ admission policies  2s",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", got, want)
	}
}
