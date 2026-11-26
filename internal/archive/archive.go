// Package archive is box's one archive format: a tar + gzip file holding a plan
// and the config trees box carries across a migration, with a manifest that
// makes a truncated copy a refusal rather than a restore that stops halfway
// (docs/box.md, "migrate").
//
// It is also `box backup`'s format, and `restore` is `migrate apply` of a box's
// own snapshot: one format, one apply path. What differs is in the manifest --
// the kind (a migration or a backup), and the mode (a rehearsal's configs came
// from a running source and may only be applied locked, a final snapshot's from a
// stopped one, a backup's from a box).
//
// The archive holds secrets: API keys, the Plex token, qBittorrent's password
// hash. It is written 0600 and nothing here redacts anything.
//
// It is not internal/snapshot: that package keeps the per-app state an image
// upgrade can rewrite, as a directory on the config claim. This is the one file
// that travels between hosts.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Format is the only manifest version this box writes and reads. A copy of an
// archive from a newer box is refused rather than half-understood.
const Format = 1

// The files inside an archive that are not an app's config.
const (
	// ManifestName is the archive's last entry. The whole archive streams out
	// before its sums can be known, so a writer puts the list at the end; a
	// reader that has it has the list of everything that must be there, and a
	// copy that stopped early has none.
	ManifestName = "manifest.json"
	// PlanName is the migration plan, what `box migrate apply` runs.
	PlanName = "plan.yaml"
	// ReportName is what discovery wrote about the source and did not migrate.
	ReportName = "report.txt"
	// ConfigPrefix is where each planned app's config tree sits inside an
	// archive: config/<instance>/. A survey holds none of them.
	ConfigPrefix = "config"
	// SourcePrefix is where the manifests discovery read sit: source/.
	SourcePrefix = "source"
)

// Kind is what an archive holds.
type Kind string

const (
	// KindMigrate is a source install's plan and configs, from `box migrate`.
	KindMigrate Kind = "migrate"
	// KindBackup is a box's own configs and databases, from `box backup`.
	KindBackup Kind = "backup"
)

// Mode is what state the source was in when the archive was taken, which is what
// apply is allowed to do with it.
type Mode string

const (
	// ModeRehearsal copied configs from a running source, so the archive may
	// only be applied locked (no internet egress, torrents stopped), and never
	// on the host that holds the source's data.
	ModeRehearsal Mode = "rehearsal"
	// ModeFinal copied configs with every planned app stopped. It is the only
	// kind of migration archive a real run applies.
	ModeFinal Mode = "final"
	// ModeSurvey is a read of a source with no configs in it: what
	// `box migrate discover` writes when it is not rehearsing. It holds the
	// plan, the report and the manifests discovery read, and nothing apply
	// could rewrite, so apply refuses it by name rather than half-running.
	ModeSurvey Mode = "survey"
	// ModeBackup is a box's own state, taken from a box that is running.
	ModeBackup Mode = "backup"
)

// Manifest is manifest.json: what the archive is, and one entry per path inside
// it. It is written last, so a reader that reaches it has the whole archive.
type Manifest struct {
	// Format is Format for a manifest this box wrote.
	Format int `json:"format"`
	// Kind is what the archive holds.
	Kind Kind `json:"kind"`
	// Mode is the state of the source the archive was taken from.
	Mode Mode `json:"mode"`
	// Created is when the archive was written, RFC3339 in UTC.
	Created string `json:"created"`
	// Box is the box version that wrote it, so an archive from another box is
	// recognisable before it is applied.
	Box string `json:"box"`
	// Entries is one entry per path in the archive, keyed by the path inside
	// it: a config tree's files under config/<instance>/, the source's
	// manifests under source/. Directories are entries too, so an archive can
	// carry an empty one.
	Entries map[string]Entry `json:"entries"`
}

// Entry is what one path inside the archive is, and what its bytes must be.
type Entry struct {
	// Size is the entry's length in bytes: a file's content, a link's target.
	Size int64 `json:"size"`
	// SHA256 is the hex digest of those bytes, so a copy that stopped halfway
	// or a byte changed in transit is refused.
	SHA256 string `json:"sha256"`
	// Mode is the source's permission bits, as octal text ("0600"), and the
	// mode the entry is extracted with: a config tree comes back as the app
	// left it, an executable script included. What keeps an extraction private
	// is the directory it is written into, which is box's and 0700.
	Mode string `json:"mode"`
}

// problems enforces what a manifest has to be for the archive behind it to be
// trustworthy.
func (m *Manifest) problems() []string {
	var problems []string
	if m.Format != Format {
		problems = append(problems, fmt.Sprintf("format %d: this box reads %d", m.Format, Format))
	}
	switch m.Kind {
	case KindMigrate, KindBackup:
	default:
		problems = append(problems, fmt.Sprintf("kind %q is not %q or %q", m.Kind, KindMigrate, KindBackup))
	}
	switch m.Mode {
	case ModeRehearsal, ModeFinal, ModeBackup, ModeSurvey:
	default:
		problems = append(problems, fmt.Sprintf("mode %q is not %q, %q, %q or %q", m.Mode, ModeRehearsal, ModeFinal, ModeBackup, ModeSurvey))
	}
	if m.Kind == KindBackup && m.Mode != ModeBackup {
		problems = append(problems, fmt.Sprintf("a %s archive's mode is %q, not %q", KindBackup, ModeBackup, m.Mode))
	}
	if m.Kind == KindMigrate && m.Mode == ModeBackup {
		problems = append(problems, fmt.Sprintf("a %s archive's mode is %q, %q or %q, not %q", KindMigrate, ModeRehearsal, ModeFinal, ModeSurvey, ModeBackup))
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		problems = append(problems, fmt.Sprintf("created %q is not a time (RFC3339)", m.Created))
	}
	if strings.ContainsAny(m.Box, "\r\n") {
		problems = append(problems, fmt.Sprintf("box %q must be one line", m.Box))
	}
	if m.lookup(ManifestName) != nil {
		problems = append(problems, ManifestName+" is the manifest; it does not list itself")
	}
	for _, name := range m.names() {
		e := m.Entries[name]
		if !safeName(name) {
			problems = append(problems, fmt.Sprintf("entry %q is not a path inside the archive", name))
		}
		if m.Mode == ModeSurvey && (name == ConfigPrefix || strings.HasPrefix(name, ConfigPrefix+"/")) {
			problems = append(problems, fmt.Sprintf("%s: a survey holds no configs, and %s is one", name, name))
		}
		if e.Size < 0 {
			problems = append(problems, fmt.Sprintf("%s: size %d is negative", name, e.Size))
		}
		if !isDigest(e.SHA256) {
			problems = append(problems, fmt.Sprintf("%s: sha256 %q is not a hex digest", name, e.SHA256))
		}
		if _, err := strconv.ParseUint(e.Mode, 8, 32); err != nil {
			problems = append(problems, fmt.Sprintf("%s: mode %q is not octal", name, e.Mode))
		}
	}
	return problems
}

// lookup is the manifest's entry for name, or nil when it names nothing.
func (m *Manifest) lookup(name string) *Entry {
	if m.Entries == nil {
		return nil
	}
	if e, ok := m.Entries[name]; ok {
		return &e
	}
	return nil
}

// names is every path the manifest lists, sorted, so a refusal reads the same
// way twice.
func (m *Manifest) names() []string {
	names := make([]string, 0, len(m.Entries))
	for name := range m.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// safeName reports whether name is a path inside the archive and can only be:
// relative, cleaned, with no parent steps. The same rule applies to a tar
// entry's name and to a manifest key, because either can be crafted.
func safeName(name string) bool {
	if name == "" || name == "." || path.IsAbs(name) || strings.HasSuffix(name, "/") {
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

// isDigest reports whether s is a lowercase hex sha256.
func isDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// modeText is a permission in the manifest's form.
func modeText(mode uint32) string { return fmt.Sprintf("%04o", mode) }
