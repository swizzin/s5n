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

// locatePbh finds the downloader the Job owns:
//
//   - a recorded id is authoritative: a rename in PBH's UI is the user's
//     business and must not create a second row, even when another downloader
//     points at the same endpoint;
//   - with the recorded id missing (or nothing recorded), the downloader this
//     box would own is adopted before one is created. The recorded id can be
//     stale without anyone deleting anything: PBH's own config migration 32
//     re-keys every downloader to a generated UUID and keeps the old key as its
//     name (MainConfigUpdateScript.assignUniqueIdForDownloader at v9.5.4), so a
//     migration that adopted a pre-UUID source's downloader under the source's
//     key records an id the app no longer has. The endpoint is what makes two
//     downloaders the same one for box (the release's qBittorrent, which the pbh
//     rewriter already wrote), and the name is the fallback;
//   - only when neither exists was it deleted, so the run recreates it and says
//     which id disappeared.
//
// PBH's own duplicate check is narrower than the endpoint match: registering a
// downloader is refused when an existing one has the same id or the same name
// (DownloaderManagerImpl.registerDownloader at v9.5.4, which is what answers
// "a similar downloader configuration already exists"), and the endpoint is
// never compared. A create beside a re-keyed copy is therefore allowed by PBH
// and leaves two downloaders polling one qBittorrent - which is why the
// endpoint is looked for first, and why two of them are a refusal rather than a
// pick.
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

// pbhEndpointMatches returns the downloaders whose endpoint is the one the spec
// names. The list reports each downloader's *API* endpoint - the configured
// endpoint with /api/v2 appended (AbstractQbittorrent.java:59,217, and
// PBHDownloaderController.handleDownloaderList) - while the spec and the status
// endpoint carry the plain one, so both sides are reduced to it first.
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
