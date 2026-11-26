package apply

// keys.go is step 8: every app keeps the API key it has today, and the Plex server
// token is seeded into the release Secret. The chart hands the key through the
// environment from the Secret (`<PREFIX>__AUTH__APIKEY`), which wins over the copied
// config, so a generated key would stop every client configured against the
// source; the source's is read and written to the Secret, the config is never
// edited, and the Secret keeps a key it holds across upgrades (secret.yaml:9). An
// unreadable key is a refusal naming file and instance. The Plex token
// (Preferences.xml's PlexOnlineToken) is the one credential no app config owns and
// every Plex-dependent row needs; it is seeded as plex-token, never over one a
// person supplied.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plex"
)

// KeyStore is the release Secret, one data key at a time; nothing here prints a
// value.
type KeyStore interface {
	// Get returns one data key's value, "" when the key is absent.
	Get(ctx context.Context, key string) (string, error)
	// Set writes one data key's value.
	Set(ctx context.Context, key, value string) error
}

// apiKey is one app's key as its own config holds it: the file inside the
// app's config directory, and how to read it out.
type apiKey struct {
	file string
	read func(raw []byte) (string, bool, error)
}

// apiKeys is every app whose key the chart takes from the release Secret. An app
// absent here has no key of the source's to keep: qBittorrent's is a PBKDF2 hash,
// qui/autobrr/PeerBanHelper use the chart's own account, and Plex's token is
// readPlexToken's job.
var apiKeys = map[string]apiKey{
	appSonarr:   {file: configXML, read: xmlAPIKey},
	appRadarr:   {file: configXML, read: xmlAPIKey},
	appLidarr:   {file: configXML, read: xmlAPIKey},
	appProwlarr: {file: configXML, read: xmlAPIKey},
	appTautulli: {file: configINI, read: iniAPIKey},
	// SABnzbd's key is api_key in [misc], the one the app answers to and the
	appSabnzbd: {file: "sabnzbd.ini", read: iniAPIKey},
}

// The two config files a kept key lives in.
const (
	configXML = "config.xml"
	configINI = "config.ini"
)

// secretKey is the chart's key name (`<instance>-apikey`, secret.yaml:24).
func secretKey(instance string) string { return instance + "-apikey" }

// plexTokenKey is `plex-token`, which wiring hands Tautulli and Maintainerr; the
// chart never generates it.
const plexTokenKey = "plex-token"

// keyed reports whether an app's key has to be kept, and where it lives.
func keyed(app string) (apiKey, bool) {
	k, ok := apiKeys[app]
	return k, ok
}

// readKeys reads every kept key before anything moves, reporting every problem at
// once: one unreadable key is a refusal while the source still has it.
func (m *migration) readKeys() error {
	if m.st.Secret {
		// An earlier run of this plan already kept them.
		return nil
	}
	var items []string
	names := make([]string, 0, len(m.p.Apps))
	for _, name := range planApps(m.p) {
		app := CatalogApp(name, m.env.Catalog)
		src, ok := keyed(app)
		if !ok {
			continue
		}
		path := filepath.Join(m.staging(name), src.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			items = append(items, fmt.Sprintf("apps.%s: its API key is in %s, which cannot be read (%v); the key it has today has to be kept", name, src.file, err))
			continue
		}
		value, ok, err := src.read(raw)
		if err != nil {
			items = append(items, fmt.Sprintf("apps.%s: %s: %v", name, src.file, err))
			continue
		}
		if !ok || value == "" {
			items = append(items, fmt.Sprintf("apps.%s: %s holds no API key; the app writes one on its first start, so start the source once before migrating it", name, src.file))
			continue
		}
		m.keys = append(m.keys, keptKey{instance: name, name: secretKey(name), value: value, what: "API key"})
		names = append(names, secretKey(name))
	}
	if item := m.readPlexToken(); item != "" {
		items = append(items, item)
	} else if k, ok := m.plexToken(); ok {
		names = append(names, k.name)
	}
	if len(items) > 0 {
		return &problems{lead: "the credentials these apps have today cannot all be kept", items: items}
	}
	sort.Strings(names)
	m.keyNames = names
	return nil
}

// readPlexToken queues the snapshot Plex's token for the Secret, or one problem
// line; "" also when there is no Plex or it was never signed in.
func (m *migration) readPlexToken() string {
	var plexApp string
	for _, name := range planApps(m.p) {
		if CatalogApp(name, m.env.Catalog) == appPlex {
			plexApp = name
			break
		}
	}
	if plexApp == "" {
		return ""
	}
	// The snapshot's copy, not the staged one: the pre-install pass stages nothing
	// of a full carry and must promise the token the run writes.
	token, ok, err := plex.ServerToken(filepath.Join(m.env.Configs, plexApp))
	if err != nil {
		return fmt.Sprintf("apps.%s: its Plex server token cannot be read (%v); a token it has today has to be kept", plexApp, err)
	}
	if !ok {
		return ""
	}
	m.keys = append(m.keys, keptKey{
		instance: plexApp,
		name:     plexTokenKey,
		value:    token,
		what:     "Plex token",
		// A person's own token wins: it is the one credential a box can be given
		// before a migration.
		ifAbsent: true,
	})
	return ""
}

// plexToken is the queued Plex server token, if this run read one.
func (m *migration) plexToken() (keptKey, bool) {
	for _, k := range m.keys {
		if k.name == plexTokenKey {
			return k, true
		}
	}
	return keptKey{}, false
}

// keptKey is one credential waiting for its Secret write; the value never leaves
// it except into the Secret.
type keptKey struct {
	instance string
	name     string
	value    string
	// what is what the value is, as the confirm's row names it ("API key",
	// "Plex token"). It is a fact about the credential, never about its value.
	what string
	// ifAbsent skips a key the Secret holds; only the Plex token.
	ifAbsent bool
}

// keepKeys is step 8: each key the source held is written into the release
// Secret, which is where the chart reads the app's key from.
func (m *migration) keepKeys(ctx context.Context) error {
	if m.st.Secret {
		return nil
	}
	if len(m.keys) > 0 && m.env.Secret == nil {
		return fmt.Errorf("this run has no release Secret to keep %d %s in, and an app whose credential the chart supplies answers with one its own clients no longer know",
			len(m.keys), plural(len(m.keys), "credential", "credentials"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, k := range m.keys {
		if k.ifAbsent {
			// Never over a held credential: a person may have supplied the Plex
			// token already.
			existing, err := m.env.Secret.Get(ctx, k.name)
			if err != nil {
				return fmt.Errorf("read the release Secret's %s before keeping %s's %s: %w", k.name, k.instance, k.what, err)
			}
			if strings.TrimSpace(existing) != "" {
				m.res.Notes = append(m.res.Notes, fmt.Sprintf("%s: the release Secret already holds a %s, so %s's was not written over", k.instance, k.name, k.instance))
				continue
			}
		}
		if err := m.env.Secret.Set(ctx, k.name, k.value); err != nil {
			// The error is wrapped without the value: a Secret that refused the
			// write says nothing about what it was asked to hold.
			return fmt.Errorf("keep %s's %s in the release Secret (key %s): %w", k.instance, k.what, k.name, err)
		}
		m.res.Keys = append(m.res.Keys, k.name)
	}
	m.st.Secret = true
	if err := saveState(m.dir, m.st); err != nil {
		return err
	}
	// Written into the cluster; the run need not hold them.
	for i := range m.keys {
		m.keys[i].value = ""
	}
	return nil
}

// plural renders "1 key" and "3 keys".
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// xmlAPIKey reads `<ApiKey>` from config.xml with the same element scan the
// rewriter uses; a full parse would add a reformatting risk.
func xmlAPIKey(raw []byte) (string, bool, error) {
	s := string(raw)
	const open, closing = "<ApiKey>", "</ApiKey>"
	i := strings.Index(s, open)
	if i < 0 {
		return "", false, nil
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return "", false, fmt.Errorf("%s is not closed", open)
	}
	return strings.TrimSpace(rest[:j]), true, nil
}

// iniAPIKey reads `api_key` from a flat INI, where the name appears once.
func iniAPIKey(raw []byte) (string, bool, error) {
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "api_key") {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`), true, nil
	}
	return "", false, nil
}
