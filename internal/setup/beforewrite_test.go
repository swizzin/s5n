package setup

// beforewrite_test.go proves the one hook a flow uses to ask about the platform
// and the seedbox together: it runs after init's last question and before init's
// first write, so a plan shown there has every answer in it and a "no" leaves
// both files, both directories and the release exactly as they were.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestBeforeWriteRunsAfterTheLastQuestionAndBeforeTheFirstWrite: the hook sees
// the answers every question settled, and nothing of the box is on disk yet --
// no grant, no values file, no directory, no hand-over.
func TestBeforeWriteRunsAfterTheLastQuestionAndBeforeTheFirstWrite(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	var (
		got      Answers
		ran      bool
		grant    bool
		values   bool
		applied  bool
		madeDir  bool
		askedLen int
	)
	opts := h.options()
	opts.BeforeWrite = func(_ context.Context, a Answers) error {
		ran = true
		got = a
		askedLen = len(h.prompt.asked)
		grant = h.exists(h.grantPath)
		values = h.exists(h.valuesPath)
		applied = len(h.applied) > 0
		madeDir = len(h.host.mkdirs) > 0
		return nil
	}
	if _, err := h.run(opts); err != nil {
		t.Fatalf("run: %v\n%s", err, h.out.String())
	}
	if !ran {
		t.Fatal("the hook never ran")
	}
	// The answers, in the shape the plan and its question read them.
	if got.Login != "alice" {
		t.Errorf("the hook saw login %q, want alice", got.Login)
	}
	if got.Result.Domain != "box.example.com" {
		t.Errorf("the hook saw domain %q, want box.example.com", got.Result.Domain)
	}
	if got.Namespace != "seedbox" {
		t.Errorf("the hook saw namespace %q, want seedbox", got.Namespace)
	}
	if got.DataPath != "/srv/s5n/data" || got.ConfigPath != "/srv/s5n/config" {
		t.Errorf("the hook saw the paths %q and %q", got.DataPath, got.ConfigPath)
	}
	if strings.Join(got.Apps, ",") != "alpha" {
		t.Errorf("the hook saw the apps %v, want [alpha]", got.Apps)
	}
	if got.Result.ReRun {
		t.Error("the hook was told this is a re-run: the first run of an empty host is not one")
	}
	// Before the last question was not asked, the hook is not run: the answers
	// it shows have to be the ones a person gave.
	if askedLen == 0 {
		t.Error("the hook ran before any question was asked")
	}
	for _, before := range []struct {
		what bool
		it   string
	}{
		{grant, "the grant was written before the hook"},
		{values, "the values file was written before the hook"},
		{applied, "the hand-over ran before the hook"},
		{madeDir, "a directory was made before the hook"},
	} {
		if before.what {
			t.Error(before.it)
		}
	}
}

// TestBeforeWriteRefusalWritesNothing: the hook is where a flow asks its one
// question, so an answer of no is the last read-only moment. Nothing is left
// behind for a person to clean up.
func TestBeforeWriteRefusalWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	changed := errors.New("the platform is not wanted")
	opts := h.options()
	opts.BeforeWrite = func(context.Context, Answers) error { return changed }
	_, err := h.run(opts)
	if !errors.Is(err, changed) {
		t.Fatalf("the hook's refusal came back as %v", err)
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Error("a refused question wrote one of the two files")
	}
	if len(h.host.mkdirs) > 0 {
		t.Errorf("a refused question made directories: %v", h.host.mkdirs)
	}
	if len(h.applied) > 0 {
		t.Error("a refused question handed over to apply")
	}
}

// TestASeedboxThatFailsAfterTheAnswerLeavesThePlatform: the hook answered yes
// and installed the platform, and then init's own write failed. The answers init
// wrote stay -- a re-run reads them back and asks only for what changed -- and
// the failure is what the run stops on, so the next run can finish the same
// install.
func TestASeedboxThatFailsAfterTheAnswerLeavesThePlatform(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.applyErr = errors.New("the release did not install")
	answered := false
	opts := h.options()
	opts.BeforeWrite = func(context.Context, Answers) error {
		answered = true
		return nil
	}
	_, err := h.run(opts)
	if !answered {
		t.Fatal("the hook never ran")
	}
	if err == nil || !strings.Contains(err.Error(), "the release did not install") {
		t.Fatalf("the failure after the answer came back as %v", err)
	}
	if !h.exists(h.grantPath) {
		t.Error("init wrote no grant before the hand-over, so a re-run has nothing to read back")
	}
	if len(h.applied) == 0 {
		t.Error("the hand-over never ran: the failure is the release's, not the hook's")
	}
}
