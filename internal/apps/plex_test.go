package apps

import (
	"context"
	"errors"
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

// plexTokenServer serves /library/sections the way a claimed Plex does: 200 to
// the server token, 401 to anything else. /identity is deliberately absent:
// VerifyToken must not be able to pass by reading an unauthenticated endpoint.
func plexTokenServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("X-Plex-Token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer size="0"></MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPlexVerifyTokenAcceptsAValidToken(t *testing.T) {
	srv := plexTokenServer(t, "good-token")
	if err := NewPlex(srv.URL).VerifyToken(context.Background(), "good-token"); err != nil {
		t.Fatalf("VerifyToken with the server's token: %v", err)
	}
}

// A token Plex does not accept must be a distinguishable, token-free error: the
// wiring receipt prints it verbatim.
func TestPlexVerifyTokenRejectsUnauthorized(t *testing.T) {
	srv := plexTokenServer(t, "good-token")
	const bad = "bad-token-xyz"
	err := NewPlex(srv.URL).VerifyToken(context.Background(), bad)
	if err == nil {
		t.Fatal("VerifyToken accepted a token Plex rejected")
	}
	var rejected *TokenRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("error is %T (%v), want *TokenRejectedError", err, err)
	}
	if rejected.Status != http.StatusUnauthorized {
		t.Fatalf("TokenRejectedError.Status = %d, want 401", rejected.Status)
	}
	if got, want := err.Error(), "plex-token rejected by Plex (401)"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("error contains the token: %q", err)
	}
}

// The token must travel in X-Plex-Token, never the query string: a URL carrying
// it would end up in url.Error/HTTPError text and from there in the receipt.
func TestPlexVerifyTokenKeepsTokenOutOfTheURL(t *testing.T) {
	const token = "secret-token-abc123"
	var query, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, path = r.URL.RawQuery, r.URL.Path
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := NewPlex(srv.URL).VerifyToken(context.Background(), token)
	if err == nil {
		t.Fatal("VerifyToken accepted a 500")
	}
	if query != "" {
		t.Fatalf("token asked for in the query string: %q", query)
	}
	if path != "/library/sections" {
		t.Fatalf("path = %q", path)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error contains the token: %q", err)
	}
}

func TestPlexTokenFingerprintIsStableAndHidesTheToken(t *testing.T) {
	const token = "secret-token-abc123"
	fp := PlexTokenFingerprint(token)
	if len(fp) != 64 {
		t.Fatalf("fingerprint = %q, want 64 hex chars", fp)
	}
	if strings.Contains(fp, token) {
		t.Fatalf("fingerprint contains the token: %q", fp)
	}
	if fp != PlexTokenFingerprint(token) {
		t.Fatal("fingerprint is not stable for the same token")
	}
	if fp == PlexTokenFingerprint(token+"x") {
		t.Fatal("two different tokens share a fingerprint")
	}
}
