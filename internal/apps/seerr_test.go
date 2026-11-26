package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
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
