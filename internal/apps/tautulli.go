package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Tautulli is the Plex watch-history/analytics app. There is no documented
// "set_settings" api/v2 command (only get_settings), so the only way to write
// its PMS_* settings programmatically is the same /configUpdate endpoint its
// own settings page posts to. That endpoint skips CSRF only under /api, so it
// needs a session cookie plus the CSRF token embedded in the /settings page,
// both obtained unauthenticated (Tautulli ships with web auth disabled by
// default and no setup wizard required for the API or this endpoint to work).
type Tautulli struct {
	client *Client
	apiKey string
}

// NewTautulli builds a client. apiKey is seeded into config.ini by the
// tautulli-config init container (see cmd/s5n), not read back from a live app:
// Tautulli's own generated key is not reachable from the wiring Job, which has
// no mount into this app's /config.
func NewTautulli(baseURL, apiKey string) *Tautulli {
	jar, _ := cookiejar.New(nil)
	c := NewClient(baseURL, &http.Client{Timeout: 60 * time.Second, Jar: jar})
	return &Tautulli{client: c, apiKey: apiKey}
}

func (t *Tautulli) api(cmd string, extra url.Values) string {
	v := url.Values{"apikey": {t.apiKey}, "cmd": {cmd}}
	for k, vals := range extra {
		v[k] = vals
	}
	return "/api/v2?" + v.Encode()
}

type apiResponse struct {
	Response struct {
		Result  string `json:"result"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	} `json:"response"`
}

func (t *Tautulli) call(ctx context.Context, cmd string, extra url.Values, out *apiResponse) error {
	if err := t.client.Get(ctx, t.api(cmd, extra), out); err != nil {
		return err
	}
	if out.Response.Result != "success" {
		return fmt.Errorf("tautulli %s: %s", cmd, out.Response.Message)
	}
	return nil
}

// Ping is the readiness probe: cmd=status succeeds once the seeded api_key is
// live and api_enabled is on, which is also proof the init container's config
// seed took.
func (t *Tautulli) Ping(ctx context.Context) error {
	var out apiResponse
	return t.call(ctx, "status", nil, &out)
}

// WaitReady blocks until the API answers or timeout passes.
func (t *Tautulli) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, t.Ping)
}

// PMSSettings is the subset of Tautulli's "PMS" settings section the data
// contract depends on. Token is a pointer because get_settings may withhold it
// (see EnsurePlexConnection): null and "the stored token is empty" must not
// look the same.
type PMSSettings struct {
	IP         string
	Port       int
	Identifier string
	Token      *string
	APIEnabled bool
}

// currentPMS reads the live PMS section (and General.api_enabled) through the
// documented, read-only get_settings command.
func (t *Tautulli) currentPMS(ctx context.Context) (PMSSettings, error) {
	var out struct {
		Response struct {
			Result string `json:"result"`
			Data   struct {
				General struct {
					APIEnabled int `json:"api_enabled"`
				} `json:"General"`
				PMS struct {
					IP         string  `json:"pms_ip"`
					Port       int     `json:"pms_port"`
					Identifier string  `json:"pms_identifier"`
					Token      *string `json:"pms_token"`
				} `json:"PMS"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := t.client.Get(ctx, t.api("get_settings", nil), &out); err != nil {
		return PMSSettings{}, fmt.Errorf("get_settings: %w", err)
	}
	if out.Response.Result != "success" {
		return PMSSettings{}, fmt.Errorf("get_settings failed")
	}
	return PMSSettings{
		IP:         out.Response.Data.PMS.IP,
		Port:       out.Response.Data.PMS.Port,
		Identifier: out.Response.Data.PMS.Identifier,
		Token:      out.Response.Data.PMS.Token,
		APIEnabled: out.Response.Data.General.APIEnabled != 0,
	}, nil
}

var csrfTokenRE = regexp.MustCompile(`csrf-token"\s+content="([^"]*)"`)

// csrfToken fetches /settings to establish a session and read the CSRF token
// embedded in its page, both of which /configUpdate requires. /settings is a
// cherrypy page, not part of the stdlib-friendly JSON API, so this is a plain
// text scrape of one meta tag rather than parsing HTML properly.
func (t *Tautulli) csrfToken(ctx context.Context) (string, error) {
	text, err := t.client.GetText(ctx, "/settings")
	if err != nil {
		return "", fmt.Errorf("fetch /settings for a CSRF token: %w", err)
	}
	m := csrfTokenRE.FindStringSubmatch(text)
	if m == nil {
		return "", fmt.Errorf("no csrf-token meta tag in /settings")
	}
	return m[1], nil
}

// EnsurePlexConnection points Tautulli's PMS section at the release's Plex,
// with the Plex server token (X-Plex-Token) supplied in the release Secret, and
// proves the connection with get_server_identity, Tautulli's equivalent of the
// Arr apps' own /test endpoint: it makes Tautulli fetch identity from the live
// Plex server, so a wrong host/port cannot pass as "configured".
//
// That proof is reachability and identity, not authorization: Tautulli's
// get_server_identity reads Plex's /identity, which answers unauthenticated on
// every build, so it cannot tell a valid token from an invalid one. Callers
// must prove the token against Plex's authenticated API first
// (apps.Plex.VerifyToken); by the time this runs the token is known good.
//
// A token is mandatory. Plex must never be configured to trust a network range
// instead (the chart's apps.plex.noAuthNetworks defaults to empty): the public
// Gateway's Envoy proxy connects to Plex from its own pod IP, which is inside
// the cluster pod CIDR, so a trusted range there would hand anyone who can
// reach the Gateway unauthenticated admin of the server. An empty token is
// therefore an error here rather than a tokenless write.
//
// The token cannot always be compared: Tautulli lists PMS_TOKEN among its
// config's do-not-download keys, so get_settings returns it as JSON null rather
// than as a value. A null (or a "********" placeholder from a build that masks
// it) is "unknown", so the comparison falls back to knownFingerprint, the
// sha256-hex fingerprint of the token last written and proven (see
// apps.PlexTokenFingerprint). An absent or different fingerprint is a rotation:
// the write goes out again. With no record at all — a first run on an existing
// release — the link is rewritten once, which is the only safe reading of
// "unknown". When the value is readable a difference is real drift, and the
// write below always carries the Secret's token, never the value just read: a
// masked placeholder must never be stored back (see MatchesFields' masked-value
// rule).
//
// api_enabled is resent on every write: /configUpdate treats an absent
// checkbox field as "unchecked" (CherryPy form semantics), so omitting it
// would silently disable Tautulli's own API on the very call that configures it.
func (t *Tautulli) EnsurePlexConnection(ctx context.Context, host string, port int, machineIdentifier, token, knownFingerprint string) (Change, error) {
	if strings.TrimSpace(token) == "" {
		return Unchanged, fmt.Errorf("no Plex token: refusing to configure a tokenless PMS link")
	}
	cur, err := t.currentPMS(ctx)
	if err != nil {
		return Unchanged, err
	}
	if cur.Identifier == machineIdentifier && cur.IP == host && cur.Port == port && cur.APIEnabled &&
		plexTokenMatches(cur.Token, token, knownFingerprint) {
		return Unchanged, nil
	}
	change := Updated
	if cur.Identifier == "" {
		change = Created
	}

	csrf, err := t.csrfToken(ctx)
	if err != nil {
		return Unchanged, err
	}
	form := url.Values{
		"pms_ip":         {host},
		"pms_port":       {strconv.Itoa(port)},
		"pms_identifier": {machineIdentifier},
		"pms_name":       {"s5n-plex"},
		"pms_ssl":        {"0"},
		"pms_is_remote":  {"0"},
		"pms_token":      {token},
		"api_enabled":    {"1"},
	}
	req := t.client.BaseURL + "/configUpdate"
	var saveResult struct {
		Result  string `json:"result"`
		Message string `json:"message"`
	}
	hdr := t.client.Header.Clone()
	hdr.Set("X-CSRF-Token", csrf)
	if err := postForm(ctx, t.client, req, form, hdr, &saveResult); err != nil {
		return Unchanged, fmt.Errorf("configUpdate: %w", err)
	}
	if saveResult.Result != "success" {
		return Unchanged, fmt.Errorf("configUpdate: %s", saveResult.Message)
	}

	var ident apiResponse
	if err := t.call(ctx, "get_server_identity", nil, &ident); err != nil {
		return Unchanged, fmt.Errorf("prove Plex connection: %w", err)
	}
	if m, ok := ident.Response.Data.(map[string]any); ok {
		if got, _ := m["machine_identifier"].(string); got != machineIdentifier {
			return Unchanged, fmt.Errorf("get_server_identity returned %q, want %q", got, machineIdentifier)
		}
	}
	return change, nil
}

// plexTokenMatches reports whether the PMS token Tautulli has stored is the one
// in the release Secret. live is nil when get_settings withheld the token
// (PMS_TOKEN is a do-not-download key there, so it arrives as JSON null); a
// masked placeholder is treated the same way. Both mean "cannot compare", so
// the answer comes from knownFingerprint: the sha256-hex fingerprint of the
// token the Job last wrote and proved. An absent fingerprint means no successful
// write is on record, and a different one means the Secret's token was rotated
// since — both are differences. A genuinely empty stored token is a difference
// too.
func plexTokenMatches(live *string, want, knownFingerprint string) bool {
	if live != nil && *live == "" {
		return false
	}
	if live == nil || masked(*live) {
		return knownFingerprint != "" && knownFingerprint == PlexTokenFingerprint(want)
	}
	return *live == want
}

// postForm is a small helper for the one non-JSON, cookie-and-header-carrying
// POST this package needs; Client.PostForm does not thread extra headers or a
// response struct through, and duplicating Client here would re-implement its
// cookie jar.
func postForm(ctx context.Context, c *Client, url string, form url.Values, hdr http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	for k, vals := range hdr {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return &HTTPError{Method: http.MethodPost, URL: url, Status: resp.StatusCode}
	}
	return decodeJSON(resp, out)
}

// decodeJSON reads, decodes and closes a response body. Tautulli's
// /configUpdate is the one call in this package that needs custom headers
// (X-CSRF-Token) alongside a JSON response, which Client's own helpers do not
// thread through.
func decodeJSON(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
