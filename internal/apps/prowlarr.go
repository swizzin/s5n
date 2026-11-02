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

// EnsureApplication registers the Arr and proves Prowlarr can talk to it. Like
// the Arr download client, the API key is never returned by the API, so a passing
// connection test is the evidence that the stored key is the right one.
func (p *Prowlarr) EnsureApplication(ctx context.Context, spec ApplicationSpec) (Change, error) {
	var schemas []Item
	if err := p.client.Get(ctx, p.api("/applications/schema"), &schemas); err != nil {
		return Unchanged, fmt.Errorf("read application schema: %w", err)
	}
	tmpl, ok := FindImplementation(schemas, spec.Implementation)
	if !ok {
		return Unchanged, fmt.Errorf("this Prowlarr build has no %s application implementation", spec.Implementation)
	}
	desired := tmpl.Clone()
	desired["name"] = spec.Name
	desired["enable"] = true
	desired["syncLevel"] = "fullSync"
	desired["tags"] = []any{}
	desired.SetField("prowlarrUrl", p.selfURL)
	desired.SetField("baseUrl", spec.BaseURL)
	desired.SetField("apiKey", spec.APIKey)
	// Auth is not used between Prowlarr and the Arrs: they share an API key.
	desired.SetField("authUsername", "")
	desired.SetField("authPassword", "")

	var list []Item
	if err := p.client.Get(ctx, p.api("/applications"), &list); err != nil {
		return Unchanged, fmt.Errorf("list applications: %w", err)
	}
	change := Created
	if cur, ok := FindByName(list, spec.Name); ok {
		desired["id"] = cur["id"]
		if SameValue(cur["enable"], true) && SameValue(cur["syncLevel"], "fullSync") &&
			MatchesFields(desired, cur, []string{"prowlarrUrl", "baseUrl"}) {
			change = Unchanged
		} else {
			change = Updated
		}
	}

	// Test before writing, with the id carried so the uniqueness check ignores the
	// application being tested.
	if err := p.client.Post(ctx, p.api("/applications/test"), desired, nil); err != nil {
		return change, fmt.Errorf("test %s: %w", spec.Name, err)
	}

	switch change {
	case Created:
		if err := p.client.Post(ctx, p.api("/applications"), desired, nil); err != nil {
			return Unchanged, fmt.Errorf("register %s: %w", spec.Name, err)
		}
	case Updated:
		if err := p.client.Put(ctx, fmt.Sprintf("%s/%d", p.api("/applications"), desired.Int("id")), desired, nil); err != nil {
			return Unchanged, fmt.Errorf("update %s: %w", spec.Name, err)
		}
	}
	return change, nil
}
