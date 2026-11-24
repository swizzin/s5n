package main

// migrate_screen_test.go is the two reads' screens as a golden: the migration
// report is the page a person reads in full (rule 7), so the screen keeps the
// counts and the file's name, and cutover's refusal is one line of state and one
// command per kind that stops what is running.
//
// Both are built from a saved source and an injected stop proof: no cluster, no
// API server.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/say"
)

// fixedClock is when the fixture source was read, so the golden holds a time
// rather than a regexp.
var fixedClock = func() time.Time { return time.Date(2026, 11, 23, 4, 50, 38, 0, time.UTC) }

// screenReporter is the transcript a test reads back, with a clock and a working
// directory the fixture chose: the screens below are bytes, whatever the host.
func screenReporter(t *testing.T) (say.Reporter, func() string) {
	t.Helper()
	buf := &strings.Builder{}
	return &say.Plain{W: buf, Err: buf}, buf.String
}

// TestTheDiscoverScreenIsCountsAndTheReport: a read writes the report and says
// only how much it read, what it decided, and where the file is. Every list that
// used to be on this screen -- the apps, the addresses, the called-out decisions,
// the notes -- is in the report, which is the same text the snapshot carries.
func TestTheDiscoverScreenIsCountsAndTheReport(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "migrate-20261123T045038Z-survey.tar.gz")
	o := readOf(t)
	o.Out, o.FromDir, o.Root = out, manifests, root
	o.Now = fixedClock
	reporter, screen := screenReporter(t)

	if _, err := migrateDiscover(context.Background(), o, reporter); err != nil {
		t.Fatalf("discover: %v\n%s", err, screen())
	}
	want := strings.Join([]string{
		"box migrate discover · survey from 2026-11-23 04:50 UTC",
		"✓ read           2 apps · 0 rewritten · 0 left as it is",
		"· snapshot       " + out,
		"· report         " + filepath.Join(dir, "migrate-20261123T045038Z-survey-report.txt"),
		"                 $ less " + filepath.Join(dir, "migrate-20261123T045038Z-survey-report.txt"),
		"",
	}, "\n")
	if got := screen(); got != want {
		t.Fatalf("the screen:\n%s\nwant:\n%s", got, want)
	}
	// The report beside the snapshot is the report inside it, and it is the
	// page: the summary first, then the sections.
	reportPath := filepath.Join(dir, "migrate-20261123T045038Z-survey-report.txt")
	info, err := os.Stat(reportPath)
	if err != nil {
		t.Fatalf("no report beside the snapshot: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the report is %04o, want 0600", mode)
	}
	beside, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"s5n migration report · survey · migrate-20261123T045038Z-survey.tar.gz",
		"\nsummary\n",
		"\napps\n  qbittorrent  from qbittorrent · in media · config /srv/apps/qbittorrent\n",
		"\ndata\n  root     /srv/library\n  renames  Downloads → downloads\n",
		"\npaths\n  qbittorrent /config      left as it is\n",
	} {
		if !strings.Contains(string(beside), want) {
			t.Errorf("the report does not say %q:\n%s", want, beside)
		}
	}
}

// TestTheCutoverStopListIsOneCommand: a source that still runs is refused with
// one line of state and one command that stops everything of a kind, written for
// the cluster box found, and nothing trailing it. The read is kept first, so
// running what it prints is always safe.
func TestTheCutoverStopListIsOneCommand(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(dir, "final.tar.gz")
	o.Now = fixedClock
	o.Replace = true
	reporter, screen := screenReporter(t)

	running := []discover.Running{
		{App: "sonarr", Kind: "Deployment", Namespace: "media", Name: "sonarr", Replicas: 1, Pods: []string{"sonarr-abc"}},
		{App: "qbittorrent", Kind: "Deployment", Namespace: "media", Name: "qbittorrent", Replicas: 1, Pods: []string{"qbittorrent-abc"}},
		{App: "prowlarr", Kind: "StatefulSet", Namespace: "media", Name: "prowlarr", Replicas: 1, Pods: []string{"prowlarr-0"}},
	}
	err := cutover(context.Background(), o, reporter, proofOf(running...))
	if !errors.Is(err, errCutoverStillRunning) {
		t.Fatalf("cutover = %v, want the still-running refusal", err)
	}
	want := strings.Join([]string{
		"cutover",
		"! 3 apps running · stop them, then run this again",
		"  $ kubectl -n media scale deployment --replicas=0 qbittorrent sonarr",
		"  $ kubectl -n media scale statefulset --replicas=0 prowlarr",
		"",
	}, "\n")
	if got := screen(); got != want {
		t.Fatalf("the refusal:\n%s\nwant:\n%s", got, want)
	}
	// The read was kept before the commands were printed: following them is
	// always safe, and the survey holds this read for the next cutover.
	surveys, err := filepath.Glob(filepath.Join(dir, "migrate-*-survey.tar.gz"))
	if err != nil || len(surveys) != 1 {
		t.Fatalf("the refusal kept %v, want one survey (%v)", surveys, err)
	}
}

// TestACutoverStopCommandWrapsLikeAShell: a source with enough apps to overflow
// the column gets a command a person can copy: continued with a trailing
// backslash, never broken inside a name, and every line inside 80 columns.
func TestACutoverStopCommandWrapsLikeAShell(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Replace = true
	reporter, screen := screenReporter(t)

	var running []discover.Running
	for _, name := range []string{
		"autobrr", "maintainerr", "peerbanhelper", "plex", "qbittorrent", "qui",
		"radarr", "radarr4k", "overseerr", "sonarr", "sonarr4k", "tautulli",
	} {
		running = append(running, discover.Running{
			App: name, Kind: "Deployment", Namespace: "default", Name: name,
			Replicas: 1, Pods: []string{name + "-abc"},
		})
	}
	if err := cutover(context.Background(), o, reporter, proofOf(running...)); !errors.Is(err, errCutoverStillRunning) {
		t.Fatalf("cutover = %v, want the still-running refusal", err)
	}
	lines := strings.Split(strings.TrimRight(screen(), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("the refusal did not wrap:\n%s", screen())
	}
	var command []string
	for _, line := range lines[1:] {
		if len(line) > 80 {
			t.Errorf("%d columns: %q", len(line), line)
		}
		command = append(command, strings.TrimSuffix(strings.TrimSpace(line), " \\"))
	}
	// Joined back up it is one kubectl call naming every app.
	joined := strings.Join(command, " ")
	for _, want := range []string{
		"scale deployment --replicas=0 autobrr maintainerr overseerr peerbanhelper",
		"plex qbittorrent qui radarr radarr4k sonarr sonarr4k tautulli",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the command does not say %q: %s", want, joined)
		}
	}
}
