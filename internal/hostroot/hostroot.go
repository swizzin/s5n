// Package hostroot is where box keeps its own files on the host: one directory,
// one name per file, and what to do about a host that still has the files where
// box kept them before 0.5.
//
// The directory is /etc/s5n and not /etc/swizzin. Swizzin classic installs
// itself as a git checkout there, so box writing its cluster record, its own
// config and every seedbox's grant into /etc/swizzin meant writing into another
// program's repository -- and `rm -rf /etc/swizzin` (how swizzin is retired)
// would have taken box's records with it. Every host path box owns lives here,
// so a move is one constant, and nothing else in the tree spells these paths
// out.
package hostroot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the directory box owns on the host.
const Dir = "/etc/s5n"

// RecordName is bootstrap's cluster record, beneath Dir.
const RecordName = "cluster.yaml"

// RecordPath is where box records the cluster decision.
const RecordPath = Dir + "/" + RecordName

// ConfigName is root's config (internal/hostconf): the update timer and the
// notice address, beside the record.
const ConfigName = "box.yaml"

// SeedboxName is the directory holding one grant per user (internal/seedbox).
const SeedboxName = "seedboxes"

// SeedboxDir holds one grant per user, root-owned and root-written.
const SeedboxDir = Dir + "/" + SeedboxName

// LegacyDir is where box kept all of this before 0.5: swizzin classic's own
// checkout. Nothing may write there any more, and a host that still has the
// files there is refused rather than silently upgraded (0.5 supports no
// upgrade, and a half-moved box is a box that loses its records).
const LegacyDir = "/etc/swizzin"

// Stat asks whether a path is there. A missing path is (false, nil); any other
// failure is the disk not answering, which is an error because "there is none"
// and "I could not look" lead to opposite decisions about refusing.
type Stat func(path string) (bool, error)

// OSStat is the host's own filesystem. It does not follow a symlink: a link
// where one of box's files belongs is not that file.
func OSStat(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// Stale is what a box older than 0.5 left at the old root.
type Stale struct {
	// Move are box's own files that only the old root has. box reads the new
	// root, so it would see a host with no record at all: a command refuses
	// until they are moved.
	Move []string
	// Shadowed are files both roots have. The new one is the one in force, and
	// the old copy is named so it can be removed: a stale record that still
	// reads like the truth is how someone edits a file box never opens.
	Shadowed []string
}

// Empty reports whether the old root holds nothing of box's.
func (s Stale) Empty() bool { return len(s.Move) == 0 && len(s.Shadowed) == 0 }

// Err is the refusal for a host that still keeps box's files at the old root,
// or nil when every one of them is at the new one. The move is shown as a
// command to check and run, not one box runs: box never moves root's files.
func (s Stale) Err() error {
	if len(s.Move) == 0 {
		return nil
	}
	mv := append([]string{"mv"}, s.Move...)
	return fmt.Errorf("box keeps its files in %s now, and %s still has box's old ones (%s). "+
		"Check, then move them: mkdir -p %s && %s %s",
		Dir, LegacyDir, strings.Join(s.Move, ", "), Dir, strings.Join(mv, " "), Dir)
}

// Warnings names every file both roots have: the new one is read, and the old
// copy is stale.
func (s Stale) Warnings() []string {
	out := make([]string, 0, len(s.Shadowed))
	for _, path := range s.Shadowed {
		out = append(out, fmt.Sprintf("%s is stale: box keeps its files in %s now, and reads that copy", path, Dir))
	}
	return out
}

// Look reports box's own files at the old root, for a run whose record is at
// recordPath and whose grants are at grantPath.
//
// Only the files the run actually reads are looked for, and only where box
// would read them by default: a run whose --record or --grant names another
// directory made that choice itself, and /etc/swizzin is not what it would
// have read. That is also what keeps an e2e's scratch record from tripping
// over a real host's old files.
func Look(recordPath, grantPath string, stat Stat) (Stale, error) {
	var s Stale
	if filepath.Dir(filepath.Clean(recordPath)) == Dir {
		for _, name := range []string{filepath.Base(recordPath), ConfigName} {
			if err := s.compare(filepath.Join(LegacyDir, name), filepath.Join(Dir, name), stat); err != nil {
				return Stale{}, err
			}
		}
	}
	if filepath.Dir(filepath.Clean(grantPath)) == SeedboxDir {
		if err := s.compare(filepath.Join(LegacyDir, SeedboxName), SeedboxDir, stat); err != nil {
			return Stale{}, err
		}
	}
	return s, nil
}

// compare files one of box's files at both roots.
func (s *Stale) compare(old, now string, stat Stat) error {
	have, err := stat(old)
	if err != nil {
		return fmt.Errorf("look at %s: %w", old, err)
	}
	if !have {
		return nil
	}
	want, err := stat(now)
	if err != nil {
		return fmt.Errorf("look at %s: %w", now, err)
	}
	if want {
		s.Shadowed = append(s.Shadowed, old)
		return nil
	}
	s.Move = append(s.Move, old)
	return nil
}
