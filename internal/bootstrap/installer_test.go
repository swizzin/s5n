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

	"github.com/swizzin/s5n/internal/say"
)

const testK0sVersion = "v9.9.9-test"

// testNodeIP is the uplink address every test plan pins: TEST-NET-1, so no
// example in this file can be mistaken for a real host's address.
const testNodeIP = "192.0.2.10"

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
	// nodeIPs is what the stub Node registered. Nil means the address the
	// plan pinned, so the ordinary sequence verifies; a test that wants a
	// mismatch sets another one.
	nodeIPs []string
	addrErr error
}

func (s *stubReady) Ready(context.Context) error {
	s.attempts++
	*s.trace = append(*s.trace, "ready")
	if s.attempts <= s.failFirst {
		return s.err
	}
	return nil
}

func (s *stubReady) NodeIPs(context.Context) ([]string, error) {
	*s.trace = append(*s.trace, "nodeips")
	if s.addrErr != nil {
		return nil, s.addrErr
	}
	if s.nodeIPs == nil {
		return []string{testNodeIP}, nil
	}
	return s.nodeIPs, nil
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
	clock := time.Unix(0, 0)
	return K0sInstaller{
		Root:     func() bool { return true },
		Evidence: func() []Finding { return nil },
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

var testPayload = []byte("#!/bin/sh\n# a k0s stand-in\n")

// testSum is the checksum of what the stub serves: the plan carries it the way
// k0sPlan carries the chart's pin, and the installer verifies the download
// against it.
func testSum() string {
	sum := sha256.Sum256(testPayload)
	return hex.EncodeToString(sum[:])
}

// testPlan is the clean-host plan a test installs: the release pin, the node
// identity box chose on a plain ethernet uplink, and the config box would
// write -- beside the binary, so the test never touches /etc.
func testPlan(config string) InstallPlan {
	return InstallPlan{
		Version:    testK0sVersion,
		Arch:       "amd64",
		SHA256:     testSum(),
		CleanHost:  true,
		NodeIP:     testNodeIP,
		Uplink:     Uplink{Interface: "eth0", Address: testNodeIP, MTU: 1500},
		ConfigPath: config,
		ConfigText: k0sNodeConfig(testNodeIP),
	}
}

// testConfigPath is where a test's k0s config goes: in the installer's own
// temporary directory.
func testConfigPath(k K0sInstaller) string {
	return filepath.Join(filepath.Dir(k.BinPath), "k0s.yaml")
}

// testInstallArgs is the one command a plan installs with, spelled out here so
// a test asserts the literal flags box runs.
func testInstallArgs(plan InstallPlan) string {
	return "install controller --single --config " + plan.ConfigPath + " --kubelet-extra-args=--node-ip=" + plan.NodeIP
}

func TestK0sInstallSequence(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	plan := testPlan(testConfigPath(k))
	if err := k.Install(context.Background(), plan); err != nil {
		t.Fatalf("install: %v", err)
	}

	want := []string{"fetch", "run " + testInstallArgs(plan), "run start", "ready", "nodeips"}
	if strings.Join(*stubs.trace, "|") != strings.Join(want, "|") {
		t.Fatalf("sequence = %v, want %v", *stubs.trace, want)
	}

	// The node IP is pinned where the kubelet reads it, with the config that
	// pins the API address beside it.
	if !strings.Contains(testInstallArgs(plan), "--kubelet-extra-args=--node-ip="+testNodeIP) {
		t.Errorf("the install does not pin the node IP: %q", testInstallArgs(plan))
	}
	gotConfig, err := os.ReadFile(plan.ConfigPath)
	if err != nil {
		t.Fatalf("the k0s config was not written: %v", err)
	}
	if string(gotConfig) != plan.ConfigText || !strings.Contains(string(gotConfig), "address: "+testNodeIP) {
		t.Errorf("config = %q, want the plan's with the API address pinned to %s", gotConfig, testNodeIP)
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
				return testPlan(testConfigPath(*k))
			},
			want: "as root",
		},
		{
			name: "the decision did not choose to install",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
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
				return testPlan(testConfigPath(*k))
			},
			want: "k0s is already on this host (state " + K0sStateDir + ")",
		},
		{
			// The sum is the pins' and nothing else re-derives it: a plan that
			// carries none is one box cannot verify, so it is refused by
			// architecture before the download, whatever asked for it.
			name: "plan carries no checksum for this architecture",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
				p.Arch = "riscv64"
				p.SHA256 = ""
				return p
			},
			want: `no checksum for k0s ` + testK0sVersion + ` on "riscv64"`,
		},
		{
			name: "plan names no release",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
				p.Version = ""
				return p
			},
			want: "names no k0s release",
		},
		{
			// The address the node registers with is what the CNI's MTU comes
			// from, so a plan that pins none is refused by architecture rather
			// than left to the host.
			name: "plan pins no node IP",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
				p.NodeIP = ""
				return p
			},
			want: "this plan pins none",
		},
		{
			name: "plan carries no k0s config",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
				p.ConfigText = ""
				return p
			},
			want: "this plan carries none",
		},
		{
			name: "the default route leaves through a tunnel",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
				p.Uplink = Uplink{Interface: "tailscale0", Address: "100.64.0.7", MTU: 1280, Tunnel: true}
				p.NodeIP = p.Uplink.Address
				return p
			},
			want: "leaves through tailscale0, a tunnel with an MTU of 1280",
		},
		{
			// k0s reads this path by default, so a file there is somebody's
			// decision about the host: box will not install over it.
			name: "a k0s config box did not write is already there",
			prepare: func(k *K0sInstaller, _ *installStubs) InstallPlan {
				p := testPlan(testConfigPath(*k))
				p.ConfigPath = filepath.Join(t.TempDir(), "k0s.yaml")
				if err := os.WriteFile(p.ConfigPath, []byte("apiVersion: k0s.k0sproject.io/v1beta1\nkind: ClusterConfig\nspec:\n  api:\n    address: 198.51.100.9\n"), 0o644); err != nil {
					t.Fatalf("write the pre-existing config: %v", err)
				}
				return p
			},
			want: "already exists and box did not write it",
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

	if err := k.Install(context.Background(), testPlan(testConfigPath(k))); err == nil {
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

// A download that does not match the pinned checksum is removed, and no
// command is ever run against it.
func TestK0sInstallRejectsBadChecksum(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	k.Fetch = &stubFetcher{trace: stubs.trace, body: []byte("tampered")}

	err := k.Install(context.Background(), testPlan(testConfigPath(k)))
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
		prepare func(*K0sInstaller, *installStubs)
		want    []string
	}{
		{
			name: "k0s could not install the service",
			prepare: func(k *K0sInstaller, s *installStubs) {
				args := testInstallArgs(testPlan(testConfigPath(*k)))
				s.run.fail[args] = errors.New("exit status 1")
				s.run.out[args] = "no systemd\n"
			},
			want: []string{"could not be installed as a service", "k0s said:", "    no systemd", "k0s stop; k0s reset"},
		},
		{
			name: "k0s could not start",
			prepare: func(_ *K0sInstaller, s *installStubs) {
				s.run.fail["start"] = errors.New("exit status 1")
				s.run.out["start"] = "controller did not come up\n"
			},
			want: []string{"did not start", "k0s said:", "    controller did not come up", "k0s stop; k0s reset"},
		},
		{
			name: "the API never answered",
			prepare: func(_ *K0sInstaller, s *installStubs) {
				s.ready.failFirst = 1 << 20
				s.ready.err = errors.New("connection refused")
			},
			want: []string{"did not answer ready", "not ready after 3s", "connection refused", "k0s stop; k0s reset"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, stubs := testInstaller(t, testPayload)
			tc.prepare(&k, stubs)
			err := k.Install(context.Background(), testPlan(testConfigPath(k)))
			if err == nil {
				t.Fatal("install succeeded, want a failure")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
			for _, want := range []string{k.BinPath, k.StateDir, testConfigPath(k)} {
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

	err := k.Install(context.Background(), testPlan(testConfigPath(k)))
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

	err := k.Install(context.Background(), testPlan(testConfigPath(k)))
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
	plan := InstallPlan{
		Version: testK0sVersion, Arch: "arm64", CleanHost: true,
		NodeIP:     testNodeIP,
		Uplink:     Uplink{Interface: "eth0", Address: testNodeIP, MTU: 1500},
		ConfigPath: "/etc/k0s/k0s.yaml",
		ConfigText: k0sNodeConfig(testNodeIP),
	}
	if err := (DryRunInstaller{Say: &say.Plain{W: &b}}).Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The dry run is the operator's one look at the node's identity before
	// anything changes, so the address, its interface and that interface's MTU
	// are all in it.
	for _, want := range []string{testK0sVersion, "arm64", "/usr/local/bin/k0s", "install controller --single", "start",
		testNodeIP, "eth0", "MTU 1500"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not mention %q", out, want)
		}
	}
}

// A k0s config box wrote itself, byte for byte, is reused: that is what a run
// that failed after writing it leaves, and a second run must be able to finish.
func TestK0sInstallReusesItsOwnConfig(t *testing.T) {
	k, _ := testInstaller(t, testPayload)
	plan := testPlan(testConfigPath(k))
	if err := os.WriteFile(plan.ConfigPath, []byte(plan.ConfigText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := k.Install(context.Background(), plan); err != nil {
		t.Fatalf("install with box's own config in place: %v", err)
	}
}
