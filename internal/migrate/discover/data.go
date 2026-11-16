package discover

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
	"github.com/swizzin/s5n/internal/qbtconf"
)

// findData decides where the source kept the downloads and the library, which is
// the one fact the whole path table is built from: box adopts the data where it
// is, so the plan names the root and the two directories under it, and apply
// renames those to box's names (plan.Data).
//
// The downloads are where qBittorrent saved, read from its own config: the
// library is where the Arrs' root folders are, read from their databases. The
// two have to be two directories directly under one parent, or a download and
// the copy imported from it cannot share an inode -- which is the one thing this
// migration exists to keep.
func (d *discovery) findData(ctx context.Context) (*plan.Data, error) {
	qbit := d.byApp("qbittorrent")
	if qbit == nil {
		return nil, errors.New("the source runs no qBittorrent: discovery finds the data root from its save path, and a plan with no data adopts nothing")
	}
	savePath, fallback, err := d.qbtSavePath(qbit)
	if err != nil {
		return nil, err
	}
	downloadsHost, ok := qbit.wl.hostDirOf(qbit.container, savePath)
	if !ok {
		return nil, fmt.Errorf("qBittorrent saves under %s, which no host-backed mount of %s covers: discovery cannot tell which host directory the downloads are in",
			savePath, workloadID(qbit.wl))
	}
	if fallback {
		d.note("qBittorrent's config names no save path, so its downloads are the mount it keeps at %s: %s", savePath, downloadsHost)
	}
	root := path.Dir(downloadsHost)
	if root == "/" || root == "." {
		return nil, fmt.Errorf("qBittorrent downloads into %s, which has no parent directory to be the data root", downloadsHost)
	}

	mediaHosts, err := d.mediaDirs(ctx, root, downloadsHost)
	if err != nil {
		return nil, err
	}
	media, err := mediaName(root, mediaHosts)
	if err != nil {
		return nil, err
	}
	data := &plan.Data{Root: root, Downloads: path.Base(downloadsHost), Media: media}
	if data.Downloads == data.Media {
		return nil, movedByHand(root, downloadsHost, mediaHosts,
			fmt.Sprintf("the downloads and the library are both under %s", path.Join(root, media)))
	}
	return data, nil
}

// mediaName is the one directory under the data root the Arrs' root folders live
// in. A root folder is a category inside it (/library/media/TV), so it is the
// first segment below the root, and every root folder has to agree on it.
func mediaName(root string, hosts []string) (string, error) {
	var name string
	var problems []string
	for _, hostPath := range hosts {
		rel, ok := trimDir(hostPath, root)
		if !ok || rel == "" {
			problems = append(problems, fmt.Sprintf("%s is not under it", hostPath))
			continue
		}
		segment, _, _ := strings.Cut(rel, "/")
		switch {
		case name == "":
			name = segment
		case segment != name:
			problems = append(problems, fmt.Sprintf("one root folder is under %s and another under %s", name, segment))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return "", movedByHand(root, "", hosts, strings.Join(problems, " and "))
	}
	if name == "" {
		return "", fmt.Errorf("no Arr root folder names the library and no mount sits beside the downloads under %s", root)
	}
	return name, nil
}

// movedByHand is the one refusal a data layout box cannot adopt gets: the
// operator moves one of the two directories so that both sit directly under one
// parent, and runs discovery again.
//
// A rename keeps every hardlink, so it is the only way the data moves; a move
// between filesystems copies, and the copy an Arr imports would stop sharing an
// inode with the download it came from. That is why this is a stop rather than
// something apply does with a copy (docs/box.md, "Data is adopted by renaming,
// never by moving").
func movedByHand(root, downloadsHost string, mediaHosts []string, problem string) error {
	where := ""
	if downloadsHost != "" {
		where = fmt.Sprintf(", and the downloads are %s", downloadsHost)
	}
	if len(mediaHosts) > 0 {
		where += fmt.Sprintf(", and the library is %s", strings.Join(mediaHosts, " and "))
	}
	return fmt.Errorf("the data has to be moved by hand first: box adopts %s as the data root and needs the downloads and the library as two directories directly under it%s, but %s",
		root, where, problem)
}

// byApp is the instance of a catalog app, when the source runs it.
func (d *discovery) byApp(app string) *instance {
	for _, in := range d.instances {
		if in.app == app {
			return in
		}
	}
	return nil
}

// qbtSavePath is the directory qBittorrent saved into, as qBittorrent saw it,
// and whether it came from the mount rather than from the app's config.
func (d *discovery) qbtSavePath(qbit *instance) (string, bool, error) {
	data, ok, err := readConfigFile(d.opt.ConfigFS, qbit.configDir, "qBittorrent/qBittorrent.conf")
	if err != nil {
		return "", false, fmt.Errorf("read qBittorrent's config: %w", err)
	}
	if ok {
		doc := qbtconf.Parse(data)
		for _, key := range []struct{ section, key string }{
			// Session/DefaultSavePath, escaped as QSettings writes it.
			{"BitTorrent", `Session\DefaultSavePath`},
			// The chart's init container seeds that section, and an install
			// that was started by an older chart can still carry the spelling
			// (internal/qbtconf/conf.go, Seal).
			{preferencesSection, `Session\DefaultSavePath`},
			// Pre-4.2, folded forward on first start.
			{preferencesSection, `Downloads\SavePath`},
		} {
			// The value as the app reads it: QSettings quotes a path holding
			// a space or list syntax, and the quotes are not part of the path.
			if v, ok := doc.Get(key.section, key.key); ok {
				if p := strings.TrimSpace(qbtconf.Unquote(v)); p != "" {
					return p, false, nil
				}
			}
		}
	}
	// Never started, or no save path set: the mount the chart names is the only
	// thing left to go on, and taking it is reported rather than assumed.
	if _, ok := qbit.container.mountAt("/downloads"); ok {
		return "/downloads", true, nil
	}
	return "", false, fmt.Errorf("qBittorrent's config names no save path and %s mounts nothing at /downloads: discovery cannot tell where the downloads are", workloadID(qbit.wl))
}

// mediaDirs is the host directories the Arrs keep their root folders in. Without
// an Arr to ask (a source whose Arrs are elsewhere), the only other thing that
// can say is the mounts themselves: one directory beside the downloads, under
// the same root, and nothing else. That is reported, because it is a shape
// rather than a setting.
func (d *discovery) mediaDirs(ctx context.Context, root, downloadsHost string) ([]string, error) {
	var out []string
	for _, in := range d.instances {
		if !in.info.library {
			continue
		}
		folders, err := d.rootFolders(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, folder := range folders {
			host, ok := in.wl.hostDirOf(in.container, folder)
			if !ok {
				return nil, fmt.Errorf("%s root folder %s is not under a host-backed mount of %s: discovery cannot tell where the library is",
					in.name, folder, workloadID(in.wl))
			}
			out = append(out, host)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	siblings := make(map[string]string)
	for _, in := range d.instances {
		for _, m := range in.container.Mounts {
			vol, ok := in.wl.Volumes[m.Volume]
			if !ok || vol.HostPath == "" || path.Clean(m.Path) == in.configMount {
				continue
			}
			if path.Dir(vol.HostPath) != root || vol.HostPath == downloadsHost {
				continue
			}
			siblings[path.Base(vol.HostPath)] = vol.HostPath
		}
	}
	if len(siblings) == 1 {
		for _, host := range siblings {
			d.note("no Arr is in the source to name its root folders, so the library is taken as %s, the one directory beside the downloads under %s", host, root)
			return []string{host}, nil
		}
	}
	names := make([]string, 0, len(siblings))
	for name := range siblings {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no Arr is in the source to name its root folders, and no mount sits beside %s under %s", downloadsHost, root)
	}
	return nil, fmt.Errorf("no Arr is in the source to name its root folders, and the directories beside %s under %s are %s, so discovery cannot tell which one is the library",
		downloadsHost, root, strings.Join(names, ", "))
}

// rootFolders reads an Arr's root folders out of its database. The database is
// copied first: discovery changes nothing in the source, and a read of a live
// SQLite file can leave a -shm or a checkpoint behind. The copy is checked with
// SQLite's own integrity check, because a copy taken between two of the app's
// writes is torn and its rows would be a wrong answer rather than a stale one.
func (d *discovery) rootFolders(ctx context.Context, in *instance) ([]string, error) {
	dbName := in.app + ".db"
	tmp, err := os.MkdirTemp(d.opt.TempDir, "discover-")
	if err != nil {
		return nil, fmt.Errorf("copy %s's database: %w", in.name, err)
	}
	defer os.RemoveAll(tmp)
	copyPath := filepath.Join(tmp, dbName)
	if err := copyDatabase(d.opt.ConfigFS, in.configDir, dbName, copyPath); err != nil {
		return nil, fmt.Errorf("%s keeps no %s under %s: discovery reads its root folders from it (%w)", in.name, dbName, in.configDir, err)
	}
	db, err := sqlitedb.OpenReadOnly(ctx, copyPath)
	if err != nil {
		return nil, fmt.Errorf("%s's database: %w", in.name, err)
	}
	defer db.Close()
	problems, err := sqlitedb.IntegrityCheck(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("check %s's database copy: %w", in.name, err)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the copy of %s's database is torn (%s): stop the app and run discovery again", in.name, strings.Join(problems, "; "))
	}
	folders, err := queryRootFolders(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%s's %s: %w", in.name, dbName, err)
	}
	if len(folders) == 0 {
		return nil, fmt.Errorf("%s has no root folder: a plan needs the directory its library lives in, so add one in the app and run discovery again", in.name)
	}
	return folders, nil
}

// queryRootFolders reads the one table discovery needs: the Arr's root folders,
// absolute as the app saw them (RootFolder.cs:8 at v5.0.0.2907 for Sonarr,
// Movie.cs's sibling at v7.0.0.10815 for Radarr).
func queryRootFolders(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT "Path" FROM "RootFolders" ORDER BY "Path"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var folder string
		if err := rows.Scan(&folder); err != nil {
			return nil, err
		}
		if folder != "" {
			out = append(out, folder)
		}
	}
	return out, rows.Err()
}

// copyDatabase copies a SQLite database and the two companion files a WAL-mode
// database needs, through the read-only config filesystem. The WAL holds the
// app's most recent writes -- the root folder it added yesterday is in it -- so
// copying the database alone would read an older state than the app has.
//
// It streams rather than reads: an Arr's database is hundreds of megabytes and
// discovery runs on the host that holds it.
func copyDatabase(fsys fs.FS, dir, name, dest string) error {
	found := false
	for _, suffix := range []string{"", "-wal", "-shm"} {
		rel := strings.TrimPrefix(path.Join(dir, name+suffix), "/")
		src, err := fsys.Open(rel)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && suffix != "" {
				continue
			}
			return err
		}
		err = copyFile(dest+suffix, src)
		src.Close()
		if err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("nothing to copy")
	}
	return nil
}

// copyFile is a whole file into a new one, without holding it in memory.
func copyFile(dst string, src fs.File) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
