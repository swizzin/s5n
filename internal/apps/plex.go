package apps

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Plex is the media server. /identity answers unauthenticated on every build, so it
// is the readiness probe and the only way to learn whether a claim token claimed the
// server: unclaimed makes the media rows pending, not a failed release.
type Plex struct {
	client *Client
}

// NewPlex builds a client. Nothing here is tokenless except /identity: clients that
// call Plex's library API get the Secret's token, because trusting a range would
// trust the pod CIDR the Gateway proxies from.
func NewPlex(baseURL string) *Plex {
	c := NewClient(baseURL, nil)
	// Plex content-negotiates /identity: an Accept of application/json returns a
	// body the XML parser cannot read.
	c.Header.Set("Accept", "application/xml")
	return &Plex{client: c}
}

// Identity is the parsed /identity response.
type Identity struct {
	Claimed           bool
	MachineIdentifier string
	Version           string
}

type identityXML struct {
	Claimed           string `xml:"claimed,attr"`
	MachineIdentifier string `xml:"machineIdentifier,attr"`
	Version           string `xml:"version,attr"`
}

// Ping is the readiness probe: /identity answers before libraries or a claim, so it
// distinguishes "not up yet" from "up and unclaimed".
func (p *Plex) Ping(ctx context.Context) error {
	_, err := p.Identity(ctx)
	return err
}

// WaitReady blocks until /identity answers or timeout passes.
func (p *Plex) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, p.Ping)
}

// Identity fetches and parses /identity.
func (p *Plex) Identity(ctx context.Context) (Identity, error) {
	text, err := p.client.GetText(ctx, "/identity")
	if err != nil {
		return Identity{}, err
	}
	var x identityXML
	if err := xml.Unmarshal([]byte(text), &x); err != nil {
		return Identity{}, fmt.Errorf("parse /identity: %w", err)
	}
	return Identity{
		Claimed:           x.Claimed == "1",
		MachineIdentifier: x.MachineIdentifier,
		Version:           x.Version,
	}, nil
}

// TokenRejectedError is Plex answering 401: the Secret's token is not one this
// server accepts. It carries only the status, never the token.
type TokenRejectedError struct {
	Status int
}

func (e *TokenRejectedError) Error() string {
	return fmt.Sprintf("plex-token rejected by Plex (%d)", e.Status)
}

// VerifyToken proves the Secret's Plex token: /library/sections answers 200 to one
// the server accepts and 401 to one it does not.
//
// /identity cannot do this: it is unauthenticated, so Tautulli's own connection test
// reports a bogus token as working. The token goes in a header, never the URL, and
// the body is drained, so no error or receipt can contain it.
func (p *Plex) VerifyToken(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("no Plex token: cannot verify an empty token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.client.BaseURL+"/library/sections", nil)
	if err != nil {
		return fmt.Errorf("check plex token: %w", err)
	}
	for k, values := range p.client.Header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("X-Plex-Token", token)
	req.Header.Set("Accept", "application/xml")

	resp, err := p.client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("check plex token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return &TokenRejectedError{Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("check plex token: GET /library/sections returned %s", http.StatusText(resp.StatusCode))
	}
	return nil
}

// PlexTokenFingerprint is sha256-hex of a Plex token, the only form the Job may
// persist: Tautulli returns PMS_TOKEN as null, so a rotation is visible only through
// this record, while the Secret stays the only place the token exists.
func PlexTokenFingerprint(token string) string {
	return SecretFingerprint(token)
}
