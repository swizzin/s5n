package apps

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Prowlarr is the indexer manager: s5n keeps indexers there and lets it sync them to
// the Arrs, so the Job registers the Arrs as applications and touches no indexer.
type Prowlarr struct {
	client  *Client
	urlBase string
	selfURL string
}

// NewProwlarr builds a client. selfURL is how the Arrs reach Prowlarr, which it hands
// to each application.
func NewProwlarr(baseURL, urlBase, selfURL, apiKey string) *Prowlarr {
	c := NewClient(baseURL, nil)
	c.Header.Set("X-Api-Key", apiKey)
	return &Prowlarr{client: c, urlBase: strings.TrimRight(urlBase, "/"), selfURL: selfURL}
}

func (p *Prowlarr) api(path string) string { return p.urlBase + "/api/v1" + path }

// Ping hits Prowlarr's unauthenticated health endpoint.
func (p *Prowlarr) Ping(ctx context.Context) error {
	_, err := p.client.GetText(ctx, p.urlBase+"/ping")
	return err
}

// WaitReady blocks until Prowlarr answers.
func (p *Prowlarr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, p.Ping)
}

// ApplicationSpec registers one Arr instance with Prowlarr.
type ApplicationSpec struct {
	Name           string
	Implementation string // "Sonarr", "Radarr" or "Lidarr"
	BaseURL        string // where Prowlarr reaches the Arr, including its URL base
	APIKey         string
}

// EnsureApplication registers the Arr and proves Prowlarr can talk to it. The key is
// never returned, so a passing test is the evidence.
//
// A nonzero ownedID is trusted over the current name, so a rename never duplicates; a
// zero adopts the application named spec.Name. On update only prowlarrUrl, baseUrl and
// apiKey are overlaid, and a deleted owned application is recreated.
//
// Prowlarr answers a mask for the key, so a rotated key is invisible to field
// comparison: knownFingerprint is the sha256 of the key last written and proven, and
// a difference rewrites the application after Prowlarr's own test.
func (p *Prowlarr) EnsureApplication(ctx context.Context, spec ApplicationSpec, ownedID int, knownFingerprint string) (EnsureResult, error) {
	var list []Item
	if err := p.client.Get(ctx, p.api("/applications"), &list); err != nil {
		return EnsureResult{}, fmt.Errorf("list applications: %w", err)
	}
	cur, found, recreated, previousID := locateOwned(list, ownedID, spec.Name)

	fields := []ownedField{
		{"prowlarrUrl", p.selfURL, false},
		{"baseUrl", spec.BaseURL, false},
		{"apiKey", spec.APIKey, true},
	}

	var desired Item
	var drift []FieldChange
	change := Created
	if found {
		desired, drift = overlayOwned(cur, fields)
		desired["id"] = cur["id"]
		rotated := knownFingerprint != SecretFingerprint(spec.APIKey)
		if len(drift) > 0 || rotated {
			change = Updated
		} else {
			change = Unchanged
		}
	} else {
		var schemas []Item
		if err := p.client.Get(ctx, p.api("/applications/schema"), &schemas); err != nil {
			return EnsureResult{}, fmt.Errorf("read application schema: %w", err)
		}
		tmpl, ok := FindImplementation(schemas, spec.Implementation)
		if !ok {
			return EnsureResult{}, fmt.Errorf("this Prowlarr build has no %s application implementation", spec.Implementation)
		}
		desired = tmpl.Clone()
		desired["name"] = spec.Name
		desired["enable"] = true
		desired["syncLevel"] = "fullSync"
		desired["tags"] = []any{}
		for _, f := range fields {
			desired.SetField(f.name, f.value)
		}
	}

	// Test before writing, carrying the id so the uniqueness check ignores the
	// application being tested.
	if err := p.client.Post(ctx, p.api("/applications/test"), desired, nil); err != nil {
		return EnsureResult{Change: change, ID: desired.Int("id")}, fmt.Errorf("test %s: %w", spec.Name, err)
	}

	switch change {
	case Created:
		var out Item
		if err := p.client.Post(ctx, p.api("/applications"), desired, &out); err != nil {
			return EnsureResult{}, fmt.Errorf("register %s: %w", spec.Name, err)
		}
		desired = out
	case Updated:
		if err := p.client.Put(ctx, fmt.Sprintf("%s/%d", p.api("/applications"), desired.Int("id")), desired, nil); err != nil {
			return EnsureResult{}, fmt.Errorf("update %s: %w", spec.Name, err)
		}
	}
	return EnsureResult{Change: change, ID: desired.Int("id"), Drift: drift, Recreated: recreated, PreviousID: previousID}, nil
}
