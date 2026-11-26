package hostconf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// The two units that make `box update` run by itself, and where systemd reads them.
const (
	// UnitDir is systemd's own directory for unit files.
	UnitDir = "/etc/systemd/system"
	// ServiceFile runs one update.
	ServiceFile = "box-update.service"
	// TimerFile wakes it.
	TimerFile = "box-update.timer"
)

// WakeInterval is how often the timer wakes box, not the check interval: box
// decides from root's config whether a wake has anything to do, so the interval
// is an edit in box.yaml and never in systemd.
const WakeInterval = 15

// Units is where the timer's two files go and how they are turned on. It is an
// interface so bootstrap's tests assert on what it wrote without touching systemd.
type Units interface {
	// Install writes both unit files and reports whether either changed.
	Install(service, timer []byte) (bool, error)
	// Enable makes systemd read the files and start the timer. It is not called
	// when nothing changed: reloading untouched units only looks like work.
	Enable(ctx context.Context) error
}

// UnitFiles renders the two units for one box: the binary, and the three paths
// this bootstrap or init run used, so the timer updates the seedbox just
// installed. The service is a oneshot that runs as root (systemd's default) with
// no timeout: an update can wait out an app's thirty-minute startup budget, and
// systemd's ninety-second default would kill it in the middle of a migration.
func UnitFiles(boxPath, record, grant, values string) (service, timer []byte) {
	var s strings.Builder
	s.WriteString("[Unit]\n")
	s.WriteString("Description=s5n: check for a newer release\n")
	s.WriteString("Wants=network-online.target\n")
	s.WriteString("After=network-online.target\n\n")
	s.WriteString("[Service]\n")
	s.WriteString("Type=oneshot\n")
	s.WriteString("# An update can wait out an app's thirty-minute startup budget.\n")
	s.WriteString("TimeoutStartSec=0\n")
	s.WriteString("ExecStart=")
	for _, arg := range []string{
		boxPath, "update", "--timer",
		"--record", record, "--grant", grant, "--values", values,
	} {
		s.WriteString(execArg(arg))
		s.WriteString(" ")
	}
	s.WriteString("\n")

	var t strings.Builder
	t.WriteString("[Unit]\n")
	fmt.Fprintf(&t, "Description=s5n: wake box every %d minutes to check for a newer release\n", WakeInterval)
	t.WriteString("\n[Timer]\n")
	t.WriteString("OnBootSec=10min\n")
	fmt.Fprintf(&t, "OnUnitActiveSec=%dmin\n", WakeInterval)
	t.WriteString("RandomizedDelaySec=10min\n")
	t.WriteString("\n[Install]\n")
	t.WriteString("WantedBy=timers.target\n")

	return []byte(s.String()), []byte(t.String())
}

// execArg is one ExecStart argument: systemd splits the line on whitespace and
// understands double quotes, so a path with a space has to be quoted or box would
// start as two commands.
func execArg(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\"'\\") {
		return arg
	}
	return strconv.Quote(arg)
}

// DirUnits writes the units into a directory. EnableWith is the difference
// between the two kinds: systemctl for real systemd, nil for --unit-dir, whose
// files are for another host.
type DirUnits struct {
	Dir string
	// EnableWith is what turns the timer on. Nil writes the files and stops
	// there.
	EnableWith func(ctx context.Context) error
}

// Install writes each file whose bytes are not already there, mode 0644: systemd
// reads a unit file, not root alone. A file that is the same keeps its mtime.
func (d DirUnits) Install(service, timer []byte) (bool, error) {
	changed := false
	for _, f := range []struct {
		name string
		data []byte
	}{
		{ServiceFile, service},
		{TimerFile, timer},
	} {
		wrote, err := writeIfChanged(filepath.Join(d.Dir, f.name), f.data)
		if err != nil {
			return changed, err
		}
		changed = changed || wrote
	}
	return changed, nil
}

// Enable asks systemd to read the files and to start the timer now.
func (d DirUnits) Enable(ctx context.Context) error {
	if d.EnableWith == nil {
		return nil
	}
	return d.EnableWith(ctx)
}

// Systemd is the units where systemd reads them, with the two commands that turn
// them on.
func Systemd() DirUnits {
	return DirUnits{Dir: UnitDir, EnableWith: systemctlEnable}
}

// systemctlEnable is `systemctl daemon-reload` then `enable --now
// box-update.timer`: the first makes systemd read the files, the second starts the
// timer now and at every boot.
func systemctlEnable(ctx context.Context) error {
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return systemctl(ctx, "enable", "--now", TimerFile)
}

// systemctl runs one systemctl command and quotes what it said when it fails.
func systemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, msg)
	}
	return nil
}

// Install renders the timer's units and puts them where units says; changed
// decides whether systemd is asked to reload.
func Install(ctx context.Context, units Units, boxPath, record, grant, values string) (changed bool, err error) {
	service, timer := UnitFiles(boxPath, record, grant, values)
	changed, err = units.Install(service, timer)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	return true, units.Enable(ctx)
}

// writeIfChanged writes data unless the file already holds exactly those bytes,
// reporting whether it wrote. A file that cannot be read is rewritten rather than
// refused: the unit is box's, and its content is the answer.
func writeIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return false, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := atomicfile.Write(path, data, 0o644); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}
