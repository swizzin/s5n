package hostconf

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnitFiles pins the two files systemd runs: the service is the run nobody
// watches, so its command line, its timeout and the timer's wake schedule are
// the whole behaviour of the feature.
func TestUnitFiles(t *testing.T) {
	service, timer := UnitFiles("/usr/local/bin/box", "/etc/s5n/cluster.yaml",
		"/etc/s5n/seedboxes/root.yaml", "/root/.config/box/values.yaml")
	wantService := `[Unit]
Description=s5n: check for a newer release
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
# An update can wait out an app's thirty-minute startup budget.
TimeoutStartSec=0
ExecStart=/usr/local/bin/box update --timer --record /etc/s5n/cluster.yaml --grant /etc/s5n/seedboxes/root.yaml --values /root/.config/box/values.yaml 
`
	if string(service) != wantService {
		t.Fatalf("service unit:\n--- got ---\n%s\n--- want ---\n%s", service, wantService)
	}
	wantTimer := `[Unit]
Description=s5n: wake box every 15 minutes to check for a newer release

[Timer]
OnBootSec=10min
OnUnitActiveSec=15min
RandomizedDelaySec=10min

[Install]
WantedBy=timers.target
`
	if string(timer) != wantTimer {
		t.Fatalf("timer unit:\n--- got ---\n%s\n--- want ---\n%s", timer, wantTimer)
	}
}

// TestUnitFilesQuoteAPathWithSpaces: ExecStart is split on whitespace, so a box
// in a directory with a space in its name would be started as two commands.
func TestUnitFilesQuoteAPathWithSpaces(t *testing.T) {
	service, _ := UnitFiles("/opt/my box/box", "/etc/s5n/cluster.yaml", "/etc/s5n/root.yaml", "/root/values.yaml")
	if !strings.Contains(string(service), `ExecStart="/opt/my box/box" update --timer `) {
		t.Fatalf("the path with a space was not quoted:\n%s", service)
	}
}

// fakeUnits is a Units a test watches: no directory, no systemctl.
type fakeUnits struct {
	service, timer []byte
	installs       int
	enables        int
	enableErr      error
	// same says the bytes it is given are already there.
	same bool
}

func (f *fakeUnits) Install(service, timer []byte) (bool, error) {
	f.installs++
	f.service, f.timer = service, timer
	return !f.same, nil
}

func (f *fakeUnits) Enable(context.Context) error {
	f.enables++
	return f.enableErr
}

// TestInstallWritesThenEnables: the first run installs both files and starts the
// timer, and a re-run whose bytes are the same does neither.
func TestInstallWritesThenEnables(t *testing.T) {
	f := &fakeUnits{}
	changed, err := Install(context.Background(), f, "/usr/local/bin/box", "/etc/s5n/cluster.yaml", "/etc/s5n/root.yaml", "/root/values.yaml")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !changed || f.enables != 1 {
		t.Fatalf("first Install wrote %v and enabled %d times, want written and enabled once", changed, f.enables)
	}
	if !strings.Contains(string(f.service), "box-update") == false && f.service == nil {
		t.Fatal("no service bytes were installed")
	}
	if !strings.Contains(string(f.timer), "OnUnitActiveSec=15min") {
		t.Fatalf("the timer unit has no wake interval:\n%s", f.timer)
	}

	// The same bytes again: nothing written, so nothing to reload.
	f.same = true
	changed, err = Install(context.Background(), f, "/usr/local/bin/box", "/etc/s5n/cluster.yaml", "/etc/s5n/root.yaml", "/root/values.yaml")
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if changed {
		t.Fatal("a re-run with the same bytes said it wrote")
	}
	if f.enables != 1 {
		t.Fatalf("a re-run with the same bytes enabled systemd %d times, want the first run's one", f.enables)
	}
}

// TestInstallReportsAFailedEnable: a unit file box wrote but could not turn on is
// an error bootstrap says out loud, not a silent timer that never runs.
func TestInstallReportsAFailedEnable(t *testing.T) {
	f := &fakeUnits{enableErr: os.ErrPermission}
	if _, err := Install(context.Background(), f, "/usr/local/bin/box", "c", "g", "v"); err == nil {
		t.Fatal("Install hid a failed systemctl")
	}
}

// TestDirUnitsWriteModeAndSkipUnchanged proves the write half without systemd: the
// files land 0644 (systemd reads them, not root alone) and a second install
// leaves their bytes and mtimes alone.
func TestDirUnitsWriteModeAndSkipUnchanged(t *testing.T) {
	dir := t.TempDir()
	u := DirUnits{Dir: dir}
	service, timer := UnitFiles("/usr/local/bin/box", "/etc/s5n/cluster.yaml", "/etc/s5n/root.yaml", "/root/values.yaml")

	changed, err := u.Install(service, timer)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !changed {
		t.Fatal("the first install of two missing files said nothing changed")
	}
	for _, name := range []string{ServiceFile, TimerFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode != 0o644 {
			t.Errorf("%s mode = %v, want 0644", name, mode)
		}
	}
	before, err := os.Stat(filepath.Join(dir, TimerFile))
	if err != nil {
		t.Fatal(err)
	}
	changed, err = u.Install(service, timer)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if changed {
		t.Fatal("a second install with the same bytes reported a change")
	}
	after, err := os.Stat(filepath.Join(dir, TimerFile))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a second install rewrote an unchanged unit file")
	}
	// And --unit-dir runs no systemctl: Enable has nothing to run.
	if err := u.Enable(context.Background()); err != nil {
		t.Fatalf("DirUnits with no Enable func: %v", err)
	}
}
