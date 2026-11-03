package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// saveRoot is where every download must live: the release's data claim, which the
// library is hardlinked from.
const saveRoot = "/data/torrents"

// Qbit is the qBittorrent WebUI API. Every call after Login rides the session
// cookie, so the client owns a jar.
type Qbit struct {
	client   *Client
	username string
	password string
}

// NewQbit builds an authenticated client. qBittorrent has no API key and no URL
// base; the chart rewrites the /qbittorrent prefix off at the Gateway.
func NewQbit(baseURL, username, password string) *Qbit {
	jar, _ := cookiejar.New(nil)
	c := NewClient(baseURL, &http.Client{Timeout: 60 * time.Second, Jar: jar})
	// qBittorrent's CSRF protection compares Referer with the Host header.
	c.Header.Set("Referer", c.BaseURL)
	return &Qbit{client: c, username: username, password: password}
}

// Login authenticates and keeps the session cookie. A wrong password is reported
// as a credential error, not retried: qBittorrent bans an address after a handful
// of failures.
func (q *Qbit) Login(ctx context.Context) error {
	form := url.Values{"username": {q.username}, "password": {q.password}}
	text, err := q.client.PostForm(ctx, "/api/v2/auth/login", form, nil)
	if err != nil {
		return err
	}
	switch strings.TrimSpace(text) {
	case "Ok.":
		return nil
	case "Fails.":
		return fmt.Errorf("qBittorrent rejected the WebUI credentials for %q", q.username)
	case "":
		// Newer builds answer 204 with no body on success.
		return nil
	default:
		return fmt.Errorf("unexpected qBittorrent login response %q", strings.TrimSpace(text))
	}
}

// WaitReady waits for the WebUI to answer, then logs in once. Login is not
// retried on a credential error: qBittorrent bans the address after a handful of
// failures, so a wrong secret must fail loudly and immediately.
//
// The readiness probe accepts any HTTP status: every API endpoint except the login
// form answers 403 until a session exists, so a 403 is progress, not a failure.
func (q *Qbit) WaitReady(ctx context.Context, timeout time.Duration) error {
	err := Retry(ctx, timeout, func(ctx context.Context) error {
		if err := q.client.AnyStatus(ctx, "/api/v2/app/version"); err != nil {
			return fmt.Errorf("WebUI not answering: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return q.Login(ctx)
}

// Version returns the app version string.
func (q *Qbit) Version(ctx context.Context) (string, error) {
	s, err := q.client.GetText(ctx, "/api/v2/app/version")
	return strings.TrimSpace(s), err
}

// EnsureDataContract makes the two settings the /data contract depends on match,
// then leaves them alone. qBittorrent's image starts with its save path on the config
// volume and (with automatic torrent management off) ignores category paths, so an
// Arr's download would land outside /data and could never be hardlinked. Seeding the
// config file is not enough: the image re-applies its own runtime default, which is
// why this is done through the API after the app is up.
func (q *Qbit) EnsureDataContract(ctx context.Context) (Change, error) {
	var prefs Item
	if err := q.client.Get(ctx, "/api/v2/app/preferences", &prefs); err != nil {
		return Unchanged, fmt.Errorf("read preferences: %w", err)
	}
	savePath, _ := prefs["save_path"].(string)
	categoryPaths, _ := prefs["use_category_paths_in_manual_mode"].(bool)
	if strings.HasPrefix(savePath, "/data/") && categoryPaths {
		return Unchanged, nil
	}
	desired := map[string]any{
		"save_path":                         saveRoot,
		"temp_path":                         saveRoot + "/temp",
		"use_category_paths_in_manual_mode": true,
	}
	payload, err := json.Marshal(desired)
	if err != nil {
		return Unchanged, fmt.Errorf("encode preferences: %w", err)
	}
	form := url.Values{"json": {string(payload)}}
	if _, err := q.client.PostForm(ctx, "/api/v2/app/setPreferences", form, nil); err != nil {
		return Unchanged, fmt.Errorf("set preferences: %w", err)
	}
	return Updated, nil
}

// DataContract asserts the two settings the /data contract depends on: everything
// downloads under /data, and a categorised torrent actually uses its category's path.
// qBittorrent's defaults fail both — its save path is on the config volume, and it
// ignores category paths in manual mode — which silently turns hardlink imports into
// copies, or into downloads nothing will ever import.
func (q *Qbit) DataContract(ctx context.Context) error {
	var prefs Item
	if err := q.client.Get(ctx, "/api/v2/app/preferences", &prefs); err != nil {
		return fmt.Errorf("read preferences: %w", err)
	}
	savePath, _ := prefs["save_path"].(string)
	if !strings.HasPrefix(savePath, "/data/") {
		return fmt.Errorf("save path %q is outside /data", savePath)
	}
	categoryPaths, _ := prefs["use_category_paths_in_manual_mode"].(bool)
	if !categoryPaths {
		return fmt.Errorf("category paths are not applied in manual mode, so categorised downloads land in %s", savePath)
	}
	return nil
}

// Category is one qBittorrent category.
type Category struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// Categories lists the categories that exist.
func (q *Qbit) Categories(ctx context.Context) (map[string]Category, error) {
	out := map[string]Category{}
	if err := q.client.Get(ctx, "/api/v2/torrents/categories", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EnsureCategory creates a category, or corrects its save path. The save path is
// what makes imports hardlink instead of copy, so a wrong one is repaired rather
// than tolerated.
func (q *Qbit) EnsureCategory(ctx context.Context, name, savePath string) (Change, error) {
	cats, err := q.Categories(ctx)
	if err != nil {
		return Unchanged, fmt.Errorf("list categories: %w", err)
	}
	cur, ok := cats[name]
	if ok && strings.TrimRight(cur.SavePath, "/") == strings.TrimRight(savePath, "/") {
		return Unchanged, nil
	}
	action := "createCategory"
	change := Created
	if ok {
		action = "editCategory"
		change = Updated
	}
	form := url.Values{"category": {name}, "savePath": {savePath}}
	if _, err := q.client.PostForm(ctx, "/api/v2/torrents/"+action, form, nil); err != nil {
		return Unchanged, fmt.Errorf("%s %q: %w", action, name, err)
	}
	return change, nil
}
