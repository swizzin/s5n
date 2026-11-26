package apply

// own_test.go is the pass that hands a staged config tree to the seedbox's
// user, after the rewriters have written into it and before any of it is moved
// into place.
//
// The bug it pins was found on a real host, where box runs as root: every
// atomicfile write a rewriter makes is a new file renamed over the app's, so
// the copy's owner (copyTree's) is replaced by root's, often 0600. A migrated
// Arr then cannot read its config.xml, PeerBanHelper's and qui's init
// containers fail on theirs, Seerr starts anyway and writes its defaults over
// the migrated settings, and every qBittorrent resume a rewriter touched is
// unreadable. The recorder here stands in for the kernel's chown, which a test
// cannot do to a uid it is not.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

func TestRunHandsEveryPathOfAStagedConfigToTheSeedboxUser(t *testing.T) {
	f := newFixture(t)
	p, raw := f.plan()
	// A mode the source's app had. config.xml is the file the Arr's rewriter
	// replaces through atomicfile, so it is the one a rename would leave with
	// the temporary file's mode rather than the app's.
	configXML := filepath.Join(f.configs, "sonarr", "config.xml")
	if err := os.Chmod(configXML, 0o640); err != nil {
		t.Fatal(err)
	}

	// The run's own user is the only one a test can hand a file to, so the
	// recorder is what proves the calls happen; a distinct pair is proven on
	// the pass itself, below.
	uid, gid := os.Geteuid(), os.Getegid()
	var mu sync.Mutex
	handed := map[string]string{} // staged path -> "uid:gid" it was handed to
	env := f.env(&fakeValues{}, &fakeOwned{}, yes(t, &Summary{}))
	env.Chown = func(path string, u, g int) error {
		mu.Lock()
		defer mu.Unlock()
		handed[path] = fmt.Sprintf("%d:%d", u, g)
		return nil
	}

	res, err := Run(context.Background(), p, raw, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Moved) == 0 {
		t.Fatal("the run moved nothing into the config root")
	}

	// Every path the run left under the config root is a path of a staged
	// instance, and the walk of the final tree is the list the pass is held
	// to: a path missing from it is one the app would find owned by root.
	staging := stateDir(f.configRoot, res.PlanID)
	seen := 0
	for _, instance := range res.Moved {
		root := filepath.Join(f.configRoot, instance)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			staged := filepath.Join(staging, instance, rel)
			got, ok := handed[staged]
			if !ok {
				t.Errorf("%s was never handed to the seedbox's user", staged)
				return nil
			}
			if want := fmt.Sprintf("%d:%d", uid, gid); got != want {
				t.Errorf("%s was handed to %s, want %s", staged, got, want)
			}
			seen++
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if seen == 0 {
		t.Fatal("the walk found nothing to hold the pass to")
	}

	// A rewritten file keeps the mode the app's original had: every rewriter
	// reads it before the write and atomicfile takes it.
	info, err := os.Stat(filepath.Join(f.configRoot, "sonarr", "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Errorf("the rewritten config.xml is %o, want the source's %o", got, want)
	}
}

func TestOwnHandsEveryEntryOverAndNeverFollowsALink(t *testing.T) {
	root := t.TempDir()
	p, err := plan.Parse([]byte(`version: 2
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
`))
	if err != nil {
		t.Fatalf("parse a one-app plan: %v", err)
	}
	m := &migration{
		p:   p,
		dir: filepath.Join(root, ".box"),
		env: Env{PUID: 4310, PGID: 4311},
	}
	staged := m.staging("sonarr")
	writeFile(t, filepath.Join(staged, "config.xml"), "<Config/>", 0o640)
	writeFile(t, filepath.Join(staged, "logs", "sonarr.txt"), "log", 0o644)
	if err := os.Symlink("config.xml", filepath.Join(staged, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(staged, "outside")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var handed []string
	m.env.Chown = func(path string, uid, gid int) error {
		mu.Lock()
		defer mu.Unlock()
		if uid != 4310 || gid != 4311 {
			t.Errorf("%s handed to %d:%d, want 4310:4311", path, uid, gid)
		}
		handed = append(handed, path)
		return nil
	}
	if err := m.own(); err != nil {
		t.Fatalf("own: %v", err)
	}
	sort.Strings(handed)
	want := []string{
		staged,
		filepath.Join(staged, "config.xml"),
		filepath.Join(staged, "inside"),
		filepath.Join(staged, "logs"),
		filepath.Join(staged, "logs", "sonarr.txt"),
		filepath.Join(staged, "outside"),
	}
	sort.Strings(want)
	if fmt.Sprint(handed) != fmt.Sprint(want) {
		t.Errorf("handed over\n  %v\nwant\n  %v", handed, want)
	}
}

func TestOwnReportsWhatItCouldNotHandOver(t *testing.T) {
	root := t.TempDir()
	p, err := plan.Parse([]byte(`version: 2
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
`))
	if err != nil {
		t.Fatalf("parse a one-app plan: %v", err)
	}
	m := &migration{p: p, dir: filepath.Join(root, ".box"), env: Env{PUID: 4310, PGID: 4311}}
	staged := m.staging("sonarr")
	writeFile(t, filepath.Join(staged, "config.xml"), "<Config/>", 0o640)
	m.env.Chown = func(path string, uid, gid int) error {
		if filepath.Base(path) == "config.xml" {
			return os.ErrPermission
		}
		return nil
	}
	err = m.own()
	if err == nil {
		t.Fatal("the pass reported success after a refused chown")
	}
	if !contains(err.Error(), "config.xml") || !contains(err.Error(), "4310:4311") {
		t.Errorf("the refusal does not name the path and the user:\n%s", err)
	}
}
