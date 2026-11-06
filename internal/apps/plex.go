package apps

import (
	"context"
	"encoding/xml"
	"fmt"
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

// NewPlex builds a client. Plex needs no API key for /identity or for any
// endpoint reached from a PLEX_NO_AUTH_NETWORKS range (see the chart's
// apps.plex.noAuthNetworks), which is how Tautulli reaches it without ever
// holding a Plex.tv account token.
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
