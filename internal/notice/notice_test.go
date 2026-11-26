package notice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Both fields a receiver might read carry the same sentence.
func TestSentences(t *testing.T) {
	o := Options{Box: "box.example.com", Seedbox: "seedbox"}
	for _, c := range []struct {
		name string
		n    Notice
		want string
	}{
		{
			name: "a step that moved and an app",
			n: Updated(o, "s5n", "0.5.1", "0.5.2", []App{
				{App: "sonarr", From: "4.0.14", To: "4.0.15"},
				{App: "radarr", From: "6.3", To: "6.4"},
			}),
			want: "box.example.com: s5n 0.5.1 → 0.5.2 (sonarr 4.0.14 → 4.0.15, radarr 6.3 → 6.4)",
		},
		{
			name: "a step that moved no image",
			n:    Updated(o, "s5n", "0.5.1", "0.5.2", nil),
			want: "box.example.com: s5n 0.5.1 → 0.5.2",
		},
		{
			name: "an app whose images box could not name",
			n:    Updated(o, "s5n", "0.5.1", "0.5.2", []App{{App: "plex"}}),
			want: "box.example.com: s5n 0.5.1 → 0.5.2 (plex)",
		},
		{
			name: "an app put back",
			n:    RolledBack(o, "s5n", "radarr", "6.3", "6.4"),
			want: "box.example.com: s5n: radarr rolled back from 6.4 to 6.3, because it kept exiting on its new image",
		},
		{
			name: "a step that stopped, and the release the box is on",
			n:    Stopped(o, "s5n", "0.5.1", "0.5.2", "health", "sonarr exited 3 times on its new image", "0.5.1"),
			want: "box.example.com: s5n 0.5.1 → 0.5.2 stopped at the health gate: sonarr exited 3 times on its new image; the box is on 0.5.1",
		},
		{
			name: "a step that stopped and left no release to read",
			n:    Stopped(o, "s5n", "0.5.1", "0.5.2", "apply", "the apply failed", ""),
			want: "box.example.com: s5n 0.5.1 → 0.5.2 stopped at the apply gate: the apply failed; the release's state could not be read",
		},
		{
			name: "an app that is not ready, with the way out",
			n:    Health(o, "radarr", false, "restarting, 4 restarts", "box rollback radarr"),
			want: "box.example.com: radarr is not ready (restarting, 4 restarts); box rollback radarr",
		},
		{
			name: "an app that is ready again",
			n:    Health(o, "radarr", true, "", ""),
			want: "box.example.com: radarr is ready again",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.n.Text != c.want {
				t.Errorf("text = %q, want %q", c.n.Text, c.want)
			}
			if c.n.Content != c.want {
				t.Errorf("content = %q, want %q", c.n.Content, c.want)
			}
		})
	}
}

func TestPostSendsTheBodyAndBothSentences(t *testing.T) {
	var got Notice
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("the body is not JSON: %v\n%s", err, body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	o := Options{Box: "box.example.com", Seedbox: "seedbox"}
	n := Updated(o, "s5n", "0.5.1", "0.5.2", []App{{App: "sonarr", From: "4.0.14", To: "4.0.15"}})
	if err := (Poster{URL: srv.URL + "/hook"}).Post(context.Background(), n); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	if got.Event != EventUpdated || got.Box != "box.example.com" || got.Seedbox != "seedbox" {
		t.Errorf("the event fields came out as %+v", got)
	}
	if got.From != "0.5.1" || got.To != "0.5.2" || len(got.Apps) != 1 || got.Apps[0].To != "4.0.15" {
		t.Errorf("what moved came out as %+v", got)
	}
	if got.Text != got.Content || got.Text != n.Text {
		t.Errorf("text %q and content %q are not the sentence %q", got.Text, got.Content, n.Text)
	}
	// Optional fields are omitted, not sent empty: a receiver can tell "no gate"
	// from "the gate was empty".
	if strings.Contains(string(mustJSON(t, got)), `"gate"`) {
		t.Errorf("an updated notice carried a gate field")
	}
}

func TestPostReportsAStatusWithoutThePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()

	secret := "/services/T00/B00/s3cret?token=abc123"
	err := (Poster{URL: srv.URL + secret}).Post(context.Background(), Health(Options{}, "radarr", false, "not ready", ""))
	if err == nil {
		t.Fatal("Post hid a 500")
	}
	assertNoSecret(t, err.Error(), secret)
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error does not name the status: %q", err)
	}
}

// A refused connection is net/http's *url.Error, whose message is the whole URL.
func TestPostNeverPrintsTheAddressWhenTheTransportFails(t *testing.T) {
	secret := "/services/T00/B00/s3cret?token=abc123"
	client := &http.Client{Timeout: 2 * time.Second}
	err := (Poster{URL: "http://127.0.0.1:1" + secret, Client: client}).Post(context.Background(), Health(Options{}, "radarr", false, "", ""))
	if err == nil {
		t.Fatal("Post hid a refused connection")
	}
	assertNoSecret(t, err.Error(), secret)
	if !strings.Contains(err.Error(), "http://127.0.0.1:1") {
		t.Errorf("the error does not name the host: %q", err)
	}
}

func TestPostWithNoAddressPostsNothing(t *testing.T) {
	if err := (Poster{Client: &http.Client{Timeout: time.Millisecond}}).Post(context.Background(), Health(Options{}, "radarr", true, "", "")); err != nil {
		t.Fatalf("Post with no address: %v", err)
	}
}

func TestFlips(t *testing.T) {
	stored := map[string]Reading{
		"sonarr": {Ready: true},
		"radarr": {Ready: false, Reason: "not ready"},
		"plex":   {Ready: true},
	}
	for _, c := range []struct {
		name   string
		before map[string]Reading
		now    map[string]Reading
		want   []string
	}{
		{
			name:   "a reason that changed alone is not a change",
			before: stored,
			now: map[string]Reading{
				"sonarr": {Ready: true},
				"radarr": {Ready: false, Reason: "restarting, 4 restarts"},
				"plex":   {Ready: true},
			},
			want: nil,
		},
		{
			name:   "two apps changed, in app order",
			before: stored,
			now: map[string]Reading{
				"sonarr": {Ready: false, Reason: "not ready"},
				"radarr": {Ready: true},
				"plex":   {Ready: true},
			},
			want: []string{"radarr", "sonarr"},
		},
		{
			name:   "the first reading of a box",
			before: nil,
			now: map[string]Reading{
				"sonarr": {Ready: false, Reason: "not ready"},
				"radarr": {Ready: false, Reason: "not ready"},
			},
			want: nil,
		},
		{
			name:   "an app enabled since the last wake",
			before: stored,
			now: map[string]Reading{
				"sonarr":  {Ready: true},
				"radarr":  {Ready: false, Reason: "not ready"},
				"plex":    {Ready: true},
				"autobrr": {Ready: false, Reason: "not ready"},
			},
			want: nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Flips(c.before, c.now)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("Flips = %v, want %v", got, c.want)
			}
		})
	}
}

// assertNoSecret fails when a message carries the address's secret: its path or
// query.
func assertNoSecret(t *testing.T, msg, secret string) {
	t.Helper()
	path, query, _ := strings.Cut(secret, "?")
	for _, part := range []string{secret, path, query} {
		if part == "" {
			continue
		}
		if strings.Contains(msg, part) {
			t.Fatalf("the message carries the address: %q in %q", part, msg)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
