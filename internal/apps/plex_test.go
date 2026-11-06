package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakePlexServer(t *testing.T, claimed bool) *httptest.Server {
	t.Helper()
	claimedAttr := "0"
	if claimed {
		claimedAttr = "1"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/identity" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="0" apiVersion="1.2.3" claimed="` + claimedAttr + `" machineIdentifier="abc123" version="1.43.5.11079-b3b13e506">
</MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPlexIdentityUnclaimed(t *testing.T) {
	srv := fakePlexServer(t, false)
	p := NewPlex(srv.URL)
	ident, err := p.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if ident.Claimed {
		t.Fatal("want unclaimed")
	}
	if ident.MachineIdentifier != "abc123" {
		t.Fatalf("machineIdentifier = %q", ident.MachineIdentifier)
	}
}

func TestPlexIdentityClaimed(t *testing.T) {
	srv := fakePlexServer(t, true)
	p := NewPlex(srv.URL)
	ident, err := p.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if !ident.Claimed {
		t.Fatal("want claimed")
	}
}

func TestPlexWaitReadyRetriesUntilUp(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer claimed="0" machineIdentifier="x"></MediaContainer>`))
	}))
	defer srv.Close()

	p := NewPlex(srv.URL)
	if err := p.WaitReady(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestPlexIdentityAsksForXML(t *testing.T) {
	// Plex content-negotiates /identity: a request whose Accept names JSON gets
	// a JSON body back, which the XML parser in Identity reads as EOF. The
	// server here mimics that exactly, so this test fails if NewPlex stops
	// asking for XML.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "xml") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"claimed":false,"machineIdentifier":"abc123","version":"1"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer claimed="0" machineIdentifier="abc123" version="1"></MediaContainer>`))
	}))
	defer srv.Close()

	ident, err := NewPlex(srv.URL).Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if ident.Claimed || ident.MachineIdentifier != "abc123" {
		t.Fatalf("identity = %+v", ident)
	}
}
