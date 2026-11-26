package discover

import (
	"strconv"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// categories is the chart's own directory for each instance of the source
// install's Arrs, which is what a caller hands discovery so the library it
// finds can be renamed onto box's layout.
func categories() map[string]string {
	return map[string]string{
		"sonarr": "tv", "sonarr-4k": "tv-4k",
		"radarr": "movies", "radarr-4k": "movies-4k",
	}
}

// arrInstall is the shape a real seedbox has: the library named after what each
// instance collects, the 4K ones in a subdirectory of their own, and the
// downloads named after the same instances because that is where their torrents
// already are.
func arrInstall(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	arr := func(name string, port int, hostConfig string) {
		f.app(t, appSpec{
			name: name, image: "lscr.io/linuxserver/" + strings.Split(name, "-")[0] + ":1", port: port,
			mounts: []mount{
				{volume: "config", path: "/config", host: hostConfig},
				{volume: "library", path: "/library", host: "/srv/library/library"},
				// A second library mount on another filesystem, which is
				// where a root folder outside the library is read from.
				{volume: "other", path: "/other", host: "/srv/other"},
			},
		})
		f.file(t, hostConfig+"/config.xml", "<Config><Port>"+strconv.Itoa(port)+"</Port></Config>\n")
	}
	arr("sonarr", 8989, "/srv/apps/sonarr")
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDBWith(t, []string{"/library/TV"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "TV"},
	}))
	arr("sonarr-4k", 8990, "/srv/apps/sonarr-4k")
	f.bytes(t, "/srv/apps/sonarr-4k/sonarr.db", arrDBWith(t, []string{"/library/4K/TV"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "4K/TV"},
	}))
	arr("radarr", 7878, "/srv/apps/radarr")
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDBWith(t, []string{"/library/Movies"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "Movies"},
	}))
	arr("radarr-4k", 7879, "/srv/apps/radarr-4k")
	f.bytes(t, "/srv/apps/radarr-4k/radarr.db", arrDBWith(t, []string{"/library/4K/Movies"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "4K/Movies"},
	}))
	f.app(t, appSpec{
		name: "qbittorrent", image: "ghcr.io/home-operations/qbittorrent:5.3.0", port: 8080,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/qbittorrent"},
			{volume: "downloads", path: "/downloads", host: "/srv/library/Downloads"},
		},
	})
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n\n[WebUI]\nPort=8080\n")
	f.app(t, appSpec{
		name: "plex", image: "linuxserver/plex:1.41.5", port: 32400,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/plex"},
			{volume: "media", path: "/media", host: "/srv/library/library"},
		},
		emptyDirs: []mount{{volume: "transcode", path: "/transcode"}},
	})
	return f
}

// withCategories hands discovery the chart's directories, the way cmd/box hands
// over the categories its values name.
func (f *fixture) withCategories() *fixture {
	f.categories = categories()
	return f
}

func TestDiscoverMapsTheLibraryOntoBoxCategories(t *testing.T) {
	f := arrInstall(t).withCategories()
	p, _ := f.discover(t)
	roundTrip(t, p)

	want := []plan.MediaFolder{
		{From: "4K/Movies", To: "movies-4k"},
		{From: "4K/TV", To: "tv-4k"},
		{From: "Movies", To: "movies"},
		{From: "TV", To: "tv"},
	}
	if len(p.Data.Folders) != len(want) {
		t.Fatalf("data.folders = %#v, want %#v", p.Data.Folders, want)
	}
	for i := range want {
		if p.Data.Folders[i] != want[i] {
			t.Fatalf("data.folders = %#v, want %#v", p.Data.Folders, want)
		}
	}
	for name, category := range categories() {
		if got := p.Apps[name].Category; got != category {
			t.Errorf("%s: category %q, want %q", name, got, category)
		}
	}
	// Each Arr's own view of its library is the box's directory, and the
	// library's root still maps to /data/media for anything else under it.
	for name, pair := range map[string][2]string{
		"radarr":    {"/library/Movies", "/data/media/movies"},
		"radarr-4k": {"/library/4K/Movies", "/data/media/movies-4k"},
		"sonarr":    {"/library/TV", "/data/media/tv"},
		"sonarr-4k": {"/library/4K/TV", "/data/media/tv-4k"},
	} {
		if got, ok := p.Rewrite(name, pair[0]); !ok || got != pair[1] {
			t.Errorf("%s: %s rewrites to %q (%v), want %q", name, pair[0], got, ok, pair[1])
		}
		if got, ok := p.Rewrite(name, "/library/Other/Show"); !ok || got != "/data/media/Other/Show" {
			t.Errorf("%s: a directory beside the mapped one rewrites to %q (%v)", name, got, ok)
		}
	}
	// Plex sees the library at /media and its sections under it, so it gets
	// the same mapping through its own mount.
	if got, ok := p.Rewrite("plex", "/media/Movies"); !ok || got != "/data/media/movies" {
		t.Errorf("plex: /media/Movies rewrites to %q (%v), want /data/media/movies", got, ok)
	}
	// The download tree keeps its names: nothing under it is renamed.
	if got, ok := p.Rewrite("qbittorrent", "/downloads/Movies"); !ok || got != "/data/downloads/Movies" {
		t.Errorf("qbittorrent: /downloads/Movies rewrites to %q (%v), want /data/downloads/Movies", got, ok)
	}
}

func TestDiscoverRecordsTheDownloadCategory(t *testing.T) {
	f := arrInstall(t).withCategories()
	p, _ := f.discover(t)
	for name, want := range map[string]string{
		"sonarr": "TV", "sonarr-4k": "4K/TV", "radarr": "Movies", "radarr-4k": "4K/Movies",
	} {
		if got := p.Apps[name].DownloadCategory; got != want {
			t.Errorf("%s: downloadCategory %q, want %q", name, got, want)
		}
	}
	// qBittorrent and Plex keep no download category: the first downloads, it
	// does not collect.
	for _, name := range []string{"qbittorrent", "plex"} {
		if got := p.Apps[name].DownloadCategory; got != "" {
			t.Errorf("%s: downloadCategory %q, want none", name, got)
		}
	}
}

func TestDiscoverIgnoresAClientItDoesNotDial(t *testing.T) {
	f := arrInstall(t).withCategories()
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDBWith(t, []string{"/library/TV"}, []clientRow{
		{implementation: "QBittorrent", enabled: false, category: "Old"},
		{implementation: "Transmission", enabled: true, category: "Torrents"},
		{implementation: "QBittorrent", enabled: true, category: "TV"},
	}))
	p, _ := f.discover(t)
	if got := p.Apps["sonarr"].DownloadCategory; got != "TV" {
		t.Errorf("downloadCategory %q, want TV: the enabled qBittorrent client is the one that downloads", got)
	}
}

func TestDiscoverChoosesBetweenTwoRootFoldersWithAReason(t *testing.T) {
	f := arrInstall(t).withCategories()
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDBWith(t, []string{"/library/Movies", "/library/Old"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "Movies"},
	}))
	p, rep := f.discover(t)
	var got []plan.MediaFolder
	for _, folder := range p.Data.Folders {
		if folder.To == "movies" {
			got = append(got, folder)
		}
	}
	if len(got) != 1 || got[0].From != "Movies" || !got[0].Default {
		t.Fatalf("the two root folders settled as %#v, want one default entry for Movies", got)
	}
	for _, want := range []string{"Movies", "Old"} {
		if !strings.Contains(got[0].Why, want) {
			t.Errorf("the default's reason does not name %s: %q", want, got[0].Why)
		}
	}
	if !mentions(rep.Notes, "two root folders") {
		t.Errorf("the notes do not say an Arr keeps two root folders: %#v", rep.Notes)
	}
}

func TestDiscoverLeavesARootFolderOutsideTheLibrary(t *testing.T) {
	f := arrInstall(t).withCategories()
	f.app(t, appSpec{
		name: "prowlarr", image: "lscr.io/linuxserver/prowlarr:2.7.0", port: 9696,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/prowlarr"},
		},
	})
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDBWith(t, []string{"/other/Movies"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "Movies"},
	}))
	p, rep := f.discover(t)
	for _, folder := range p.Data.Folders {
		if folder.To == "movies" {
			t.Fatalf("a root folder outside the library was mapped: %#v", folder)
		}
	}
	if !mentions(rep.Notes, "/other/Movies") {
		t.Errorf("the notes do not name the root folder left alone: %#v", rep.Notes)
	}
}

func TestDiscoverMapsABoxNamedLibraryToItself(t *testing.T) {
	f := arrInstall(t).withCategories()
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDBWith(t, []string{"/library/movies"}, []clientRow{
		{implementation: "QBittorrent", enabled: true, category: "Movies"},
	}))
	p, _ := f.discover(t)
	var got *plan.MediaFolder
	for i := range p.Data.Folders {
		if p.Data.Folders[i].To == "movies" {
			got = &p.Data.Folders[i]
		}
	}
	if got == nil || got.From != "movies" {
		t.Fatalf("a box-named library mapped as %#v, want movies -> movies", got)
	}
	if renames := p.Data.MediaRenames(); len(renames) != 3 {
		t.Errorf("MediaRenames() = %v, want the three the source named otherwise", renames)
	}
}

func TestDiscoverWithoutChartCategoriesKeepsTheLibrary(t *testing.T) {
	f := arrInstall(t)
	p, rep := f.discover(t)
	if len(p.Data.Folders) != 0 {
		t.Fatalf("a read with no categories mapped %#v", p.Data.Folders)
	}
	if !mentions(rep.Notes, "no chart categories") {
		t.Errorf("the notes do not say the read had no categories: %#v", rep.Notes)
	}
}

func mentions(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// clientRow is one row of an Arr's DownloadClients table, as the database
// stores it: the implementation, whether the app dials it, and the category its
// settings name.
type clientRow struct {
	implementation string
	enabled        bool
	category       string
}
