package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wiring config is the chart's render, and it names only the objects the
// Job reconciles: qBittorrent, Prowlarr, Plex, Tautulli, Seerr, Maintainerr,
// qui, autobrr, PeerBanHelper and one entry per enabled Arr instance.
//
// The decode is strict, so a chart that started writing a key the Job does not
// act on is a loud failure instead of a key quietly ignored -- a typo, or an app
// added to the chart a release before the Job learns to reconcile it.
func TestConfigRefusesAKeyItDoesNotReconcile(t *testing.T) {
	for _, app := range []string{"exampleapp"} {
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

// A release with no wired app is a shape a seedbox is installed in, not a bad
// render: `box init --apps` turns every Arr and qBittorrent off, and `box
// migrate apply` adopts into that box and switches them on itself. The Job has
// nothing per-app to do there, and the config must load so it can say so.
func TestConfigAcceptsAReleaseWithNoWiredApp(t *testing.T) {
	for name, cfg := range map[string]string{
		// The box `box migrate apply` adopts into: Prowlarr is not part of a
		// plan, and none of the apps a plan names is on yet.
		"prowlarr only": `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,"instances":[],` +
			`"prowlarr":{"url":"http://prowlarr:9696","urlBase":"/prowlarr","selfUrl":"http://prowlarr:9696/prowlarr",` +
			`"apiKeyFile":"/etc/s5n/keys/prowlarr-apikey"}}`,
		// Every app off: nothing at all to reconcile, and the receipt is the
		// whole of what the Job has to publish.
		"nothing at all": `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,"instances":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wiring.json")
			if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err != nil {
				t.Fatalf("a release with no wired app was refused: %v", err)
			}
		})
	}
}

// Maintainerr is a wiring key: its Plex connection is a row in its own settings,
// and nothing else can write it. Its BASE_PATH is part of the config because the
// app mounts every route -- its API and its health route -- below the prefix.
func TestConfigAcceptsMaintainerr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wiring.json")
	cfg := `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,` +
		`"instances":[],` +
		`"plex":{"url":"http://plex:32400"},` +
		`"maintainerr":{"url":"http://maintainerr:6246","urlBase":"/maintainerr","plexTokenFile":"/etc/s5n/keys/plex-token"}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("a render with maintainerr was refused: %v", err)
	}
	if c.Maintainerr == nil || c.Maintainerr.URLBase != "/maintainerr" {
		t.Fatalf("maintainerr = %+v", c.Maintainerr)
	}

	// Without Plex there is no server to point it at, and a half-configured
	// connection is worse than a refusal.
	cfg = `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,` +
		`"instances":[],` +
		`"maintainerr":{"url":"http://maintainerr:6246","urlBase":"/maintainerr"}}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("maintainerr without plex was accepted")
	}
}
