package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/runlog"
)

// signalChildEnv marks the child process TestSecondSignalKills starts: the
// child has to be killed by a signal, which would take the test binary with it
// if it ran in this process.
const signalChildEnv = "BOX_SIGNAL_CHILD"

// entryChildEnv marks the child process TestRunBootstrapUsesItsContext starts:
// the command exits the process, which would take the test binary with it.
const entryChildEnv = "BOX_ENTRY_CHILD"

func TestSignalContextCancelsOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("box runs on Linux hosts")
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := signalContext(context.Background(), sig)
			defer stop()
			self, err := os.FindProcess(os.Getpid())
			if err != nil {
				t.Fatalf("find this process: %v", err)
			}
			if err := self.Signal(sig); err != nil {
				t.Fatalf("send %v to this process: %v", sig, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the signal did not cancel the context")
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("context error = %v, want context.Canceled", ctx.Err())
			}
		})
	}
}

func TestSecondSignalKills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("box runs on Linux hosts")
	}
	if os.Getenv(signalChildEnv) != "" {
		t.Skip("child process, run by TestSecondSignalKills")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalChild$")
	cmd.Env = append(os.Environ(), signalChildEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe the child's stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	lines := scanLines(stdout)
	waitLine(t, lines, "ready")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send the first signal: %v", err)
	}
	waitLine(t, lines, "cancelled")
	// signalContext stops handling signals from the goroutine AfterFunc starts,
	// so the child is only killable once that has run: give it a moment rather
	// than racing it.
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send the second signal: %v", err)
	}
	err = waitFor(t, cmd.Wait, 10*time.Second)
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("child ended %v, want killed by a signal", err)
	}
	status, ok := exited.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("child wait status = %v, want SIGINT", status)
	}
}

func TestSignalChild(t *testing.T) {
	if os.Getenv(signalChildEnv) == "" {
		t.Skip("runs in a child process from TestSecondSignalKills")
	}
	ctx, stop := signalContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("cancelled")
	// Nothing below may return before the parent's second signal arrives. The
	// sleep keeps a timer alive so the runtime's deadlock detector cannot fire
	// first.
	for {
		time.Sleep(time.Hour)
	}
}

func TestRunBootstrapUsesItsContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("box runs on Linux hosts")
	}
	if os.Getenv(entryChildEnv) != "" {
		t.Skip("child process, run by TestRunBootstrapUsesItsContext")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapCancelChild$")
	// A KUBECONFIG in the test environment would take the explicit-cluster
	// branch, whose refusal is not the cancellation this test is about.
	cmd.Env = append(withoutEnv(os.Environ(), "KUBECONFIG"), entryChildEnv+"=1", runlog.DirEnv+"="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 {
		t.Fatalf("bootstrap with a cancelled context exited %v (want 1):\n%s", err, out)
	}
	if !strings.Contains(string(out), "✗ context canceled") {
		t.Fatalf("bootstrap did not report the cancellation:\n%s", out)
	}
}

func TestBootstrapCancelChild(t *testing.T) {
	if os.Getenv(entryChildEnv) == "" {
		t.Skip("runs in a child process from TestRunBootstrapUsesItsContext")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The record is a path that does not exist: a recorded cluster would take
	// the re-probe branch, which reports a stop rather than the cancellation.
	runBootstrap(ctx, []string{"--dry-run", "--record", filepath.Join(t.TempDir(), "cluster.yaml")})
	t.Fatal("bootstrap returned without reporting the cancellation")
}

// withoutEnv returns env without key, so a child process cannot inherit the
// kubeconfig this machine happens to have.
func withoutEnv(env []string, key string) []string {
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func scanLines(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		s := bufio.NewScanner(r)
		for s.Scan() {
			ch <- s.Text()
		}
	}()
	return ch
}

// waitLine reads until the wanted line arrives, and fails the test on a
// timeout or on the child's output ending without it.
func waitLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the child's output ended before %q", want)
			}
			if line == want {
				return
			}
		case <-deadline:
			t.Fatalf("waited 10s for the child to print %q", want)
		}
	}
}

// waitFor runs fn and gives it a deadline, so a child that survives the second
// signal fails the test instead of hanging it.
func waitFor(t *testing.T, fn func() error, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("waited %s for the child to exit", d)
		return nil
	}
}
