package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// summaryEntries bounds the "what is already there" summary: one bounded directory
// read is enough to tell "empty" from "an existing library", and init never walks the
// tree, since a multi-TB walk makes an installer look hung.
const summaryEntries = 200

// summaryNames is how many top-level names the report prints before "and N
// more".
const summaryNames = 5

// PathReport is what init found at one host path, shown before anything is written:
// the device, mount, filesystem, free space and owner. The failure it exists to
// catch is a disk that did not mount and left an empty directory on /.
type PathReport struct {
	Kind    string // "data" or "config"
	Path    string
	Exists  bool
	Owner   string // "1000:1000", as stat reports it
	Mount   Mount
	Free    int64
	Summary string
	// OnRoot says the path (or the ancestor a missing one lands under) is on the root
	// filesystem: the unmounted-disk case.
	OnRoot bool
	// SameDevice says the path is on the root filesystem's device without being on
	// its mount point: a separate partition of the system disk, whose filling takes
	// the host down just the same. Only the live renderer warns about it; the line
	// prompt's report stays byte for byte what it was.
	SameDevice bool
}

// PathCheck is the whole report: one entry per host path and the line about the
// directories the apps create themselves. Structure rather than text, so a prompter
// can draw a table and a coloured warning; Text is the plain rendering.
type PathCheck struct {
	Reports []PathReport
	// Footer is the line about the directories beneath the paths, which init creates
	// without asking.
	Footer string
}

// PathTitle is the report's heading: a lowercase noun, no colon (rule 9).
const PathTitle = "where things live"

// PathRow is one path as the table shows it: the columns a person compares down,
// and notes too long for one. Both renderers print these (rule 13).
type PathRow struct {
	Kind   string
	Path   string
	State  string // "new", or "exists"
	Device string
	FSType string
	// Free is how much room is left, as a person reads it: "1.8 TiB free".
	Free string
	// Notes are what a column cannot hold: what is already at the path, and who owns
	// it when that is not this box's owner.
	Notes []string
}

// Rows is the report as rows: the same columns Text prints, so a prompter
// draws the terminal's version of the transcript's table.
func (c PathCheck) Rows() []PathRow {
	rows := make([]PathRow, 0, len(c.Reports))
	for _, r := range c.Reports {
		rows = append(rows, PathRow{
			Kind:   r.Kind,
			Path:   r.Path,
			State:  r.state(),
			Device: r.Mount.MajorMinor(),
			FSType: r.Mount.FSType,
			Free:   humanBytes(r.Free) + " free",
			Notes:  r.notes(),
		})
	}
	return rows
}

// Text renders the check as the line prompt prints it: one row per path with the
// device, filesystem and free space in columns, then what is already there. Values
// are columns, not sentences (rule 13).
func (c PathCheck) Text() string {
	var b strings.Builder
	b.WriteString(PathTitle + "\n")
	rows := c.Rows()
	kindW, pathW := 0, 0
	for _, r := range rows {
		kindW = max(kindW, len(r.Kind))
		pathW = max(pathW, len(r.Path))
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s  %-*s  %-6s  %-6s  %-4s  %s\n",
			kindW, r.Kind, pathW, r.Path, r.State, r.Device, r.FSType, r.Free)
		for _, line := range r.Notes {
			fmt.Fprintf(&b, "  %-*s  %s\n", kindW, "", line)
		}
	}
	if c.Footer != "" {
		b.WriteString("  " + strings.TrimSpace(c.Footer) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// state is the state column's word: `new` or `exists`.
func (r PathReport) state() string {
	if r.Exists {
		return "exists"
	}
	return "new"
}

// notes are what the row cannot say in a column, and the unmounted-disk warning.
func (r PathReport) notes() []string {
	var lines []string
	if r.Exists {
		for _, line := range strings.Split(r.Summary, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				lines = append(lines, s)
			}
		}
		return lines
	}
	if r.OnRoot {
		lines = append(lines, "! that is the root filesystem, not a separate disk: if one was meant to be mounted here, it is not there")
	}
	return lines
}

// inspect builds one path's report. A missing path is reported against its nearest
// existing ancestor, which decides the filesystem a new directory lands on.
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
			rep.Summary += fmt.Sprintf("\n          note: owned %d:%d, not this box's %d:%d; the apps cannot write there as it is", info.UID, info.GID, uid, gid)
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

// summarise describes what is already at a path: one bounded read, no recursion. A
// top-level file's size counts, a directory's does not, since that means walking it.
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

// nearestExisting walks up to the first ancestor that is there: that one decides
// which filesystem a new directory lands on.
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

// mountFor picks the longest mount point that is a prefix of the path, by element,
// so /srv/s5n is not taken to be under /srv/s5.
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

// makeDirs creates the directories init owns, each with the box's owner: the path
// roots it may create and one directory per app instance under the config path,
// which the chart mounts as `subPath: <instance>`. A kubelet creating a missing one
// makes it root-owned: an app that starts and cannot save its settings.
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

// SettingsDirs is the per-instance list under the config path, named as the chart
// names the workload (sonarr, sonarr-4k), which is the subPath each pod mounts. The
// instances come from the grant's values, so this agrees with the chart without box
// knowing which apps are Arrs. init creates these, update copies them, and both name
// them here rather than each deriving a subPath.
func SettingsDirs(configPath string, apps []string, values map[string]any) []string {
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
