package apps

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Sabnzbd is SABnzbd's own API (sabnzbd/api.py at 5.1.3). Every call is a GET
// under the app's url_base with mode=..., and output=json is what makes it
// answer JSON instead of XML.
//
// Its settings are not files the Job may write: the app keeps them in memory
// and rewrites sabnzbd.ini whole on every save, and a server password it stores
// is obfuscated ("!!!encoded!!!" + hexlify, sabnzbd/config.py:1224-1240), so the
// only honest way in is the API. The three settings the chart seals before the
// first start are asserted here as well, because an image upgrade or a source's
// ini may put them back and a job that downloaded to /config can never be
// hardlinked.
type Sabnzbd struct {
	client  *Client
	urlBase string
	apiKey  string
}

// NewSabnzbd builds a client. urlBase is the prefix the chart serves the app
// under ("/sabnzbd"), which is also where its API lives.
func NewSabnzbd(baseURL, urlBase, apiKey string) *Sabnzbd {
	return &Sabnzbd{client: NewClient(baseURL, nil), urlBase: strings.TrimRight(urlBase, "/"), apiKey: apiKey}
}

// call is one API request. mode=version is the only mode that needs no key; the
// rest send it, and the app answers 403 to a wrong one.
func (s *Sabnzbd) call(ctx context.Context, mode string, params url.Values, out any) error {
	q := url.Values{"mode": {mode}, "output": {"json"}}
	for k, v := range params {
		q[k] = v
	}
	if mode != "version" {
		q.Set("apikey", s.apiKey)
	}
	return s.client.Get(ctx, s.urlBase+"/api?"+q.Encode(), out)
}

// Ping asks the one endpoint SABnzbd answers without a key. It is the chart's
// readiness path too (mode=version is exempt from check_apikey,
// sabnzbd/api.py:1081 and interface.py:397-420 at 5.1.3).
func (s *Sabnzbd) Ping(ctx context.Context) error {
	var body Item
	if err := s.call(ctx, "version", nil, &body); err != nil {
		return err
	}
	if body.Str("version") == "" {
		return fmt.Errorf("version answered %v, want a version", body)
	}
	return nil
}

// WaitReady blocks until the app answers or timeout passes.
func (s *Sabnzbd) WaitReady(ctx context.Context, timeout time.Duration) error {
	err := Retry(ctx, timeout, s.Ping)
	if err != nil && IsForbidden(err) {
		return fmt.Errorf("%w: SABnzbd refused the request, so its host_whitelist or X-Forwarded-For setting does not admit this address", err)
	}
	return err
}

// Version returns the app version string.
func (s *Sabnzbd) Version(ctx context.Context) (string, error) {
	var body Item
	if err := s.call(ctx, "version", nil, &body); err != nil {
		return "", err
	}
	return body.Str("version"), nil
}

// Config is the part of SABnzbd's [misc] the data contract depends on.
type Config struct {
	// CompleteDir is where a finished download is left: the directory an Arr
	// imports from.
	CompleteDir string
	// DownloadDir is where a job is unpacked while it runs. It must be on the
	// same filesystem as CompleteDir, or SAB's own move is a copy and the
	// Arr's import can never be a hardlink.
	DownloadDir string
}

// Config reads the two download paths back.
func (s *Sabnzbd) config(ctx context.Context) (Config, error) {
	var out Config
	for _, item := range []struct {
		key string
		dst *string
	}{{"complete_dir", &out.CompleteDir}, {"download_dir", &out.DownloadDir}} {
		value, _, err := s.misc(ctx, item.key)
		if err != nil {
			return Config{}, err
		}
		*item.dst = value
	}
	return out, nil
}

// misc reads one setting out of [misc].
func (s *Sabnzbd) misc(ctx context.Context, keyword string) (string, bool, error) {
	var body Item
	err := s.call(ctx, "get_config", url.Values{fieldSection: {"misc"}, fieldKeyword: {keyword}}, &body)
	if err != nil {
		return "", false, fmt.Errorf("read misc.%s: %w", keyword, err)
	}
	cfg, _ := body["config"].(map[string]any)
	misc, _ := cfg["misc"].(map[string]any)
	value, ok := misc[keyword].(string)
	return value, ok, nil
}

// EnsureDataContract makes the two download paths match and reads them back.
//
// SABnzbd's image starts with Downloads/incomplete and Downloads/complete
// relative to its config directory (sabnzbd/constants.py:61-62, and
// cfg.py:743's set_root(home)), which is the qBittorrent /config/Downloads trap
// again: every job would be unpacked on the config claim, where an Arr's import
// could only copy. The chart seals them before the first start
// (internal/sabconf); this repairs a file an image upgrade rewrote and proves
// the running app agrees, which is the only state a job actually uses.
func (s *Sabnzbd) EnsureDataContract(ctx context.Context, want Config) (Change, error) {
	cur, err := s.config(ctx)
	if err != nil {
		return Unchanged, err
	}
	if cur == want {
		return Unchanged, nil
	}
	for _, item := range []struct{ key, want, got string }{
		{"complete_dir", want.CompleteDir, cur.CompleteDir},
		{"download_dir", want.DownloadDir, cur.DownloadDir},
	} {
		if item.got == item.want {
			continue
		}
		if err := s.setMisc(ctx, item.key, item.want); err != nil {
			return Unchanged, err
		}
	}
	got, err := s.config(ctx)
	if err != nil {
		return Unchanged, err
	}
	if got != want {
		return Unchanged, fmt.Errorf("SABnzbd accepted the download paths but reports complete_dir %q and download_dir %q, want %q and %q",
			got.CompleteDir, got.DownloadDir, want.CompleteDir, want.DownloadDir)
	}
	return Updated, nil
}

func (s *Sabnzbd) setMisc(ctx context.Context, keyword, value string) error {
	err := s.call(ctx, "set_config", url.Values{fieldSection: {"misc"}, fieldKeyword: {keyword}, "value": {value}}, nil)
	if err != nil {
		return fmt.Errorf("set misc.%s: %w", keyword, err)
	}
	return nil
}

// Category is one SABnzbd category and the directory under complete_dir its
// jobs are unpacked into.
type SabnzbdCategory struct {
	Name string
	Dir  string
}

// categories lists the configured categories.
func (s *Sabnzbd) categories(ctx context.Context) (map[string]SabnzbdCategory, error) {
	var body Item
	if err := s.call(ctx, "get_config", url.Values{fieldSection: {"categories"}}, &body); err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	out := map[string]SabnzbdCategory{}
	cfg, _ := body["config"].(map[string]any)
	list, _ := cfg["categories"].([]any)
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		name, _ := item["name"].(string)
		dir, _ := item["dir"].(string)
		out[name] = SabnzbdCategory{Name: name, Dir: dir}
	}
	return out, nil
}

// Category reads one category, and reports whether it exists.
func (s *Sabnzbd) Category(ctx context.Context, name string) (SabnzbdCategory, bool, error) {
	list, err := s.categories(ctx)
	if err != nil {
		return SabnzbdCategory{}, false, err
	}
	c, ok := list[strings.ToLower(name)]
	return c, ok, nil
}

// EnsureCategory creates a category with its directory, or repairs the
// directory of one that exists, and reads it back.
//
// The name is what the Arr sends, lower-cased: SAB's own set_config handler
// folds every category name with name.lower() (sabnzbd/api.py:1349-1366), and
// its job path folds the incoming category the same way before looking it up
// (sabnzbd/misc.py:282-296, cat_to_opts). The directory keeps the spelling the
// Arrs use -- /data/downloads/4K/Movies -- because that is the directory the
// data contract and qBittorrent's category already name; only the lookup key is
// folded.
//
// An unknown category is the silent failure this exists for: SAB falls back to
// "*" with no directory (sabnzbd/config.py:1153-1159, get_category), so a job
// unpacks into the root of complete_dir and the Arr's import still works, while
// the box's category layout is wrong forever. The read-back is what proves the
// name survived the app's own section-name cleaning.
func (s *Sabnzbd) EnsureCategory(ctx context.Context, name, dir string) (Change, error) {
	key := strings.ToLower(name)
	cur, ok, err := s.Category(ctx, key)
	if err != nil {
		return Unchanged, err
	}
	if ok && cur.Dir == dir {
		return Unchanged, nil
	}
	change := Created
	if ok {
		change = Updated
	}
	// pp=3 is the post-processing SAB's own "*" category uses (download,
	// unpack, repair and delete); script=None is no script; priority=0 is
	// normal. Every one of them is SAB's own default for a new category.
	form := url.Values{
		fieldSection: {"categories"},
		fieldKeyword: {key},
		fieldName:    {key},
		"dir":        {dir},
		"pp":         {"3"},
		"script":     {"None"},
		"priority":   {"0"},
	}
	if err := s.call(ctx, "set_config", form, nil); err != nil {
		return Unchanged, fmt.Errorf("set category %q: %w", key, err)
	}
	got, ok, err := s.Category(ctx, key)
	if err != nil {
		return Unchanged, err
	}
	if !ok || got.Dir != dir {
		return Unchanged, fmt.Errorf("SABnzbd accepted category %q but reports dir %q (present %v), want %q", key, got.Dir, ok, dir)
	}
	return change, nil
}
