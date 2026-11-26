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

// Qui is qui's WebUI API (v1.35.0): instances under /api/instances
// (internal/api/server.go), a create answering 201 before it tests the connection,
// and a PUT without a password keeping the stored one (internal/api/handlers/
// instances.go).
//
// The session cookie is Secure whenever the OIDC redirect URL is https
// (internal/domain/config.go's SecureSessionCookie, and every s5n box sets one), so
// a jar will not replay it over plain http: Login captures Set-Cookie itself. A
// cookie-authenticated write also needs X-Requested-With
// (internal/api/middleware/auth.go).
type Qui struct {
	client   *Client
	urlBase  string
	username string
	password string
	session  string
}

// NewQui builds a client. urlBase is qui's own base URL ("/qui"), set through
// QUI__BASE_URL.
func NewQui(baseURL, urlBase, username, password string) *Qui {
	c := NewClient(baseURL, nil)
	c.Header.Set("X-Requested-With", "XMLHttpRequest")
	return &Qui{client: c, urlBase: strings.TrimRight(urlBase, "/"), username: username, password: password}
}

func (q *Qui) api(path string) string { return q.urlBase + "/api" + path }

// Ping is the readiness probe: /healthz/readiness is on the root router, so the
// prefixed path answers and /api/healthz/... is a 404.
func (q *Qui) Ping(ctx context.Context) error {
	_, err := q.client.GetText(ctx, q.urlBase+"/healthz/readiness")
	return err
}

// WaitReady blocks until qui answers. It starts without its OIDC issuer, so a pass
// says nothing about the login: the Job's sign-in proves that.
func (q *Qui) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, q.Ping)
}

// Login opens the session the instance calls ride on, keeping only the cookie's
// name=value. A wrong password is a LoginRejectedError, never retried.
//
// With login switched off (QUI__AUTH_DISABLED) the endpoint answers 403 and needs no
// session: that is "no session", not a failure.
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

// EnsureInstance makes qui's qBittorrent instance match the spec and proves it with
// qui's own test.
//
// A nonzero ownedID is authoritative, so a UI rename never duplicates; zero adopts
// an instance at the spec's host, the shape a migration leaves, because qui has no
// uniqueness on name or host and a blind create would add a second row. An adopted
// instance keeps its name.
//
// On update only the data-contract fields are overlaid, and username/password are
// written empty: qBittorrent's login is bypassed, so there is no credential and none
// to fingerprint.
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

// locateQui finds the qBittorrent instance the Job owns: a recorded id is
// authoritative; with nothing recorded, an instance at the spec's host is adopted (a
// migration leaves one under the operator's name, already pointed at the release),
// with the name as fallback; a missing id means recreate. Two instances at one host
// are a refusal, because nothing says which history to keep.
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

// normalizeQuiHost is the stored form: trimmed, http:// added when absent, the URL
// re-serialised (models.validateAndNormalizeHost at v1.35.0), and no trailing slash.
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

// TestInstance asks qui to dial now: it answers 200 with connected:false on failure,
// so the status alone would pass a broken instance.
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

// EnsureArrInstance makes one Arr row match the spec and proves it with qui's own
// test -- an Arr call from qui's pod, the only proof the NetworkPolicy admits it.
//
// The id keeps a rename from duplicating, and the key fingerprint is the only way a
// Secret rotation is visible. Only the key is written to an existing row: a disabled
// one stays disabled and is not tested.
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

// locateQuiArr finds the Arr row the Job owns: a recorded id is authoritative; with
// nothing recorded (or its row gone), the row at this box's address is adopted,
// because the table is UNIQUE on (type, base_url) and a migrated row is already
// pointed there. Any row counts, and the lowest id wins on duplicate spellings.
//
// There is deliberately no fallback by name: a same-named row at another address is
// another Arr, and adopting it would leave the remote one holding this release's key.
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

// normalizeQuiArrBaseURL compares as qui stores it (internal/models/arr_instance.go:
// normalizeBaseURL at v1.36.1): scheme and host case-folded, the path compared as
// stored, because the path says which app an address meant.
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

// TestArrInstance asks qui to dial (POST /api/arr/instances/{id}/test): it answers
// 200 either way, so the status alone would pass a row that reaches nothing.
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
