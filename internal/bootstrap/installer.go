package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/say"
)

// Downloader fetches one URL. The caller closes the body.
type Downloader interface {
	Fetch(ctx context.Context, url string) (io.ReadCloser, error)
}

// Runner runs one command and returns what it said, so a failure can be quoted
// under box's message and a success can be dropped.
type Runner interface {
	Run(ctx context.Context, path string, args ...string) ([]byte, error)
}

// Readiness answers one question: is the API server box just started ready to
// serve? It is polled, so it must be cheap and must not write anything.
type Readiness interface {
	Ready(ctx context.Context) error
}

// The readiness wait. A first start has to bring containerd and the kubelet up
// and register the node, which is tens of seconds on a fast host and minutes on
// a small VPS; three minutes is the point where box stops guessing.
const (
	DefaultReadyTimeout  = 3 * time.Minute
	DefaultReadyInterval = 2 * time.Second
)

// K0sInstaller is the real Installer: it downloads the pinned release, checks
// it against the checksum compiled into box, writes it to K0sBinaryPath,
// installs and starts a single-node controller, then waits until the API and a
// node answer. The zero value is usable; every field exists so a test can
// replace one piece without a host, a network or a systemd.
type K0sInstaller struct {
	// Say is every progress line the installer writes; k0s's own output never
	// goes here, it appears only in a failure, quoted under box's message.
	Say say.Reporter
	// Root reports whether box may install a system service. Default euid 0.
	Root func() bool
	// Evidence reports what is already on the host. Default k0sEvidence.
	Evidence func() []Finding
	// Release resolves the pin for an architecture. Default k0sReleaseFor;
	// tests replace it with a checksum they control.
	Release func(arch string) (k0sRelease, error)
	Fetch   Downloader
	Run     Runner
	// Ready checks the API. Nil means the admin kubeconfig at ConfPath.
	Ready Readiness
	// BinPath, StateDir and ConfPath are the install target, k0s's state and
	// the admin kubeconfig k0s writes. Empty means the constants.
	BinPath, StateDir, ConfPath string
	// Timeout and Interval bound the readiness wait. Zero means the defaults.
	Timeout, Interval time.Duration
	// Now and Sleep are the clock, so the wait is testable without waiting.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// Install is the whole clean-host sequence. Everything that can refuse happens
// before the first byte is fetched; everything after the binary is in place
// reports what it left on the host and how to undo it.
func (k K0sInstaller) Install(ctx context.Context, plan InstallPlan) error {
	arch := plan.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	rel, err := k.releaseFor(arch)
	if err != nil {
		return err
	}
	if plan.Version != rel.Version || (plan.SHA256 != "" && plan.SHA256 != rel.SHA256) {
		return fmt.Errorf("install plan asks for k0s %s (sha256 %q), but box is pinned to %s (sha256 %s for %s): refusing to install something else",
			plan.Version, plan.SHA256, rel.Version, rel.SHA256, rel.Arch)
	}
	if why := installRefusal(plan, k.root(), k.evidence()); why != "" {
		return errors.New(why)
	}

	r := say.Or(k.Say, nil)
	bin := k.binPath()
	step := r.Start(say.Op{
		Label: fmt.Sprintf("installing k0s %s for %s", rel.Version, rel.Arch),
		Plain: fmt.Sprintf("installing k0s %s for %s", rel.Version, rel.Arch),
	})
	if err := k.writeBinary(ctx, rel, bin); err != nil {
		step.Failed(err)
		return err
	}
	step.Done(fmt.Sprintf("installed %s", bin))

	// From here the host is changed by this run: every failure names what is
	// there now and the one command that removes it. box never runs that
	// command itself -- `k0s reset` would also remove a k0s box did not
	// install, and destroying a cluster is the operator's call to see happen.
	out, err := k.runK0s(ctx, "install", "controller", "--single")
	if err != nil {
		return k.leftBehind("k0s could not be installed as a service", out, err)
	}
	out, err = k.runK0s(ctx, "start")
	if err != nil {
		return k.leftBehind("k0s did not start", out, err)
	}
	if err := k.waitReady(ctx); err != nil {
		return k.leftBehind("the cluster API did not answer ready", nil, err)
	}
	return nil
}

// writeBinary downloads into a temporary file beside the target and renames it
// into place, so a failed or interrupted download never leaves a partial k0s
// where the unit would exec it. The checksum is computed while the bytes are
// written, and a mismatch removes the file without touching the target.
func (k K0sInstaller) writeBinary(ctx context.Context, rel k0sRelease, bin string) error {
	body, err := k.fetch().Fetch(ctx, rel.URL())
	if err != nil {
		return fmt.Errorf("download k0s %s: %w", rel.Version, err)
	}
	defer func() { _ = body.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(bin), ".k0s-download-*")
	if err != nil {
		return fmt.Errorf("stage k0s beside %s: %w", bin, err)
	}
	name := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(name)
		}
	}()

	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download k0s %s: %w", rel.Version, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write k0s %s: %w", rel.Version, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != rel.SHA256 {
		return fmt.Errorf("k0s %s from %s has sha256 %s, want %s: refusing to install it", rel.Version, rel.URL(), got, rel.SHA256)
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return fmt.Errorf("make k0s executable: %w", err)
	}
	if err := os.Rename(name, bin); err != nil {
		return fmt.Errorf("install k0s to %s: %w", bin, err)
	}
	committed = true
	return nil
}

// runK0s runs the binary box just wrote with the arguments k0s needs.
func (k K0sInstaller) runK0s(ctx context.Context, args ...string) ([]byte, error) {
	run := k.Run
	if run == nil {
		run = ExecRunner{}
	}
	return run.Run(ctx, k.binPath(), args...)
}

// waitReady polls the API until it is ready or the deadline passes, keeping the
// last refusal so the failure says what was actually wrong.
func (k K0sInstaller) waitReady(ctx context.Context) error {
	timeout, interval := k.Timeout, k.Interval
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	if interval <= 0 {
		interval = DefaultReadyInterval
	}
	now, sleep := k.Now, k.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}

	r := say.Or(k.Say, nil)
	wait := r.Start(say.Op{
		Label:  "waiting for the cluster API",
		Plain:  fmt.Sprintf("waiting up to %s for the cluster API", timeout),
		Budget: timeout,
	})
	deadline := now().Add(timeout)
	var last error
	for {
		if err := ctx.Err(); err != nil {
			wait.Failed(err)
			return err
		}
		last = k.ready().Ready(ctx)
		if last == nil {
			wait.Done("the cluster API is ready")
			return nil
		}
		if !now().Before(deadline) {
			wait.Failed(last)
			return fmt.Errorf("not ready after %s: %w", timeout, last)
		}
		sleep(interval)
	}
}

// leftBehind is the failure box returns once this run has changed the host: it
// names what is there now and the one command that undoes it.
func (k K0sInstaller) leftBehind(what string, out []byte, err error) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v", what, err)
	if quoted := quoteOutput(out); quoted != "" {
		fmt.Fprintf(&b, "\n  k0s said:\n%s", quoted)
	}
	fmt.Fprintf(&b, "\n  what is left on this host: the k0s binary at %s and what k0s wrote under %s", k.binPath(), k.stateDir())
	fmt.Fprintf(&b, "\n  undo it with: k0s stop; k0s reset")
	return errors.New(b.String())
}

// quoteOutput indents a command's output so it reads as quoted material under
// box's own sentence, rather than as box saying it.
func quoteOutput(out []byte) string {
	trimmed := strings.TrimSpace(strings.ReplaceAll(string(out), "\r\n", "\n"))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		lines[i] = "    " + strings.TrimRight(line, "\r")
	}
	return strings.Join(lines, "\n")
}

func (k K0sInstaller) releaseFor(arch string) (k0sRelease, error) {
	if k.Release != nil {
		return k.Release(arch)
	}
	return k0sReleaseFor(arch)
}

func (k K0sInstaller) root() bool {
	if k.Root != nil {
		return k.Root()
	}
	return os.Geteuid() == 0
}

func (k K0sInstaller) evidence() []Finding {
	if k.Evidence != nil {
		return k.Evidence()
	}
	return k0sEvidence()
}

func (k K0sInstaller) fetch() Downloader {
	if k.Fetch != nil {
		return k.Fetch
	}
	return HTTPDownloader{}
}

func (k K0sInstaller) ready() Readiness {
	if k.Ready != nil {
		return k.Ready
	}
	return APIReadiness{ConfigPath: k.confPath()}
}

func (k K0sInstaller) binPath() string {
	if k.BinPath != "" {
		return k.BinPath
	}
	return K0sBinaryPath
}

func (k K0sInstaller) stateDir() string {
	if k.StateDir != "" {
		return k.StateDir
	}
	return K0sStateDir
}

func (k K0sInstaller) confPath() string {
	if k.ConfPath != "" {
		return k.ConfPath
	}
	return K0sAdminConfig
}

// DryRunInstaller prints the plan instead of carrying it out. `box bootstrap
// --dry-run` reaches the same clean-host branch as a real run, and this is the
// installer it gets: a dry run probes nothing and writes nothing, so the only
// honest thing it can do with an install is say what it would have done.
type DryRunInstaller struct {
	Say say.Reporter
}

// Install prints the planned k0s install.
func (p DryRunInstaller) Install(_ context.Context, plan InstallPlan) error {
	arch := plan.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	r := say.Or(p.Say, nil)
	r.Line(fmt.Sprintf("box would install k0s %s for %s on this clean host: /usr/local/bin/k0s, install controller --single, start", plan.Version, arch))
	return nil
}
