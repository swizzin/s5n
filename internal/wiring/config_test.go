package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wiring config is the chart's render, and it names only the objects the
// Job reconciles: qBittorrent, Prowlarr, Plex, Tautulli, Seerr, qui, autobrr
// and one entry per enabled Arr instance. A catalog app that needs no wiring is
// simply absent from it, which is what makes adding one a chart-only change.
//
// The decode is strict, so a chart that started writing an app the Job does
// not know is a loud failure instead of a key quietly ignored. This pins that:
// maintainerr is in the catalog and is not a wiring key.
func TestConfigRefusesAnAppItDoesNotWire(t *testing.T) {
	for _, app := range []string{"maintainerr"} {
		t.Run(app, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wiring.json")
			cfg := `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,` +
				`"instances":[],` +
				`"qbittorrent":{"url":"http://qbittorrent:8080"},` +
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

// qui and autobrr are wiring keys now: both apps own their user table, so the
// Job signs in and registers their connections. A render that leaves a field
// out is an error naming the app, not a half-configured client.
func TestConfigAcceptsQuiAndAutobrr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wiring.json")
	cfg := `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,` +
		`"instances":[],` +
		`"qbittorrent":{"url":"http://qbittorrent:8080"},` +
		`"qui":{"url":"http://qui:7476","urlBase":"/qui","username":"s5n","passwordFile":"/etc/s5n/keys/qui-password"},` +
		`"autobrr":{"url":"http://autobrr:7474","urlBase":"/autobrr","username":"s5n","passwordFile":"/etc/s5n/keys/autobrr-password"}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("a render with both apps was refused: %v", err)
	}
	if c.Qui == nil || c.Autobrr == nil {
		t.Fatalf("qui = %+v, autobrr = %+v", c.Qui, c.Autobrr)
	}

	// The same render with one app's password file missing: the failure names
	// the app and the field, so a chart change is obvious in the Job's log.
	broken := strings.Replace(cfg, `"passwordFile":"/etc/s5n/keys/qui-password"`, `"passwordFile":""`, 1)
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a qui block with no password file loaded")
	} else if !strings.Contains(err.Error(), "qui: passwordFile is empty") {
		t.Fatalf("error = %v, want it to name qui's passwordFile", err)
	}
}

// The qBittorrent block carries a URL and nothing else. A render that still
// passes a credential is refused by name rather than silently ignored: the Job
// has no password to read, and qBittorrent's own login is bypassed for the
// release's addresses.
func TestConfigRefusesAQbitCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wiring.json")
	cfg := `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,"instances":[],` +
		`"qbittorrent":{"url":"http://qbittorrent:8080","passwordFile":"/etc/s5n/keys/qbittorrent-password"}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a qBittorrent block with a password file loaded")
	} else if !strings.Contains(err.Error(), `unknown field "passwordFile"`) {
		t.Fatalf("error = %v, want an unknown-field refusal naming passwordFile", err)
	}
}
