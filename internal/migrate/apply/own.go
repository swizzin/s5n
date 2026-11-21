package apply

// own.go is step 6: every path of every staged config tree is handed to the
// seedbox's user.
//
// It runs after the write pass and before anything is moved into place, and
// that position is the whole point. A rewriter writes through atomicfile, which
// creates a new file beside the app's, writes it, and renames it over the
// original: the copy's owner -- copyTree gives every entry to the release's
// uid -- is replaced by whoever ran box, which on a real host is root. A file a
// rewriter touched is therefore root:root, often 0600, and the app that reads
// it is not root. On a real host that looked like this: a migrated Radarr and
// Sonarr crash-looped on a config.xml they could not read, qBittorrent and its
// 1532 resume records were unreadable to it, PeerBanHelper's and qui's init
// containers failed on theirs, and Seerr started anyway on an empty
// settings.json and wrote its defaults over the settings the migration had
// just carried -- the one of them that loses data rather than failing.
//
// One pass here, after every rewriter and before the move, is what keeps a
// future rewriter from having to remember it: a file it replaces or creates
// gets the same rule as one it never opens. The pass is not a list of files
// any app is known to write, deliberately: the copy is generic (stage.go) and
// so is this.
//
// Two things are outside it, each for a reason:
//   - The runner's exchange directory (<config>/.box/migrate/<id>/.runner) is
//     box's, not an app's, and the pod that reads it runs as the release's uid
//     already (runner.go's own chowns cover it).
//   - The Plex runner's output lands in the staged tree from a pod that runs as
//     PUID:PGID, so it is owned right before it arrives; this pass covers it
//     again when it does, because the pass runs after every Write.
//
// Links are chowned, never followed. A config tree may hold a link inside
// itself (stage.go carries them, and rewrites an absolute one to the relative
// form), and the file it names is already in the walk; following it would hand
// whichever path it names -- possibly outside the tree -- to the seedbox's user.

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
