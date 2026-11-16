package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stepTimeLayout is the shape of the timestamp in a step directory's name: UTC,
// fixed width, so the directories sort by name in the order they were taken.
// Prune reads it back to decide which snapshot is the oldest.
const stepTimeLayout = "20060102T150405Z"

// boxDir is box's own directory under a config root: the one place an app's
// config directories are not, so what box writes there is never rendered into a
// pod and never mounted.
const boxDir = ".box"

// Root is where a seedbox's snapshots live: <configRoot>/.box/snapshots, beside
// the app config directories rather than inside one, so no app can see another
// app's state and an app pointed at its own directory cannot copy the
// snapshots into itself.
func Root(configRoot string) string {
	return filepath.Join(configRoot, boxDir, "snapshots")
}

// Dir names one app's snapshot for one update step:
// <configRoot>/.box/snapshots/<from>-<to>-<time>/<app>.
//
// The versions come from chart metadata and the app name from the catalog, and
// a name that is not a single path element would move the snapshot out of the
// tree: a `..` version would write somewhere else entirely, and a restore would
// then read from there. So each name is checked, not cleaned.
func Dir(configRoot, from, to string, at time.Time, app string) (string, error) {
	named := []struct{ what, name string }{
		{"the version it moves from", from},
		{"the version it moves to", to},
		{"the app", app},
	}
	for _, n := range named {
		if err := checkName(n.name); err != nil {
			return "", fmt.Errorf("snapshot: %s %q: %w", n.what, n.name, err)
		}
	}
	step := from + "-" + to + "-" + at.UTC().Format(stepTimeLayout)
	return filepath.Join(Root(configRoot), step, app), nil
}

// checkName refuses a single path element that is not one.
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New("is empty")
	case name == "." || name == "..":
		return errors.New("is not a name")
	case strings.ContainsAny(name, `/\`):
		return errors.New("has a path separator")
	}
	return nil
}

// snapshotsTree opens the config root that holds one of box's snapshots roots,
// and names that root inside it.
//
// The tree is opened on the config root rather than on the snapshots root itself,
// because os.OpenRoot follows the path it is handed: a link planted at
// .box/snapshots would become the tree's own root, and everything below it would
// be read and written wherever the link points. Spelled as a name inside the
// config root, that link is a link out of the tree, which the root refuses.
func snapshotsTree(snapshots string) (*tree, string, error) {
	t, err := openTree(filepath.Dir(filepath.Dir(snapshots)))
	if err != nil {
		return nil, "", err
	}
	name, err := t.rel(snapshots)
	if err != nil {
		t.Close()
		return nil, "", err
	}
	return t, name, nil
}

// Prune keeps the newest keep snapshots of one app and deletes the rest, by the
// time in the step directory's name (not the manifest's: the directory name is
// what box chose, and a manifest can be missing while its directory is real).
//
// A directory that is not a step of ours is left alone, because the snapshots
// root sits inside a config root the operator owns: anything else there is
// theirs. A step left with no apps after the prune goes too, so an emptied
// directory does not accumulate.
func Prune(root, app string, keep int) error {
	if keep < 1 {
		return fmt.Errorf("prune %s: keep %d would delete every snapshot; box keeps at least one", app, keep)
	}
	if err := checkName(app); err != nil {
		return fmt.Errorf("prune: app %q: %w", app, err)
	}
	t, snapshots, err := snapshotsTree(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("prune %s: %w", root, err)
	}
	defer t.Close()
	steps, err := t.readDir(snapshots)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("prune %s: %w", root, err)
	}
	type step struct {
		dir string
		at  time.Time
	}
	var have []step
	for _, e := range steps {
		if !e.IsDir() {
			continue
		}
		at, ok := stepTime(e.Name())
		if !ok {
			continue
		}
		// A step the app has no copy in is not this app's to prune. A step box
		// could not read at all is an error rather than a skip: "the disk did
		// not answer" and "there is no copy here" lead to opposite decisions
		// about what may be deleted.
		if _, err := t.lstat(path.Join(snapshots, e.Name(), app)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("prune %s: %w", t.path(path.Join(snapshots, e.Name())), err)
		}
		have = append(have, step{dir: e.Name(), at: at})
	}
	sort.Slice(have, func(i, j int) bool {
		if !have[i].at.Equal(have[j].at) {
			return have[i].at.After(have[j].at)
		}
		return have[i].dir > have[j].dir
	})
	for _, s := range have[min(keep, len(have)):] {
		stepDir := path.Join(snapshots, s.dir)
		if err := t.removeAll(path.Join(stepDir, app)); err != nil {
			return fmt.Errorf("prune %s: %w", t.path(stepDir), err)
		}
		rest, err := t.readDir(stepDir)
		if err != nil {
			return fmt.Errorf("prune %s: %w", t.path(stepDir), err)
		}
		if len(rest) == 0 {
			if err := t.remove(stepDir); err != nil {
				return fmt.Errorf("prune %s: %w", t.path(stepDir), err)
			}
		}
	}
	return nil
}

// stepTime reads the timestamp out of a step directory's name. A name that does
// not end in one is not a step box made.
func stepTime(name string) (time.Time, bool) {
	if len(name) <= len(stepTimeLayout) || name[len(name)-len(stepTimeLayout)-1] != '-' {
		return time.Time{}, false
	}
	at, err := time.Parse(stepTimeLayout, name[len(name)-len(stepTimeLayout):])
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// ErrNoSnapshot says a box holds no complete snapshot of an app. It is a
// sentinel because "there is nothing to roll back to" is a state a caller
// answers differently from a disk it could not read.
var ErrNoSnapshot = errors.New("snapshot: there is no snapshot of this app")

// Newest finds the newest complete snapshot of one instance under root and
// reads its manifest, which is what a rollback needs: the files to put back and
// the image the app ran when they were copied.
//
// The order is the time in the step directory's name, not the manifest's: the
// directory name is what box chose and it is there even for a tree whose
// manifest is unreadable. A tree still being built (`<app>.partial`, which Take
// renames only when it is complete) is not a snapshot of the app, so it is
// never picked; neither is another instance's copy in the same step.
//
// A step directory is left alone unless it is one of box's own names, because
// the snapshots root sits inside a config root the operator owns.
func Newest(root, instance string) (string, Manifest, error) {
	if err := checkName(instance); err != nil {
		return "", Manifest{}, fmt.Errorf("snapshot: instance %q: %w", instance, err)
	}
	t, snapshots, err := snapshotsTree(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", Manifest{}, ErrNoSnapshot
		}
		return "", Manifest{}, fmt.Errorf("snapshot %s: %w", root, err)
	}
	defer t.Close()
	steps, err := t.readDir(snapshots)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", Manifest{}, ErrNoSnapshot
		}
		return "", Manifest{}, fmt.Errorf("snapshot %s: %w", root, err)
	}
	type found struct {
		// dir names the copy inside the tree; path is where that is on the
		// host, which is what the caller and the messages use.
		dir  string
		path string
		at   time.Time
	}
	var have []found
	for _, e := range steps {
		if !e.IsDir() {
			continue
		}
		at, ok := stepTime(e.Name())
		if !ok {
			continue
		}
		dir := path.Join(snapshots, e.Name(), instance)
		info, err := t.lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", Manifest{}, fmt.Errorf("snapshot %s: %w", t.path(dir), err)
		}
		if !info.IsDir() {
			continue
		}
		have = append(have, found{dir: dir, path: t.path(dir), at: at})
	}
	if len(have) == 0 {
		return "", Manifest{}, ErrNoSnapshot
	}
	sort.Slice(have, func(i, j int) bool {
		if !have[i].at.Equal(have[j].at) {
			return have[i].at.After(have[j].at)
		}
		return have[i].path > have[j].path
	})
	// The newest tree that exists is the answer, and a manifest it cannot read
	// is an error rather than a reason to fall back to an older one: the older
	// copy holds state the app has already written past.
	man, err := loadManifest(t, have[0].dir)
	if err != nil {
		return "", Manifest{}, fmt.Errorf("snapshot %s: %w", have[0].path, err)
	}
	return have[0].path, man, nil
}

// StepName is the step a snapshot directory belongs to, as the directory name
// that holds it. It is what a hold records, so a caller can find the other
// app's snapshots taken by the same step.
func StepName(dir string) string {
	return filepath.Base(filepath.Dir(dir))
}

// CheckGlob checks one catalog glob: a relative path, in path.Match syntax,
// matched against a whole path relative to an app's config directory.
//
// The rule lives here, beside the code that copies and deletes by it, and
// internal/catalog calls it: the globs decide which files a restore deletes, so
// there is one definition and a manifest that satisfies the loader is one the
// copier can run.
func CheckGlob(glob string) error {
	switch {
	case glob == "":
		return errors.New("is empty")
	case path.IsAbs(glob):
		return errors.New("is absolute; an app's state is somewhere under its config directory")
	}
	for _, seg := range strings.Split(glob, "/") {
		if seg == ".." {
			return errors.New(`has a ".." segment; a snapshot never leaves the app's config directory`)
		}
	}
	// A pattern that is not clean would match nothing the way its author read
	// it (`./db.sqlite3` matches no relative path as this package walks them),
	// so it is refused at load rather than discovered as an empty snapshot at
	// the first update.
	if clean := path.Clean(glob); clean != glob {
		return fmt.Errorf("is not a clean relative path (write %q)", clean)
	}
	// path.Match validates the syntax against an empty name: a malformed
	// pattern reports ErrBadPattern whatever the name.
	if _, err := path.Match(glob, ""); err != nil {
		return fmt.Errorf("is not a path.Match pattern: %w", err)
	}
	return nil
}

// CheckGlobs checks a list, naming the glob that failed.
func CheckGlobs(globs []string) error {
	for _, glob := range globs {
		if err := CheckGlob(glob); err != nil {
			return fmt.Errorf("snapshot: glob %q: %w", glob, err)
		}
	}
	return nil
}

// checkRel refuses a manifest path that would leave the config directory it is
// written into. The paths come from a file box wrote, but it is a file on disk
// an app's user could have reached, and Restore writes where it points.
func checkRel(rel string) error {
	switch {
	case rel == "":
		return errors.New("a file's path is empty")
	case path.IsAbs(rel):
		return fmt.Errorf("%s is absolute", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("%s has a \"..\" segment", rel)
		}
	}
	if clean := path.Clean(rel); clean != rel || clean == "." {
		return fmt.Errorf("%s is not a clean relative path", rel)
	}
	return nil
}
