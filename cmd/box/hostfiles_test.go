package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/hostroot"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// hostDisk is a host with nothing on it but what a case puts there.
type hostDisk map[string]bool

func (d hostDisk) stat(path string) (bool, error) { return d[path], nil }

// oldBox is the host a 0.4 box left behind: its files at the root box used
// before 0.5, and nothing at the new one.
func oldBox(names ...string) hostDisk {
	d := hostDisk{}
	for _, name := range names {
		d[filepath.Join(hostroot.LegacyDir, name)] = true
	}
	return d
}

func checkFiles(t *testing.T, d hostDisk, record, grant string) (error, string) {
	t.Helper()
	var buf bytes.Buffer
	out := &say.Plain{W: &buf, Err: &buf}
	err := checkHostFiles(d.stat, record, grant, out)
	return err, buf.String()
}

// TestHostFilesRefusesAOldBoxOnANewBox: this is the upgrade 0.5 does not
// support. A record at the old root and none at the new one must stop the
// command before it reads "no record" and decides to install over a box that
// is already there, and it says the move as a command to check first.
func TestHostFilesRefusesAOldBoxOnANewBox(t *testing.T) {
	err, said := checkFiles(t, oldBox("cluster.yaml"), hostroot.RecordPath, seedbox.GrantPath("root"))
	if err == nil {
		t.Fatalf("a box with its record at %s was not refused; it said:\n%s", hostroot.LegacyDir, said)
	}
	for _, want := range []string{"box keeps its files in /etc/s5n now", "mkdir -p /etc/s5n", "mv /etc/swizzin/cluster.yaml /etc/s5n"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not say %q:\n%s", want, err)
		}
	}
	if said != "" {
		t.Fatalf("the refusal printed something of its own as well:\n%s", said)
	}
}

// TestHostFilesWarnsWhenBothRootsHaveIt: a box that did the move reads the new
// files and runs. The old copy is named once, because it is the file somebody
// would go on editing.
func TestHostFilesWarnsWhenBothRootsHaveIt(t *testing.T) {
	d := oldBox("cluster.yaml")
	d[hostroot.RecordPath] = true
	err, said := checkFiles(t, d, hostroot.RecordPath, seedbox.GrantPath("root"))
	if err != nil {
		t.Fatalf("a moved record was refused: %v", err)
	}
	if want := "/etc/swizzin/cluster.yaml is stale"; !strings.Contains(said, want) {
		t.Fatalf("no stale-file warning (%q); it said:\n%s", want, said)
	}
	if strings.Count(said, "\n") != 1 {
		t.Fatalf("the warning is not one line:\n%s", said)
	}
}

// TestHostFilesIsSilentOnAFreshHost: a host box has never run on says nothing
// about the old root.
func TestHostFilesIsSilentOnAFreshHost(t *testing.T) {
	err, said := checkFiles(t, hostDisk{}, hostroot.RecordPath, seedbox.GrantPath("root"))
	if err != nil || said != "" {
		t.Fatalf("a fresh host said %q (%v)", said, err)
	}
}

// TestHostFilesIgnoresTheRunsOwnPaths: an e2e (or an operator) that passes
// --record and --grant somewhere else is not reading the default root, so an
// old box on the machine is none of its business.
func TestHostFilesIgnoresTheRunsOwnPaths(t *testing.T) {
	err, said := checkFiles(t, oldBox("cluster.yaml", "box.yaml", "seedboxes"), "/srv/e2e/cluster.yaml", "/srv/e2e/root.yaml")
	if err != nil || said != "" {
		t.Fatalf("a run with its own paths looked at the default root: %q (%v)", said, err)
	}
}
