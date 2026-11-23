// Package bundle is box's migrate snapshot on disk: one archive holding what
// discovery read (plan.yaml, report.txt, source/) and the config trees apply
// rewrites (config/<instance>/), written into one directory and opened into a
// private one (docs/box.md, "migrate").
//
// internal/archive is the file format -- the tar, the gzip, the manifest with a
// sha256 per entry, the 0600 -- and this package is only the shape inside it:
// which files a snapshot has, what its name says, and which one a run with no
// file named should pick up. Nothing here duplicates the format, and nothing
// here reads a config: a snapshot is bytes in, bytes out.
//
// It holds secrets, and it is the only thing that carries them off a host, so
// the directory it lives in is 0700 and every file it writes is 0600.
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

// Dir is where box keeps a host's snapshots: the default location
// `box migrate discover` writes to and the one `box migrate apply` with no file
// reads. It is not on a k0s reset's path, so a cutover's snapshot survives the
// reset that follows it.
const Dir = "/var/lib/s5n/migrate"

// prefix is the name every snapshot starts with, which is what makes a
// directory listing read as a history and what Newest looks for.
const prefix = "migrate-"

// stamp is the time in a snapshot's name: UTC, to the second, no punctuation a
// shell would have to quote.
const stamp = "20060102T150405Z"

// FileName is what a snapshot taken at one time in one mode is called:
// migrate-<UTC yyyymmddThhmmssZ>-<mode>.tar.gz.
//
// The time is the snapshot's, not the file's: two hosts that copy a snapshot
// around keep the name that says when the source was read, and a name sorts the
// way the snapshots were taken.
func FileName(mode archive.Mode, at time.Time) string {
	return prefix + at.UTC().Format(stamp) + "-" + string(mode) + ".tar.gz"
}

// Config is one planned app's config directory, as it sits on the source host.
type Config struct {
	// Instance is box's name for the app, which is the directory name it gets
	// inside the snapshot (config/<instance>/).
	Instance string
	// Dir is the source's own directory, read and never written.
	Dir string
	// Sub is the directory inside Dir that the app's own data is in, in slash
	// form, when a snapshot carries that and not the whole of Dir: Plex's home
	// is Library/Application Support/Plex Media Server, and whatever else the
	// source's config mount holds is not Plex's. Empty means Dir itself.
	Sub string
	// Files are the files a snapshot carries out of Sub, with the directories
	// above them. Empty means all of Sub: an app whose config is its state as a
	// whole takes its whole tree, and one that keeps a library's worth of
	// covers beside the state it needs (Plex) names the state.
	Files []File
	// Skip are the directories a snapshot leaves out of Sub's whole tree, with
	// everything under them, in slash form relative to Sub. Only an app whose
	// config is as much a library as a state has them (Plex's caches), and the
	// whole tree travels otherwise. Files and Skip are never both set.
	Skip []string
}

// File is one file a Config carries instead of its whole tree.
type File struct {
	// Path is the file, relative to Config.Dir, in slash form.
	Path string
	// Required says a snapshot without this file is refused: the app cannot
	// start on the state it was given without it. The companion files a
	// database writes beside itself (-wal, -shm) are not required, because
	// they are there only while something has the database open.
	Required bool
}

// Snapshot is what one run writes: the plan, the report, the manifests
// discovery read, and the config trees a rehearsal or a final snapshot carries.
type Snapshot struct {
	// Mode is what the source was when the snapshot was taken. A survey holds
	// no configs, a rehearsal's came from a running source, and a final
	// snapshot's from a stopped one.
	Mode archive.Mode
	// Plan is plan.yaml, byte for byte as discovery wrote it: apply's plan id
	// is a digest of these bytes.
	Plan []byte
	// Report is report.txt, what discovery would not migrate.
	Report []byte
	// Source is the manifests discovery read, keyed by the name archive.Source
	// gives them. They are the record of what the plan was built from.
	Source map[string][]byte
	// Configs are the apps whose config trees travel in the snapshot, in
	// config/<instance>/.
	Configs []Config
	// Box is the version that wrote the snapshot, so an archive from another
	// box is recognisable before it is applied.
	Box string
	// Now is the clock the manifest's created and the caller's name come from.
	// Empty means time.Now.
	Now func() time.Time
}

// Written is what one write did.
type Written struct {
	// Path is the snapshot's file.
	Path string
	// Mode is the mode its manifest carries.
	Mode archive.Mode
	// Skipped are the links the config trees carried that the archive left
	// out, with the reason (internal/archive's rule: a link at the pod's
	// stdout is not part of a config tree).
	Skipped []archive.Skipped
}

// Write writes one snapshot at path, creating its directory 0700 if it is not
// there. The file appears whole or not at all (internal/archive commits on
// Close), so a run that stops halfway leaves nothing a later run could mistake
// for a snapshot.
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
	// The directory is box's to make and to set the mode of; one the operator
	// named with --out is theirs, and its mode is not box's business.
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
		// A config the snapshot carries a part of: the state the app cannot
		// rebuild, and the directories above it, and nothing else.
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

// writableMode refuses a mode this box does not write a migration snapshot in.
// A backup's mode is its own kind's (internal/archive refuses the pair), and a
// snapshot of nothing is not a mode at all.
func writableMode(mode archive.Mode) error {
	switch mode {
	case archive.ModeSurvey, archive.ModeRehearsal, archive.ModeFinal:
		return nil
	}
	return fmt.Errorf("mode %q is not one a migrate snapshot is written in (%q, %q or %q)",
		mode, archive.ModeSurvey, archive.ModeRehearsal, archive.ModeFinal)
}

// checkConfigs refuses a config list a snapshot cannot be built from: a name
// that is not one directory under config/, the same instance twice, a directory
// that is not there, a file name that is not a path inside it, or the same file
// twice. All of them are named before the archive is created, so a refusal
// leaves no file.
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

// filePath reports whether name is one file inside a config directory, in slash
// form: the thing an archive path may carry under config/<instance>/.
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

// has reports whether a list holds a value, in one place so a refusal reads the
// same as the miss it is about.
func has(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// instanceName reports whether name is one directory name: the thing an archive
// path may carry as config/<name>/.
func instanceName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return false
	}
	return path.Clean(name) == name
}

// Opened is a snapshot extracted into a private directory: what apply reads,
// with the paths it finds each part at.
type Opened struct {
	// Path is the file it was opened from.
	Path string
	// Dir is the private directory the whole snapshot was extracted into.
	Dir string
	// Mode is what the source was when the snapshot was taken.
	Mode archive.Mode
	// Plan is plan.yaml's bytes, which apply's plan id is computed over.
	Plan []byte
	// Report is report.txt, empty when the snapshot carries none.
	Report []byte
	// Source is the manifests discovery read, keyed as the snapshot names
	// them.
	Source map[string][]byte
	// Configs is each planned app's config directory inside Dir, keyed by
	// box's instance name. Empty in a survey.
	Configs map[string]string
	// Contents is what the snapshot holds of each planned app's config, counted
	// by the pass that verified it: the numbers a copy of it writes, whether that
	// copy is the extraction under Configs or the tree Stage writes.
	Contents map[string]Contents
	// manifest is the verified list of everything the archive holds, which Stage
	// writes a part of from.
	manifest *archive.Manifest
}

// Contents is what a snapshot holds of one planned app's config: the copy a run
// writes into the config root.
type Contents struct {
	// Files and Bytes are what a copy of it writes: the regular files and their
	// bytes, which is what a tree's own copy reports.
	Files, Bytes int64
	// Tree says the snapshot holds more of it than the extraction kept: the app's
	// whole data, for the one app whose config is a library as well as its state.
	// A run stages it once, straight into the config root (Stage).
	Tree bool
}

// Open verifies a snapshot and extracts it into dir, which must not exist yet and
// belongs to the caller afterwards (apply removes it when it is done).
//
// A snapshot with no plan is refused: apply runs one, and a survey from a box
// that could not read the source is not something to hand it.
//
// keep decides what is extracted now, and nil extracts everything. The one thing
// a run leaves for later is the bulk of an app whose config is a library as well
// as its state -- Plex's artwork and thumbnails, tens of gigabytes that no check
// reads -- and the state alone is extracted in its place, so the checks, the
// pre-install pass and the staging that follows them read it. Stage writes the
// rest, once, at the config root.
//
// disk is where the room that extraction needs is asked about; nil asks the
// host's own filesystems. The extraction is refused before its first write when
// there is not room for it beside the snapshot, and the numbers it needs are the
// manifest's, not a walk of a tree that is not on the host.
func Open(file, dir string, keep archive.Filter, disk space.Disk) (*Opened, error) {
	// The archive is verified whole before any of it is extracted: a run writes
	// part of it after the box is installed, and a damaged copy has to stop the
	// run now rather than halfway through the last thing it does.
	//
	// The same pass is what counts the snapshot: it is the only one that knows a
	// directory from a file without walking a tree, and a tree is not on the host
	// until Stage writes it.
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
		return nil, err
	}
	if err := roomFor(dir, m, keep, disk); err != nil {
		return nil, err
	}
	if err := archive.Extract(file, dir, m, keep, nil); err != nil {
		return nil, err
	}
	o := &Opened{Path: file, Dir: dir, Mode: m.Mode, Source: map[string][]byte{}, Configs: map[string]string{}, Contents: held, manifest: m}
	o.Plan, err = readPart(dir, archive.PlanName)
	if err != nil {
		return nil, err
	}
	if len(o.Plan) == 0 {
		return nil, fmt.Errorf("%s holds no %s: a snapshot is the plan apply runs, and this one has none", file, archive.PlanName)
	}
	if o.Report, err = readPart(dir, archive.ReportName); err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(m.Entries) {
		switch {
		case strings.HasPrefix(name, archive.SourcePrefix+"/"):
			rel := strings.TrimPrefix(name, archive.SourcePrefix+"/")
			info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				continue
			}
			key := strings.TrimSuffix(rel, ".json")
			raw, err := readPart(dir, name)
			if err != nil {
				return nil, err
			}
			o.Source[key] = raw
		case strings.HasPrefix(name, archive.ConfigPrefix+"/"):
			rel := strings.TrimPrefix(name, archive.ConfigPrefix+"/")
			if strings.Contains(rel, "/") {
				continue // a file inside a tree, whose directory is the entry
			}
			at := filepath.Join(dir, filepath.FromSlash(name))
			info, err := os.Lstat(at)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("%s holds %s, and every entry directly under %s/ is one app's config directory", file, name, archive.ConfigPrefix)
			}
			o.Configs[rel] = at
		}
	}
	return o, nil
}

// Stage writes one instance's config out of the snapshot into dst, which must not
// exist, owned by uid:gid, and reports what it wrote.
//
// It is the other half of Open's filter: the tree a full carry holds, written
// once -- straight from the archive into the staging tree that becomes the
// instance's config directory -- so the bulk is never a second copy on the host,
// and never beside the checks. The bytes are checked against the manifest again
// as they are written.
func (o *Opened) Stage(instance, dst string, uid, gid int) (Contents, error) {
	prefix := archive.ConfigPrefix + "/" + instance
	if err := archive.ExtractTree(o.Path, dst, prefix, o.manifest, &archive.Owner{UID: uid, GID: gid}); err != nil {
		return Contents{}, err
	}
	return o.Contents[instance], nil
}

// configInstance is the app one path inside an archive belongs to, when it is one:
// config/<instance>/... is that instance's config, and the manifest's paths are
// the only place the name comes from.
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

// roomFor refuses a snapshot whose extraction will not fit beside it, before
// anything is written: the plan, the report, the sources and the state of every
// app, summed from the manifest.
//
// It is the extraction's own room and nothing more -- the bytes it writes, with
// no margin -- because that is all it needs: a margin here would refuse a
// migration for a disk that has room for everything it must write. The margin is
// the carry's (carry.go), where a tree is written and space is left beside it.
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

// readPart reads one file of an extracted snapshot. A file the manifest named
// but the extraction did not leave is a refusal: archive.Open promises both
// halves are there.
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

// Newest is the snapshot `box migrate apply` runs when no file was named: the
// newest migration snapshot in dir that is not a survey. A survey is a read of
// a source with no configs in it, so it is never the answer; a rehearsal is,
// and apply refuses it by name because the lock it applies under lands next.
//
// The newest is by name, which is the time the source was read, so a copy that
// arrives with an older name does not become "the newest" by being copied
// second.
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
			// One unreadable file is not the directory's answer: the operator
			// gets the newest snapshot that reads, and the broken one stays
			// where it is for them to look at.
			continue
		}
		if m.Mode == archive.ModeSurvey {
			continue
		}
		return path, nil
	}
	return "", noSnapshot(dir, nil)
}

// noSnapshot is the refusal when a directory holds nothing apply can run. It
// names the two commands that write one, because "apply" with no file is the
// documented way to finish a cutover.
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

// nameStamp is the time a snapshot's name carries: when the source was read.
// isSnapshotName is this, so a name that parses here is a snapshot's and one that
// does not is left alone.
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

// sortedKeys is a map's keys, so a snapshot's entries are added the same way
// twice and two snapshots of one source compare by their bytes' names.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
