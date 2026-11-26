package discover

import (
	"context"
	"database/sql"
	"encoding/json"
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

// findData decides where the source kept downloads and library, the one fact the
// path table is built from. Downloads come from qBittorrent's save path, the
// library from the Arrs' root folders, and both must be directories directly under
// one root: otherwise a download and its import cannot share an inode, the one
// thing this migration keeps. The library's directories map onto box's
// categories.
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

	placed, err := d.rootFolderPaths(ctx)
	if err != nil {
		return nil, err
	}
	mediaHosts, err := d.mediaDirs(root, downloadsHost, placed)
	if err != nil {
		return nil, err
	}
	media, err := mediaName(root, mediaHosts)
	if err != nil {
		return nil, err
	}
	data := &plan.Data{Root: root, Downloads: path.Base(downloadsHost), Media: media}
	data.Folders = d.mediaFolders(root, media, placed)
	if data.Downloads == data.Media {
		return nil, movedByHand(root, downloadsHost, mediaHosts,
			fmt.Sprintf("the downloads and the library are both under %s", path.Join(root, media)))
	}
	return data, nil
}

// placed is one root folder as the app saw it and as it is on this host.
type placed struct {
	// instance is the box instance whose database named it.
	instance string
	// container is the directory inside the app's pod.
	container string
	// host is the directory behind the app's library mount.
	host string
}

// rootFolderPaths is every library root folder as the app saw it and on this host;
// both the library directory and the categories come from it.
func (d *discovery) rootFolderPaths(ctx context.Context) ([]placed, error) {
	var out []placed
	for _, in := range d.instances {
		if !in.info.library {
			continue
		}
		folders, category, err := d.readLibrary(ctx, in)
		if err != nil {
			return nil, err
		}
		in.downloadCategory = category
		for _, folder := range folders {
			host, ok := in.wl.hostDirOf(in.container, folder)
			if !ok {
				return nil, fmt.Errorf("%s root folder %s is not under a host-backed mount of %s: discovery cannot tell where the library is",
					in.name, folder, workloadID(in.wl))
			}
			out = append(out, placed{instance: in.name, container: folder, host: host})
		}
	}
	return out, nil
}

// mediaFolders maps each instance's library directory onto its box category.
//
// One root folder under the library per instance is the normal case. Two is a
// decision rather than a guess: the first in name order is taken, marked as a
// default with the reason, and a guided run shows it. A root folder somewhere
// else is not this library at all -- it is left where it is, and the Arr keeps
// it beside the category box adds.
func (d *discovery) mediaFolders(root, media string, placed []placed) []plan.MediaFolder {
	library := path.Join(root, media)
	byInstance := map[string][]string{}
	for _, p := range placed {
		rel, ok := trimDir(p.host, library)
		if !ok || rel == "" {
			d.note("%s keeps its root folder %s outside %s, so it is not renamed: box adds /data/media/%s beside it",
				p.instance, p.container, library, d.categoryOf(p.instance))
			continue
		}
		byInstance[p.instance] = append(byInstance[p.instance], rel)
	}
	if len(d.opt.Categories) == 0 {
		d.note("box was given no chart categories, so every library directory keeps the name the source gave it")
		return nil
	}
	var out []plan.MediaFolder
	taken := map[string]bool{}
	for _, in := range d.instances {
		rels := byInstance[in.name]
		if len(rels) == 0 {
			continue
		}
		sort.Strings(rels)
		category := d.categoryOf(in.name)
		if category == "" {
			d.note("box's values name no category for %s, so %s keeps the name the source gave it", in.name, rels[0])
			continue
		}
		folder := plan.MediaFolder{From: rels[0], To: category}
		if len(rels) > 1 {
			folder.Default = true
			folder.Why = fmt.Sprintf("rewrite to %s: %s keeps two or more root folders under %s (%s), and the first is the one that becomes its category",
				category, in.name, library, strings.Join(rels, ", "))
			d.note("%s keeps two root folders under %s (%s), so only %s becomes %s and the rest keep their names",
				in.name, library, strings.Join(rels, ", "), rels[0], category)
		}
		if taken[folder.From] {
			// Two instances keeping one directory is one directory: it can
			// only become one category, and the second Arr keeps it as it is.
			d.note("%s keeps the same root folder as another instance (%s), so %s is not renamed again",
				in.name, path.Join(library, folder.From), folder.From)
			continue
		}
		taken[folder.From] = true
		out = append(out, folder)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// categoryOf is the directory box's values give one instance, empty when the
// chart named none.
func (d *discovery) categoryOf(instance string) string {
	return d.opt.Categories[instance]
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

// mediaDirs is the host directories the Arrs keep their root folders in, as far
// as they are under the data root. Without an Arr to ask (a source whose Arrs
// are elsewhere), the only other thing that can say is the mounts themselves:
// one directory beside the downloads, under the same root, and nothing else.
// That is reported, because it is a shape rather than a setting.
//
// A root folder outside the root is not part of this library: box adopts the
// data where it is, and a library on another filesystem cannot share an inode
// with the download it came from. It is left where it is, and the mapping says
// so once.
func (d *discovery) mediaDirs(root, downloadsHost string, placed []placed) ([]string, error) {
	var out []string
	for _, p := range placed {
		if rel, ok := trimDir(p.host, root); ok && rel != "" {
			out = append(out, p.host)
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
			d.note("no Arr root folder names the library, so it is taken as %s, the one directory beside the downloads under %s", host, root)
			return []string{host}, nil
		}
	}
	names := make([]string, 0, len(siblings))
	for name := range siblings {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		if len(placed) > 0 {
			return nil, fmt.Errorf("every Arr root folder is outside %s (%s), so discovery cannot tell which directory under it is the library",
				root, strings.Join(placedHosts(placed), ", "))
		}
		return nil, fmt.Errorf("no Arr is in the source to name its root folders, and no mount sits beside %s under %s", downloadsHost, root)
	}
	return nil, fmt.Errorf("no Arr is in the source to name its root folders, and the directories beside %s under %s are %s, so discovery cannot tell which one is the library",
		downloadsHost, root, strings.Join(names, ", "))
}

// placedHosts is every root folder's host directory, for a refusal that names
// what it found.
func placedHosts(placed []placed) []string {
	out := make([]string, len(placed))
	for i, p := range placed {
		out[i] = p.host
	}
	return out
}

// readLibrary reads an Arr's root folders and the category its download client
// uses, out of its database. The database is copied first: discovery changes
// nothing in the source, and a read of a live SQLite file can leave a -shm or a
// checkpoint behind. The copy is checked with SQLite's own integrity check,
// because a copy taken between two of the app's writes is torn and its rows
// would be a wrong answer rather than a stale one.
func (d *discovery) readLibrary(ctx context.Context, in *instance) ([]string, string, error) {
	dbName := in.app + ".db"
	tmp, err := os.MkdirTemp(d.opt.TempDir, "discover-")
	if err != nil {
		return nil, "", fmt.Errorf("copy %s's database: %w", in.name, err)
	}
	defer os.RemoveAll(tmp)
	copyPath := filepath.Join(tmp, dbName)
	if err := copyDatabase(d.opt.ConfigFS, in.configDir, dbName, copyPath); err != nil {
		return nil, "", fmt.Errorf("%s keeps no %s under %s: discovery reads its root folders from it (%w)", in.name, dbName, in.configDir, err)
	}
	db, err := sqlitedb.OpenReadOnly(ctx, copyPath)
	if err != nil {
		return nil, "", fmt.Errorf("%s's database: %w", in.name, err)
	}
	defer db.Close()
	problems, err := sqlitedb.IntegrityCheck(ctx, db)
	if err != nil {
		return nil, "", fmt.Errorf("check %s's database copy: %w", in.name, err)
	}
	if len(problems) > 0 {
		return nil, "", fmt.Errorf("the copy of %s's database is torn (%s): stop the app and run discovery again", in.name, strings.Join(problems, "; "))
	}
	folders, err := queryRootFolders(ctx, db)
	if err != nil {
		return nil, "", fmt.Errorf("%s's %s: %w", in.name, dbName, err)
	}
	if len(folders) == 0 {
		return nil, "", fmt.Errorf("%s has no root folder: a plan needs the directory its library lives in, so add one in the app and run discovery again", in.name)
	}
	category, err := queryDownloadCategory(ctx, db, in.app)
	if err != nil {
		return nil, "", fmt.Errorf("%s's %s: %w", in.name, dbName, err)
	}
	return folders, category, nil
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

// queryDownloadCategory reads the category the Arr's own download client
// downloads into: the one enabled qBittorrent client's category, because that
// is the directory the app's torrents are in and a migrated box never moves
// them.
//
// The category is a field of the client's settings, and which of the two a
// client has is its own: a Sonarr client stores tvCategory, a Radarr one
// movieCategory, a Lidarr one musicCategory (Download/Clients/QBittorrent/
// QBittorrentSettings.cs at the versions this package names). A client that
// names no category downloads into the client's save path with no category at
// all, and that is no answer: the caller then keeps box's own.
func queryDownloadCategory(ctx context.Context, db *sql.DB, app string) (string, error) {
	found, err := hasTable(ctx, db, "DownloadClients")
	if err != nil || !found {
		return "", err
	}
	rows, err := db.QueryContext(ctx, `SELECT "Id", "Enable", "Implementation", "Settings" FROM "DownloadClients" ORDER BY "Id"`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	category := ""
	seen := 0
	for rows.Next() {
		var id int64
		var enable sql.NullInt64
		var implementation, settings sql.NullString
		if err := rows.Scan(&id, &enable, &implementation, &settings); err != nil {
			return "", err
		}
		if !strings.EqualFold(implementation.String, "QBittorrent") {
			continue
		}
		if enable.Valid && enable.Int64 == 0 {
			continue
		}
		seen++
		if seen > 1 {
			// Two enabled clients are two directories, and box wires one:
			// the first the app has is the one it was set up with.
			continue
		}
		category = settingsCategory(settings.String, app)
	}
	return category, rows.Err()
}

// settingsCategory is the category one client's settings name, read out of the
// JSON blob the Arr stores it in. The key is camel-cased on disk whatever the
// class calls it, so it is matched without case.
func settingsCategory(settings, app string) string {
	if strings.TrimSpace(settings) == "" {
		return ""
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(settings), &f); err != nil {
		return ""
	}
	key := "tvCategory"
	if app == "radarr" {
		key = "movieCategory"
	}
	if app == "lidarr" {
		// A Lidarr client has one category and it is the one its downloads are
		// in (QBittorrentSettings.MusicCategory at v3.1.2.4902).
		key = "musicCategory"
	}
	for k, v := range f {
		if !strings.EqualFold(k, key) {
			continue
		}
		s, _ := v.(string)
		return strings.TrimSpace(s)
	}
	return ""
}

// hasTable reports whether a database has a table of that name. A schema is the
// app's, not this package's: a synthetic one, and an app before it wrote its
// first client, have none.
func hasTable(ctx context.Context, db *sql.DB, name string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
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
