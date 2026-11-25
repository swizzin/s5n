package wiring

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// TestWiringWiresSabnzbd: the usenet client is the second download client, and
// every Arr gets it beside qBittorrent with its own category. The two paths are
// the data contract's; on the image's defaults every import would be a copy.
func TestWiringWiresSabnzbd(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.stack.sabnzbdHandler("/sabnzbd", "sab-key"))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(h.dir, "sabnzbd.key")
	if err := os.WriteFile(keyFile, []byte("sab-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.config.Sabnzbd = &SabnzbdConfig{
		URL: srv.URL, URLBase: "/sabnzbd", APIKeyFile: keyFile,
		SavePath: "/data/downloads", DownloadDir: "/data/downloads/incomplete",
	}
	h.runner.Config = h.config

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	for _, name := range []string{
		"sabnzbd/ready", "sabnzbd/data-contract",
		"sabnzbd/category/tv", "sabnzbd/category/movies",
		"sonarr/sabnzbd", "radarr/sabnzbd",
	} {
		if step := stepByName(t, res, name); step.Result != "ok" {
			t.Errorf("%s = %+v, want ok", name, step)
		}
	}
	if got := h.stack.sabMisc["complete_dir"]; got != "/data/downloads" {
		t.Errorf("complete_dir = %q, want the data claim", got)
	}
	if got := h.stack.sabMisc["download_dir"]; got != "/data/downloads/incomplete" {
		t.Errorf("download_dir = %q, want the data claim", got)
	}
	for _, cat := range []string{"tv", "movies"} {
		if got := h.stack.sabCats[cat]; got != cat {
			t.Errorf("category %s dir = %q, want %q", cat, got, cat)
		}
	}
	for instance, category := range map[string]string{"sonarr": "tv", "radarr": "movies"} {
		var got apps.Item
		for _, c := range h.stack.clients[instance] {
			if strings.EqualFold(c.Str("implementation"), "Sabnzbd") {
				got = c
			}
		}
		if got == nil {
			t.Fatalf("%s has no SABnzbd client: %v", instance, h.stack.clients[instance])
		}
		if v, _ := got.FieldValue("urlBase"); v != "/sabnzbd" {
			t.Errorf("%s urlBase = %v, want /sabnzbd", instance, v)
		}
		if v, _ := got.FieldValue("apiKey"); v != "sab-key" {
			t.Errorf("%s apiKey = %v, want the release Secret's", instance, v)
		}
		if v, _ := got.FieldValue(arrCategoryField(instance)); v != category {
			t.Errorf("%s category = %v, want %s", instance, v, category)
		}
	}

	writes := h.stack.count()
	second, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if got := h.stack.count(); got != writes {
		t.Fatalf("a second run made %d writes", got-writes)
	}
	for _, name := range []string{"sabnzbd/data-contract", "sabnzbd/category/tv", "sonarr/sabnzbd"} {
		if step := stepByName(t, second, name); step.Detail != "unchanged" {
			t.Errorf("second run %s = %+v, want unchanged", name, step)
		}
	}
}

// TestWiringAdoptsAMigratedSabnzbdClient: a migrated Arr's database already has
// a SABnzbd client row, under the operator's own name and pointing at the
// release's SABnzbd after the rewrite. The Job adopts it by its address instead
// of creating a second one, and the receipt says it changed the fields it owns.
func TestWiringAdoptsAMigratedSabnzbdClient(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.stack.sabnzbdHandler("/sabnzbd", "sab-key"))
	t.Cleanup(srv.Close)
	keyFile := filepath.Join(h.dir, "sabnzbd.key")
	if err := os.WriteFile(keyFile, []byte("sab-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.config.Sabnzbd = &SabnzbdConfig{
		URL: srv.URL, URLBase: "/sabnzbd", APIKeyFile: keyFile,
		SavePath: "/data/downloads", DownloadDir: "/data/downloads/incomplete",
	}
	h.runner.Config = h.config

	// The row a source install leaves behind: its own name, no url base, its
	// own category. The rewrite points the host at the release's Service.
	host, _ := mustHostPort(t, srv.URL)
	h.stack.clients["sonarr"] = []apps.Item{{
		"id": 9, "name": "SAB", "implementation": "Sabnzbd", "enable": true, "protocol": "nzb", "priority": 1, "tags": []any{},
		"fields": []any{
			map[string]any{"name": "host", "value": host},
			map[string]any{"name": "port", "value": 8080},
			map[string]any{"name": "useSsl", "value": false},
			map[string]any{"name": "urlBase", "value": "/"},
			map[string]any{"name": "username", "value": ""},
			map[string]any{"name": "password", "value": ""},
			map[string]any{"name": "apiKey", "value": "********"},
			map[string]any{"name": "tvCategory", "value": "TV"},
		},
	}}

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	step := stepByName(t, res, "sonarr/sabnzbd")
	if step.Result != "ok" || step.Detail != "updated" {
		t.Fatalf("sonarr/sabnzbd = %+v, want updated", step)
	}
	var sabs []apps.Item
	for _, c := range h.stack.clients["sonarr"] {
		if strings.EqualFold(c.Str("implementation"), "Sabnzbd") {
			sabs = append(sabs, c)
		}
	}
	if len(sabs) != 1 {
		t.Fatalf("sonarr has %d SABnzbd clients, want the adopted one only", len(sabs))
	}
	adopted := sabs[0]
	if adopted.Int("id") != 9 || adopted.Str("name") != "SAB" {
		t.Fatalf("the original row was not adopted: %+v", adopted)
	}
	if v, _ := adopted.FieldValue("urlBase"); v != "/sabnzbd" {
		t.Errorf("adopted urlBase = %v, want the chart's prefix", v)
	}
	if v, _ := adopted.FieldValue("tvCategory"); v != "tv" {
		t.Errorf("adopted category = %v, want the instance's", v)
	}
	if h.stack.clients["sonarr"][0].Int("id") != 9 {
		t.Fatalf("the adopted client is not first in the list: %v", h.stack.clients["sonarr"])
	}
}
