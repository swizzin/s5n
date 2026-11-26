package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Autobrr is autobrr's HTTP API (v1.90.0): clients under /api/downloaders
// (internal/http/server.go), a client's Arr details being its host plus
// settings.apikey, with the host used verbatim as the Arr's base URL.
//
// PUT treats only "<redacted>" as "keep the old secret", so every update sends the
// complete object with the real secrets (internal/downloader/service.go's Update).
// POST accepts any number of clients with the same name, so a create must match
// first or it adds a second row.
type Autobrr struct {
	client   *Client
	urlBase  string
	username string
	password string
}

// NewAutobrr builds a client. urlBase is its own base URL ("/autobrr"), set through
// AUTOBRR__BASE_URL. Its session cookie is not Secure (internal/http/server.go), so a
// jar replays it over plain http.
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

// WaitReady blocks until autobrr answers. It starts with an unreachable OIDC issuer
// and retries in the background, which does not affect this API.
func (a *Autobrr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, a.Ping)
}

// Login opens the session. autobrr answers 403 for bad credentials, so the body
// separates a rejection from an auth mode that does not offer password login
// (refused at the endpoint since v1.91.0).
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
		if mode := loginUnavailableMode(he.Body); mode != "" {
			return &LoginUnavailableError{App: "autobrr", Mode: mode}
		}
		return &LoginRejectedError{App: "autobrr", Username: a.username}
	}
	return err
}

// loginUnavailableMode reads the mode out of autobrr's refusal, or "" when the body
// says something else (internal/http/auth.go at v1.91.1).
func loginUnavailableMode(body string) string {
	var out struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &out) != nil {
		return ""
	}
	const prefix = "password login is unavailable in "
	const suffix = " authentication mode"
	msg := strings.TrimSpace(out.Message)
	if !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, suffix) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(msg, prefix), suffix)
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

// locateAutobrrClient is locateArrClient's rule for autobrr: a recorded id is
// authoritative; a stale one, or none, adopts the client at this box's own address
// (the row the operator's filters refer to), with the name as fallback; only when
// there is nothing to adopt is one created. Two at one address: the lowest id is
// adopted and the receipt says so.
func locateAutobrrClient(list []Item, spec DownloaderSpec, ownedID int) (cur Item, found, recreated bool, previousID int, note string) {
	if ownedID != 0 {
		for _, item := range list {
			if item.Int("id") == ownedID {
				return item, true, false, 0, ""
			}
		}
	}
	if same := autobrrHostMatches(list, spec); len(same) > 0 {
		best := same[0]
		for _, item := range same[1:] {
			if item.Int("id") < best.Int("id") {
				best = item
			}
		}
		return best, true, false, 0, adoptionNote(best, same, ownedID)
	}
	if ownedID != 0 {
		return nil, false, true, ownedID, ""
	}
	if item, ok := FindByName(list, spec.Name); ok {
		return item, true, false, 0, ""
	}
	return nil, false, false, 0, ""
}

// adoptionNote is the receipt's line for a client found by its address rather
// than by the id on record: which row box took, and why that one.
func adoptionNote(best Item, same []Item, ownedID int) string {
	id := best.Int("id")
	note := ""
	if len(same) > 1 {
		ids := make([]int, 0, len(same))
		for _, item := range same {
			ids = append(ids, item.Int("id"))
		}
		sort.Ints(ids)
		named := make([]string, len(ids))
		for i, n := range ids {
			named[i] = strconv.Itoa(n)
		}
		note = fmt.Sprintf("adopted id %d of %s", id, strings.Join(named, ", "))
	} else if ownedID != 0 {
		note = fmt.Sprintf("adopted id %d", id)
	}
	if note != "" && ownedID != 0 {
		note += fmt.Sprintf(" (recorded %d was gone)", ownedID)
	}
	return note
}

// autobrrHostMatches returns the enabled clients of the spec's type at its address:
// another type is another app, and a disabled one is never dialled.
func autobrrHostMatches(list []Item, spec DownloaderSpec) []Item {
	want := normalizeDownloaderHost(spec.Host)
	if want == "" {
		return nil
	}
	var out []Item
	for _, item := range list {
		if !strings.EqualFold(item.Str("type"), spec.Type) {
			continue
		}
		if on, ok := item["enabled"].(bool); ok && !on {
			continue
		}
		if normalizeDownloaderHost(autobrrAddress(item)) == want {
			out = append(out, item)
		}
	}
	return out
}

// The two schemes a stored download-client address can carry: autobrr dials
// plain HTTP inside the release, and https is a row an operator made by hand.
const (
	schemeHTTP  = "http://"
	schemeHTTPS = "https://"
)

// autobrrAddress is the address a client dials: autobrr folds scheme and port into a
// qBittorrent host, and a migrated row can still carry them apart.
func autobrrAddress(item Item) string {
	host := strings.TrimSpace(item.Str(fieldHost))
	if host == "" {
		return ""
	}
	scheme, rest, ok := splitScheme(host)
	if !ok {
		// autobrr builds the scheme from the switch: a host without one and tls
		// set is a different address.
		scheme = schemeHTTP
		if item.Bool("tls") {
			scheme = schemeHTTPS
		}
	}
	head, path := rest, ""
	if i := strings.Index(rest, "/"); i >= 0 {
		head, path = rest[:i], rest[i:]
	}
	// A port already written into the host, or an IPv6 literal's own colons,
	// mean the stored port is not part of this address.
	if p := item.Int("port"); p != 0 && !strings.Contains(head, ":") {
		head += ":" + strconv.Itoa(p)
	}
	return scheme + head + path
}

// normalizeDownloaderHost compares addresses: scheme and DNS name case-blind, no
// trailing slash, the base path as stored.
func normalizeDownloaderHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	scheme, rest, ok := splitScheme(s)
	if !ok {
		scheme = schemeHTTP
	}
	rest = strings.TrimRight(rest, "/")
	host, path := rest, ""
	if i := strings.Index(rest, "/"); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	return strings.ToLower(scheme) + strings.ToLower(host) + path
}

// splitScheme separates a stored address into its scheme, "//" included, and
// the rest: the scheme is case-blind, and an address without one is the case
// the caller supplies a default for.
func splitScheme(raw string) (scheme, rest string, ok bool) {
	lower := strings.ToLower(raw)
	for _, s := range []string{schemeHTTPS, schemeHTTP} {
		if strings.HasPrefix(lower, s) {
			return s, raw[len(s):], true
		}
	}
	return "", raw, false
}

// autobrrDesired is the complete object an update sends: the live row with the owned
// fields overlaid. PUT replaces the whole row, so everything else is carried through.
//
// port and tls are pinned to 0/false: qBittorrent folds them into the host
// (domain.Downloader.qbitBuildLegacyHost), and a legacy port would be appended twice.
// Secrets are written unconditionally; the fingerprint in owned.json says whether they
// changed.
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

// EnsureDownloader makes one client match the spec and proves it with autobrr's test
// before writing.
//
// The id keeps a rename working, and the fingerprint is the only way a rotated Secret
// is visible: a changed one is an update even when every readable field matches.
func (a *Autobrr) EnsureDownloader(ctx context.Context, spec DownloaderSpec, ownedID int, knownFingerprint string) (EnsureResult, error) {
	list, err := a.Downloaders(ctx)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("list autobrr downloaders: %w", err)
	}
	cur, found, recreated, previousID, note := locateAutobrrClient(list, spec, ownedID)

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
	return EnsureResult{Change: change, ID: desired.Int("id"), Drift: drift, Recreated: recreated, PreviousID: previousID, Note: note}, nil
}

// downloaderFingerprint is the secret that can rotate under a client: an Arr's key.
// qBittorrent's client has none (its login is bypassed) and answers "".
func downloaderFingerprint(spec DownloaderSpec) string {
	if spec.APIKey != "" {
		return SecretFingerprint(spec.APIKey)
	}
	if spec.Password != "" {
		return SecretFingerprint(spec.Password)
	}
	return ""
}
