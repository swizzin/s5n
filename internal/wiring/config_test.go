package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wiring config is the chart's render, and it names only the objects the
// Job reconciles: qBittorrent, Prowlarr, Plex, Tautulli, Seerr and one entry
// per enabled Arr instance. A catalog app that needs no wiring is simply
// absent from it, which is what makes adding one a chart-only change.
//
// The decode is strict, so a chart that started writing an app the Job does
// not know is a loud failure instead of a key quietly ignored. This pins that:
// the app names below are the catalog's, and none of them is a wiring key.
func TestConfigRefusesAnAppItDoesNotWire(t *testing.T) {
	for _, app := range []string{"autobrr", "qui", "maintainerr"} {
		t.Run(app, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wiring.json")
			cfg := `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,` +
				`"instances":[],` +
				`"qbittorrent":{"url":"http://qbittorrent:8080","username":"admin","passwordFile":"/etc/s5n/keys/qbittorrent-password"},` +
				`"` + app + `":{"url":"http://` + app + `:1234"}}`
			if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("a config naming %q loaded: the Job would ignore a key the chart meant it to act on", app)
			} else if !strings.Contains(err.Error(), `unknown field "`+app+`"`) {
				t.Fatalf("error = %v, want an unknown-field refusal naming %q", err, app)
			}
		})
	}
}
