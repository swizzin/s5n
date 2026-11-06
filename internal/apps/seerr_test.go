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
