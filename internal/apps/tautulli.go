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

// Tautulli is the Plex watch-history app. There is no set_settings API command, so
// the PMS_* settings are written through the same /configUpdate endpoint its settings
// page posts to: that needs a session cookie and the CSRF token from /settings, both
// obtained unauthenticated because the app's own login is off.
type Tautulli struct {
	client *Client
	apiKey string
}

// NewTautulli builds a client; apiKey is seeded into config.ini by the init
// container, because the Job has no mount into the app's /config.
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

// Ping is the readiness probe: status succeeds once the seeded key is live and the
// API on, which also proves the config seed took.
func (t *Tautulli) Ping(ctx context.Context) error {
	var out apiResponse
	return t.call(ctx, "status", nil, &out)
}

// WaitReady blocks until the API answers or timeout passes.
func (t *Tautulli) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, t.Ping)
}

// PMSSettings is the PMS section the data contract depends on. Token is a pointer
// because get_settings may withhold it: null and an empty token must not look
// alike.
type PMSSettings struct {
	IP         string
	Port       int
	Identifier string
	Token      *string
	APIEnabled bool
}

// currentPMS reads the live PMS section and api_enabled through get_settings.
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

// csrfToken fetches /settings for a session and the CSRF token /configUpdate
// requires: a plain scrape of one meta tag, not an HTML parse.
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

// EnsurePlexConnection points the PMS section at the release's Plex with the
// Secret's token, and proves it with get_server_identity: address and identity, not
// authorization, because that call reads Plex's unauthenticated /identity. Callers
// prove the token against Plex first (apps.Plex.VerifyToken).
//
// A token is mandatory: trusting a network range instead would hand anyone who can
// reach the Gateway admin of the server, because Envoy connects from the pod CIDR.
//
// Tautulli returns PMS_TOKEN as null (a do-not-download key), so the comparison falls
// back to knownFingerprint; an absent or different one is a rotation. The write always
// carries the Secret's token, never a value just read.
//
// api_enabled is resent: /configUpdate treats an absent checkbox as unchecked, which
// would disable the API on the call that configures it.
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

// plexTokenMatches reports whether the stored PMS token is the Secret's. A nil or
// masked live value cannot be compared, so knownFingerprint decides; an absent one
// means no write on record, and an empty stored token is a difference.
func plexTokenMatches(live *string, want, knownFingerprint string) bool {
	if live != nil && *live == "" {
		return false
	}
	if live == nil || masked(*live) {
		return knownFingerprint != "" && knownFingerprint == PlexTokenFingerprint(want)
	}
	return *live == want
}

// postForm is for the one POST needing extra headers and a response struct;
// Client.PostForm threads neither.
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

// decodeJSON reads, decodes and closes a response body for /configUpdate.
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
