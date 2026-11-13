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

// Plex is the media server. Its /identity endpoint answers unauthenticated on
// every build (claimed or not), which is both the readiness probe and the only
// way the wiring Job learns whether a claim token actually claimed the server:
// a missing or expired claim token leaves it unclaimed, and Tautulli/Seerr's
// media rows report that rather than failing the release (AGENTS.md invariant
// in the "Next work" list).
type Plex struct {
	client *Client
}

// NewPlex builds a client. /identity needs no credential at all: Plex answers
// it unauthenticated on every build, claimed or not. Nothing else here is
// tokenless — clients that call Plex's library API (Tautulli) are given the
// server token from the release Secret, because trusting a network range
// instead would trust the pod CIDR the Gateway proxies from.
func NewPlex(baseURL string) *Plex {
	c := NewClient(baseURL, nil)
	// Plex serves /identity as XML by default and content-negotiates: an
	// Accept of application/json returns a JSON body instead, which the XML
	// parser in Identity cannot read. Ask for the format this client parses.
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

// Ping is the readiness probe: /identity answers before the server has any
// libraries or claim, so it is also what distinguishes "not up yet" from
// "up and unclaimed".
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

// TokenRejectedError is Plex answering 401 to an authenticated call: the server
// token in the release Secret is not one this server accepts. It carries the
// status code and nothing else — never the token, which travels in a request
// header rather than the URL, so no error value below can contain it.
type TokenRejectedError struct {
	Status int
}

func (e *TokenRejectedError) Error() string {
	return fmt.Sprintf("plex-token rejected by Plex (%d)", e.Status)
}

// VerifyToken proves the release's Plex server token (X-Plex-Token) against Plex
// itself: /library/sections answers 200 to a token the server accepts and 401 to
// one it does not.
//
// /identity cannot do this job. It answers unauthenticated on every build, so
// Tautulli's get_server_identity — its own "connection test", which reads
// /identity through its configured PMS host — reports a bogus or expired token
// as a working link. Only an authenticated Plex call can tell the difference.
//
// The token goes in a header, never a query string: URL and transport errors
// then carry only /library/sections, and the response body is drained rather
// than quoted, so a server echo can never reach a receipt.
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

// PlexTokenFingerprint is sha256-hex of a Plex server token: the only form of it
// the wiring Job may persist. Tautulli lists PMS_TOKEN among its
// do-not-download keys, so get_settings returns it as null; without a record of
// what was last proven, a rotated token in the release Secret would never be
// pushed. The fingerprint makes that comparison possible while the Secret stays
// the only place the token exists.
func PlexTokenFingerprint(token string) string {
	return SecretFingerprint(token)
}
