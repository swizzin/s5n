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

// SaveRoot is where every download must live: the release's data claim, which
// the library is hardlinked from. internal/qbtconf imports this constant
// rather than defining its own, so the data contract's path lives in one
// place.
const SaveRoot = "/data/downloads"

// Qbit is the qBittorrent WebUI API. Every call after Login rides the session
// cookie, so the client owns a jar.
type Qbit struct {
	client   *Client
	username string
	password string
}

// NewQbit builds a client. qBittorrent has no API key and no URL base; the
// chart rewrites the /qbittorrent prefix off at the Gateway. The credentials
// are empty for s5n's own qBittorrent, whose login is bypassed for the
// release's addresses; a client built that way never signs in (see Login).
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
//
// A client built without a password signs in to nothing: qBittorrent's WebUI
// skips its login for the private ranges and for loopback, which is the state
// internal/qbtconf seals (docs/box.md, "Logins"). Posting the empty login form
// anyway would be a request that can only add to the failed-login count that
// gets an address banned.
func (q *Qbit) Login(ctx context.Context) error {
	if q.password == "" {
		return nil
	}
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

// WaitReady waits for the WebUI to answer, then logs in once (a client with no
// password has nothing to log in to; see Login). Login is not retried on a
// credential error: qBittorrent bans the address after a handful of failures, so
// a wrong secret must fail loudly and immediately.
//
// The readiness probe accepts any HTTP status: without a session the API answers
// 403 to every endpoint, and with the login bypassed for the release's
// addresses it answers 200, so a 403 is progress rather than the failure.
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

// dataContract is what qBittorrent must report for the /data contract to hold.
var dataContract = map[string]string{
	"save_path": SaveRoot,
	"temp_path": SaveRoot + "/temp",
}

// EnsureDataContract makes the settings the /data contract depends on match, then
// reads them back. qBittorrent's image starts with its save path on the config volume
// and (with automatic torrent management off) ignores category paths, so an Arr's
// download would land outside /data and could never be hardlinked. Seeding the config
// file is not enough: the image re-applies its own runtime default, which is why this
// is done through the API after the app is up, and why the write is not trusted
// until a fresh read agrees with it.
func (q *Qbit) EnsureDataContract(ctx context.Context) (Change, error) {
	prefs, err := q.preferences(ctx)
	if err != nil {
		return Unchanged, err
	}
	if contractDrift(prefs) == "" {
		return Unchanged, nil
	}
	desired := map[string]any{"use_category_paths_in_manual_mode": true}
	for k, v := range dataContract {
		desired[k] = v
	}
	payload, err := json.Marshal(desired)
	if err != nil {
		return Unchanged, fmt.Errorf("encode preferences: %w", err)
	}
	form := url.Values{"json": {string(payload)}}
	if _, err := q.client.PostForm(ctx, "/api/v2/app/setPreferences", form, nil); err != nil {
		return Unchanged, fmt.Errorf("set preferences: %w", err)
	}
	if err := q.DataContract(ctx); err != nil {
		return Unchanged, fmt.Errorf("qBittorrent accepted the preferences but did not apply them: %w", err)
	}
	return Updated, nil
}

// DataContract asserts the settings the /data contract depends on: downloads land
// exactly under the save root, and a categorised torrent actually uses its
// category's path. qBittorrent's defaults fail both — its save path is on the config
// volume, and it ignores category paths in manual mode — which silently turns
// hardlink imports into copies, or into downloads nothing will ever import.
func (q *Qbit) DataContract(ctx context.Context) error {
	prefs, err := q.preferences(ctx)
	if err != nil {
		return err
	}
	if drift := contractDrift(prefs); drift != "" {
		return fmt.Errorf("%s", drift)
	}
	return nil
}

func (q *Qbit) preferences(ctx context.Context) (Item, error) {
	var prefs Item
	if err := q.client.Get(ctx, "/api/v2/app/preferences", &prefs); err != nil {
		return nil, fmt.Errorf("read preferences: %w", err)
	}
	return prefs, nil
}

// contractDrift describes the first way prefs break the data contract, or "" when
// they hold. Paths are compared exactly (trailing slash aside): "somewhere under
// /data" is not the contract, because the Arrs' categories and root folders are
// derived from this one root.
func contractDrift(prefs Item) string {
	for _, key := range []string{"save_path", "temp_path"} {
		got, _ := prefs[key].(string)
		if strings.TrimRight(got, "/") != dataContract[key] {
			return fmt.Sprintf("%s is %q, want %q", key, got, dataContract[key])
		}
	}
	if on, _ := prefs["use_category_paths_in_manual_mode"].(bool); !on {
		return "category paths are not applied in manual mode, so categorised downloads ignore their category's save path"
	}
	return ""
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
//
// A name may be one qBittorrent subcategory ("4K/Movies"). qBittorrent 5.3
// accepts that shape — its isValidCategoryName refuses only backslashes and empty
// segments — and creates the parent category ("4K") itself. There is no
// subcategories preference to set: 5.2 removed the switch 5.0 and 5.1 had
// (default off), and the explicit save path written here is what the data
// contract relies on either way.
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
