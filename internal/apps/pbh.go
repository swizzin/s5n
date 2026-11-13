package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Pbh is PeerBanHelper's WebAPI (v9.5.4). It is the downloader-side half of the
// app: PBH reads qBittorrent's Web API to find peers to ban, so the only object
// s5n owns in it is one downloader.
//
// Three of PBH's own behaviours shape this client:
//
//   - Every answer is wrapped: {"success":bool,"message":string,"data":...}. A
//     write answers with data:null, and a failure carries its reason in message
//     rather than in the status alone ("Downloader created successfully" is the
//     201 body).
//   - Downloaders are keys in PBH's own config.yml (`client: <id>:`), so the id
//     is a string the caller chooses and everything after a create addresses
//     /api/downloaders/<id>. The same id may be a name a human renamed, which is
//     why the record in owned.json holds it (internal/wiring/owned.go).
//   - PUT creates and PATCH replaces; a second PUT of the same id answers 400
//     ("already exists"), so an update has to be the PATCH. The list endpoint
//     reports each downloader's *API* endpoint (http://host:port/api/v2), while
//     the config - and what the status endpoint echoes back - carries the plain
//     one, so a comparison of the two would drift on every run.
//
// The WebAPI is a config-file editor with a token in front of it, not an OAuth
// surface: the token goes in an Authorization: Bearer header, and the chart
// gives this Job the release Secret's pbh-token.
type Pbh struct {
	client *Client
}

// NewPbh builds a client. baseURL is PBH's own Service address; the WebAPI is
// mounted at the root (there is no base-path setting, which is why the chart
// serves PBH on a host of its own). token is the release Secret's pbh-token.
func NewPbh(baseURL, token string) *Pbh {
	c := NewClient(baseURL, nil)
	c.Header.Set("Authorization", "Bearer "+token)
	return &Pbh{client: c}
}

// pbhFieldEndpoint is qBittorrent's endpoint in PBH's downloader config: the
// plain http://host:port, no /api/v2 and no trailing slash (the loader strips a
// trailing one, and a create stores what it is given verbatim).
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

// WaitReady blocks until PBH answers or timeout passes. A refused token is
// returned at once rather than retried: no timeout makes a wrong credential
// right, and PBH blacklists an address for 15 minutes after ten failed
// attempts on one endpoint.
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

// Version is PBH's own version string, from the manifest every WebUI reads
// first. It is unauthenticated, so it is also the receipt's evidence that the
// app serving requests is the one the chart pinned.
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

// DownloaderStatus is one downloader's health and the config PBH stored for it.
// The config is the read path for the fields s5n owns - and it echoes the
// qBittorrent password in clear text, which is why nothing here ever compares
// or records one (the fingerprint in owned.json is the comparison).
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

// pbhDesired is the stored config with only the fields s5n owns overlaid, in
// the shape PBH's status endpoint reports: everything else (paused, the shadow
// ban, TLS verification, the ignore-private flag, a name a person renamed)
// survives an update. The password is a secret field: it is always written —
// empty, since qBittorrent skips its login for the release's addresses — and
// never compared.
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

// pbhCreateConfig is the whole object a create sends. PBH deserialises it into
// its own config class, which has no defaults of its own: an absent key is
// null, so every field the class has is written here.
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

// locatePbh finds the downloader the Job owns, by the same rule the Arr objects
// use: a recorded id is authoritative (a rename in PBH's UI is the user's
// business and must not create a second row), and with nothing recorded an
// existing downloader of the expected name is adopted. PBH refuses a duplicate
// id or name, so an adopt-by-name create would fail rather than duplicate -
// which makes adoption the only way a first run can take over a downloader the
// user added by hand.
func locatePbh(list []Item, ownedID, name string) (cur Item, found, recreated bool, previousID string) {
	if ownedID != "" {
		for _, item := range list {
			if item.Str("id") == ownedID {
				return item, true, false, ""
			}
		}
		return nil, false, true, ownedID
	}
	if item, ok := FindByName(list, name); ok {
		return item, true, false, ""
	}
	return nil, false, false, ""
}

// EnsureDownloader makes PBH's qBittorrent downloader match the spec and
// returns the id it lives under, which the caller records only after
// WaitHealthy has proven the write.
//
// ownedID is owned.json's record: it keeps a rename working. There is no
// fingerprint beside it any more: the downloader's only secret was the
// qBittorrent password, and that is written empty now that qBittorrent skips
// its login for the release's addresses.
func (p *Pbh) EnsureDownloader(ctx context.Context, spec PbhDownloaderSpec, ownedID string) (EnsureResult, error) {
	list, err := p.Downloaders(ctx)
	if err != nil {
		return EnsureResult{}, err
	}
	cur, found, recreated, previousID := locatePbh(list, ownedID, spec.Name)

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
	// A registered downloader always reports its config; one that does not is a
	// config s5n cannot compare, so it is written back whole rather than trusted.
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

// WaitHealthy polls until PBH reports the downloader HEALTHY, which is the only
// evidence that it really logged in to qBittorrent with the password the Job
// wrote. A create or a PATCH only registers the downloader: PBH's own poll loop
// (the ban wave, every five seconds by default) is what dials it, so the status
// is UNKNOWN for a moment and ERROR if the credential is wrong.
//
// It returns the last status it saw, so a caller can say what it waited on.
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
