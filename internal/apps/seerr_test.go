package apps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSeerrWaitReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/settings/public" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"initialized":false}`))
	}))
	defer srv.Close()

	s := NewSeerr(srv.URL)
	if err := s.WaitReady(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

// initialized is what tells a box with an admin from one nobody signed in to; a
// migrated Seerr carries it true.
func TestSeerrSettingsReadsInitialized(t *testing.T) {
	for _, want := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/settings/public" {
				http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"initialized":` + map[bool]string{true: "true", false: "false"}[want] + `}`))
		}))
		s := NewSeerr(srv.URL)
		got, err := s.Settings(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("Settings: %v", err)
		}
		if got.Initialized != want {
			t.Fatalf("Settings.Initialized = %v, want %v", got.Initialized, want)
		}
	}
}

// A refused read is an error, never "no admin": a guess would misstate the row.
func TestSeerrSettingsRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := NewSeerr(srv.URL).Settings(context.Background()); err == nil {
		t.Fatal("Settings accepted a 503")
	}
}

// The sign-in is the Jellyfin account's, against the address Seerr dials, with the
// server type that makes Seerr a Jellyfin instance rather than a Plex one, and the
// session it sets is what every settings call after it rides on.
func TestSeerrLoginJellyfinSendsTheAccountAndKeepsTheSession(t *testing.T) {
	var body map[string]any
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/auth/jellyfin":
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("sign-in body: %v", err)
			}
			http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "s%3Aabc.def", Path: "/", HttpOnly: true})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"jellyfinUsername":"alice"}`))
		case "/api/v1/settings/jellyfin":
			if got := r.Header.Get("Cookie"); got != "connect.sid=s%3Aabc.def" {
				t.Errorf("settings call carried Cookie %q, want the session's", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := NewSeerr(srv.URL)
	if err := s.LoginJellyfin(context.Background(), "alice", "hunter2", "jellyfin", 8096); err != nil {
		t.Fatalf("LoginJellyfin: %v", err)
	}
	want := map[string]any{
		"username": "alice", "password": "hunter2",
		"hostname": "jellyfin", "port": float64(8096),
		"urlBase": "", "useSsl": false, "serverType": float64(2),
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("sign-in body %s = %#v, want %#v (body %v)", k, body[k], v, body)
		}
	}
	if err := s.SetExternalHostname(context.Background(), "https://jellyfin.example.test"); err != nil {
		t.Fatalf("SetExternalHostname: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("calls = %v, want the sign-in and the settings write", seen)
	}
}

// A server refusing the password and an account that is not its administrator are the
// person's to settle: each is its own typed error carrying Seerr's code, never a
// generic failure, and neither may hold the password.
func TestSeerrLoginJellyfinRefusalsKeepSeerrsCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusUnauthorized, "INVALID_CREDENTIALS"},
		{http.StatusForbidden, "NOT_ADMIN"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"` + tc.code + `","errors":""}`))
		}))
		s := NewSeerr(srv.URL)
		err := s.LoginJellyfin(context.Background(), "alice", "wrong-password", "jellyfin", 8096)
		srv.Close()
		var refused *SeerrSignInRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("%d: err = %v, want SeerrSignInRefusedError", tc.status, err)
		}
		if refused.Code != tc.code {
			t.Fatalf("%d: code = %q, want %q", tc.status, refused.Code, tc.code)
		}
		if refused.Username != "alice" {
			t.Fatalf("%d: username = %q", tc.status, refused.Username)
		}
		if got := err.Error(); strings.Contains(got, "wrong-password") {
			t.Fatalf("%d: the error carries the password: %q", tc.status, got)
		}
	}
}

// A Seerr that answers the sign-in without a session leaves every later call
// unauthenticated: that is an error here, not a run that finds out at the next step.
func TestSeerrLoginJellyfinRequiresASession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	err := NewSeerr(srv.URL).LoginJellyfin(context.Background(), "alice", "pw", "jellyfin", 8096)
	if err == nil {
		t.Fatal("a sign-in with no session cookie was accepted")
	}
}

// The setup sequence is the setup page's: sync the server's libraries, switch on the
// ones this release made, then initialize. Each call is the app's own endpoint.
func TestSeerrJellyfinSetupSequence(t *testing.T) {
	var calls []string
	var enabled []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/jellyfin":
			http.SetCookie(w, &http.Cookie{Name: "connect.sid", Value: "s%3Asession", Path: "/"})
			_, _ = w.Write([]byte(`{"id":1}`))
		case "/api/v1/settings/jellyfin/library/sync":
			_, _ = w.Write([]byte(`[{"id":"a1","name":"Movies","type":"movies","enabled":false},` +
				`{"id":"b2","name":"TV","type":"tvshows","enabled":true}]`))
		case "/api/v1/settings/jellyfin/library/a1":
			var body struct {
				Enabled bool `json:"enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			enabled = append(enabled, "a1")
			_, _ = w.Write([]byte(`{"id":"a1","name":"Movies","enabled":true}`))
		case "/api/v1/settings/jellyfin":
			_, _ = w.Write([]byte(`{"externalHostname":"https://jellyfin.example.test"}`))
		case "/api/v1/settings/initialize":
			_, _ = w.Write([]byte(`{"initialized":true}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := NewSeerr(srv.URL)
	ctx := context.Background()
	if err := s.LoginJellyfin(ctx, "alice", "pw", "jellyfin", 8096); err != nil {
		t.Fatalf("LoginJellyfin: %v", err)
	}
	libs, err := s.SyncLibraries(ctx)
	if err != nil {
		t.Fatalf("SyncLibraries: %v", err)
	}
	if len(libs) != 2 || libs[0].Name != "Movies" || !libs[1].Enabled {
		t.Fatalf("SyncLibraries = %+v", libs)
	}
	if err := s.SetLibraryEnabled(ctx, "a1", true); err != nil {
		t.Fatalf("SetLibraryEnabled: %v", err)
	}
	if err := s.SetExternalHostname(ctx, "https://jellyfin.example.test"); err != nil {
		t.Fatalf("SetExternalHostname: %v", err)
	}
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	want := []string{
		"POST /api/v1/auth/jellyfin",
		"POST /api/v1/settings/jellyfin/library/sync",
		"PUT /api/v1/settings/jellyfin/library/a1",
		"POST /api/v1/settings/jellyfin",
		"POST /api/v1/settings/initialize",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
	if len(enabled) != 1 {
		t.Fatalf("library writes = %v, want the one library", enabled)
	}
}

// A refusal from one of the setup endpoints is Seerr's own message, not a bare status:
// the person reads the code the app answered with.
func TestSeerrSetupRefusalCarriesSeerrsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/settings/jellyfin/library/sync" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"SYNC_ERROR_NO_LIBRARIES"}`))
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewSeerr(srv.URL).SyncLibraries(context.Background())
	if err == nil {
		t.Fatal("SyncLibraries accepted a 404")
	}
	if got := err.Error(); !strings.Contains(got, "SYNC_ERROR_NO_LIBRARIES") {
		t.Fatalf("error = %q, want Seerr's own message in it", got)
	}
}
