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
// rejection from the app's auth mode not offering a password login at all
// (v1.91.0, where the switch that only hid the form started refusing the
// endpoint).
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

// loginUnavailableMode reads the auth mode out of autobrr's own refusal of a
// password login, or "" when the body says something else. The sentence is
// internal/http/auth.go's at v1.91.1: "password login is unavailable in %s
// authentication mode" in the API's {"message": ...} envelope.
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

// locateAutobrrClient finds the download client the Job owns. It is
// locateArrClient's rule for autobrr, which has the same facts to go on:
//
//   - a recorded id is authoritative: a rename in autobrr is the operator's
//     business and must never create a second client beside it;
//   - a recorded id that no longer exists (the operator deleted the row the Job
//     made) is not a create. A migrated box's client is adopted by the address
//     it dials, which is this box's own Arr. That is what repairs a box a
//     migration left holding both the source's clients and the Job's: delete
//     the Job's, run again, and the source's is adopted -- the row the
//     operator's own filters already refer to by id;
//   - with nothing recorded, the same adoption is what a first run does, so a
//     migrated client named "Sonarr4k" is not duplicated under the instance's
//     name. The name stays the fallback, for a box that ran wiring before it
//     recorded anything;
//   - only when there is nothing to adopt is a client created.
//
// autobrr allows two enabled clients of one type at one address, and nothing
// says which the operator means: the lowest id is adopted and the receipt says
// so, rather than a third being made.
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

// autobrrHostMatches returns the enabled clients of the spec's own type whose
// address is the one the spec names. autobrr runs several kinds of client, and
// an Arr client of the other Arr is another app: neither is the one this box
// feeds, and neither is a client autobrr ever dials when it is switched off.
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

// autobrrAddress is the address a client dials, as one string. autobrr folds
// the scheme and port into a qBittorrent client's host, and a migrated row can
// still have them apart (host "qbittorrent", port 8080, tls off) while the one
// this Job writes carries both in the host; the two are one address.
func autobrrAddress(item Item) string {
	host := strings.TrimSpace(item.Str(fieldHost))
	if host == "" {
		return ""
	}
	scheme, rest, ok := splitScheme(host)
	if !ok {
		// autobrr builds the scheme out of the switch, so a row whose host
		// carries none but has tls set dials a different address.
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

// normalizeDownloaderHost is a client's address the way two of them are
// compared: the scheme and the DNS name are case-blind, a trailing slash is not
// a different address, and the Arr's base path is compared as the app spells
// it.
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
