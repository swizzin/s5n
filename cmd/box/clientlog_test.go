package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	"github.com/swizzin/s5n/internal/runlog"
)

// captureStderr takes box's stderr away until the test ends and returns what
// landed there: klog writes to os.Stderr itself (klog/v2 klog.go:936).
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("stderr capture: %v", err)
	}
	old := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		_ = f.Close()
	})
	return func() string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("stderr capture: %v", err)
		}
		return string(b)
	}
}

// restoreKlog puts klog back on stderr, after the test restored os.Stderr: box
// points it once, and a test must not leave it on a log it closed.
func restoreKlog(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})
}

// openTestLog is box's setup in a directory of the test's own.
func openTestLog(t *testing.T) {
	t.Helper()
	t.Setenv(runlog.DirEnv, t.TempDir())
	old := runLog
	runLog = nil
	t.Cleanup(func() {
		closeLog()
		runLog = old
	})
	restoreKlog(t)
	startLog()
	if logPath(runLog) == "" {
		t.Fatal("box's setup opened no run log")
	}
}

// TestKlogWritesOnlyTheRunLog is box's setup, then one klog call per shape and
// severity: nothing on the terminal, one line each in the transcript, marked as
// client-go's.
func TestKlogWritesOnlyTheRunLog(t *testing.T) {
	openTestLog(t)
	stderr := captureStderr(t)

	klog.Info("a client-go info line")
	klog.Warning("a client-go warning")
	klog.Error("a client-go error")
	// InfoS and ErrorS are the shapes client-go's own loggers use.
	klog.InfoS("a client-go structured line", "type", "/v1, Kind=Secret")
	klog.ErrorS(errors.New("boom"), "a client-go structured error")

	if got := stderr(); got != "" {
		t.Fatalf("klog wrote to box's stderr:\n%s", got)
	}
	body := readLog(t)
	if marked := strings.Count(body, "  "+clientPrefix); marked != 5 {
		t.Fatalf("the run log has %d marked lines, want one per klog call:\n%s", marked, body)
	}
	for _, want := range []struct{ severity, msg string }{
		{"client-go: I", "a client-go info line"},
		{"client-go: W", "a client-go warning"},
		{"client-go: E", "a client-go error"},
		{"client-go: I", "a client-go structured line"},
		{"client-go: E", "a client-go structured error"},
	} {
		line := lineWith(body, want.msg)
		if line == "" {
			t.Fatalf("the run log has no %q:\n%s", want.msg, body)
		}
		if !strings.Contains(line, want.severity) {
			t.Errorf("klog's line for %q reads %q, want it marked %q", want.msg, line, want.severity)
		}
	}
}

// readLog is the transcript box's setup opened.
func readLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(logPath(runLog))
	if err != nil {
		t.Fatalf("read the run log: %v", err)
	}
	return string(b)
}

// lineWith is the one line carrying msg, or "".
func lineWith(body, msg string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, msg) {
			return line
		}
	}
	return ""
}

// TestKlogLinesAreRedacted: klog's bytes go through the run log like box's own,
// so a secret a client-go line carries is replaced there too.
func TestKlogLinesAreRedacted(t *testing.T) {
	openTestLog(t)

	redact("a-real-secret-value")
	klog.InfoS("connecting", "token", "a-real-secret-value")

	body := readLog(t)
	if strings.Contains(body, "a-real-secret-value") {
		t.Fatalf("the run log kept a secret a klog line carried:\n%s", body)
	}
	if !strings.Contains(lineWith(body, runlog.Redacted), "client-go: ") {
		t.Fatalf("the klog line is not in the run log, redacted:\n%s", body)
	}
}

// TestClientLinesPrefixEveryLine: one call's bytes are prefixed per line, and a
// chunk that stops mid-line does not get a second prefix (klog/v2 klog.go:797).
func TestClientLinesPrefixEveryLine(t *testing.T) {
	var got strings.Builder
	c := &clientLines{w: &got}
	if _, err := c.Write([]byte("one\ntwo")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte(" and a half\nthree\n")); err != nil {
		t.Fatal(err)
	}
	want := "client-go: one\nclient-go: two and a half\nclient-go: three\n"
	if got.String() != want {
		t.Fatalf("clientLines wrote %q, want %q", got.String(), want)
	}
}

// TestKlogWithNoRunLogWritesNothing: an unwritable directory is runlog's one
// muted line, and klog then has nowhere to write, the terminal included.
func TestKlogWithNoRunLogWritesNothing(t *testing.T) {
	t.Run("nil log", func(t *testing.T) {
		klogWritesNothing(t, nil)
	})
	t.Run("disabled log", func(t *testing.T) {
		blocked := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blocked, nil, 0o600); err != nil {
			t.Fatalf("block the log directory: %v", err)
		}
		lg, err := runlog.OpenAt(filepath.Join(blocked, "logs"), "test")
		if err == nil {
			t.Fatal("a log under a plain file opened")
		}
		klogWritesNothing(t, lg)
	})
}

// klogWritesNothing points klog at l, logs at every shape and severity and
// fails if any byte reached the terminal.
func klogWritesNothing(t *testing.T, l *runlog.Log) {
	t.Helper()
	restoreKlog(t)
	stderr := captureStderr(t)
	pointClientLogs(l)

	klog.Info("a client-go info line")
	klog.Warning("a client-go warning")
	klog.Error("a client-go error")
	klog.InfoS("a client-go structured line", "type", "/v1, Kind=Secret")
	klog.ErrorS(errors.New("boom"), "a client-go structured error")

	if got := stderr(); got != "" {
		t.Fatalf("klog wrote to box's stderr with no run log:\n%s", got)
	}
}

// TestAStartWithNoOutputKeepsNoLog is main's first step alone, the whole run of a
// command such as status that opens no output: klog reaches neither the terminal
// nor a run log, so a read takes none of the run log's places.
func TestAStartWithNoOutputKeepsNoLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(runlog.DirEnv, dir)
	old := runLog
	runLog = nil
	t.Cleanup(func() {
		closeLog()
		runLog = old
	})
	restoreKlog(t)
	stderr := captureStderr(t)
	atStart()
	klog.InfoS("a client-go structured line", "type", "/v1, Kind=Secret")
	klog.Error("a client-go error line")
	klog.Flush()
	if got := stderr(); got != "" {
		t.Errorf("stderr = %q, want nothing", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the run log directory holds %d entries after a start that opened no output, want none", len(entries))
	}
}
