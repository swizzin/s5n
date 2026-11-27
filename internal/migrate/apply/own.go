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

	"github.com/swizzin/s5n/internal/safefs"
)

// own is step 6: every path under every staged instance belongs to the
// seedbox's user. The walk is resolved from the staged tree's own handle, and a
// chown names the entry inside it: a path walked by name would follow a `.box` the
// user had swapped out of the config root and hand away whatever it names.
func (m *migration) own() error {
	chown := m.env.Chown
	for _, name := range planApps(m.p) {
		tree, err := safefs.Open(m.root, m.stagingName(name))
		if err != nil {
			return fmt.Errorf("hand the staged %s to the seedbox's user %d:%d: %w", name, m.env.PUID, m.env.PGID, err)
		}
		err = fs.WalkDir(tree.Root().FS(), ".", func(rel string, d fs.DirEntry, err error) error {
			if err != nil {
				return tree.Err(rel, err)
			}
			if chown != nil {
				err = chown(tree.Path(rel), m.env.PUID, m.env.PGID)
			} else {
				err = tree.Root().Lchown(rel, m.env.PUID, m.env.PGID)
			}
			if err != nil {
				return fmt.Errorf("hand %s to the seedbox's user %d:%d: %w", tree.Path(rel), m.env.PUID, m.env.PGID, err)
			}
			return nil
		})
		tree.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
