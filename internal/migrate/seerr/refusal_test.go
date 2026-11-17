package seerr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// An Arr entry's activeDirectory is a root folder in the Arr's own filesystem:
// Seerr never opens it, it hands it to the Arr as rootFolderPath
// (server/api/servarr/radarr.ts:166,234). So it moves by the Arr's path rules,
// and Seerr's own table, which only ever covers Seerr's mounts, is not asked.
const arrPathsPlan = `
version: 1
apps:
  plex:
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
  tautulli:
    source: tautulli
    config: /srv/apps/tautulli
    ports: [8181]
    paths: []
  sonarr:
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths:
      - from: /library/media
        to: /data/media
  radarr:
    source: radarr
    config: /srv/apps/radarr
    ports: [7878]
    paths:
      - from: /library/media
        to: /data/media
  seerr:
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
