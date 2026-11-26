// Package bundle is box's migrate snapshot on disk: discovery's plan, report and
// manifests plus the config trees apply rewrites, in the archive internal/archive
// defines (docs/box.md, "migrate"). This package is only the shape inside it:
// which files a snapshot has, what its name says, which one a run picks. It holds
// secrets and is what carries them off a host, so its directory is 0700 and every
// file 0600.
package bundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/space"
)

// Dir is where box keeps snapshots: what discover writes and apply with no file
// reads. A k0s reset does not touch it, so a cutover's snapshot survives.
const Dir = "/var/lib/s5n/migrate"

// prefix is what makes a listing read as a history and what Newest looks for.
const prefix = "migrate-"

// stamp is the time in a snapshot's name: UTC, to the second, shell-safe.
const stamp = "20060102T150405Z"

// FileName is migrate-<UTC yyyymmddThhmmssZ>-<mode>.tar.gz. The time is when the
// source was read, so a copied snapshot keeps it and names sort by that time.
func FileName(mode archive.Mode, at time.Time) string {
	return prefix + at.UTC().Format(stamp) + "-" + string(mode) + ".tar.gz"
}

// Config is one planned app's config directory, as it sits on the source host.
type Config struct {
	// Instance is box's name for the app and its directory in the snapshot.
	Instance string
	// Dir is the source's directory, read and never written.
	Dir string
	// Sub is the directory inside Dir holding the app's own data, in slash form,
	// when the whole of Dir is not the app's (Plex's home). Empty means Dir.
	Sub string
	// Files are the files carried out of Sub with their directories; empty means
	// all of Sub. An app that keeps a library beside its state (Plex) names the
	// state instead.
	Files []File
	// Skip are directories left out of Sub's whole tree, relative to Sub; only an
	// app whose config is partly a library has them. Never set with Files.
	Skip []string
}

// File is one file a Config carries instead of its whole tree.
type File struct {
	// Path is the file, relative to Config.Dir, in slash form.
	Path string
	// Required refuses a snapshot without this file: the app cannot start without
	// it. A database's -wal and -shm are not required, since they exist only while
	// it is open.
	Required bool
}

// Snapshot is what one run writes: plan, report, manifests and config trees.
type Snapshot struct {
	// Mode says what the source was: a survey holds no configs, a rehearsal's came
	// from a running source, a final one's from a stopped source.
	Mode archive.Mode
	// Plan is plan.yaml as discovery wrote it; apply's plan id is its digest.
	Plan []byte
	// Report is report.txt, what discovery would not migrate.
	Report []byte
	// Source is the manifests discovery read, keyed as archive.Source names them.
	Source map[string][]byte
	// Configs are the apps whose config trees travel in the snapshot, in
	// config/<instance>/.
	Configs []Config
	// Box is the version that wrote it, recognisable before applying.
	Box string
	// Now is the clock; empty means time.Now.
	Now func() time.Time
}

// Written is what one write did.
type Written struct {
	// Path is the snapshot's file.
	Path string
	// Mode is the mode its manifest carries.
	Mode archive.Mode
	// Skipped are links the archive left out, with the reason (internal/archive's
	// rule).
	Skipped []archive.Skipped
}

// Write writes one snapshot at path, making its directory 0700. The file appears
// whole or not at all (archive commits on Close), so a half run leaves nothing.
func Write(path string, s Snapshot) (Written, error) {
	var res Written
	if len(s.Plan) == 0 {
		return res, errors.New("no plan: a snapshot carries the plan `box migrate apply` runs, and this one has none")
	}
	if err := writableMode(s.Mode); err != nil {
		return res, err
	}
	if s.Mode == archive.ModeSurvey && len(s.Configs) > 0 {
		return res, fmt.Errorf("a survey holds no configs, and this one was handed %d: a survey is the read `box migrate discover` writes, and `--rehearsal` is what copies the configs", len(s.Configs))
	}
	if err := checkConfigs(s.Configs); err != nil {
		return res, err
	}
	if _, err := os.Lstat(path); err == nil {
		return res, fmt.Errorf("%s is there already: a snapshot is never written over, and its name carries the time it was taken", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("write %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	// Box makes and modes its own directory; one named with --out is the
	// operator's.
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return res, fmt.Errorf("create %s: %w", dir, err)
		}
	} else if err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	w, err := archive.Create(path, archive.Manifest{
		Kind:    archive.KindMigrate,
		Mode:    s.Mode,
		Created: now().UTC().Format(time.RFC3339),
		Box:     s.Box,
	})
	if err != nil {
		return res, err
	}
	if err := w.AddFile(archive.PlanName, s.Plan, 0o600); err != nil {
		w.Discard()
		return res, err
	}
	if len(s.Report) > 0 {
		if err := w.AddFile(archive.ReportName, s.Report, 0o600); err != nil {
			w.Discard()
			return res, err
		}
	}
	for _, name := range sortedKeys(s.Source) {
		entry := archive.SourcePrefix + "/" + name + ".json"
		if err := w.AddFile(entry, s.Source[name], 0o600); err != nil {
			w.Discard()
			return res, err
		}
	}
	for _, c := range s.Configs {
		if len(c.Files) == 0 {
			skipped, err := w.AddTreeExcept(archive.ConfigPrefix+"/"+c.Instance, c.Dir, c.Sub, c.Skip)
			if err != nil {
				w.Discard()
				return res, fmt.Errorf("add %s's config from %s: %w", c.Instance, c.Dir, err)
			}
			res.Skipped = append(res.Skipped, skipped...)
			continue
		}
		// Only the state the app cannot rebuild, with the directories above it.
		names := make([]string, len(c.Files))
		for i, f := range c.Files {
			names[i] = f.Path
		}
		carried, err := w.AddFiles(archive.ConfigPrefix+"/"+c.Instance, c.Dir, c.Sub, names)
		if err != nil {
			w.Discard()
			return res, fmt.Errorf("add %s's config from %s: %w", c.Instance, c.Dir, err)
		}
		res.Skipped = append(res.Skipped, carried.Skipped...)
		for _, f := range c.Files {
			if !f.Required || !has(carried.Missing, f.Path) {
				continue
			}
			w.Discard()
			return res, fmt.Errorf("%s: %s has no %s, and a snapshot of this app carries only the files it names: state the app cannot start without is a refusal, not a snapshot missing it",
				c.Instance, c.Dir, f.Path)
		}
	}
	if err := w.Close(); err != nil {
		return res, err
	}
	res.Path, res.Mode = path, s.Mode
	return res, nil
}

// writableMode refuses a mode this box does not write a migrate snapshot in.
func writableMode(mode archive.Mode) error {
	switch mode {
	case archive.ModeSurvey, archive.ModeRehearsal, archive.ModeFinal:
		return nil
	}
	return fmt.Errorf("mode %q is not one a migrate snapshot is written in (%q, %q or %q)",
		mode, archive.ModeSurvey, archive.ModeRehearsal, archive.ModeFinal)
}

// checkConfigs refuses what a snapshot cannot be built from, before the archive is
// created, so a refusal leaves no file.
func checkConfigs(configs []Config) error {
	seen := make(map[string]bool, len(configs))
	for _, c := range configs {
		if !instanceName(c.Instance) {
			return fmt.Errorf("instance %q is not a name a config directory can be filed under: it is one path element, with no slash and no . or .. in it", c.Instance)
		}
		if seen[c.Instance] {
			return fmt.Errorf("instance %q is in the snapshot twice", c.Instance)
		}
		seen[c.Instance] = true
		if c.Sub != "" && !filePath(c.Sub) {
			return fmt.Errorf("%s carries %q as the directory its data is in, which is not one inside its config directory: a relative path, with no . or .. in it", c.Instance, c.Sub)
		}
		files := make(map[string]bool, len(c.Files))
		for _, f := range c.Files {
			if !filePath(f.Path) {
				return fmt.Errorf("%s carries %q, which is not a file inside its config directory: a carried file is a relative path, with no . or .. in it", c.Instance, f.Path)
			}
			if files[f.Path] {
				return fmt.Errorf("%s carries %s twice", c.Instance, f.Path)
			}
			files[f.Path] = true
		}
		if len(c.Files) > 0 && len(c.Skip) > 0 {
			return fmt.Errorf("%s carries %d files and leaves %d directories out: a snapshot carries a tree with things left out of it, or a list of files, never both", c.Instance, len(c.Files), len(c.Skip))
		}
		skips := make(map[string]bool, len(c.Skip))
		for _, dir := range c.Skip {
			if !filePath(dir) {
				return fmt.Errorf("%s leaves out %q, which is not a directory inside its config directory: a left-out directory is a relative path, with no . or .. in it", c.Instance, dir)
			}
			if skips[dir] {
				return fmt.Errorf("%s leaves %s out twice", c.Instance, dir)
			}
			skips[dir] = true
		}
		info, err := os.Lstat(c.Dir)
		if err != nil {
			return fmt.Errorf("%s's config directory %s: %w", c.Instance, c.Dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s's config %s is not a directory", c.Instance, c.Dir)
		}
	}
	return nil
}

// filePath reports whether name may be carried under config/<instance>/.
func filePath(name string) bool {
	if name == "" || name == "." || path.IsAbs(name) || strings.Contains(name, "\\") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == ".." {
			return false
		}
	}
	return true
}

// has reports whether a list holds a value.
func has(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// instanceName reports whether name may be carried as config/<name>/.
func instanceName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return false
	}
	return path.Clean(name) == name
}

// Opened is a snapshot extracted into a private directory, with each part's path.
type Opened struct {
	// Path is the file it was opened from.
	Path string
	// Dir is the private directory it was extracted into.
	Dir string
	// Mode is what the source was when the snapshot was taken.
	Mode archive.Mode
	// Taken is the manifest's time: when the source was read.
	Taken time.Time
	// Box is the box that wrote it.
	Box string
	// Plan is plan.yaml's bytes, which apply's plan id is computed over.
	Plan []byte
	// Report is report.txt, empty when the snapshot carries none.
	Report []byte
	// Source is the manifests, keyed as the snapshot names them.
	Source map[string][]byte
	// Configs is each app's config directory inside Dir, by instance name; empty
	// in a survey.
	Configs map[string]string
	// Contents is what the snapshot holds of each app's config, counted while
	// verifying.
	Contents map[string]Contents
	// manifest is the verified entry list, which Stage writes from.
	manifest *archive.Manifest
}

// Contents is what a snapshot holds of one app's config.
type Contents struct {
	// Files and Bytes are what a copy writes.
	Files, Bytes int64
	// Tree says the snapshot holds more than the extraction kept -- a config that
	// is a library as well as state -- which Stage writes once.
	Tree bool
}

// Open verifies a snapshot and extracts it into dir, which must not exist yet and
// belongs to the caller afterwards. A snapshot with no plan is refused. keep
// decides what is extracted now, nil extracting everything: a run leaves the bulk
// of a config that is also a library (Plex's artwork) for Stage, extracting the
// state alone so the checks read it. disk is where room is asked about, nil
// meaning the host's own, and the extraction is refused before its first write
// when the manifest's bytes do not fit.
func Open(file, dir string, keep archive.Filter, disk space.Disk) (*Opened, error) {
	m, held, err := verify(file, keep)
	if err != nil {
		return nil, err
	}
	if err := roomFor(dir, m, keep, disk); err != nil {
		return nil, err
	}
	if err := archive.Extract(file, dir, m, keep, nil); err != nil {
		return nil, err
	}
	o := &Opened{Path: file, Dir: dir, Mode: m.Mode, Box: m.Box, Source: map[string][]byte{}, Configs: map[string]string{}, Contents: held, manifest: m}
	if at, err := time.Parse(time.RFC3339, m.Created); err == nil {
		o.Taken = at.UTC()
	}
	if o.Plan, err = readPart(dir, archive.PlanName); err != nil {
		return nil, err
	}
	if len(o.Plan) == 0 {
		return nil, fmt.Errorf("%s holds no %s: a snapshot is the plan apply runs, and this one has none", file, archive.PlanName)
	}
	if o.Report, err = readPart(dir, archive.ReportName); err != nil {
		return nil, err
	}
	if err := o.parts(); err != nil {
		return nil, err
	}
	return o, nil
}

// verify checks the whole archive before any extraction -- part is written after
// the box is installed, so damage must stop the run now -- and counts each app's
// config in the same pass, the only one that knows a directory from a file without
// a tree on the host.
func verify(file string, keep archive.Filter) (*archive.Manifest, map[string]Contents, error) {
	held := map[string]Contents{}
	m, err := archive.Verify(file, func(s archive.Seen) {
		instance, ok := configInstance(s.Name)
		if !ok {
			return
		}
		c := held[instance]
		if s.Kind == archive.EntryFile {
			c.Files++
			c.Bytes += s.Size
		}
		if keep != nil && !keep(s.Name) {
			c.Tree = true
		}
		held[instance] = c
	})
	if err != nil {
		return nil, nil, err
	}
	return m, held, nil
}

// parts finds the manifests and each app's config directory; an entry that is not
// the directory it must be is a refusal naming it.
func (o *Opened) parts() error {
	for _, name := range sortedKeys(o.manifest.Entries) {
		switch {
		case strings.HasPrefix(name, archive.SourcePrefix+"/"):
			rel := strings.TrimPrefix(name, archive.SourcePrefix+"/")
			info, err := os.Lstat(filepath.Join(o.Dir, filepath.FromSlash(name)))
			if err != nil {
				return err
			}
			if info.IsDir() {
				continue
			}
			raw, err := readPart(o.Dir, name)
			if err != nil {
				return err
			}
			o.Source[strings.TrimSuffix(rel, ".json")] = raw
		case strings.HasPrefix(name, archive.ConfigPrefix+"/"):
			rel := strings.TrimPrefix(name, archive.ConfigPrefix+"/")
			if strings.Contains(rel, "/") {
				continue // a file inside a tree, whose directory is the entry
			}
			at := filepath.Join(o.Dir, filepath.FromSlash(name))
			info, err := os.Lstat(at)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("%s holds %s, and every entry directly under %s/ is one app's config directory", o.Path, name, archive.ConfigPrefix)
			}
			o.Configs[rel] = at
		}
	}
	return nil
}

// Stage writes one instance's whole tree out of the snapshot into dst, owned by
// uid:gid: the other half of Open's filter, straight from the archive, so the bulk
// is never a second copy on the host. Bytes are checked against the manifest
// again.
func (o *Opened) Stage(instance, dst string, uid, gid int) (Contents, error) {
	prefix := archive.ConfigPrefix + "/" + instance
	if err := archive.ExtractTree(o.Path, dst, prefix, o.manifest, &archive.Owner{UID: uid, GID: gid}); err != nil {
		return Contents{}, err
	}
	return o.Contents[instance], nil
}

// configInstance is the app a path belongs to: config/<instance>/..., the only
// place the name comes from.
func configInstance(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, archive.ConfigPrefix+"/")
	if !ok {
		return "", false
	}
	instance, _, _ := strings.Cut(rest, "/")
	if instance == "" {
		return "", false
	}
	return instance, true
}

// roomFor refuses a snapshot whose extraction will not fit beside it, summed from
// the manifest: only the bytes it writes, with no margin, because a margin here
// would refuse a disk that has room for everything. The margin belongs to the
// carry (carry.go).
func roomFor(dir string, m *archive.Manifest, keep archive.Filter, disk space.Disk) error {
	var need int64
	for name, e := range m.Entries {
		if keep != nil && !keep(name) {
			continue
		}
		need += e.Size
	}
	if need == 0 {
		return nil
	}
	if disk == nil {
		disk = space.Real{}
	}
	free, err := disk.Free(dir)
	if err != nil {
		return fmt.Errorf("the room %s needs: %w", dir, err)
	}
	if free < need {
		return fmt.Errorf("extracting the snapshot's plan, report and state needs %s and %s is free at %s: box extracts it beside the snapshot, so the room has to be there",
			space.Human(need), space.Human(free), dir)
	}
	return nil
}

// readPart reads one file of an extracted snapshot.
func readPart(dir, name string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return raw, nil
}

// Newest is what apply runs with no file named: the newest non-survey snapshot. A
// survey holds no configs, so it is never the answer; a rehearsal is, and apply
// refuses it by name. Newest is by name, the time the source was read.
func Newest(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", noSnapshot(dir, err)
		}
		return "", fmt.Errorf("read %s: %w", dir, err)
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !isSnapshotName(e.Name()) {
			continue
		}
		candidates = append(candidates, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	for _, name := range candidates {
		path := filepath.Join(dir, name)
		m, err := archive.Read(path)
		if err != nil {
			// An unreadable file is skipped, not the answer: a broken copy stays
			// for the operator.
			continue
		}
		if m.Mode == archive.ModeSurvey {
			continue
		}
		return path, nil
	}
	return "", noSnapshot(dir, nil)
}

// noSnapshot names the two commands that write one, since apply with no file is
// how a cutover is finished.
func noSnapshot(dir string, cause error) error {
	msg := fmt.Sprintf("no snapshot in %s to apply: `box migrate cutover` writes the one a real run applies, and `box migrate apply <file>` takes one from another host", dir)
	if cause != nil {
		return fmt.Errorf("%s (%v)", msg, cause)
	}
	return errors.New(msg)
}

// isSnapshotName reports whether a file name is a snapshot's, so a stray file in
// the directory is left alone rather than parsed.
func isSnapshotName(name string) bool {
	_, ok := nameStamp(name)
	return ok
}

// nameStamp is the time a snapshot's name carries.
func nameStamp(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tar.gz") {
		return time.Time{}, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tar.gz")
	i := strings.LastIndex(rest, "-")
	if i < 0 {
		return time.Time{}, false
	}
	at, err := time.Parse(stamp, rest[:i])
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// sortedKeys is a map's keys, so entries are added the same way twice.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
