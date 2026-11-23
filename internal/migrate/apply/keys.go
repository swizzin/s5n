package apply

// keys.go is step 8: every migrated app keeps the API key it has today, and the
// Plex server token goes into the release Secret so a migrated box knows it.
//
// The chart hands an Arr its key through the environment, from the release
// Secret (charts/s5n/templates/workloads.yaml, `<PREFIX>__AUTH__APIKEY`), and the
// environment wins over the copied config file. A release that generated its own
// key would therefore leave the app answering a key nobody's other tools know:
// every API client the operator configured against the source stops working at
// the moment the box takes over. So the source's key is read out of the copied
// config and written into the release Secret, which is where the chart reads it
// from. The copied config is never edited: the app's own file keeps the key it
// had, and the two agree.
//
// The release Secret keeps a key it already holds across upgrades (`lookup`,
// charts/s5n/templates/secret.yaml:9), so a key written before the apply that
// switches the instance on is the key the app starts with.
//
// A key that cannot be read is a refusal naming the instance and the file. A
// silent new key is exactly the failure this step exists to prevent, and a box
// that cannot tell what the key was has nothing better to offer than to say so.
//
// The Plex server token is the one credential that is not an app's own key and
// is not left out: it is the account token the migrated server is signed in
// with (Preferences.xml's PlexOnlineToken), it lives in no release Secret until
// a person supplies one, and every Plex-dependent row on the receipt (Tautulli's
// PMS link, Maintainerr's connection) is pending without it. So a migration
// reads it out of the Plex it is copying and seeds the Secret's plex-token with
// it -- never over one already there, because an existing key is either the
// person's own or one this step wrote on an earlier run of the same plan.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plex"
)

// KeyStore is the release Secret, as one data key at a time.
//
// The implementation reads and updates the Secret box's chart renders
// (`<release>-keys`, or the one `secrets.existingSecret` names), and it decodes
// on the way in and encodes on the way out, so this package only ever handles
// the value. Nothing here prints one.
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

// apiKeys is every app whose key the chart hands it from the release Secret and
// whose own config file carries it.
//
// The Arrs and Prowlarr hold theirs in config.xml, which every one of their
// images writes. Tautulli holds one in config.ini, and it is the key its API
// clients use, so it is kept the same way; the reader is here rather than in
// internal/migrate/tautulli because that package rewrites paths and has no
// reason to learn one more key.
//
// An app that is not here has no key of the source's to keep: qBittorrent's
// password is a PBKDF2 hash in its settings file, and the release's WebUI skips
// its login for the box's own addresses; qui's, autobrr's and PeerBanHelper's
// secrets are the chart's own automation account, not the source's; plex-token
// and plex-claim-token are the Plex account's, and no app config holds them —
// except Plex's own, which is readPlexToken's job rather than this table's,
// because a Plex config directory is not a file this package scans.
var apiKeys = map[string]apiKey{
	appSonarr:   {file: configXML, read: xmlAPIKey},
	appRadarr:   {file: configXML, read: xmlAPIKey},
	appProwlarr: {file: configXML, read: xmlAPIKey},
	appTautulli: {file: configINI, read: iniAPIKey},
}

// The two config files a kept key lives in.
const (
	configXML = "config.xml"
	configINI = "config.ini"
)

// secretKey is the release Secret's key for one instance's API key. It is the
// name the chart renders (`<instance>-apikey`, secret.yaml:24) for the Arrs and
// for Tautulli alike.
func secretKey(instance string) string { return instance + "-apikey" }

// plexTokenKey is the release Secret's `plex-token`: the Plex server token the
// wiring Job hands Tautulli and Maintainerr (charts/s5n/templates/secret.yaml).
// It is optional for the chart and is never generated, so on a plain box it
// exists only when a person supplied one.
const plexTokenKey = "plex-token"

// keyed reports whether an app's key has to be kept, and where it lives.
func keyed(app string) (apiKey, bool) {
	k, ok := apiKeys[app]
	return k, ok
}

// readKeys reads every kept key out of the staged copy of each instance's
// config, before anything moves: a key that cannot be read is a refusal while
// the source is still the only thing on the host that has one.
//
// Every instance is checked and every problem is reported, like the rest of the
// run: a config tree with three Arrs and no key in one of them is one edit.
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

// readPlexToken reads the Plex server token out of the staged Plex config and
// queues it for the release Secret. It returns one problem line for the refusal
// (and queues nothing), or "" -- including when the box is not migrating a Plex
// at all, or the Plex it is copying was never signed in.
//
// The token comes from the server's own Preferences.xml, which is the same file
// the rewriter edits: a Plex that was claimed carries the account's token there,
// and a Plex that was not carries an empty one, which is exactly the state the
// wiring Job reports pending for.
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
	token, ok, err := plex.ServerToken(m.staging(plexApp))
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
		// A person's own token wins. The Secret's plex-token is the one
		// credential here that a box can be given before a migration (the
		// documented way to patch one in), so it is never written over.
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

// keptKey is one credential read out of a source config, waiting for its Secret
// write. The value never leaves this struct except into the Secret: no summary,
// no error and no log line carries it.
type keptKey struct {
	instance string
	name     string
	value    string
	// what is what the value is, as the confirm's row names it ("API key",
	// "Plex token"). It is a fact about the credential, never about its value.
	what string
	// ifAbsent makes the write skip a key the release Secret already holds. Only
	// the Plex token is written this way: the others are the apps' own keys, and
	// the source's is authoritative for them.
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
			// Never over a credential the release Secret already holds: the
			// Plex token is the account's, and a person may have supplied one
			// before this run. Nothing about the value is read out here.
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
	// The values have been written into a cluster object; there is no reason
	// for this run to hold them a moment longer.
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

// xmlAPIKey reads an Arr's `<ApiKey>` out of config.xml.
//
// The file is a flat list of elements (an Arr writes it with its own small
// writer), and this is the same element scan the Arrs' rewriter does for the
// elements it edits: a full XML parse would only add a reformatting risk to a
// file another package is about to rewrite.
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

// iniAPIKey reads `api_key` out of Tautulli's config.ini.
//
// The key sits in [General] and Tautulli's ConfigObj reads it as one scalar; a
// scan for the key is enough because the file is the app's own flat INI and the
// name appears once. A later section's key of the same name would be a second
// setting, which Tautulli does not have.
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
