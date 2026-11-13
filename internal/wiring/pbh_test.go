package wiring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

// fakePbh is PeerBanHelper's WebAPI as the wiring Job meets it: an envelope
// around every answer, downloaders keyed by a string id, PUT creating, PATCH
// replacing, and a status endpoint that reports what its own poll loop found.
// The health is a switch so a test can make it healthy, stuck or refused.
type fakePbh struct {
	mu        sync.Mutex
	token     string
	order     []string
	cfg       map[string]apps.Item
	health    string // lastStatus answered for every downloader; default HEALTHY
	healthMsg string
	puts      int
	patches   int
	statuses  int
}

func (f *fakePbh) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePbh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.write(w, http.StatusForbidden, map[string]any{"success": false, "message": "Not logged in", "data": nil})
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/metadata/manifest":
		f.write(w, http.StatusOK, map[string]any{"success": true, "message": nil,
			"data": map[string]any{"version": map[string]any{"version": "9.5.4"}}})

	case path == "/api/downloaders" && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, id := range f.order {
			c := f.cfg[id]
			out = append(out, map[string]any{"id": id, "name": c.Str("name"),
				"endpoint": c.Str("endpoint") + "/api/v2", "type": c.Str("type"), "paused": false})
		}
		f.write(w, http.StatusOK, map[string]any{"success": true, "message": nil, "data": out})

	case path == "/api/downloaders" && r.Method == http.MethodPut:
		f.puts++
		var body struct {
			ID     string    `json:"id"`
			Config apps.Item `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, dup := f.cfg[body.ID]; dup {
			f.write(w, http.StatusBadRequest, map[string]any{"success": false,
				"message": "Downloader creation failed: already exists", "data": nil})
			return
		}
		f.order = append(f.order, body.ID)
		f.cfg[body.ID] = body.Config
		f.write(w, http.StatusCreated, map[string]any{"success": true, "message": "Downloader created successfully", "data": nil})

	case strings.HasPrefix(path, "/api/downloaders/") && r.Method == http.MethodPatch:
		f.patches++
		id := strings.TrimPrefix(path, "/api/downloaders/")
		var body struct {
			Config apps.Item `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.cfg[id] = body.Config
		f.write(w, http.StatusOK, map[string]any{"success": true, "message": "Downloader updated successfully", "data": nil})

	case strings.HasPrefix(path, "/api/downloaders/") && strings.HasSuffix(path, "/status"):
		f.statuses++
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/downloaders/"), "/status")
		c, ok := f.cfg[id]
		if !ok {
			f.write(w, http.StatusNotFound, map[string]any{"success": false, "message": "Downloader not exists", "data": nil})
			return
		}
		status, msg := f.health, f.healthMsg
		if status == "" {
			status = "HEALTHY"
		}
		f.write(w, http.StatusOK, map[string]any{"success": true, "message": nil, "data": map[string]any{
			"lastStatus": status, "lastStatusMessage": msg, "config": c, "paused": false}})

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// withPbh points the harness at a PeerBanHelper whose token is the release
// Secret's pbh-token, and returns the fake so a test can read what was stored.
func (h *harness) withPbh(t *testing.T) *fakePbh {
	t.Helper()
	f := &fakePbh{token: "pbh-token-abc", cfg: map[string]apps.Item{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	h.config.Pbh = &PbhConfig{URL: srv.URL, TokenFile: h.write(t, "pbh-token", "pbh-token-abc\n")}
	h.runner.Config = h.config
	h.pbh = f
	return f
}

// PBH holds one qBittorrent downloader, and it is recorded only once PBH's own
// status says HEALTHY - which is the proof it logged in with the password
// written. A second run then writes nothing.
func TestWiringRegistersPbhDownloader(t *testing.T) {
	h := newHarness(t)
	f := h.withPbh(t)
	ctx := context.Background()

	first, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, Summary(first))
	}
	step := stepByName(t, first, "pbh/downloader")
	if step.Result != "ok" || step.Detail != "created" {
		t.Fatalf("pbh/downloader = %+v, want created", step)
	}
	if ready := stepByName(t, first, "pbh/ready"); ready.Result != "ok" || !strings.Contains(ready.Detail, "9.5.4") {
		t.Fatalf("pbh/ready = %+v, want the app's version", ready)
	}
	stored := f.cfg["qbittorrent"]
	if stored == nil {
		t.Fatal("PBH holds no downloader named qbittorrent")
	}
	// The endpoint has no /api/v2 and no trailing slash: PBH appends its own.
	if got := stored.Str("endpoint"); got != h.config.Qbittorrent.URL {
		t.Fatalf("PBH dials %q, want the Job's plain qBittorrent URL %q", got, h.config.Qbittorrent.URL)
	}
	if got := stored.Str("password"); got != "hunter2" {
		t.Fatalf("PBH holds %q, want the release Secret's password", got)
	}

	rec := ownedOf(t, h)
	if rec.PbhDownloaderID() != "qbittorrent" || rec.PbhQbitPasswordFingerprint() == "" {
		t.Fatalf("pbh not recorded: %+v", rec.Pbh)
	}
	if strings.Contains(h.cmData["owned.json"], "hunter2") {
		t.Fatal("owned.json carries the qBittorrent password")
	}

	second, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, Summary(second))
	}
	if second.Writes.Created != 0 || second.Writes.Updated != 0 {
		t.Fatalf("second run was not a no-op: %+v\n%s", second.Writes, Summary(second))
	}
	if f.puts != 1 || f.patches != 0 {
		t.Fatalf("PBH saw %d PUTs and %d PATCHes, want 1 and 0", f.puts, f.patches)
	}
	if again := stepByName(t, second, "pbh/downloader"); again.Detail != "unchanged" {
		t.Fatalf("pbh/downloader on the second run = %+v, want unchanged", again)
	}
}

// A downloader PBH cannot log in to is a failed step, not a recorded one: the
// fingerprint must not be written for a write the app never accepted, or the
// next rotation would be invisible.
func TestWiringPbhWaitsForHealthy(t *testing.T) {
	h := newHarness(t)
	f := h.withPbh(t)
	f.health = "ERROR"
	f.healthMsg = "Unable to connect to qBittorrent: login refused"

	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with an unhealthy PBH downloader\n%s", Summary(res))
	}
	step := stepByName(t, res, "pbh/downloader")
	if step.Result != "failed" || !strings.Contains(step.Detail, "login refused") {
		t.Fatalf("pbh/downloader = %+v, want failed with PBH's message", step)
	}
	if got := ownedOf(t, h).PbhQbitPasswordFingerprint(); got != "" {
		t.Fatalf("a fingerprint was recorded for an unhealthy downloader: %q", got)
	}
}

// Without qBittorrent there is nothing for PBH to read, and the step says so
// rather than failing.
func TestWiringSkipsPbhWithoutQbittorrent(t *testing.T) {
	h := newHarness(t)
	h.withPbh(t)
	h.config.Qbittorrent = nil
	h.runner.Config = h.config
	h.runner.Config.Instances = nil

	res, err := h.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, Summary(res))
	}
	if step := stepByName(t, res, "pbh/downloader"); step.Result != "skipped" {
		t.Fatalf("pbh/downloader = %+v, want skipped", step)
	}
}

// A token that is not PBH's is a release credential error with the sentence an
// operator can act on, and it is not retried into PBH's own fail2ban.
func TestWiringNamesARejectedPbhToken(t *testing.T) {
	h := newHarness(t)
	h.withPbh(t)
	h.write(t, "pbh-token", "not-the-token\n")
	h.config.Pbh.TokenFile = h.dir + "/pbh-token"
	h.runner.Config = h.config

	res, err := h.runner.Run(context.Background())
	if err == nil {
		t.Fatalf("run succeeded with PBH refusing the token\n%s", Summary(res))
	}
	step := stepByName(t, res, "pbh/ready")
	if step.Result != "failed" || !strings.Contains(step.Detail, "pbh-token") {
		t.Fatalf("pbh/ready = %+v, want the Secret key named", step)
	}
}
