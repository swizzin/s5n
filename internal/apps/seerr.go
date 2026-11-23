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
// waits for Seerr to come up and reports what `initialized` says; wiring it to
// Plex/Sonarr/Radarr is always left to a human's first sign-in (see
// internal/wiring).
type Seerr struct {
	client *Client
}

// NewSeerr builds a client for the readiness wait and the one public read.
func NewSeerr(baseURL string) *Seerr {
	return &Seerr{client: NewClient(baseURL, nil)}
}

// Public is the part of Seerr's public settings the wiring Job reads.
//
// Initialized is Seerr's own "an admin exists": the flag is false in its
// shipped defaults and `POST /api/v1/settings/initialize` -- an admin-only
// endpoint -- is what sets it after the first sign-in (server/lib/settings/
// index.ts at v3.5.2, server/routes/settings/index.ts). A migrated Seerr has
// its admin in the settings its snapshot carried, so the flag is true there
// without anyone signing in on this box.
type Public struct {
	Initialized bool `json:"initialized"`
}

// Settings reads /api/v1/settings/public, the one endpoint Seerr answers
// without any session: it exists so a reverse proxy or a setup wizard can tell
// an initialized install apart from "not up yet" before any user exists.
func (s *Seerr) Settings(ctx context.Context) (Public, error) {
	var out Public
	err := s.client.Get(ctx, "/api/v1/settings/public", &out)
	return out, err
}

// Ping is the readiness probe: the public settings answer once Seerr's HTTP
// server and its database are up, whether or not an admin exists.
func (s *Seerr) Ping(ctx context.Context) error {
	_, err := s.Settings(ctx)
	return err
}

// WaitReady blocks until Seerr answers or timeout passes.
func (s *Seerr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, s.Ping)
}
