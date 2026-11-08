package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// summaryEntries bounds the "what is already there" summary. A seedbox data
// path can hold a million names; one directory read of 200 of them is cheap and
// enough to tell "empty" from "an existing library", and it is why init never
// walks the tree (a multi-TB walk is the thing that makes an installer look
// hung).
const summaryEntries = 200

// summaryNames is how many top-level names the report prints before "and N
// more".
const summaryNames = 5

// PathReport is what init found at one of the box's two host paths. It is what
// the operator is shown before anything is written: the device, the mount, the
// filesystem, the free space and the owner, because the failure this exists to
// catch is a disk that did not mount and left an empty directory on /.
type PathReport struct {
	Kind    string // "data" or "config"
	Path    string
	Exists  bool
	Owner   string // "1000:1000", as stat reports it
	Mount   Mount
	Free    int64
	Summary string
	// OnRoot says the path (or the ancestor a missing path would land under)
	// is on the root filesystem, which is the unmounted-disk case.
	OnRoot bool
	// SameDevice says the path sits on the same device as the root filesystem
	// without being on its mount point: a separate partition of the system
	// disk. Filling it takes the host down just the same, and the plain text
	// does not say so (it is a warning a terminal UI draws; the line prompt's
	// report is older than this field and stays byte for byte what it was).
	SameDevice bool
}

// PathCheck is the whole report: one entry per host path, and the line about
// the directories the apps create themselves on first start. It is structure
// rather than text so a prompter can draw it as a table with glyphs and a
// coloured warning; Text is the plain rendering init has always shown.
type PathCheck struct {
	Reports []PathReport
	// Footer is the line about the directories beneath the paths, which init
	// does create without asking.
	Footer string
}

// Text renders the check the way `box init`'s line prompt has always printed
// it: a heading, one block per path, and the footer. It is the contract for
// prompters that cannot style anything.
func (c PathCheck) Text() string {
	var b strings.Builder
	b.WriteString("paths\n")
	for _, r := range c.Reports {
		b.WriteString(r.String())
	}
	if c.Footer != "" {
		b.WriteString(c.Footer)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// String renders one path for the report block. Continuation lines are indented
// under the path so the block reads as one table.
func (r PathReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %-6s %s\n", r.Kind, r.Path)
	where := r.location()
	if r.Exists {
		fmt.Fprintf(&b, "          %s, %s free\n", where, humanBytes(r.Free))
		fmt.Fprintf(&b, "          owner %s\n", r.Owner)
		fmt.Fprintf(&b, "          %s\n", r.Summary)
		return b.String()
	}
	fmt.Fprintf(&b, "          does not exist yet: it would be a new directory on %s, %s free\n", where, humanBytes(r.Free))
	if r.OnRoot {
		fmt.Fprintf(&b, "          note: that is the root filesystem, not a separate disk. If a disk\n")
		fmt.Fprintf(&b, "          was meant to be mounted here, it is not there.\n")
	}
	return b.String()
}

func (r PathReport) location() string {
	if r.Mount.Point == "" {
		return "an unknown filesystem"
	}
	return fmt.Sprintf("device %d:%d, mounted at %s (%s)", r.Mount.Major, r.Mount.Minor, r.Mount.Point, r.Mount.FSType)
}

// inspect builds one path's report. A missing path is reported against the
// nearest ancestor that does exist, because that is where the filesystem the
// new directory would land on is decided.
func (d *Decision) inspect(kind, path string, uid, gid int) (PathReport, error) {
	rep := PathReport{Kind: kind, Path: path}
	anchor := path

	info, err := d.Host.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir {
			return rep, fmt.Errorf("%s path %s is a file, not a directory", kind, path)
		}
		rep.Exists = true
		rep.Owner = fmt.Sprintf("%d:%d", info.UID, info.GID)
		summary, err := d.summarise(path)
		if err != nil {
			return rep, err
		}
		rep.Summary = summary
		if info.UID != uid || info.GID != gid {
			rep.Summary += fmt.Sprintf("\n          note: owned %d:%d, not the box's %d:%d; the apps cannot write there as it is", info.UID, info.GID, uid, gid)
		}
	case errors.Is(err, fs.ErrNotExist):
		anchor = nearestExisting(d.Host, path)
		if anchor == "" {
			anchor = string(filepath.Separator)
		}
	default:
		return rep, fmt.Errorf("read %s: %w", path, err)
	}

	mounts, err := d.Host.MountInfo()
	if err != nil {
		return rep, fmt.Errorf("read the mount table: %w", err)
	}
	if m, ok := mountFor(mounts, anchor); ok {
		rep.Mount = m
		rep.OnRoot = m.Point == string(filepath.Separator)
		if root, ok := mountFor(mounts, string(filepath.Separator)); ok {
			rep.SameDevice = root.Major == m.Major && root.Minor == m.Minor
		}
	}
	fsinfo, err := d.Host.StatFS(anchor)
	if err != nil {
		return rep, fmt.Errorf("read free space of %s: %w", anchor, err)
	}
	rep.Free = fsinfo.Free
	return rep, nil
}

// summarise describes what is already at a path, cheaply: one bounded directory
// read, no recursion. A top-level file's size counts; a directory's does not,
// because knowing it means walking the tree.
func (d *Decision) summarise(path string) (string, error) {
	entries, more, err := d.Host.ReadDir(path, summaryEntries)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return summariseEntries(entries, more), nil
}

// summariseEntries is the summary itself, over whatever ReadDir returned.
func summariseEntries(entries []DirEntry, more bool) string {
	if len(entries) == 0 {
		return "empty"
	}
	dirs, files := 0, 0
	var bytes int64
	for _, e := range entries {
		if e.IsDir {
			dirs++
			continue
		}
		files++
		bytes += e.Size
	}
	count := fmt.Sprintf("%d %s", len(entries), plural(len(entries), "entry", "entries"))
	if more {
		count = fmt.Sprintf("%d+ entries", len(entries))
	}
	names := entries
	rest := 0
	if len(names) > summaryNames {
		rest = len(names) - summaryNames
		names = names[:summaryNames]
	}
	parts := make([]string, 0, len(names))
	for _, e := range names {
		parts = append(parts, e.Name)
	}
	line := fmt.Sprintf("holds %s at the top level: %s", count, strings.Join(parts, ", "))
	if rest > 0 {
		line += fmt.Sprintf(" and %d more", rest)
	} else if more {
		line += " and more"
	}
	if files > 0 {
		line += fmt.Sprintf(" (%d %s, %s)", files, plural(files, "file", "files"), humanBytes(bytes))
	} else if dirs > 0 {
		line += fmt.Sprintf(" (%d %s)", dirs, plural(dirs, "directory", "directories"))
	}
	return line
}

// nearestExisting walks up from a missing path to the first ancestor that is
// there. That ancestor, not the missing path, is what decides which filesystem
// a new directory would land on.
func nearestExisting(h PathHost, path string) string {
	for p := filepath.Clean(path); ; {
		if _, err := h.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}

// mountFor picks the mount a path lives on: the longest mount point that is a
// prefix of it. The comparison is by path element, so /srv/s5n is not taken to
// be under /srv/s5.
func mountFor(mounts []Mount, path string) (Mount, bool) {
	best := Mount{}
	found := false
	for _, m := range mounts {
		if m.Point == "" {
			continue
		}
		if !under(path, m.Point) {
			continue
		}
		if !found || len(m.Point) > len(best.Point) {
			best, found = m, true
		}
	}
	return best, found
}

// under reports whether path is the mount point itself or below it.
func under(path, point string) bool {
	if point == string(filepath.Separator) {
		return strings.HasPrefix(path, string(filepath.Separator))
	}
	if path == point {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(point, "/")+"/")
}

// makeDirs creates the directories init owns and gives each the box's owner:
// the two path roots it was allowed to create, and one directory per app
// instance beneath the config path. Those per-instance directories are what the
// chart mounts as `subPath: <instance>`; the images expect them to exist and be
// writable by puid, and a kubelet creating a missing one makes it root-owned,
// which is a container that starts and cannot save its settings.
func (d *Decision) makeDirs(paths []string, uid, gid int) ([]string, error) {
	seen := map[string]bool{}
	var created []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if err := d.Host.MkdirAll(p, 0o775); err != nil {
			return created, fmt.Errorf("create %s: %w", p, err)
		}
		if err := d.Host.Chown(p, uid, gid); err != nil {
			return created, fmt.Errorf("give %s to %d:%d: %w", p, uid, gid, err)
		}
		created = append(created, p)
	}
	return created, nil
}

// settingsDirs is the per-instance list under the config path: one directory
// per enabled instance, named as the chart names the workload (sonarr,
// sonarr-4k), because that is the subPath each pod mounts. It reads the
// instances from the values, which are the grant's, so it agrees with what the
// chart renders without box knowing which apps are Arrs.
func settingsDirs(configPath string, apps []string, values map[string]any) []string {
	var out []string
	for _, name := range apps {
		entry, _ := lookupPath(values, "apps."+name)
		cfg, _ := entry.(map[string]any)
		instances, hasInstances := cfg["instances"].([]any)
		if !hasInstances {
			out = append(out, filepath.Join(configPath, name))
			continue
		}
		for _, raw := range instances {
			inst, _ := raw.(map[string]any)
			enabled, _ := inst["enabled"].(bool)
			if !enabled {
				continue
			}
			suffix, _ := inst["suffix"].(string)
			dir := name
			if suffix != "" {
				dir = name + "-" + suffix
			}
			out = append(out, filepath.Join(configPath, dir))
		}
	}
	return out
}

// humanBytes renders a size the way the report says it: 24.0 TiB, 512 GiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
