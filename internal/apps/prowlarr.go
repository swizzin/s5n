package apps

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Prowlarr is the indexer manager. s5n keeps indexers in Prowlarr and lets it
// sync them out to the Arrs, so the wiring Job registers the Arrs as
// applications rather than touching indexers at all.
type Prowlarr struct {
	client  *Client
	urlBase string
	selfURL string
}

// NewProwlarr builds a client. selfURL is how the Arrs reach Prowlarr
// (http://prowlarr:9696/prowlarr), which Prowlarr hands to each application it
// registers.
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
	Implementation string // "Sonarr" or "Radarr"
	BaseURL        string // where Prowlarr reaches the Arr, including its URL base
	APIKey         string
}

// EnsureApplication registers the Arr and proves Prowlarr can talk to it.
// Like the Arr download client, the API key is never returned by the API, so
// a passing connection test is the evidence that the stored key is the right
// one.
//
// ownedID carries the id the wiring Job recorded the last time it created or
// adopted this application: a nonzero id is trusted over the object's current
// name, so a rename never creates a duplicate registration. A zero ownedID
// adopts an existing application named spec.Name instead.
//
// On update the live object is cloned and only prowlarrUrl, baseUrl and
// apiKey are overlaid; name, enable, syncLevel, tags and anything else are
// left exactly as the user set them. A deleted owned application is
// recreated.
//
// apiKey is the Arr's key, copied out of the release Secret, and Prowlarr
// answers a mask for it rather than the value, so a rotated key is invisible to
// a field comparison: knownFingerprint is the sha256 of the key last written and
// proven, and a difference rewrites the application. Prowlarr's own test is what
// proves the new key before anything is written.
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

	// Test before writing, with the id carried (when present) so the
	// uniqueness check ignores the application being tested.
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
