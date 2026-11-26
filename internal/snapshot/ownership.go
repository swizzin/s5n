package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dirMode is how box creates the snapshot tree and any config directory a restore puts
// back: 0750, handed to the config root's owner. An app reaches only its own subPath
// (charts/s5n/templates/workloads.yaml) and cannot see .box; what the owner buys is a
// rollback without root, the seedbox's own box reading its snapshots and writing its
// hold.
const dirMode fs.FileMode = 0o750

// treeFileMode is how box writes the files it owns inside that tree, the manifest and
// the hold file: the seedbox's box reads both, and a file only root could read is a
// snapshot only root can roll back to.
const treeFileMode fs.FileMode = 0o640

// treeOwner is the uid and gid box gives what it creates under a config root.
type treeOwner struct {
	uid int
	gid int
}

// treeOwnerOf reads who a snapshot tree belongs to from the config root itself: as
// root that is the config root's owner, the seedbox's user, and otherwise whoever
// box runs as. Nothing here guesses at a user of its own: root has no way to know a
// seedbox's ids except from what the seedbox already owns.
func treeOwnerOf(t *tree) (treeOwner, error) {
	if os.Geteuid() != 0 {
		return treeOwner{uid: os.Geteuid(), gid: os.Getegid()}, nil
	}
	info, err := t.stat(".")
	if err != nil {
		return treeOwner{}, fmt.Errorf("snapshot: %w", err)
	}
	uid, gid := owner(info)
	if uid < 0 || gid < 0 {
		return treeOwner{}, fmt.Errorf("snapshot %s: cannot read its owner, and the seedbox's own box has to read what box writes below it", t.path("."))
	}
	return treeOwner{uid: uid, gid: gid}, nil
}

// ownIn hands a name box created to the tree's owner. A non-root run already created
// it as that user; root created it as root and must hand it over, or the app's user
// cannot read the copy the restore depends on.
func (o treeOwner) ownIn(t *tree, name string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return t.chown(name, o.uid, o.gid)
}

// makeDirIn creates dir inside the tree and every missing parent with dirMode, giving
// each one it created to the tree's owner; a directory already there is left as it
// is, the config root and anything beside box's own tree belonging to the operator.
//
// The mode is set explicitly rather than left to the umask: 0750 is the promise, the
// seedbox's user reads the tree and nobody else does.
func makeDirIn(t *tree, dir string, o treeOwner) error {
	if dir == "." {
		return nil
	}
	at := ""
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if at == "" {
			at = seg
		} else {
			at += "/" + seg
		}
		err := t.mkdir(at, dirMode)
		// Created between the check and here by someone else: theirs to own,
		// and not this run's to change.
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := t.chmod(at, dirMode); err != nil {
			return err
		}
		if err := o.ownIn(t, at); err != nil {
			return err
		}
	}
	return nil
}
