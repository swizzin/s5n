package apps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// Autobrr is autobrr's HTTP API (v1.90.0). Its download clients live under
// /api/downloaders (internal/http/server.go), and a client's Arr details are
// its host plus settings.apikey: the host is used verbatim as the Arr's base
// URL, so it carries the Arr's own path prefix ("http://sonarr:8989/sonarr").
//
// Two of the API's behaviours shape the write path:
//
//   - PUT treats only the literal "<redacted>" as "keep the old secret"; an
//     absent or empty password and apikey overwrite what is stored with
//     nothing (internal/downloader/service.go's Update). So every update sends
//     the complete object, with the real secrets from the release Secret.
//   - POST accepts any number of clients with the same name, so a create has to
//     match first or it adds a second row beside the user's own.
type Autobrr struct {
	client   *Client
	urlBase  string
	username string
	password string
}

// NewAutobrr builds a client. urlBase is autobrr's own base URL ("/autobrr"),
// which the chart sets through AUTOBRR__BASE_URL; its API is under it.
//
// The session cookie is not Secure (internal/http/server.go sets it to false,
// unconditionally), so a cookie jar replays it over the plain http:// Service.
func NewAutobrr(baseURL, urlBase, username, password string) *Autobrr {
	jar, _ := cookiejar.New(nil)
	c := NewClient(baseURL, &http.Client{Timeout: 60 * time.Second, Jar: jar})
	return &Autobrr{client: c, urlBase: strings.TrimRight(urlBase, "/"), username: username, password: password}
}

func (a *Autobrr) api(path string) string { return a.urlBase + "/api" + path }

// Ping is the readiness probe. autobrr applies its base URL to every route,
// health included.
func (a *Autobrr) Ping(ctx context.Context) error {
	_, err := a.client.GetText(ctx, a.urlBase+"/api/healthz/readiness")
	return err
}

// WaitReady blocks until autobrr answers or timeout passes. A Ready pod proves
// less than qui's does: autobrr starts with an unreachable OIDC issuer and
// retries in the background, which changes nothing about the API this client
// uses, and its password login is routed either way.
func (a *Autobrr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, a.Ping)
}

// Login opens the session. autobrr answers 403 for bad credentials (not 401),
// so the status alone cannot classify the failure; the body is what separates a
// rejection from any other 4xx.
func (a *Autobrr) Login(ctx context.Context) error {
	var out Item
	err := a.client.Post(ctx, a.api("/auth/login"), map[string]string{
		fieldUsername: a.username,
		fieldPassword: a.password,
	}, &out)
	if err == nil {
		return nil
	}
	if he, ok := err.(*HTTPError); ok && (he.Status == http.StatusForbidden || he.Status == http.StatusUnauthorized) {
		return &LoginRejectedError{App: "autobrr", Username: a.username}
	}
	return err
}

func (a *Autobrr) Downloaders(ctx context.Context) ([]Item, error) {
	var out []Item
	if err := a.client.Get(ctx, a.api("/downloaders"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DownloaderSpec is one download client s5n registers: qBittorrent with a
// password, or an Arr with its API key.
type DownloaderSpec struct {
	Name     string
	Type     string // QBITTORRENT, SONARR or RADARR
	Host     string // the URL autobrr dials, Arr base path included
	Username string
	Password string
	APIKey   string
}

// autobrrDesired is the complete object an update must send: the live row with
// only the fields s5n owns overlaid. Everything else (enabled, throttling rules,
// basic auth, the Arr's external-client links) is carried through untouched,
// because autobrr's PUT replaces the whole row.
//
// port and tls are owned and pinned to 0/false: for qBittorrent autobrr folds
// them into the host itself (domain.Downloader.qbitBuildLegacyHost), and a
// leftover legacy port would be appended to a host that already carries one.
//
// The secrets are never compared — the API only ever returns a mask — so they
// are written unconditionally and the fingerprint in owned.json is what says
// whether they changed.
func autobrrDesired(live Item, spec DownloaderSpec) (Item, []FieldChange) {
	desired := live.Clone()
	if desired == nil {
		desired = Item{}
	}
	var drift []FieldChange
	set := func(name string, value any) {
		if cur, ok := live[name]; ok && !SameValue(cur, value) {
			drift = append(drift, FieldChange{Field: name, Old: formatValue(cur), New: formatValue(value)})
		}
		desired[name] = value
	}
	set("type", spec.Type)
	set(fieldHost, spec.Host)
	set(fieldPort, 0)
	set("tls", false)
	set(fieldUsername, spec.Username)
	desired[fieldPassword] = spec.Password

	// settings carries the Arr's API key, plus the throttling rules and any
	// auth the user set: only the key is s5n's, and it is never compared
	// because the API answers "<redacted>", not the value.
	settings := Item{}
	if raw, ok := desired["settings"].(map[string]any); ok {
		settings = Item(raw)
	}
	if spec.APIKey != "" {
		settings["apikey"] = spec.APIKey
	}
	desired["settings"] = map[string]any(settings)
	return desired, drift
}

// EnsureDownloader makes one client match the spec and proves it with autobrr's
// own test endpoint before writing anything, the way the Arr download clients
// are proved.
//
// ownedID and knownFingerprint are owned.json's record: the id keeps a rename
// working and a second same-named client from being created, and the
// fingerprint of the secret last written and proven is the only way a rotated
// Secret is visible (autobrr never returns the stored password or API key). A
// changed fingerprint is an update even when every readable field matches.
func (a *Autobrr) EnsureDownloader(ctx context.Context, spec DownloaderSpec, ownedID int, knownFingerprint string) (EnsureResult, error) {
	list, err := a.Downloaders(ctx)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("list autobrr downloaders: %w", err)
	}
	cur, found, recreated, previousID := locateOwned(list, ownedID, spec.Name)

	change := Created
	var desired Item
	var drift []FieldChange
	if found {
		desired, drift = autobrrDesired(cur, spec)
		desired["id"] = cur["id"]
		rotated := knownFingerprint != downloaderFingerprint(spec)
		if len(drift) > 0 || rotated {
			change = Updated
		} else {
			change = Unchanged
		}
	} else {
		settings := map[string]any{"rules": map[string]any{"enabled": false}}
		if spec.APIKey != "" {
			settings["apikey"] = spec.APIKey
		}
		desired = Item{
			"name":        spec.Name,
			"type":        spec.Type,
			"enabled":     true,
			fieldHost:     spec.Host,
			fieldUsername: spec.Username,
			fieldPassword: spec.Password,
			"settings":    settings,
		}
	}

	// Test before writing, with the id carried when there is one, so autobrr
	// dials the client as it would be saved. A failed test writes nothing.
	if err := a.client.Post(ctx, a.api("/downloaders/test"), desired, nil); err != nil {
		return EnsureResult{Change: change, ID: desired.Int("id")}, fmt.Errorf("test autobrr downloader %q: %w", spec.Name, err)
	}

	switch change {
	case Created:
		var out Item
		if err := a.client.Post(ctx, a.api("/downloaders"), desired, &out); err != nil {
			return EnsureResult{}, fmt.Errorf("create autobrr downloader %q: %w", spec.Name, err)
		}
		desired = out
	case Updated:
		if err := a.client.Put(ctx, a.api("/downloaders"), desired, nil); err != nil {
			return EnsureResult{}, fmt.Errorf("update autobrr downloader %q: %w", spec.Name, err)
		}
	}
	return EnsureResult{Change: change, ID: desired.Int("id"), Drift: drift, Recreated: recreated, PreviousID: previousID}, nil
}

// downloaderFingerprint is what owned.json records per client: the secret that
// can rotate under it. That is an Arr client's API key, the Arr's own key from
// the release Secret. qBittorrent's client has no secret at all now — its
// username and password are written empty, because qBittorrent skips its login
// for the release's addresses — so it answers "" and never counts as rotated.
func downloaderFingerprint(spec DownloaderSpec) string {
	if spec.APIKey != "" {
		return SecretFingerprint(spec.APIKey)
	}
	if spec.Password != "" {
		return SecretFingerprint(spec.Password)
	}
	return ""
}
