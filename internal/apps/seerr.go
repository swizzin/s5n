package apps

import (
	"context"
	"time"
)

// Seerr is the request front end for Plex. Every /api/v1/settings endpoint needs an
// admin session, and the first admin exists only after an interactive Plex sign-in:
// a claim token authenticates the server to plex.tv, not an account into Seerr. So
// the Job waits for it and reports `initialized`; wiring the rest is a person's.
type Seerr struct {
	client *Client
}

// NewSeerr builds a client for the readiness wait and the one public read.
func NewSeerr(baseURL string) *Seerr {
	return &Seerr{client: NewClient(baseURL, nil)}
}

// Public is the part of Seerr's public settings the Job reads. Initialized is "an
// admin exists": POST /api/v1/settings/initialize sets it after the first sign-in
// (server/lib/settings/index.ts at v3.5.2), and a migrated Seerr brings it true.
type Public struct {
	Initialized bool `json:"initialized"`
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
