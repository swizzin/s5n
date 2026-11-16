package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/notice"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/snapshot"
	"github.com/swizzin/s5n/internal/update"
)

// healthState is update.json for a health test.
type healthState struct {
	state update.State
	saves int
	load  error
}

func (s *healthState) Load() (update.State, error) {
	if s.load != nil {
		return update.State{}, s.load
	}
	return s.state, nil
}

func (s *healthState) Save(state update.State) error {
	s.saves++
	s.state = state
	return nil
}

// receiver is one httptest server that records the notices posted to it.
type receiver struct {
	url  string
	got  []notice.Notice
	seen int
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var n notice.Notice
		if err := json.Unmarshal(body, &n); err != nil {
			t.Errorf("the body is not a notice: %v\n%s", err, body)
		}
		r.got = append(r.got, n)
		r.seen++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL + "/hook?token=s3cret"
	return r
}

// healthHarness is one health read wired to a fake reading and a real receiver.
func healthHarness(t *testing.T, state *healthState, now map[string]update.AppHealth, rec *receiver) (healthRead, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return healthRead{
		Read:       func(context.Context) (map[string]update.AppHealth, error) { return now, nil },
		State:      state,
		Poster:     notice.Poster{URL: rec.url, Client: &http.Client{Timeout: 5 * time.Second}},
		Options:    notice.Options{Box: "box.example.com", Seedbox: "seedbox"},
		Out:        &say.Plain{W: &buf},
		ConfigRoot: t.TempDir(),
		Namespace:  "seedbox",
	}, &buf
}

// TestAHealthFlipPostsOneNotice: the timer tells someone when an app changes
// state, once, and keeps the new reading so the next wake is quiet.
func TestAHealthFlipPostsOneNotice(t *testing.T) {
	state := &healthState{state: update.State{
		LastCheck: time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC),
		Health: map[string]update.AppHealth{
			"sonarr": {Ready: true},
			"radarr": {Ready: false, Reason: "not ready"},
		},
	}}
	rec := newReceiver(t)
	now := map[string]update.AppHealth{
		"sonarr": {Ready: false, Reason: "restarting, 4 restarts"},
		"radarr": {Ready: false, Reason: "not ready"},
	}
	h, out := healthHarness(t, state, now, rec)
	h.run(context.Background())

	if rec.seen != 1 {
		t.Fatalf("posted %d notices, want one for the app that changed: %q", rec.seen, out)
	}
	got := rec.got[0]
	if got.Event != notice.EventUnhealthy || len(got.Apps) != 1 || got.Apps[0].App != "sonarr" {
		t.Fatalf("notice = %+v, want sonarr unhealthy", got)
	}
	if !strings.Contains(got.Text, "sonarr is not ready (restarting, 4 restarts)") {
		t.Fatalf("text = %q", got.Text)
	}
	if !strings.Contains(got.Text, "kubectl -n seedbox logs statefulset/sonarr") {
		t.Fatalf("text = %q, want the logs named for an app box has no copy of", got.Text)
	}
	if state.saves != 1 || state.state.Health["sonarr"].Ready || state.state.Health["sonarr"].Reason != "restarting, 4 restarts" {
		t.Fatalf("saved %d times with %+v, want the new reading", state.saves, state.state.Health)
	}
	if !state.state.LastCheck.Equal(time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("LastCheck moved to %v: a health read checks nothing", state.state.LastCheck)
	}
}

// TestTheFirstReadingIsSilent: a box's first wake records what it sees instead of
// paging about every app it runs.
func TestTheFirstReadingIsSilent(t *testing.T) {
	state := &healthState{}
	rec := newReceiver(t)
	h, _ := healthHarness(t, state, map[string]update.AppHealth{
		"sonarr": {Ready: false, Reason: "not ready"},
		"radarr": {Ready: true},
	}, rec)
	h.run(context.Background())
	if rec.seen != 0 {
		t.Fatalf("a first reading posted %d notices", rec.seen)
	}
	if len(state.state.Health) != 2 {
		t.Fatalf("the first reading was not recorded: %+v", state.state.Health)
	}
}

// TestACloudOfFlipsPostsInAppOrder: several apps changing is several notices,
// named in one order every time so two runs read the same.
func TestACloudOfFlipsPostsInAppOrder(t *testing.T) {
	state := &healthState{state: update.State{Health: map[string]update.AppHealth{
		"autobrr": {Ready: false, Reason: "not ready"},
		"plex":    {Ready: false, Reason: "not ready"},
		"sonarr":  {Ready: true},
	}}}
	rec := newReceiver(t)
	h, _ := healthHarness(t, state, map[string]update.AppHealth{
		"autobrr": {Ready: true},
		"plex":    {Ready: true},
		"sonarr":  {Ready: true},
	}, rec)
	h.run(context.Background())
	if len(rec.got) != 2 {
		t.Fatalf("posted %d notices, want two", len(rec.got))
	}
	if rec.got[0].Apps[0].App != "autobrr" || rec.got[1].Apps[0].App != "plex" {
		t.Fatalf("order = %s, %s", rec.got[0].Apps[0].App, rec.got[1].Apps[0].App)
	}
	if rec.got[0].Event != notice.EventHealthy {
		t.Fatalf("event = %q, want healthy", rec.got[0].Event)
	}
}

// TestTheRollbackCommandNeedsASnapshotFromTheLastStep: `box rollback` is only the
// way out when the app moved in the last step and box has that step's copy; every
// other unhealthy app is the logs.
func TestTheRollbackCommandNeedsASnapshotFromTheLastStep(t *testing.T) {
	root := t.TempDir()
	step := "0.5.1-0.5.2-20261115T031500Z"
	if err := os.MkdirAll(filepath.Join(root, ".box", "snapshots", step, "radarr"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".box", "snapshots", step, "radarr", snapshot.ManifestName), []byte(`{"app":"radarr","files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := update.State{LastStep: &update.StepRecord{From: "0.5.1", To: "0.5.2", Moved: []string{"radarr"}}}
	h := healthRead{ConfigRoot: root, Namespace: "seedbox"}

	if got := h.command("radarr", state); got != "box rollback radarr" {
		t.Fatalf("command = %q, want the rollback for an app with this step's copy", got)
	}
	// An app that did not move, one the step already put back, and one whose
	// newest copy is an older step's all keep the logs.
	for _, c := range []struct {
		name  string
		app   string
		state update.State
	}{
		{"did not move", "sonarr", state},
		{"was already put back", "radarr", update.State{LastStep: &update.StepRecord{From: "0.5.1", To: "0.5.2", Moved: []string{"radarr"}, RolledBack: []string{"radarr"}}}},
		{"no step at all", "radarr", update.State{}},
		{"another step's copy", "radarr", update.State{LastStep: &update.StepRecord{From: "0.4.0", To: "0.5.0", Moved: []string{"radarr"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := "kubectl -n seedbox logs statefulset/" + c.app
			if got := h.command(c.app, c.state); got != want {
				t.Fatalf("command = %q, want %q", got, want)
			}
		})
	}
}

// TestAHealthReadThatFailsIsOneLine: a box that cannot read its apps says so and
// keeps the record it had; the update it just did is not undone by it.
func TestAHealthReadThatFailsIsOneLine(t *testing.T) {
	state := &healthState{state: update.State{Health: map[string]update.AppHealth{"sonarr": {Ready: true}}}}
	var buf bytes.Buffer
	h := healthRead{
		Read: func(context.Context) (map[string]update.AppHealth, error) {
			return nil, errors.New("the cluster did not answer")
		},
		State: state,
		Out:   &say.Plain{W: &buf},
	}
	h.run(context.Background())
	if !strings.Contains(buf.String(), "not checking on the apps: the cluster did not answer") {
		t.Fatalf("output = %q", buf.String())
	}
	if state.saves != 0 {
		t.Fatal("a failed read overwrote the last reading")
	}
}

// TestANoticeThatCannotBePostedStillRecordsTheFlip: a receiver that is down is one
// warning naming the host and never the address, and the flip is not reported
// again on every wake.
func TestANoticeThatCannotBePostedStillRecordsTheFlip(t *testing.T) {
	state := &healthState{state: update.State{Health: map[string]update.AppHealth{"sonarr": {Ready: true}}}}
	var buf bytes.Buffer
	h := healthRead{
		Read: func(context.Context) (map[string]update.AppHealth, error) {
			return map[string]update.AppHealth{"sonarr": {Ready: false}}, nil
		},
		State:   state,
		Poster:  notice.Poster{URL: "http://127.0.0.1:1/services/T00/B00/s3cret", Client: &http.Client{Timeout: 2 * time.Second}},
		Out:     &say.Plain{W: &buf},
		Options: notice.Options{Box: "box.example.com"},
	}
	h.run(context.Background())
	if !strings.Contains(buf.String(), "posting a notice") {
		t.Fatalf("output = %q, want a warning about the notice", buf.String())
	}
	for _, secret := range []string{"services/T00/B00", "s3cret"} {
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("the warning prints the address: %q", buf.String())
		}
	}
	if state.saves != 1 || state.state.Health["sonarr"].Ready {
		t.Fatal("the flip was not recorded after a failed post")
	}
}

// TestNoticeFromMapsEveryEvent: update reports the three events; the body says
// what a person reads, and a rolled-back app's images are the right way round.
func TestNoticeFromMapsEveryEvent(t *testing.T) {
	o := notice.Options{Box: "box.example.com", Seedbox: "seedbox"}
	got := noticeFrom(o, update.Notice{
		Event: update.EventRolledBack, Name: "s5n", From: "0.5.1", To: "0.5.2",
		Apps: []update.AppChange{{App: "radarr", From: "6.3", To: "6.4"}},
	})
	if got.Event != notice.EventRolledBack || !strings.Contains(got.Text, "radarr rolled back from 6.4 to 6.3") {
		t.Fatalf("rolled-back notice = %+v", got)
	}
	got = noticeFrom(o, update.Notice{
		Event: update.EventUpdated, Name: "s5n", From: "0.5.1", To: "0.5.2",
		Apps: []update.AppChange{{App: "sonarr", From: "4.0.14", To: "4.0.15"}},
	})
	if got.Text != "box.example.com: s5n 0.5.1 → 0.5.2 (sonarr 4.0.14 → 4.0.15)" {
		t.Fatalf("updated notice = %q", got.Text)
	}
}
