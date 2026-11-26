package apps

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Maintainerr deletes what nobody watched, on rules a person writes. It has no login
// and one SQLite database, so every connection it acts on is a row in its settings.
//
// It applies BASE_PATH to every route, health included, so every path here carries
// URLBase. v3.33.0 is the pinned version: settings.controller.ts (GET/PATCH
// /api/settings, POST /api/settings/plex/token, GET /api/settings/test/plex),
// settings-operations.service.ts (a connection change is refused until a token is
// stored) and health.controller.ts (/api/health/ready).
type Maintainerr struct {
	client  *Client
	urlBase string
}

// NewMaintainerr builds a client; urlBase is the prefix every route is below.
func NewMaintainerr(baseURL, urlBase string) *Maintainerr {
	return &Maintainerr{client: NewClient(baseURL, nil), urlBase: strings.TrimRight(urlBase, "/")}
}

// maintainerrPlexName is the display name box gives the Plex server: a label, never
// resolved, and the same name Tautulli's link carries.
const maintainerrPlexName = "s5n-plex"

// MaintainerrSettings is the settings row the wiring depends on. PlexAuthToken is
// Maintainerr's mask (`abc...xyz` when set, null when none), never the value: a token
// cannot be read back, which is why the Job keeps a fingerprint.
type MaintainerrSettings struct {
	MediaServerType string  `json:"media_server_type"`
	PlexName        string  `json:"plex_name"`
	PlexHostname    string  `json:"plex_hostname"`
	PlexPort        int     `json:"plex_port"`
	PlexSSL         int     `json:"plex_ssl"`
	PlexManualMode  int     `json:"plex_manual_mode"`
	PlexAuthToken   *string `json:"plex_auth_token"`
}

// PlexHost returns the server address Maintainerr would dial.
func (s MaintainerrSettings) PlexHost() string {
	return strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s.PlexHostname)), "https://"), "http://")
}

// HasPlexToken reports whether Maintainerr holds some Plex account token.
func (s MaintainerrSettings) HasPlexToken() bool {
	return s.PlexAuthToken != nil && strings.TrimSpace(*s.PlexAuthToken) != ""
}

// Ping is the readiness probe: 200 once the database is reachable, 503 while not.
func (m *Maintainerr) Ping(ctx context.Context) error {
	return m.client.Get(ctx, m.urlBase+"/api/health/ready", nil)
}

// WaitReady blocks until Maintainerr answers or timeout passes.
func (m *Maintainerr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, m.Ping)
}

// Settings reads the one settings row.
func (m *Maintainerr) Settings(ctx context.Context) (MaintainerrSettings, error) {
	var out MaintainerrSettings
	err := m.client.Get(ctx, m.urlBase+"/api/settings", &out)
	return out, err
}

// PlexConnection is the release's Plex, plain HTTP inside the cluster, with the
// Secret's token.
type PlexConnection struct {
	Host  string
	Port  int
	Token string
	// KnownFingerprint is the sha256 of the token last written and proved: the only
	// way a rotation is visible through the mask.
	KnownFingerprint string
}

// EnsurePlexConnection points Maintainerr at the release's Plex and proves it with
// the app's own connection test.
//
// Three things are the app's:
//   - the token is saved on its own endpoint: a connection PATCH is refused until a
//     token is stored, so it goes first;
//   - plex_manual_mode is 1, so Maintainerr never re-discovers the server from
//     plex.tv (plex-api.service.ts at v3.33.0) and replaces the in-cluster address
//     with a plex.direct name the pod cannot reach;
//   - media_server_type is plex, because a box has one media server.
//
// The token is never compared: KnownFingerprint decides whether the Secret's is the
// one last written.
func (m *Maintainerr) EnsurePlexConnection(ctx context.Context, spec PlexConnection) (Change, error) {
	if strings.TrimSpace(spec.Token) == "" {
		return Unchanged, fmt.Errorf("no Plex token: refusing to configure Maintainerr's Plex connection without one")
	}
	cur, err := m.Settings(ctx)
	if err != nil {
		return Unchanged, fmt.Errorf("read settings: %w", err)
	}
	needToken := !cur.HasPlexToken() ||
		spec.KnownFingerprint == "" ||
		spec.KnownFingerprint != PlexTokenFingerprint(spec.Token)
	needConn := !strings.EqualFold(cur.PlexHost(), strings.TrimSpace(spec.Host)) ||
		cur.PlexPort != spec.Port ||
		cur.PlexSSL != 0 ||
		cur.PlexManualMode != 1 ||
		cur.MediaServerType != "plex"
	if !needToken && !needConn {
		return Unchanged, nil
	}
	change := Updated
	if !cur.HasPlexToken() && cur.PlexHost() == "" {
		change = Created
	}

	if needToken {
		var out struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		body := map[string]string{"plex_auth_token": spec.Token}
		if err := m.client.Post(ctx, m.urlBase+"/api/settings/plex/token", body, &out); err != nil {
			// The token was in the body: no value travels out of here.
			return Unchanged, fmt.Errorf("save the Plex token: %w", err)
		}
		if !strings.EqualFold(out.Status, "OK") {
			return Unchanged, fmt.Errorf("save the Plex token: %s", out.Message)
		}
	}
	if needConn {
		// Only the owned fields: Maintainerr merges a PATCH, so its rules, cron and
		// other connections stay.
		body := map[string]any{
			"plex_hostname":     strings.TrimSpace(spec.Host),
			"plex_port":         spec.Port,
			"plex_ssl":          0,
			"plex_manual_mode":  1,
			"plex_name":         maintainerrPlexName,
			"media_server_type": "plex",
		}
		var out struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if err := m.client.Patch(ctx, m.urlBase+"/api/settings", body, &out); err != nil {
			return Unchanged, fmt.Errorf("save the Plex connection: %w", err)
		}
		if !strings.EqualFold(out.Status, "OK") {
			return Unchanged, fmt.Errorf("save the Plex connection: %s", out.Message)
		}
	}

	// The app's own test dials the stored server with the stored token, so a write
	// that did not stick fails here rather than in a rule that never runs.
	var test struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := m.client.Get(ctx, m.urlBase+"/api/settings/test/plex", &test); err != nil {
		return Unchanged, fmt.Errorf("prove the Plex connection: %w", err)
	}
	if !strings.EqualFold(test.Status, "OK") {
		return Unchanged, fmt.Errorf("prove the Plex connection: %s", test.Message)
	}
	return change, nil
}
