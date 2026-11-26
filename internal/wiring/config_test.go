package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wiring config is the chart's render, naming only the objects the Job
// reconciles. The decode is strict, so a key the Job does not act on is a loud
// failure rather than one quietly ignored.
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

// qui and autobrr own their user tables, so the Job signs in and registers their
// connections; a render that leaves a field out names the app.
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

	// One app's password file missing names the app and the field.
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

// The qBittorrent block carries a URL and nothing else: the Job has no password to
// read, and an unknown key is refused by name.
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

// A release with no wired app is valid: a seedbox can be installed with apps off,
// and the Job then has nothing per-app to do.
func TestConfigAcceptsAReleaseWithNoWiredApp(t *testing.T) {
	for name, cfg := range map[string]string{
		// The box `box migrate apply` adopts into, with its apps still off.
		"prowlarr only": `{"namespace":"seedbox","configMap":"s5n-wiring","timeoutSeconds":300,"instances":[],` +
			`"prowlarr":{"url":"http://prowlarr:9696","urlBase":"/prowlarr","selfUrl":"http://prowlarr:9696/prowlarr",` +
			`"apiKeyFile":"/etc/s5n/keys/prowlarr-apikey"}}`,
		// Every app off: the receipt is all the Job has to publish.
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

// Maintainerr's Plex connection is a row in its own settings, and its BASE_PATH
// covers every route -- API and health alike.
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

	// Without Plex there is no server to point it at.
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
