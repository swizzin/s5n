package main

// migrate_apply_screen_test.go is the confirm and report as a person reads them.

import (
	"bytes"
	"github.com/swizzin/s5n/internal/migrate/report"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/say"
)

// confirmFixture is a final run over six apps: one whose login box takes over,
// one that carries a whole library, one whose check waits for its image, two that
// keep a client they have and one they cannot run, and the renames that put the
// source's data into box's layout.
func confirmFixture() apply.Summary {
	leaveWhy := "Deluge client: this box runs qBittorrent"
	instances := []apply.InstanceSummary{
		{
			App:      "autobrr",
			Staged:   apply.Staged{App: "autobrr", Files: 5, Bytes: 9 << 30},
			Changes:  []rewrite.Change{{File: "autobrr.db", Where: "client.host", What: "host", Count: 2}},
			Accounts: []rewrite.Account{{From: "the-source-owner", To: "s5n"}},
			Enable:   true,
		},
		{
			App:    "plex",
			Staged: apply.Staged{App: "plex", Files: 7, Bytes: 48 << 30},
			Carry:  "its whole data: metadata, artwork and thumbnails",
			Token:  "plex-token",
			Enable: true,
		},
		{
			App:     "qbittorrent",
			Staged:  apply.Staged{App: "qbittorrent", Files: 10, Bytes: 570 << 20},
			Changes: []rewrite.Change{{File: "qBittorrent.conf", Where: "Session\\DefaultSavePath", What: "path", Count: 1565}},
			Enable:  true,
		},
		{
			App:    "qui",
			Staged: apply.Staged{App: "qui", Files: 2, Bytes: 16 << 20},
			Enable: true,
		},
		{
			App:     "radarr",
			Staged:  apply.Staged{App: "radarr", Files: 10, Bytes: 1 << 30},
			Changes: []rewrite.Change{{File: "radarr.db", Where: "RootFolders.Path", What: "path", Count: 2937}},
			Owned:   []rewrite.Owned{{Kind: "downloadclient", ID: 1, Name: "qBT", Target: "qbittorrent"}},
			Keys:    []string{"radarr-apikey"},
			Left:    []rewrite.Left{{File: "radarr.db", Where: "DownloadClients.Settings.host", What: `"Deluge" (172.17.0.1:10365)`, Why: leaveWhy}},
			Notes:   []string{"its own login is switched off, and you sign in through the box"},
			Enable:  true,
		},
		{
			App:    "sonarr",
			Staged: apply.Staged{App: "sonarr", Files: 10, Bytes: 1 << 30},
			Keys:   []string{"sonarr-apikey"},
			Left:   []rewrite.Left{{File: "sonarr.db", Where: "DownloadClients.Settings.host", What: `"rT" (172.17.0.1:443)`, Why: leaveWhy}},
			Enable: true,
		},
	}
	return apply.Summary{
		Instances: instances,
		Renames:   []string{"Downloads to downloads", "library to media"},
		MediaRenames: []string{
			"4K/Movies to movies-4k", "4K/TV to tv-4k", "Movies to movies", "TV to tv",
		},
		Outside: []string{"cert-manager/cert-manager", "ci/qbt-ci-20261123-044730"},
	}
}

func TestTheConfirmScreenIsTheDecidingPage(t *testing.T) {
	var out bytes.Buffer
	ask := &countingAsker{}
	screen := confirmScreen{
		Head:   "box migrate apply · s5n-alice · final snapshot from 2026-11-23 04:50 UTC",
		Report: "",
	}
	confirm := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", askOptions{}, nil, "", screen)
	if _, err := confirm(confirmFixture()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	want := strings.Join([]string{
		"box migrate apply · s5n-alice · final snapshot from 2026-11-23 04:50 UTC",
		"",
		"apps           carries    changes",
		"  autobrr      9.0 GiB    2 addresses · login → s5n",
		"  plex         48.0 GiB   its whole data: metadata, artwork and thumbnails ·",
		"                          keeps its Plex token",
		"  qbittorrent  570.0 MiB  1,565 paths",
		"  qui          16.0 MiB",
		"  radarr       1.0 GiB    2,937 paths · keeps download client \"qBT\" · keeps its",
		"                          API key",
		"  sonarr       1.0 GiB    keeps its API key",
		"  ✓ data     Downloads → downloads · library → media",
		"  ✓ library  4K/Movies → movies-4k · 4K/TV → tv-4k · Movies → movies · TV → tv",
		"  ! radarr, sonarr  kept      \"Deluge\" (172.17.0.1:10365) and \"rT\"",
		"                              (172.17.0.1:443) · Deluge client: this box runs",
		"                              qBittorrent",
		"  1 note · 2 other workloads stay behind · 2 values left as they are ·",
		"    --details",
		"",
	}, "\n")
	got := out.String()
	if got != want {
		t.Fatalf("the confirm:\n%s\nwant:\n%s", got, want)
	}
}

func TestTheAppFoldFoldsTheTail(t *testing.T) {
	s := confirmFixture()
	for _, name := range []string{"autobrr-2", "prowlarr", "radarr-4k", "sonarr-4k", "tautulli"} {
		s.Instances = append(s.Instances, apply.InstanceSummary{App: name, Staged: apply.Staged{App: name, Files: 1, Bytes: 1 << 20}})
	}
	var out bytes.Buffer
	ask := &countingAsker{}
	if _, err := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", askOptions{}, nil, "", confirmScreen{})(s); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	printed := out.String()
	for _, line := range strings.Split(printed, "\n") {
		if report.Cols(line) > 80 {
			t.Errorf("%d columns: %q", report.Cols(line), line)
		}
	}
	// Eleven apps: eight rows, then one naming the three that did not fit.
	if !strings.Contains(printed, "  + 3 more  ") {
		t.Errorf("the confirm does not fold the tail of eleven apps:\n%s", printed)
	}
	for _, name := range []string{"radarr-4k", "sonarr-4k", "tautulli"} {
		if !strings.Contains(printed, name) {
			t.Errorf("the fold does not name %s, so a person cannot tell what moves:\n%s", name, printed)
		}
	}
}

func TestTheRunWritesItsOwnPage(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "migrate-20261123T045038Z-final.tar.gz")
	path := filepath.Join(dir, "migrate-20261123T045038Z-final-apply-report.txt")
	var out bytes.Buffer
	ask := &countingAsker{}
	screen := confirmScreen{
		Head:     "box migrate apply · s5n-alice · final snapshot from 2026-11-23 04:50 UTC",
		Snapshot: snapshot,
		Report:   path,
		Kind:     "final",
		Taken:    time.Date(2026, 11, 23, 4, 50, 38, 0, time.UTC).Format(time.RFC3339),
	}
	if _, err := migrateConfirm(&say.Plain{W: &out}, ask, "s5n-alice", askOptions{}, nil, "", screen)(confirmFixture()); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the run wrote no report: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the report is %04o, want 0600", mode)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"s5n migration report · final · " + filepath.Base(snapshot),
		"\nsummary\n  apps              6\n",
		"\napps\n  autobrr      9.0 GiB · 2 addresses · account \"the-source-owner\" becomes s5n\n",
		"\nnot migrated\n  cert-manager  cert-manager\n  ci            qbt-ci-20261123-044730\n",
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("the report does not say %q:\n%s", want, text)
		}
	}
}
