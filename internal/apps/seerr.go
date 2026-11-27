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

// seerrServerJellyfin is Seerr's MediaServerType for a Jellyfin server
// (server/constants/server.ts at v3.5.2: PLEX = 1, JELLYFIN, EMBY, NOT_CONFIGURED).
const seerrServerJellyfin = 2

// Seerr is the request front end for a media server: Plex on a box that runs Plex,
// Jellyfin on one that does not.
//
// Every /api/v1/settings endpoint needs an admin session. On Plex the first admin
// exists only after an interactive sign-in through a browser: a claim token
// authenticates the server to plex.tv, not an account into Seerr. So the Job waits for
// it and reports `initialized`. On Jellyfin that admin is the person's own Jellyfin
// account, whose password is in the release Secret, so the Job signs in, enables the
// libraries and initializes in the order the setup page does.
type Seerr struct {
	client *Client
}

// NewSeerr builds a client for the readiness wait and the public read.
func NewSeerr(baseURL string) *Seerr {
	return &Seerr{client: NewClient(baseURL, nil)}
}

// Public is the part of Seerr's public settings the Job reads. Initialized is "an
// admin exists": POST /api/v1/settings/initialize sets it after the first sign-in
// (server/lib/settings/index.ts at v3.5.2), and a migrated Seerr brings it true.
type Public struct {
	Initialized bool `json:"initialized"`
}

// SeerrLibrary is one library Seerr synced from the media server: the server's own
// key, the title it shows, and whether Seerr scans it.
type SeerrLibrary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Settings reads /api/v1/settings/public, the one endpoint Seerr answers without a
// session.
func (s *Seerr) Settings(ctx context.Context) (Public, error) {
	var out Public
	err := s.client.Get(ctx, "/api/v1/settings/public", &out)
	return out, err
}

// Ping is the readiness probe: public settings answer once the server and database
// are up, whether or not an admin exists.
func (s *Seerr) Ping(ctx context.Context) error {
	_, err := s.Settings(ctx)
	return err
}

// WaitReady blocks until Seerr answers or timeout passes.
func (s *Seerr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, s.Ping)
}

// LoginJellyfin signs in through POST /api/v1/auth/jellyfin as a Jellyfin account,
// and keeps the session cookie the answer sets.
//
// With no users and no media server configured Seerr makes that account user 1 with
// an administrator's permissions, records the server it reached and mints its own API
// key there (server/routes/auth.ts:255 at v3.5.2), which is the whole reason the
// account has to be an administrator on the server. host and port are the server as
// Seerr dials it: the Service, not the address a person's browser uses.
func (s *Seerr) LoginJellyfin(ctx context.Context, username, password, host string, port int) error {
	payload, err := json.Marshal(map[string]any{
		fieldUsername: username,
		fieldPassword: password,
		// Seerr's own spelling: the server it reaches, not the address it serves.
		"hostname":   host,
		fieldPort:    port,
		"urlBase":    "",
		"useSsl":     false,
		"serverType": seerrServerJellyfin,
	})
	if err != nil {
		return fmt.Errorf("encode seerr sign-in: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.client.BaseURL+"/api/v1/auth/jellyfin", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 401 is the server refusing the password, 403 an account that cannot own a Seerr
	// (NOT_ADMIN): both are the person's to settle, and the caller reports them pending.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &SeerrSignInRefusedError{Username: username, Code: seerrMessage(data)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Method: http.MethodPost, URL: req.URL.String(), Status: resp.StatusCode, Body: string(data)}
	}
	cookie := sessionCookie(resp.Header)
	if cookie == "" {
		return fmt.Errorf("POST %s: Seerr answered %d without a session cookie", req.URL, resp.StatusCode)
	}
	s.client.Header.Set("Cookie", cookie)
	return nil
}

// SyncLibraries asks Seerr to read the media server's libraries again, and returns
// what it now lists. A library already in Seerr keeps the enabled flag it had.
func (s *Seerr) SyncLibraries(ctx context.Context) ([]SeerrLibrary, error) {
	var out []SeerrLibrary
	// No body and no content type: the endpoint takes none, and the API's own request
	// validator is strict about a body an operation does not declare.
	err := s.client.send(ctx, http.MethodPost, "/api/v1/settings/jellyfin/library/sync", "", nil, &out, nil)
	return out, err
}

// SetLibraryEnabled turns one synced library on or off, by the server's own key.
func (s *Seerr) SetLibraryEnabled(ctx context.Context, id string, enabled bool) error {
	return s.client.Put(ctx, "/api/v1/settings/jellyfin/library/"+id, map[string]bool{fieldEnabled: enabled}, nil)
}

// SetExternalHostname stores where people reach the media server: Seerr builds the
// links it shows -- and the reset-password page -- from it, and it is the one setting
// the setup page's configuration step exists for.
func (s *Seerr) SetExternalHostname(ctx context.Context, url string) error {
	return s.client.Post(ctx, "/api/v1/settings/jellyfin", map[string]string{"externalHostname": url}, nil)
}

// Initialize marks the instance set up, which is what makes Seerr leave its setup page
// for the request page. It needs the admin session.
func (s *Seerr) Initialize(ctx context.Context) error {
	return s.client.send(ctx, http.MethodPost, "/api/v1/settings/initialize", "", nil, nil, nil)
}

// SeerrSignInRefusedError is Seerr refusing the Jellyfin account: the server answered
// the password in the release Secret with INVALID_CREDENTIALS, or the account is not an
// administrator there (NOT_ADMIN). It carries Seerr's own code and never the password.
type SeerrSignInRefusedError struct {
	Username string
	Code     string
}

func (e *SeerrSignInRefusedError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("Seerr refused the Jellyfin account %q", e.Username)
	}
	return fmt.Sprintf("Seerr refused the Jellyfin account %q: %s", e.Username, e.Code)
}

// seerrMessage reads Seerr's error body, which is {"message": "<code>", "errors": ""}
// (server/index.ts's error handler at v3.5.2). It is the app's own spelling of why.
func seerrMessage(body []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return out.Message
}

// sessionCookie is the name=value of the session Seerr set. express-session's default
// name is connect.sid, and the first cookie is the session when a proxy adds none.
func sessionCookie(h http.Header) string {
	for _, line := range h.Values("Set-Cookie") {
		pair := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		name, _, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if name == "connect.sid" {
			return pair
		}
	}
	for _, line := range h.Values("Set-Cookie") {
		pair := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		if name, _, ok := strings.Cut(pair, "="); ok && strings.TrimSpace(name) != "" {
			return pair
		}
	}
	return ""
}
