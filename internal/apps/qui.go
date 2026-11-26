package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Qui is qui's WebUI API (v1.35.0). Route and payload shapes are the app's own:
// instances live under /api/instances (internal/api/server.go), a create
// answers 201 before it tests the connection, and a PUT without a password
// keeps the stored one (internal/api/handlers/instances.go).
//
// Two things shape this client beyond the shared Client:
//
//   - The session cookie is Secure whenever the OIDC redirect URL is https
//     (internal/domain/config.go's SecureSessionCookie, and every s5n box sets
//     one), so a cookie jar will not replay it over the plain http:// Service
//     the Job talks to. Login therefore captures Set-Cookie itself and every
//     request carries the raw name=value as a Cookie header.
//   - A cookie-authenticated write needs X-Requested-With
//     (internal/api/middleware/auth.go), which is set on every request here.
type Qui struct {
	client   *Client
	urlBase  string
	username string
	password string
	session  string
}

// NewQui builds a client. urlBase is qui's own base URL ("/qui"), which the
// chart sets through QUI__BASE_URL; its API is under it.
func NewQui(baseURL, urlBase, username, password string) *Qui {
	c := NewClient(baseURL, nil)
	c.Header.Set("X-Requested-With", "XMLHttpRequest")
	return &Qui{client: c, urlBase: strings.TrimRight(urlBase, "/"), username: username, password: password}
}

func (q *Qui) api(path string) string { return q.urlBase + "/api" + path }

// Ping is the readiness probe. qui registers /healthz/readiness on its root
// router and only /api and the UI under its base URL, so the prefixed path
// (not /api/healthz/...) is the one that answers: the API-prefixed form is a
// 404 and the bare prefix is the SPA's catch-all.
func (q *Qui) Ping(ctx context.Context) error {
	_, err := q.client.GetText(ctx, q.urlBase+"/healthz/readiness")
	return err
}

// WaitReady blocks until qui answers or timeout passes. qui starts without its
// OIDC issuer (it discovers the provider per request), so a pass here says
// nothing about the login: the wiring Job's own sign-in is what proves that.
func (q *Qui) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, q.Ping)
}

// Login opens the session the instance calls ride on and keeps only the
// cookie's name=value. A wrong password is reported as a LoginRejectedError,
// never retried: a retry cannot fix it, and the app's own failure tracking
// would only add noise.
//
// qui with its login switched off (a plain helm install, or a grant from before
// the OIDC provider, where the chart sets QUI__AUTH_DISABLED) answers this
// endpoint 403 and needs no session at all -- its API is then open to anything
// inside the release, which the NetworkPolicy already limits to the Gateway and
// the wiring Job. That case is "no session", not a failure.
func (q *Qui) Login(ctx context.Context) error {
	payload, err := json.Marshal(map[string]string{fieldUsername: q.username, fieldPassword: q.password})
	if err != nil {
		return fmt.Errorf("encode qui login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.client.BaseURL+q.api("/auth/login"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
	case resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "authentication is disabled"):
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &LoginRejectedError{App: "qui", Username: q.username}
	default:
		return &HTTPError{Method: http.MethodPost, URL: req.URL.String(), Status: resp.StatusCode, Body: string(body)}
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name != "" && cookie.Value != "" {
			q.session = cookie.Name + "=" + cookie.Value
			q.client.Header.Set("Cookie", q.session)
			break
		}
	}
	return nil
}

func (q *Qui) instances(ctx context.Context) ([]Item, error) {
	var out []Item
	if err := q.client.Get(ctx, q.api("/instances/"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// QuiInstanceSpec is the one qBittorrent instance wiring registers.
type QuiInstanceSpec struct {
	Name                     string
	Host                     string
	Username                 string
	Password                 string
	HasLocalFilesystemAccess bool
}

// EnsureInstance makes qui's instance for s5n's qBittorrent match the spec and
// proves it with qui's own connection test.
//
// ownedID, when nonzero, is the id the Job recorded the last time it created or
// adopted this instance: only an id match counts as "ours", so a rename in
// qui's UI is the user's business and never creates a duplicate. A zero ownedID
// means nothing is recorded yet, so an existing instance at the spec's host is
// adopted instead -- the shape a migration leaves behind, and the reason this
// looks at the host before the name. qui has no uniqueness on name or host, so
// a blind create would add a second row beside it.
//
// An adopted instance keeps its name: it is the operator's, chosen in qui's own
// UI or brought over from their old install, and box has no reason to change it.
//
// On update the live object is cloned and only the data-contract fields are
// overlaid (name is left as the user renamed it only on a create: the API
// requires a name, and the live one is what goes back). Its username and
// password are written empty, like the Arrs' download clients: qBittorrent's
// login is bypassed for the release's addresses, so there is no credential to
// keep in step and none to fingerprint.
func (q *Qui) EnsureInstance(ctx context.Context, spec QuiInstanceSpec, ownedID int) (EnsureResult, error) {
	list, err := q.instances(ctx)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("list qui instances: %w", err)
	}
	cur, found, recreated, previousID, err := locateQui(list, spec, ownedID)
	if err != nil {
		return EnsureResult{}, err
	}

	fields := []ownedField{
		{fieldHost, spec.Host, false},
		{fieldUsername, spec.Username, false},
		{fieldPassword, spec.Password, true},
		{"hasLocalFilesystemAccess", spec.HasLocalFilesystemAccess, false},
	}

	change := Created
	var desired Item
	var drift []FieldChange
	if found {
		desired, drift = overlayOwned(cur, fields)
		desired["id"] = cur["id"]
		if len(drift) > 0 {
			change = Updated
		} else {
			change = Unchanged
		}
	}

	switch change {
	case Created:
		body := Item{
			"name":                     spec.Name,
			fieldHost:                  spec.Host,
			fieldUsername:              spec.Username,
			fieldPassword:              spec.Password,
			"hasLocalFilesystemAccess": spec.HasLocalFilesystemAccess,
		}
		var out Item
		if err := q.client.Post(ctx, q.api("/instances/"), body, &out); err != nil {
			return EnsureResult{}, fmt.Errorf("create qui instance %q: %w", spec.Name, err)
		}
		desired = out
	case Updated:
		if err := q.client.Put(ctx, q.api(fmt.Sprintf("/instances/%d", desired.Int("id"))), desired, nil); err != nil {
			return EnsureResult{}, fmt.Errorf("update qui instance %q: %w", spec.Name, err)
		}
	}

	id := desired.Int("id")
	// The create answers 201 before it tests the connection, so a 201 is not
	// evidence of anything: the app's own test endpoint is.
	if err := q.TestInstance(ctx, id); err != nil {
		return EnsureResult{Change: change, ID: id, Drift: drift, Recreated: recreated, PreviousID: previousID}, err
	}
	return EnsureResult{Change: change, ID: id, Drift: drift, Recreated: recreated, PreviousID: previousID}, nil
}

// locateQui finds the qBittorrent instance the Job owns:
//
//   - a recorded id is authoritative: a rename in qui's UI is the user's
//     business and must not create a second row, even when another instance
//     points at the same host;
//   - with nothing recorded, the instance this box would own is adopted before
//     one is created. On a migrated box there is no record yet (owned.json is
//     box's own file, and the migration did not write one), and the source's
//     qui usually had one instance under the operator's own name -- which the
//     migration's rewriter has already pointed at the release's qBittorrent.
//     The host is what makes two instances the same one for box, and the name
//     is the fallback for a box that ran wiring before it recorded anything;
//   - only when neither exists was it deleted, so the run creates one and says
//     which id disappeared.
//
// Two instances at the same host are a refusal rather than a pick: qui allows
// both (it has no uniqueness on name or host), and nothing says which history
// is the one to keep.
func locateQui(list []Item, spec QuiInstanceSpec, ownedID int) (cur Item, found, recreated bool, previousID int, err error) {
	if ownedID != 0 {
		for _, item := range list {
			if item.Int("id") == ownedID {
				return item, true, false, 0, nil
			}
		}
		return nil, false, true, ownedID, nil
	}
	switch same := quiHostMatches(list, spec.Host); len(same) {
	case 0:
	case 1:
		return same[0], true, false, 0, nil
	default:
		return nil, false, false, 0, fmt.Errorf("qui: %d instances point at %s (ids %d and %d); remove all but one, this box can own only one",
			len(same), spec.Host, same[0].Int("id"), same[1].Int("id"))
	}
	if item, ok := FindByName(list, spec.Name); ok {
		return item, true, false, 0, nil
	}
	return nil, false, false, 0, nil
}

// quiHostMatches returns the instances whose host is the one the spec names.
// The comparison is in qui's own stored form (normalizeQuiHost), because the
// list reports exactly what its store kept.
func quiHostMatches(list []Item, host string) []Item {
	want := normalizeQuiHost(host)
	if want == "" {
		return nil
	}
	var out []Item
	for _, item := range list {
		if normalizeQuiHost(item.Str(fieldHost)) == want {
			out = append(out, item)
		}
	}
	return out
}

// normalizeQuiHost is the form qui itself stores and reports a host in: its
// store trims the answer, adds http:// when the operator typed no scheme, and
// re-serialises the parsed URL (models.validateAndNormalizeHost at v1.35.0).
// A trailing slash is dropped on both sides: a base URL with one is the same
// qBittorrent as without, and qBittorrent appends its own API path either way.
func normalizeQuiHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.TrimRight(s, "/")
	}
	return strings.TrimRight(u.String(), "/")
}

// TestInstance asks qui to dial the instance now. It answers 200 with
// {"connected":false,"error":...} on a failure, so the status alone would pass a
// broken instance.
func (q *Qui) TestInstance(ctx context.Context, id int) error {
	var out struct {
		Connected bool   `json:"connected"`
		Message   string `json:"message"`
		Error     string `json:"error"`
	}
	if err := q.client.Post(ctx, q.api(fmt.Sprintf("/instances/%d/test", id)), nil, &out); err != nil {
		return fmt.Errorf("test qui instance %d: %w", id, err)
	}
	if !out.Connected {
		detail := out.Error
		if detail == "" {
			detail = out.Message
		}
		return fmt.Errorf("qui instance %d is not connected: %s", id, detail)
	}
	return nil
}

// QuiArrSpec is one Arr instance qui registers for ID lookups: a Sonarr or a
// Radarr on the box, dialled by qui's own pod.
type QuiArrSpec struct {
	Name    string
	Type    string // sonarr or radarr, the only two qui has (internal/models/arr_instance.go at v1.36.1)
	BaseURL string // the Arr's in-cluster URL, its own URL base included
	APIKey  string
}

// quiArrTimeoutSeconds is the timeout a created row carries: qui's own default
// (internal/api/handlers/arr.go at v1.36.1 fills in 15 when the request names
// none), written out so the row says what it means.
const quiArrTimeoutSeconds = 15

// ArrInstances lists qui's Arr instances (GET /api/arr/instances). The answer
// never carries a stored API key: the model marks it json:"-", so there is
// nothing to compare and nothing that could be written back.
func (q *Qui) ArrInstances(ctx context.Context) ([]Item, error) {
	var out []Item
	if err := q.client.Get(ctx, q.api("/arr/instances"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureArrInstance makes one Arr instance match the spec and proves it with
// qui's own test, which is an Arr API call made from qui's pod: it is the only
// thing that shows the release's NetworkPolicy admits qui to that Arr.
//
// ownedID and knownFingerprint are owned.json's record. The id keeps a rename in
// qui's UI from creating a second row; the fingerprint of the key last written
// and proven is the only way a rotation in the release Secret is visible, since
// qui never answers a stored key.
//
// Only the key is ever written to a row that already exists. A row box adopted
// is the operator's -- its name, its enabled flag, its priority and its timeout
// are what they chose, or what their old install had -- and a disabled one is
// left disabled: nothing dials it, so there is nothing to prove.
func (q *Qui) EnsureArrInstance(ctx context.Context, spec QuiArrSpec, ownedID int, knownFingerprint string) (EnsureResult, error) {
	list, err := q.ArrInstances(ctx)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("list qui Arr instances: %w", err)
	}
	cur, found, recreated, previousID, note := locateQuiArr(list, spec, ownedID)

	change := Created
	if found {
		change = Unchanged
		if knownFingerprint != SecretFingerprint(spec.APIKey) {
			change = Updated
		}
	}

	id := 0
	switch change {
	case Created:
		body := Item{
			fieldType: spec.Type, fieldName: spec.Name, "base_url": spec.BaseURL,
			"api_key": spec.APIKey, fieldEnabled: true, "priority": 0,
			"timeout_seconds": quiArrTimeoutSeconds,
		}
		var out Item
		if err := q.client.Post(ctx, q.api("/arr/instances"), body, &out); err != nil {
			return EnsureResult{}, fmt.Errorf("create qui Arr instance %q: %w", spec.Name, err)
		}
		id = out.Int("id")
	case Updated:
		id = cur.Int("id")
		if err := q.client.Put(ctx, q.api(fmt.Sprintf("/arr/instances/%d", id)), Item{"api_key": spec.APIKey}, nil); err != nil {
			return EnsureResult{}, fmt.Errorf("update qui Arr instance %q: %w", spec.Name, err)
		}
	default:
		id = cur.Int("id")
	}
	if id == 0 {
		return EnsureResult{}, fmt.Errorf("qui answered no id for its Arr instance %q", spec.Name)
	}

	disabled := found && !quiArrEnabled(cur)
	if disabled {
		if note == "" {
			note = "left disabled"
		} else {
			note += ", left disabled"
		}
	}
	result := EnsureResult{Change: change, ID: id, Recreated: recreated, PreviousID: previousID, Note: note}
	if disabled {
		return result, nil
	}
	if err := q.TestArrInstance(ctx, id); err != nil {
		return result, err
	}
	return result, nil
}

// locateQuiArr finds the Arr instance the Job owns:
//
//   - a recorded id is authoritative, so a rename in qui's UI never creates a
//     second row;
//   - with nothing recorded -- or a record whose row is gone -- the row at this
//     box's address is adopted. The table is UNIQUE on (type, base_url), so a
//     create there is refused 409, and a migrated box's row is already pointed
//     at the release by internal/migrate/qui. Any row counts, enabled or not:
//     the uniqueness is the reason, not the state. Two spellings of one address
//     (the index is on the interned text, so a differently-cased host is a
//     second row) are the lowest id, with the receipt naming both;
//   - there is deliberately no fallback by name. A row of the right name at
//     another address is another Arr -- the operator's remote one, say -- and
//     adopting it would point nothing at this box's Arr while leaving the
//     remote one holding a key from this release;
//   - only when neither exists was the recorded row deleted, so the run creates
//     one and says which id went.
func locateQuiArr(list []Item, spec QuiArrSpec, ownedID int) (cur Item, found, recreated bool, previousID int, note string) {
	if ownedID != 0 {
		for _, item := range list {
			if item.Int("id") == ownedID {
				return item, true, false, 0, ""
			}
		}
	}
	if same := quiArrURLMatches(list, spec); len(same) > 0 {
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
	return nil, false, false, 0, ""
}

// quiArrURLMatches returns the rows of the spec's own type whose base URL is the
// one the spec names.
func quiArrURLMatches(list []Item, spec QuiArrSpec) []Item {
	want := normalizeQuiArrBaseURL(spec.BaseURL)
	if want == "" {
		return nil
	}
	var out []Item
	for _, item := range list {
		if !strings.EqualFold(item.Str(fieldType), spec.Type) {
			continue
		}
		if normalizeQuiArrBaseURL(item.Str("base_url")) == want {
			out = append(out, item)
		}
	}
	return out
}

// normalizeQuiArrBaseURL is an Arr base URL the way two of them are compared.
// qui stores what the operator typed, minus the surrounding space and any
// trailing slash (internal/models/arr_instance.go: normalizeBaseURL at
// v1.36.1), so only what DNS and HTTP themselves fold is folded here: the
// scheme and the host are case-blind, and the path is compared as stored --
// box's own shape serves several apps under one name, and the path is what says
// which one an address meant.
func normalizeQuiArrBaseURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	scheme, rest, ok := splitScheme(s)
	if !ok {
		scheme = ""
	}
	host, path := rest, ""
	if i := strings.Index(rest, "/"); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	return strings.ToLower(scheme) + strings.ToLower(host) + strings.TrimRight(path, "/")
}

// quiArrEnabled reads a row's enabled flag. The model answers it as a bool on
// every row (internal/models/arr_instance.go at v1.36.1), so an absent one is a
// body this client did not write, and treating it as enabled is what keeps the
// test running rather than silently skipping the proof.
func quiArrEnabled(item Item) bool {
	if on, ok := item[fieldEnabled].(bool); ok {
		return on
	}
	return true
}

// TestArrInstance asks qui to dial the Arr now (POST
// /api/arr/instances/{id}/test). It answers 200 either way, with
// {"success":false,"error":...} on a failure, so the status alone would pass a
// row that reaches nothing.
func (q *Qui) TestArrInstance(ctx context.Context, id int) error {
	var out struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := q.client.Post(ctx, q.api(fmt.Sprintf("/arr/instances/%d/test", id)), nil, &out); err != nil {
		return fmt.Errorf("test qui Arr instance %d: %w", id, err)
	}
	if !out.Success {
		detail := out.Error
		if detail == "" {
			detail = "qui did not say why"
		}
		return fmt.Errorf("qui Arr instance %d is not reachable: %s", id, detail)
	}
	return nil
}
