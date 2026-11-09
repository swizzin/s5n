package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testK0sVersion = "v9.9.9-test"

// The stubs record into one shared trace, so a test can assert the order of
// the whole sequence and not only its outcome.
type installStubs struct {
	trace *[]string
	fetch *stubFetcher
	run   *stubRunner
	ready *stubReady
}

type stubFetcher struct {
	trace *[]string
	body  []byte
	err   error
	urls  []string
}

func (s *stubFetcher) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	s.urls = append(s.urls, url)
	*s.trace = append(*s.trace, "fetch")
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

type stubRunner struct {
	trace *[]string
	calls []string
	out   map[string]string
	fail  map[string]error
}

func (r *stubRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	r.calls = append(r.calls, key)
	*r.trace = append(*r.trace, "run "+key)
	return []byte(r.out[key]), r.fail[key]
}

type stubReady struct {
	trace     *[]string
	attempts  int
	failFirst int
	err       error
}

func (s *stubReady) Ready(context.Context) error {
	s.attempts++
	*s.trace = append(*s.trace, "ready")
	if s.attempts <= s.failFirst {
		return s.err
	}
	return nil
}

// testInstaller builds an installer that touches only t.TempDir(): the release
// pin is a checksum of the payload the stub serves, and the clock does not
// actually wait.
func testInstaller(t *testing.T, payload []byte) (K0sInstaller, *installStubs) {
	t.Helper()
	dir := t.TempDir()
	trace := &[]string{}
	fetch := &stubFetcher{trace: trace, body: payload}
	stubs := &installStubs{
		trace: trace,
		fetch: fetch,
		run:   &stubRunner{trace: trace, out: map[string]string{}, fail: map[string]error{}},
		ready: &stubReady{trace: trace},
	}
	sum := sha256.Sum256(payload)
	clock := time.Unix(0, 0)
	return K0sInstaller{
		Log:      io.Discard,
		Root:     func() bool { return true },
		Evidence: func() []Finding { return nil },
		Release: func(arch string) (k0sRelease, error) {
			return k0sRelease{Version: testK0sVersion, Arch: arch, SHA256: hex.EncodeToString(sum[:])}, nil
		},
		Fetch:    fetch,
		Run:      stubs.run,
		Ready:    stubs.ready,
		BinPath:  filepath.Join(dir, "k0s"),
		StateDir: filepath.Join(dir, "state"),
		ConfPath: filepath.Join(dir, "admin.conf"),
		Timeout:  3 * time.Second,
		Interval: time.Second,
		Now:      func() time.Time { return clock },
		Sleep:    func(d time.Duration) { clock = clock.Add(d) },
	}, stubs
}

func testPlan() InstallPlan {
	return InstallPlan{Version: testK0sVersion, Arch: "amd64", CleanHost: true}
}

var testPayload = []byte("#!/bin/sh\n# a k0s stand-in\n")

func TestK0sInstallSequence(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	if err := k.Install(context.Background(), testPlan()); err != nil {
		t.Fatalf("install: %v", err)
	}

	want := []string{"fetch", "run install controller --single", "run start", "ready"}
	if strings.Join(*stubs.trace, "|") != strings.Join(want, "|") {
		t.Fatalf("sequence = %v, want %v", *stubs.trace, want)
	}
	got, err := os.ReadFile(k.BinPath)
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if !bytes.Equal(got, testPayload) {
		t.Errorf("installed bytes differ from the verified download")
	}
	fi, err := os.Stat(k.BinPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
	if want := "https://github.com/k0sproject/k0s/releases/download/" + testK0sVersion + "/k0s-" + testK0sVersion + "-amd64"; len(stubs.fetch.urls) != 1 || stubs.fetch.urls[0] != want {
		t.Errorf("fetched %v, want %s", stubs.fetch.urls, want)
	}
}

// Every refusal happens before the first byte is fetched and before anything is
// written: a run that stops here leaves the host exactly as it was.
func TestK0sInstallRefusesBeforeFetch(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*K0sInstaller, *installStubs) InstallPlan
		want    string
	}{
		{
			name: "not root",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				k.Root = func() bool { return false }
				return testPlan()
			},
			want: "as root",
		},
		{
			name: "the decision did not choose to install",
			prepare: func(_ *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan()
				p.CleanHost = false
				return p
			},
			want: "did not choose to install a cluster",
		},
		{
			name: "k0s is already on the host",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				k.Evidence = func() []Finding {
					return []Finding{{Kind: "state", Name: K0sStateDir}}
				}
				return testPlan()
			},
			want: "k0s is already on this host (state " + K0sStateDir + ")",
		},
		{
			name: "no pin for this architecture",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				k.Release = k0sReleaseFor
				p := testPlan()
				p.Arch = "riscv64"
				return p
			},
			want: `not published for "riscv64"`,
		},
		{
			name: "plan names another version",
			prepare: func(_ *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan()
				p.Version = "v0.0.1"
				return p
			},
			want: "refusing to install something else",
		},
		{
			name: "plan names another checksum",
			prepare: func(_ *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan()
				p.SHA256 = strings.Repeat("0", 64)
				return p
			},
			want: "refusing to install something else",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, stubs := testInstaller(t, testPayload)
			plan := tc.prepare(&k, stubs)
			err := k.Install(context.Background(), plan)
			if err == nil {
				t.Fatal("install succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
			if len(*stubs.trace) != 0 {
				t.Errorf("refusal still touched something: %v", *stubs.trace)
			}
			entries, _ := os.ReadDir(filepath.Dir(k.BinPath))
			if len(entries) != 0 {
				t.Errorf("refusal left files behind: %v", entries)
			}
		})
	}
}

// A host that already carries k0s keeps the binary it has, byte for byte.
func TestK0sInstallNeverOverwritesAPreExistingBinary(t *testing.T) {
	k, stubs := testInstaller(t, []byte("new binary"))
	old := []byte("#!/bin/sh\n# k0s that was already here\n")
	if err := os.WriteFile(k.BinPath, old, 0o755); err != nil {
		t.Fatal(err)
	}
	k.Evidence = func() []Finding { return []Finding{{Kind: "binary", Name: k.BinPath}} }

	if err := k.Install(context.Background(), testPlan()); err == nil {
		t.Fatal("install succeeded over a pre-existing k0s")
	}
	if len(*stubs.trace) != 0 {
		t.Errorf("touched the host: %v", *stubs.trace)
	}
	got, err := os.ReadFile(k.BinPath)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("pre-existing binary changed: %q, %v", got, err)
	}
}

// A download that does not match the compiled-in checksum is removed, and no
// command is ever run against it.
func TestK0sInstallRejectsBadChecksum(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	k.Fetch = &stubFetcher{trace: stubs.trace, body: []byte("tampered")}

	err := k.Install(context.Background(), testPlan())
	if err == nil || !strings.Contains(err.Error(), "refusing to install it") {
		t.Fatalf("err = %v, want a checksum refusal", err)
	}
	if _, statErr := os.Stat(k.BinPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a mismatched download was installed at %s", k.BinPath)
	}
	entries, _ := os.ReadDir(filepath.Dir(k.BinPath))
	if len(entries) != 0 {
		t.Errorf("mismatched download left files behind: %v", entries)
	}
	for _, c := range stubs.run.calls {
		t.Errorf("ran %q after a checksum failure", c)
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("err = %q, want both checksums named", err)
	}
}

// Once the binary is in place, a failure names what is left and the command
// that removes it -- and box never runs that command itself.
func TestK0sInstallFailureNamesWhatItLeft(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*installStubs)
		want    []string
	}{
		{
			name: "k0s could not install the service",
			prepare: func(s *installStubs) {
				s.run.fail["install controller --single"] = errors.New("exit status 1")
				s.run.out["install controller --single"] = "no systemd\n"
			},
			want: []string{"could not be installed as a service", "k0s said:", "    no systemd", "k0s stop; k0s reset"},
		},
		{
			name: "k0s could not start",
			prepare: func(s *installStubs) {
				s.run.fail["start"] = errors.New("exit status 1")
				s.run.out["start"] = "controller did not come up\n"
			},
			want: []string{"did not start", "k0s said:", "    controller did not come up", "k0s stop; k0s reset"},
		},
		{
			name: "the API never answered",
			prepare: func(s *installStubs) {
				s.ready.failFirst = 1 << 20
				s.ready.err = errors.New("connection refused")
			},
			want: []string{"did not answer ready", "not ready after 3s", "connection refused", "k0s stop; k0s reset"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, stubs := testInstaller(t, testPayload)
			tc.prepare(stubs)
			err := k.Install(context.Background(), testPlan())
			if err == nil {
				t.Fatal("install succeeded, want a failure")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
			for _, want := range []string{k.BinPath, k.StateDir} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to name %s", err, want)
				}
			}
			for _, c := range stubs.run.calls {
				if c == "reset" || c == "stop" || strings.HasPrefix(c, "stop ") {
					t.Errorf("box ran %q: it must never reset a cluster on its own", c)
				}
			}
			// The binary is still there, which is exactly what the message says.
			if _, statErr := os.Stat(k.BinPath); statErr != nil {
				t.Errorf("binary missing after a failure the message says left it: %v", statErr)
			}
		})
	}
}

// The wait is a deadline, not an endless poll: the stub clock proves how many
// attempts box made and that it stopped at the timeout.
func TestK0sInstallWaitStopsAtDeadline(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	k.Timeout = 5 * time.Second
	k.Interval = time.Second
	stubs.ready.failFirst = 1 << 20
	stubs.ready.err = errors.New("the node is not Ready yet")

	err := k.Install(context.Background(), testPlan())
	if err == nil {
		t.Fatal("install succeeded, want a readiness failure")
	}
	if !strings.Contains(err.Error(), "not ready after 5s") {
		t.Errorf("err = %q, want the deadline named", err)
	}
	if stubs.ready.attempts != 6 { // 5s / 1s, plus the attempt at t=0
		t.Errorf("attempts = %d, want 6 (t=0 through t=5s)", stubs.ready.attempts)
	}
}

// A failure before the binary is written is not a host box changed: no undo
// note, because there is nothing to undo.
func TestK0sInstallDownloadFailureLeavesNoUndoNote(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	k.Fetch = &stubFetcher{trace: stubs.trace, err: errors.New("dial tcp: network unreachable")}

	err := k.Install(context.Background(), testPlan())
	if err == nil {
		t.Fatal("install succeeded, want a download failure")
	}
	if !strings.Contains(err.Error(), "download k0s") {
		t.Errorf("err = %q, want the download named", err)
	}
	if strings.Contains(err.Error(), "k0s reset") {
		t.Errorf("err = %q, but nothing was left on the host", err)
	}
	for _, c := range stubs.run.calls {
		t.Errorf("ran %q after a failed download", c)
	}
}

func TestDryRunInstallerPrintsThePlan(t *testing.T) {
	var b strings.Builder
	plan := InstallPlan{Version: testK0sVersion, Arch: "arm64", CleanHost: true}
	if err := (DryRunInstaller{W: &b}).Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{testK0sVersion, "arm64", "/usr/local/bin/k0s", "install controller --single", "start"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not mention %q", out, want)
		}
	}
}
