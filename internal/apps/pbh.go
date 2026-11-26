package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Pbh is PeerBanHelper's WebAPI (v9.5.4): it reads qBittorrent's API for peers to
// ban, so the only object s5n owns is one downloader.
//
// Every answer is wrapped in {"success","message","data"}; downloaders are keys in
// its config.yml, so the id is a chosen string; PUT creates and PATCH replaces (a
// second PUT answers 400), and the list reports the /api/v2 endpoint while the config
// carries the plain one. The token goes in an Authorization: Bearer header.
type Pbh struct {
	client *Client
}

// NewPbh builds a client. The WebAPI is at the root (PBH has no base-path setting,
// which is why the chart serves it on a host of its own); token is the Secret's
// pbh-token.
func NewPbh(baseURL, token string) *Pbh {
	c := NewClient(baseURL, nil)
	c.Header.Set("Authorization", "Bearer "+token)
	return &Pbh{client: c}
}

// pbhFieldEndpoint is the plain http://host:port, no /api/v2 and no trailing slash
// (the loader strips one, and a create stores what it is given).
const pbhFieldEndpoint = "endpoint"

// pbhResponse is PBH's envelope for every WebAPI answer. Data is kept raw so one
// helper serves the list, the status and the writes, whose data is null.
type pbhResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// raw performs a request and returns the envelope's data, decoding the body
// itself because the envelope wraps whatever the endpoint answers.
func (p *Pbh) raw(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	req, err := p.client.request(ctx, method, path, "application/json", nil)
	if err != nil {
		return nil, err
	}
	if body != nil {
		payload, merr := json.Marshal(body)
		if merr != nil {
			return nil, fmt.Errorf("encode %s %s: %w", method, path, merr)
		}
		req, err = p.client.request(ctx, method, path, "application/json", payload)
		if err != nil {
			return nil, err
		}
	}
	resp, err := p.client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope pbhResponse
	if derr := json.NewDecoder(resp.Body).Decode(&envelope); derr != nil {
		return nil, fmt.Errorf("%s %s: decode response: %w", method, req.URL, derr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := envelope.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, &HTTPError{Method: method, URL: req.URL.String(), Status: resp.StatusCode, Body: msg}
	}
	if !envelope.Success {
		// A 2xx with success:false is PBH saying no in its own words.
		return nil, fmt.Errorf("%s %s: %s", method, req.URL, envelope.Message)
	}
	return envelope.Data, nil
}

// Ping is the readiness check. /api/downloaders is the cheapest route that
// proves the WebAPI is serving, and it is the one this Job then writes to.
func (p *Pbh) Ping(ctx context.Context) error {
	_, err := p.raw(ctx, http.MethodGet, "/api/downloaders", nil)
	return err
}

// WaitReady blocks until PBH answers. A refused token returns at once: PBH
// blacklists an address for 15 minutes after ten failed attempts.
func (p *Pbh) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay := 500 * time.Millisecond
	for {
		err := p.Ping(ctx)
		if err == nil {
			return nil
		}
		if IsForbidden(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}

// Version is PBH's version from the unauthenticated manifest: the receipt's evidence
// that the app serving is the one the chart pinned.
func (p *Pbh) Version(ctx context.Context) (string, error) {
	data, err := p.raw(ctx, http.MethodGet, "/api/metadata/manifest", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Version struct {
			Version string `json:"version"`
		} `json:"version"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode /api/metadata/manifest: %w", err)
	}
	return out.Version.Version, nil
}

// Downloaders lists what PBH has configured. Each entry carries id, name,
// endpoint (the /api/v2 one), type and paused - never the stored credential, so
// this list cannot say whether a copy is stale.
func (p *Pbh) Downloaders(ctx context.Context) ([]Item, error) {
	data, err := p.raw(ctx, http.MethodGet, "/api/downloaders", nil)
	if err != nil {
		return nil, fmt.Errorf("list pbh downloaders: %w", err)
	}
	var out []Item
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode pbh downloaders: %w", err)
	}
	return out, nil
}

// DownloaderStatus is one downloader's health and stored config. It echoes the
// password in clear text, so nothing here compares or records one.
func (p *Pbh) DownloaderStatus(ctx context.Context, id string) (Item, error) {
	data, err := p.raw(ctx, http.MethodGet, "/api/downloaders/"+url.PathEscape(id)+"/status", nil)
	if err != nil {
		return nil, fmt.Errorf("pbh downloader %q status: %w", id, err)
	}
	var out Item
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode pbh downloader %q status: %w", id, err)
	}
	return out, nil
}

// PbhDownloaderSpec is the qBittorrent PBH should read.
type PbhDownloaderSpec struct {
	// ID is the key PBH stores the downloader under (its `client:` section).
	ID string
	// Name is the label a person sees in PBH's UI.
	Name string
	// Endpoint is the plain http://host:port, with no /api/v2 and no trailing
	// slash: PBH appends /api/v2 itself.
	Endpoint string
	Username string
	Password string
}

// pbhDesired is the stored config with the owned fields overlaid; everything else
// survives. The password is written empty (qBittorrent skips its login) and never
// compared.
func pbhDesired(live Item, spec PbhDownloaderSpec) (Item, []FieldChange) {
	desired, drift := overlayOwned(live, []ownedField{
		{pbhFieldEndpoint, spec.Endpoint, false},
		{fieldUsername, spec.Username, false},
		{fieldPassword, spec.Password, true},
	})
	// saveToYaml dereferences basicAuth without a nil check, so a config that
	// lost it makes any save answer 500 (QBittorrentConfigImpl.saveToYaml:60).
	if _, ok := desired["basicAuth"].(map[string]any); !ok {
		desired["basicAuth"] = map[string]any{"user": "", "pass": ""}
	}
	return desired, drift
}

// pbhCreateConfig is the whole object a create sends: PBH's config class has no
// defaults, so every field is written.
func pbhCreateConfig(spec PbhDownloaderSpec) Item {
	return Item{
		"type":           "qbittorrent",
		fieldName:        spec.Name,
		pbhFieldEndpoint: spec.Endpoint,
		fieldUsername:    spec.Username,
		fieldPassword:    spec.Password,
		"apiKey":         "",
		"basicAuth":      map[string]any{"user": "", "pass": ""},
		"incrementBan":   false,
		"useShadowBan":   false,
		"verifySsl":      true,
		"ignorePrivate":  false,
		"paused":         false,
	}
}

// locatePbh finds the downloader the Job owns: a recorded id is authoritative, but
// PBH's config migration 32 re-keys downloaders to a UUID
// (MainConfigUpdateScript.assignUniqueIdForDownloader at v9.5.4), so a stale id is
// normal. With none matching, the endpoint (the release's qBittorrent) is adopted,
// with the name as fallback; a missing one is recreated.
//
// PBH refuses a duplicate id or name, never a duplicate endpoint
// (DownloaderManagerImpl.registerDownloader at v9.5.4), so two at one endpoint are a
// refusal rather than a third row.
func locatePbh(list []Item, spec PbhDownloaderSpec, ownedID string) (cur Item, found, recreated bool, previousID string, err error) {
	if ownedID != "" {
		for _, item := range list {
			if item.Str("id") == ownedID {
				return item, true, false, "", nil
			}
		}
	}
	switch same := pbhEndpointMatches(list, spec.Endpoint); len(same) {
	case 0:
	case 1:
		return same[0], true, false, "", nil
	default:
		return nil, false, false, "", fmt.Errorf("pbh: %d downloaders point at %s (ids %q and %q); remove all but one, this box can own only one",
			len(same), spec.Endpoint, same[0].Str("id"), same[1].Str("id"))
	}
	if item, ok := FindByName(list, spec.Name); ok {
		return item, true, false, "", nil
	}
	return nil, false, ownedID != "", ownedID, nil
}

// pbhEndpointMatches returns downloaders whose endpoint matches: the list reports the
// /api/v2 form (AbstractQbittorrent.java:59,217) and the config the plain one, so both
// are reduced.
func pbhEndpointMatches(list []Item, endpoint string) []Item {
	want := normalizePbhEndpoint(endpoint)
	var out []Item
	for _, item := range list {
		if want != "" && normalizePbhEndpoint(item.Str(pbhFieldEndpoint)) == want {
			out = append(out, item)
		}
	}
	return out
}

// normalizePbhEndpoint is the plain http://host:port a downloader is configured
// with: no API suffix, no trailing slash, scheme and host case-folded (they are
// the only case-insensitive parts a URL has).
func normalizePbhEndpoint(endpoint string) string {
	s := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	s = strings.TrimRight(strings.TrimSuffix(s, "/api/v2"), "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.Path
}

// EnsureDownloader makes the downloader match the spec and returns its id, which the
// caller records only after WaitHealthy proves the write. There is no fingerprint:
// the only secret was the qBittorrent password, written empty.
func (p *Pbh) EnsureDownloader(ctx context.Context, spec PbhDownloaderSpec, ownedID string) (EnsureResult, error) {
	list, err := p.Downloaders(ctx)
	if err != nil {
		return EnsureResult{}, err
	}
	cur, found, recreated, previousID, err := locatePbh(list, spec, ownedID)
	if err != nil {
		return EnsureResult{}, err
	}

	if !found {
		body := map[string]any{"id": spec.ID, "config": pbhCreateConfig(spec)}
		if _, err := p.raw(ctx, http.MethodPut, "/api/downloaders", body); err != nil {
			return EnsureResult{Change: Created, StringID: spec.ID}, fmt.Errorf("create pbh downloader %q: %w", spec.ID, err)
		}
		return EnsureResult{Change: Created, StringID: spec.ID, Recreated: recreated, PreviousStringID: previousID}, nil
	}

	id := cur.Str("id")
	status, err := p.DownloaderStatus(ctx, id)
	if err != nil {
		return EnsureResult{StringID: id}, err
	}
	live := Item{}
	if raw, ok := status["config"].(map[string]any); ok {
		live = Item(raw)
	}
	// A downloader that reports no config cannot be compared, so it is written
	// whole.
	missing := len(live) == 0
	if missing {
		live = pbhCreateConfig(spec)
	}
	desired, drift := pbhDesired(live, spec)

	change := Unchanged
	if len(drift) > 0 || missing {
		change = Updated
	}
	if change == Updated {
		if _, err := p.raw(ctx, http.MethodPatch, "/api/downloaders/"+url.PathEscape(id),
			map[string]any{"config": desired}); err != nil {
			return EnsureResult{Change: change, StringID: id}, fmt.Errorf("update pbh downloader %q: %w", id, err)
		}
	}
	return EnsureResult{Change: change, StringID: id, Drift: drift, Recreated: recreated}, nil
}

// WaitHealthy polls until PBH reports HEALTHY, the only evidence it logged in with the
// password written: a create or PATCH only registers, and PBH's own poll loop dials
// it. The last status is returned so a caller can say what it waited on.
func (p *Pbh) WaitHealthy(ctx context.Context, id string, timeout time.Duration) (string, error) {
	last := ""
	deadline := time.Now().Add(timeout)
	delay := time.Second
	for {
		status, err := p.DownloaderStatus(ctx, id)
		if err != nil {
			if IsNotFound(err) {
				return last, err
			}
			return last, fmt.Errorf("after %s: %w", timeout, err)
		}
		last = status.Str("lastStatus")
		if last == "HEALTHY" {
			return last, nil
		}
		if time.Now().After(deadline) {
			detail := status.Str("lastStatusMessage")
			if detail == "" {
				detail = "no message"
			}
			return last, fmt.Errorf("after %s: PBH reports %q (%s)", timeout, last, detail)
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("%w (last status %s)", ctx.Err(), last)
		case <-time.After(delay):
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}
