package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/update"
)

// A step that stops before its apply -- the release could not be read, the
// apps could not be stopped or copied -- has drawn only its ✗ row. Its reason
// is the error, and it is said once, under that row: a ✗ with no reason is a
// failure nobody can act on without the log.
func TestAFailedStepSaysWhyUnderItsRow(t *testing.T) {
	var buf bytes.Buffer
	out := &say.Plain{W: &buf, Err: &buf}
	cause := errors.New("update s5n to 0.6.4: radarr cannot be snapshotted: no space left for a copy")
	res := update.Result{Target: "0.6.4"}

	err := reportUpdateFailure(out, res, cause)
	if !say.ReportedBy(err) {
		t.Fatalf("the error is not marked as drawn, so the command would print it a second time")
	}
	got := buf.String()
	if !strings.Contains(strings.Join(strings.Fields(got), " "), "radarr cannot be snapshotted: no space left for a copy") {
		t.Fatalf("the reason is not on screen:\n%s", got)
	}
	if strings.Contains(got, "✗") {
		t.Fatalf("the reason drew a second ✗; the step's row is the one:\n%s", got)
	}
}

// Nothing planned: the chart is the subject, and its row carries the reason.
func TestAnUpdateThatPlannedNothingNamesTheChart(t *testing.T) {
	var buf bytes.Buffer
	out := &say.Plain{W: &buf, Err: &buf}
	err := reportUpdateFailure(out, update.Result{}, errors.New("read oci://example/charts/s5n: unauthorized"))
	if !say.ReportedBy(err) {
		t.Fatalf("the error is not marked as drawn")
	}
	if got := buf.String(); !strings.Contains(got, "✗ chart") || !strings.Contains(got, "unauthorized") {
		t.Fatalf("the chart row does not carry the reason:\n%s", got)
	}
}
