package seerr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// activeDirectory is a path in the Arr's filesystem, handed over as rootFolderPath
// (radarr.ts:166,234), so it moves by the Arr's rules, never Seerr's.
const arrPathsPlan = `
version: 2
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
  tautulli:
    kind: kubernetes
    source: tautulli
    config: /srv/apps/tautulli
    ports: [8181]
    paths: []
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths:
      - from: /library/media
        to: /data/media
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    ports: [7878]
    paths:
      - from: /library/media
        to: /data/media
  seerr:
    kind: kubernetes
    source: seerr
    config: /srv/apps/seerr
    paths:
      - from: /srv/media
        to: /data/media
hosts:
  - service: sonarr.default
    app: sonarr
`

func TestArrRootFolderMovesByTheArrsRules(t *testing.T) {
	ctx := context.Background()
	settings := strings.NewReplacer(
		`"/srv/media/Movies"`, `"/library/media/Movies"`,
		`"/srv/media/TV"`, `"/library/media/TV"`,
		`"/srv/media/Anime"`, `"/library/media/Anime"`,
	).Replace(settingsFixture)
	in := setup(t, settings)
	p, err := plan.Parse([]byte(arrPathsPlan))
	if err != nil {
		t.Fatal(err)
	}
	in.Plan = p
	// The database's folders are the Arrs' too: the same rule moves them.
	exec(t, in.Dir, `UPDATE override_rule SET rootFolder = '/library/media/Movies'`)
	exec(t, in.Dir, `UPDATE media_request SET rootFolder = '/library/media/Old Requests'`)

	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Uncovered) > 0 {
		t.Fatalf("an Arr's root folder was looked up in Seerr's own table: %v", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	f := readSettings(t, in.Dir)
	for ptr, want := range map[string]string{
		"/sonarr/0/activeDirectory":      "/data/media/TV",
		"/sonarr/0/activeAnimeDirectory": "/data/media/Anime",
		"/radarr/0/activeDirectory":      "/data/media/Movies",
	} {
		n, ok := f.find(ptr)
		if !ok || n.str != want {
			t.Errorf("%s = %q, want %q", ptr, n.str, want)
		}
	}
	// Both name radarr #0, so both move through Radarr's table.
	if got := readColumn(t, in.Dir, "override_rule", "rootFolder"); got != "/data/media/Movies" {
		t.Errorf("override_rule.rootFolder = %q, want the folder in Radarr's tree", got)
	}
	if got := readColumn(t, in.Dir, "media_request", "rootFolder"); got != "/data/media/Old Requests" {
		t.Errorf("media_request.rootFolder = %q, want the folder in Radarr's tree", got)
	}
}

func readSettings(t *testing.T, dir string) *jsonFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, settingsName))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestArrRootFolderFollowsTheLibrarysCategory: the folder handed over as
// rootFolderPath is the renamed one, /data/media/movies, not the source's Movies.
func TestArrRootFolderFollowsTheLibrarysCategory(t *testing.T) {
	ctx := context.Background()
	settings := strings.NewReplacer(
		`"/srv/media/Movies"`, `"/library/Movies"`,
		`"/srv/media/TV"`, `"/library/TV"`,
		`"/srv/media/Anime"`, `"/library/Anime"`,
	).Replace(settingsFixture)
	in := setup(t, settings)
	p, err := plan.Parse([]byte(`
version: 2
data:
  root: /srv/library
  downloads: Downloads
  media: library
  folders:
    - {from: Movies, to: movies}
    - {from: TV, to: tv}
apps:
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
  tautulli:
    kind: kubernetes
    source: tautulli
    config: /srv/apps/tautulli
    ports: [8181]
    paths: []
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    category: tv
    ports: [8989]
    paths:
      - from: /library/TV
        to: /data/media/tv
      - from: /library
        to: /data/media
  radarr:
    kind: kubernetes
    source: radarr
    config: /srv/apps/radarr
    category: movies
    ports: [7878]
    paths:
      - from: /library/Movies
        to: /data/media/movies
      - from: /library
        to: /data/media
  seerr:
    kind: kubernetes
    source: seerr
    config: /srv/apps/seerr
    paths:
      - from: /srv/media
        to: /data/media
hosts:
  - service: sonarr.default
    app: sonarr
`))
	if err != nil {
		t.Fatal(err)
	}
	in.Plan = p
	exec(t, in.Dir, `UPDATE override_rule SET rootFolder = '/library/Movies'`)
	exec(t, in.Dir, `UPDATE media_request SET rootFolder = '/library/Movies'`)
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	f := readSettings(t, in.Dir)
	for ptr, want := range map[string]string{
		"/sonarr/0/activeDirectory": "/data/media/tv",
		"/radarr/0/activeDirectory": "/data/media/movies",
	} {
		n, ok := f.find(ptr)
		if !ok || n.str != want {
			t.Errorf("%s = %q, want %q", ptr, n.str, want)
		}
	}
	if got := readColumn(t, in.Dir, "override_rule", "rootFolder"); got != "/data/media/movies" {
		t.Errorf("override_rule.rootFolder = %q, want the Arr's category", got)
	}
	if got := readColumn(t, in.Dir, "media_request", "rootFolder"); got != "/data/media/movies" {
		t.Errorf("media_request.rootFolder = %q, want the Arr's category", got)
	}
}
