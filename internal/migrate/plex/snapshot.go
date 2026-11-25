package plex

// snapshot.go is what a migration snapshot takes out of a Plex config directory.
//
// A Plex config directory is two things: its state, which is Preferences.xml and
// the library databases, and a library of its own -- Metadata, Media, the
// bundles, the artwork, the chapter thumbnails and the video preview thumbnails
// -- which Plex builds from the media it is given. Building it again is hours of
// the machine's time at full load (thumbnail creation and intro detection
// above all), so a migration carries the library too when there is room for it,
// and its state alone when there is not.
//
// What a full carry leaves out is what Plex rebuilds or downloads again by
// itself, with no library scan behind it:
//
//	Cache                  the HTTP cache, the photo transcoder's output and the
//	                       transcode buffer, all of it regenerated on demand
//	Plug-in Support/Caches the agents' and the photo transcoder's caches
//	Codecs                 the transcoder's codec packages, downloaded again
//	                       the first time one is needed
//	Logs, Crash Reports, Diagnostics, Updates
//	                       Plex's own records of a run, and an update package it
//	                       has already applied
//
// Metadata and Media are kept: they hold the artwork, the chapter thumbnails and
// the .bif preview thumbnails, which is the work this exists to avoid.
//
// The paths are relative to Plex's home, in slash form, and Sub says where that
// home is inside a config directory: the image keeps it under
// PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR, and a snapshot of Plex is a snapshot
// of that directory and of nothing else in the mount.

import (
	"path"
	"path/filepath"

	"github.com/swizzin/s5n/internal/migrate/space"
)

// File is one file a migration snapshot carries out of Plex's config directory,
// relative to it in slash form.
type File struct {
	Path string
	// Required says a snapshot without this file is refused: Plex cannot start
	// on the state it was given without it.
	Required bool
}

// Scope is what a migration snapshot takes out of one Plex config directory.
type Scope struct {
	// Full says the tree travels rather than the state.
	Full bool
	// Sub is the directory inside the app's config directory that is Plex's
	// own: its home, and the one place a snapshot takes anything from.
	Sub string
	// Files is the state a snapshot carries when the tree does not travel, with
	// the -wal and -shm a writer leaves beside a database, relative to Sub.
	Files []File
	// Skip is where a full carry leaves Plex's own caches and records behind,
	// each a directory with everything under it, relative to Sub.
	Skip []string
	// Note is the one line a read's report and apply's confirm say about what
	// the snapshot carries.
	Note string
	// Summary is the same, for the app's row on the confirm: the tree in a few
	// words, or the state and what the app builds again.
	Summary string
}

// state is the files a snapshot of Plex's state alone carries. The two are
// required: a snapshot without either is one Plex cannot start on. The
// companions are there only while something has the database open, so a
// snapshot that lacks them is a database nobody had open.
var state = []File{
	{Path: prefsName, Required: true},
	{Path: dbRel, Required: true},
	{Path: dbRel + "-wal"},
	{Path: dbRel + "-shm"},
	{Path: blobsRel},
	{Path: blobsRel + "-wal"},
	{Path: blobsRel + "-shm"},
}

// blobsRel is the second library database, which holds the artwork Plex stores
// beside its metadata.
const blobsRel = "Plug-in Support/Databases/com.plexapp.plugins.library.blobs.db"

// disposable is where a full carry leaves off, with what each directory is. The
// reasons are what the docs and this file's header say; they are not printed one
// by one, because the report says the whole decision in one line.
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
	// summaryState and summaryFull are the two rows of a confirm, in the words
	// of what the operator is deciding about: whether Plex builds its library
	// again.
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

// Home is where Plex's own home is inside the config directory at dir, as a
// slash path relative to it: the image keeps it under Library/Application
// Support, and a source that mounted the support directory itself has it at the
// top. Empty is a home at the top of the mount, which is not an error.
func Home(dir string) (string, error) { return supportDirIn(dir) }

// State reports whether one path inside a Plex config directory -- in slash form,
// relative to it -- is part of the state a check reads: Preferences.xml or one of
// the library databases, under whichever layout the config directory keeps its
// home in.
//
// It is what an extraction keeps of a full carry. A Plex config directory is its
// state and a library of its own, and only the state is ever read by a check, by
// the pass before an install or by a rewriter; the tree (Metadata, Media) is
// written once, at the config root, straight out of the snapshot.
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

// Size is the bytes a full carry of the Plex config directory at dir takes: the
// tree under Plex's home, minus the directories Snapshot(true) leaves out. It is
// a walk with no reads, so it is the size a decision before the copy can afford.
func Size(dir string) (int64, error) {
	home, err := supportDirIn(dir)
	if err != nil {
		return 0, err
	}
	return space.Size(filepath.Join(dir, filepath.FromSlash(home)), disposable)
}
