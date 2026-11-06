package apps

import (
	"context"
	"time"
)

// Seerr is the request/discovery front end for Plex (Jellyfin/Emby are not in
// the v1 catalog). Every endpoint under /api/v1/settings is isAuthenticated
// (Permission.ADMIN) in Seerr's own router, and the only way to create its
// first admin user is a successful interactive Plex (or Jellyfin) OAuth
// sign-in through a browser: there is no bootstrap-by-env-var, and a Plex
// claim token does not help, because it authenticates the Plex *server* to
// plex.tv, not a plex.tv *account* into Seerr. The wiring Job therefore only
// waits for Seerr to come up; wiring it to Plex/Sonarr/Radarr is always
// reported pending until a human signs in once (see internal/wiring).
type Seerr struct {
	client *Client
}

// NewSeerr builds a client for the readiness wait only.
func NewSeerr(baseURL string) *Seerr {
	return &Seerr{client: NewClient(baseURL, nil)}
}

// Ping hits the one endpoint Seerr answers without any session:
// /api/v1/settings/public, which exists so a reverse proxy or a setup wizard
// can tell initialized state apart from "not up yet" before any user exists.
func (s *Seerr) Ping(ctx context.Context) error {
	var out struct {
		Initialized bool `json:"initialized"`
	}
	return s.client.Get(ctx, "/api/v1/settings/public", &out)
}

// WaitReady blocks until Seerr answers or timeout passes.
func (s *Seerr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, s.Ping)
}
