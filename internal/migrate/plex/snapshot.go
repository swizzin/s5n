package plex

// snapshot.go is what a migration snapshot takes out of a Plex config directory.
// It is two things: state (Preferences.xml and the library databases) and a
// library Plex built from the media -- artwork, chapter thumbnails and .bif
// previews, hours of machine time -- so a full carry takes the library when there
// is room and the state alone when not. A full carry leaves out what Plex rebuilds
// or downloads again with no scan: Cache, Plug-in Support/Caches, Codecs, and
// Logs/Crash Reports/Diagnostics/Updates. Metadata and Media are kept. Paths are
// relative to Plex's home (Sub), and a snapshot of Plex is that directory and
// nothing else in the mount.

import (
	"path"
	"path/filepath"

	"github.com/swizzin/s5n/internal/migrate/space"
)

// File is one file a snapshot carries, relative to the config directory.
type File struct {
	Path string
	// Required: Plex cannot start on state missing this file.
	Required bool
}

// Scope is what a migration snapshot takes out of one Plex config directory.
type Scope struct {
	// Full says the tree travels rather than the state.
	Full bool
	// Sub is Plex's home, the one place a snapshot takes anything from.
	Sub string
	// Files is the state a non-full carry takes, with the -wal and -shm a writer
	// leaves, relative to Sub.
	Files []File
	// Skip is what a full carry leaves behind, each with everything under it.
	Skip []string
	// Note is the line a report and confirm say about what the snapshot carries.
	Note string
	// Summary is the same for the app's row.
	Summary string
}

// state is what a state-only snapshot carries. The -wal and -shm exist only while
// a writer has the database open.
var state = []File{
	{Path: prefsName, Required: true},
	{Path: dbRel, Required: true},
	{Path: dbRel + "-wal"},
	{Path: dbRel + "-shm"},
	{Path: blobsRel},
	{Path: blobsRel + "-wal"},
	{Path: blobsRel + "-shm"},
}

// blobsRel is the second library database, holding the artwork.
const blobsRel = "Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db"

// disposable is what a full carry leaves off; the report says the decision in one
// line, so the reasons are not printed one by one.
var disposable = []string{
	"Cache",
	"Plug-in Support/Caches",
	"Codecs",
	"Logs",
	"Crash Reports",
	"Diagnostics",
	"Updates",
}

const (
	// noteState is the line for a snapshot carrying Plex's state alone.
	noteState = "plex: carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)"
	// noteFull is the line for one carrying its whole data.
	noteFull = "plex: carried its whole data -- metadata, artwork, thumbnails and its databases -- except the caches, codecs, logs and crash reports Plex rebuilds or downloads again"
	// summaryState and summaryFull are the confirm's two rows: whether Plex builds
	// its library again.
	summaryState = "its state only: Plex builds its artwork, thumbnails and detection again"
	summaryFull  = "its whole data: metadata, artwork and thumbnails"
)

// Snapshot is what a migration snapshot takes out of a Plex config directory:
// its whole data when full, and its state when not.
func Snapshot(full bool) Scope {
	if !full {
		return Scope{Sub: supportDir, Files: append([]File(nil), state...), Note: noteState, Summary: summaryState}
	}
	return Scope{Sub: supportDir, Full: true, Skip: append([]string(nil), disposable...), Note: noteFull, Summary: summaryFull}
}

// Home is Plex's home under the config directory, a slash path: the image keeps it
// under Library/Application Support, and a source that mounted it has it at the
// top (empty).
func Home(dir string) (string, error) { return supportDirIn(dir) }

// State reports whether a path inside a Plex config directory is state a check
// reads (Preferences.xml or a database), under whichever layout it keeps its home
// in. Only state is read by a check or a rewriter; the tree is written once.
func State(rel string) bool {
	if rel == "" {
		return false
	}
	for _, home := range supportCandidates {
		for _, f := range state {
			if path.Join(home, f.Path) == rel {
				return true
			}
		}
	}
	return false
}

// Size is the bytes a full carry takes: Plex's home less what Snapshot(true)
// leaves out, from a walk with no reads.
func Size(dir string) (int64, error) {
	home, err := supportDirIn(dir)
	if err != nil {
		return 0, err
	}
	return space.Size(filepath.Join(dir, filepath.FromSlash(home)), disposable)
}
