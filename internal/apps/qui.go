package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// WaitReady blocks until qui answers or timeout passes. qui refuses to start
// at all when its OIDC issuer is unreachable, so a failure here is a real
// configuration error rather than a slow boot.
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
// means nothing is recorded yet, so an existing instance with the expected name
// is adopted instead -- qui has no uniqueness on name or host, so a blind
// create would add a second row beside it.
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
	cur, found, recreated, previousID := locateOwned(list, ownedID, spec.Name)

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
