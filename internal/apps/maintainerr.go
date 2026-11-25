package apps

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Maintainerr is the app that deletes what nobody watched, on rules a person
// writes. It has no login of its own (the edge is the only thing in front of
// it), and its whole state is one SQLite database, so every connection it acts
// on -- Plex, Tautulli, Seerr, an Arr, qBittorrent -- is a row in its settings.
//
// It applies BASE_PATH to every route, health included, so a box serves it under
// its own prefix (charts/s5n/templates/workloads.yaml) and every path here
// carries URLBase.
//
// v3.33.0 is the pinned version the endpoints and shapes below were read from:
// apps/server/src/modules/settings/settings.controller.ts (GET/PATCH
// /api/settings, POST /api/settings/plex/token, GET /api/settings/test/plex),
// apps/server/src/modules/settings/settings-operations.service.ts (the token is
// saved on its own, and a Plex *connection* change is refused until a token is
// stored) and apps/server/src/app/health.controller.ts (/api/health/ready).
type Maintainerr struct {
	client  *Client
	urlBase string
}

// NewMaintainerr builds a client. urlBase is the prefix the chart serves it
// under, because the app mounts every route below it.
func NewMaintainerr(baseURL, urlBase string) *Maintainerr {
	return &Maintainerr{client: NewClient(baseURL, nil), urlBase: strings.TrimRight(urlBase, "/")}
}

// maintainerrPlexName is the display name box gives the Plex server it wires.
// It is a label in Maintainerr's own UI, never something it resolves, and the
// same name Tautulli's PMS link carries: one server, one name across a box.
const maintainerrPlexName = "s5n-plex"

// MaintainerrSettings is the part of Maintainerr's settings row the wiring
// depends on. PlexAuthToken is Maintainerr's mask for a stored token (`abc...xyz`
// when one is set, null when none is), never the value: a token cannot be read
// back out of this endpoint, which is why the Job keeps a fingerprint of the one
// it wrote (see internal/wiring).
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

// Ping is the readiness probe: Maintainerr's own readiness answers 200 once its
// database is reachable and 503 while it is not.
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

// PlexConnection is the Plex server a box wires Maintainerr to: the release's
// own Plex, on plain HTTP inside the cluster, with the Plex server token from
// the release Secret.
type PlexConnection struct {
	Host  string
	Port  int
	Token string
	// KnownFingerprint is the sha256-hex of the token this Job last wrote and
	// proved (owned.json). Maintainerr masks the token it holds, so this is the
	// only way a rotation in the release Secret is visible.
	KnownFingerprint string
}

// EnsurePlexConnection points Maintainerr at the release's Plex, with the Plex
// server token from the release Secret, and proves it with Maintainerr's own
// connection test.
//
// Three things here are the app's, not a choice:
//   - The token is saved on its own endpoint. A PATCH that changes the server
//     address is refused until a token is stored ("Authenticate with Plex before
//     saving Plex server settings."), so on a Maintainerr that has none the
//     token goes first.
//   - plex_manual_mode is set to 1. In manual mode Maintainerr never re-discovers
//     the server from plex.tv (apps/server/src/modules/api/plex-api/
//     plex-api.service.ts initialize/rediscoverConnection at v3.33.0), which is
//     the whole point: re-discovery would replace the release's in-cluster
//     address with a plex.direct hostname the pod cannot reach, on the first
//     transient failure.
//   - media_server_type becomes plex. A box has one media server, and a migrated
//     Maintainerr that named another one is naming an app this box does not run.
//
// The token itself is never compared and never read back: Maintainerr masks it,
// so KnownFingerprint decides whether the Secret's token is the one last written.
// A different or absent fingerprint is a rotation, and the write goes out again.
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
			// The error is the app's own answer, and the request carried the
			// token in its body: no value travels out of here.
			return Unchanged, fmt.Errorf("save the Plex token: %w", err)
		}
		if !strings.EqualFold(out.Status, "OK") {
			return Unchanged, fmt.Errorf("save the Plex token: %s", out.Message)
		}
	}
	if needConn {
		// Only the fields box owns: Maintainerr merges a PATCH over the stored
		// row, so everything else -- its rules, its cron, its other connections
		// -- is left exactly as it is.
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

	// The proof, and it is the app's own: its connection test dials the stored
	// server with the stored token and reads an authenticated endpoint, so a
	// write that did not stick (or a token this Plex refuses) fails here rather
	// than in a rule that silently never runs.
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
