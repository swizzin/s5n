package wiring

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The shape box reads: a check is its name and what it cost on one line, because
// the wait shows the newest line beside what it waits for.
func TestEveryStepIsOneLineOfItsOwn(t *testing.T) {
	h := newHarness(t)
	var log bytes.Buffer
	h.runner.Log = &log

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("wiring: %v\n%s", err, Summary(res))
	}
	out := log.String()
	for _, step := range res.Steps {
		start := step.Name + ": "
		if !strings.HasPrefix(out, start) && !strings.Contains(out, "\n"+start) {
			t.Fatalf("the log has no %q line:\n%s", start, out)
		}
	}
	if strings.Contains(out, "sonarr/downloadclient                   created") {
		t.Fatalf("a step is still padded to the summary's column:\n%s", out)
	}
	// A check is one line.
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, " ") {
			t.Fatalf("a step line is indented under nothing:\n%s", out)
		}
		if i := strings.Index(line, ": "); i <= 0 {
			continue
		}
		if strings.Contains(strings.SplitN(line, ": ", 2)[0], "  ") {
			t.Fatalf("a step's name carries its old column padding:\n%s", out)
		}
	}
}
