package apply

// own.go is step 6: every path of every staged config tree is handed to the
// seedbox's user, after every rewriter and before the move. That position is the
// point: atomicfile renames a new file over the app's, so a file the rewriter
// touched is root:root, often 0600, and the app crash-loops or -- worse, Seerr --
// starts on defaults and overwrites what was carried. One generic pass spares every
// future rewriter from remembering it. The runner's exchange is box's, and a Plex
// pod's output is already PUID:PGID. Links are chowned, never followed, since they
// may name a path outside the tree.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// own is step 6: every path under every staged instance belongs to the
// seedbox's user.
func (m *migration) own() error {
	chown := m.env.Chown
	if chown == nil {
		chown = os.Lchown
	}
	for _, name := range planApps(m.p) {
		root := m.staging(name)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := chown(path, m.env.PUID, m.env.PGID); err != nil {
				return fmt.Errorf("hand %s to the seedbox's user %d:%d: %w", path, m.env.PUID, m.env.PGID, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
