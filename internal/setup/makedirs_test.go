package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingHost is the real host with every Lchown written down, so a test on a
// temporary tree can say what root would have re-owned.
type recordingHost struct {
	LinuxPathHost
	chowned []string
}

func (h *recordingHost) Lchown(path string, uid, gid int) error {
	h.chowned = append(h.chowned, path)
	return h.LinuxPathHost.Lchown(path, uid, gid)
}

// The box's owner can write the config path, and init re-owns every settings
// directory in it as root on every run. A settings directory swapped for a link
// must stop init before anything is chowned, or the link's target -- /etc, in the
// case that matters -- becomes the owner's. The real MkdirAll accepts a link to a
// directory, which is why this runs on a real tree and not the fixture.
func TestMakeDirsRefusesALinkedSettingsDirectory(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	target := filepath.Join(root, "etc")
	for _, dir := range []string{config, target} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	linked := filepath.Join(config, "alpha")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}

	host := &recordingHost{}
	d := &Decision{Host: host}
	created, err := d.makeDirs([]string{filepath.Join(config, "beta"), linked}, os.Getuid(), os.Getgid())
	if err == nil {
		t.Fatalf("a settings directory that is a link to %s was accepted", target)
	}
	if !strings.Contains(err.Error(), linked) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the refusal does not name the link: %v", err)
	}
	for _, p := range host.chowned {
		if p == linked || p == target {
			t.Fatalf("%s was chowned before the refusal: %v", p, host.chowned)
		}
	}
	// The plain directory before it was made and owned as usual.
	if len(created) != 1 || created[0] != filepath.Join(config, "beta") {
		t.Fatalf("created = %v, want only the plain directory", created)
	}
}

// Something that is not a directory at a settings path is refused by name too:
// init cannot keep an app's settings in a file.
func TestMakeDirsRefusesAFileInPlaceOfADirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "alpha")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	host := &recordingHost{}
	d := &Decision{Host: host}
	if _, err := d.makeDirs([]string{file}, os.Getuid(), os.Getgid()); err == nil || !strings.Contains(err.Error(), file) {
		t.Fatalf("a file at a settings path was not refused by name: %v", err)
	}
	if len(host.chowned) != 0 {
		t.Fatalf("chowned %v", host.chowned)
	}
}

// A plain directory, new or already there, is made and owned: the refusal is for
// links, not for re-runs.
func TestMakeDirsOwnsPlainDirectories(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "alpha")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(root, "beta")
	host := &recordingHost{}
	d := &Decision{Host: host}
	created, err := d.makeDirs([]string{existing, fresh}, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || len(host.chowned) != 2 {
		t.Fatalf("created %v, chowned %v: want both", created, host.chowned)
	}
	if info, err := os.Lstat(fresh); err != nil || !info.IsDir() {
		t.Fatalf("%s was not created as a directory: %v", fresh, err)
	}
}
