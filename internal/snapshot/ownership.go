package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// dirMode is how box creates the snapshot tree and any config directory a
// restore has to put back: 0750, and handed to the config root's owner rather
// than left as the root process that made it.
//
// An app reaches only its own subPath of the config root (`subPath:
// {{ $inst.name }}` in charts/s5n/templates/workloads.yaml), so no app can see
// .box at all; what the owner buys is a rollback without root. Everything under
// .box belongs to the seedbox's user, so a seedbox's own box, run as that user,
// reads its snapshots and writes its hold. A tree only root could read would
// make rolling back a user's own app a root command.
const dirMode fs.FileMode = 0o750

// treeFileMode is how box writes the files it owns inside that tree: the
// manifest each snapshot carries and the hold file beside them. The seedbox's
// box reads both, and a file only root could read is a snapshot only root can
// roll back to.
const treeFileMode fs.FileMode = 0o640

// treeOwner is the uid and gid box gives what it creates under a config root.
type treeOwner struct {
	uid int
	gid int
}

// treeOwnerOf reads who a snapshot tree belongs to from the config root itself.
// As root that is the config root's owner -- the seedbox's user, because the
// config root is theirs -- and otherwise it is whoever box runs as, which is
// also who creates the files. Nothing here guesses at a user of its own: root
// has no way to know a seedbox's ids except from what the seedbox already owns.
func treeOwnerOf(configRoot string) (treeOwner, error) {
	if os.Geteuid() != 0 {
		return treeOwner{uid: os.Geteuid(), gid: os.Getegid()}, nil
	}
	info, err := os.Stat(configRoot)
	if err != nil {
		return treeOwner{}, fmt.Errorf("snapshot: %w", err)
	}
	uid, gid := owner(info)
	if uid < 0 || gid < 0 {
		return treeOwner{}, fmt.Errorf("snapshot %s: cannot read its owner, and the seedbox's own box has to read what box writes below it", configRoot)
	}
	return treeOwner{uid: uid, gid: gid}, nil
}

// own hands a path box created to the tree's owner. A run that is not root
// already created it as that user, so there is nothing to change; root created
// it as root and must hand it over, or the app's user cannot read the copy the
// restore depends on.
func (o treeOwner) own(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Chown(path, o.uid, o.gid); err != nil {
		return err
	}
	return nil
}

// makeDir creates dir and every missing parent of it with dirMode, and gives
// each directory it created to the tree's owner. A directory that is already
// there is left exactly as it is: the config root and anything beside box's own
// tree belong to the operator, not to a run that happened to walk past.
//
// The mode is set explicitly rather than left to the umask, because 0750 is the
// promise: the seedbox's user reads the tree, and nobody else does.
func makeDir(dir string, o treeOwner) error {
	var missing []string
	for at := dir; ; at = filepath.Dir(at) {
		if _, err := os.Lstat(at); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, at)
		if parent := filepath.Dir(at); parent == at {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], dirMode); err != nil {
			// Created between the check and here by someone else: theirs to
			// own, and not this run's to change.
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		if err := os.Chmod(missing[i], dirMode); err != nil {
			return err
		}
		if err := o.own(missing[i]); err != nil {
			return err
		}
	}
	return nil
}
