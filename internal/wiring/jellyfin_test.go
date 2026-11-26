package wiring

// Jellyfin's first-run wizard is closed inside the pod, not by this Job. A server
// whose wizard is still open is refused, and the account the person was told to
// use is reported, never invented or renamed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/swizzin/s5n/internal/apps"
)

func TestTheJobRefusesAServerWhoseWizardIsOpen(t *testing.T) {
	r := &Runner{}
	res := &Result{}
	err := r.jellyfinWizard(res, false)
	if err == nil {
		t.Fatal("the Job completed a wizard it does not own")
	}
	if step := stepByName(t, res, "jellyfin/wizard"); step.Result != "failed" ||
		!strings.Contains(step.Detail, "init container") {
		t.Fatalf("jellyfin/wizard = %+v, want a failure naming the init container", step)
	}

	ok := &Result{}
	if err := r.jellyfinWizard(ok, true); err != nil {
		t.Fatalf("a complete wizard was refused: %v", err)
	}
	if step := stepByName(t, ok, "jellyfin/wizard"); step.Result != "ok" {
		t.Fatalf("jellyfin/wizard = %+v, want ok", step)
	}
}

// jellyfinUsers is a server with a migrated box's accounts and box's own.
func jellyfinUsers(t *testing.T, rows ...string) *apps.Jellyfin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var users []string
		for _, name := range rows {
			users = append(users, `{"Id":"`+name+`","Name":"`+name+`"}`)
		}
		_, _ = w.Write([]byte("[" + strings.Join(users, ",") + "]"))
	}))
	t.Cleanup(srv.Close)
	j := apps.NewJellyfin(srv.URL)
	j.SetToken("test-token")
	return j
}

// jellyfinNoAccount is a server neither password opens, as a migrated box is: every
// signed-in call is recorded, so a test can prove the Job stopped asking.
type jellyfinNoAccount struct {
	mu       sync.Mutex
	signIns  []string
	sessions []string
}

func (f *jellyfinNoAccount) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/System/Info/Public":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"12.4","StartupWizardCompleted":true}`))
		case "/Users/AuthenticateByName":
			var body struct{ Username string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.signIns = append(f.signIns, body.Username)
			// A 12.4 server answers 401 for an unknown name and a wrong password
			// alike (Jellyfin.Api/Middleware/ExceptionMiddleware.cs).
			w.WriteHeader(http.StatusUnauthorized)
		default:
			f.sessions = append(f.sessions, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}
}

// box holding no password for a migrated server's administrator is normal, not an
// error: the run says what to do, skips session-bound steps, and goes on.
func TestAMigratedServerWithoutASessionIsPendingNotFatal(t *testing.T) {
	r, media := newMediaHarness(t, true)
	f := &jellyfinNoAccount{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	userPassword := dir + "/jellyfin-user-password"
	boxPassword := dir + "/jellyfin-box-password"
	for _, p := range []struct{ path, value string }{
		{userPassword, "person-secret-4b1d\n"},
		{boxPassword, "box-secret-4b1d\n"},
	} {
		if err := os.WriteFile(p.path, []byte(p.value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.Config.Jellyfin = &JellyfinConfig{
		URL:              srv.URL,
		Username:         "alice",
		UserPasswordFile: userPassword,
		BoxPasswordFile:  boxPassword,
		Libraries:        []JellyfinLibrary{{Name: "tv", CollectionType: "tvshows", Path: "/data/media/tv"}},
	}

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("a server box cannot sign in to failed the whole Job: %v\n%s", err, Summary(res))
	}
	if res.Status != "ok" {
		t.Fatalf("receipt status = %q, want ok\n%s", res.Status, Summary(res))
	}
	login := stepByName(t, res, "jellyfin/login")
	if login.Result != stepPending {
		t.Fatalf("jellyfin/login = %+v, want pending", login)
	}
	for _, want := range []string{"s5n", "alice", "jellyfin-box-password"} {
		if !strings.Contains(login.Detail, want) {
			t.Errorf("the reason does not name %q: %s", want, login.Detail)
		}
	}
	if strings.Contains(login.Detail, "4b1d") {
		t.Fatalf("the reason prints a secret: %s", login.Detail)
	}
	for _, name := range []string{"jellyfin/account", "jellyfin/apikey", "jellyfin/person", "jellyfin/library/tv"} {
		got := stepByName(t, res, name)
		if got.Result != "skipped" {
			t.Fatalf("%s = %+v, want skipped with the login's reason", name, got)
		}
		if got.Detail != login.Detail {
			t.Errorf("%s detail = %q, want the login's reason", name, got.Detail)
		}
	}
	// The app wired after Jellyfin ran: nothing here stopped the run.
	if s := stepByName(t, res, "plex/ready"); s.Result != "ok" {
		t.Fatalf("plex/ready = %+v: the Job did not carry on past Jellyfin", s)
	}
	if media.pmsID != "machine-123" {
		t.Fatalf("Plex was not wired: pmsID = %q", media.pmsID)
	}
	// Only what a signed-out caller may ask: two sign-ins, no session call.
	if got := strings.Join(f.signIns, ","); got != "s5n,alice" {
		t.Fatalf("the Job tried to sign in as %q, want the box's account and the person's", got)
	}
	if len(f.sessions) > 0 {
		t.Fatalf("the Job made signed-in calls with no session: %v", f.sessions)
	}
}

// A 5xx or an unreachable server is not "box lacks this password": that is a broken
// release.
func TestAServerThatRefusesForAnotherReasonStillFails(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"12.4","StartupWizardCompleted":true}`))
		default:
			http.Error(w, "kaboom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(broken.Close)
	r := &Runner{}
	res := &Result{}
	c := &JellyfinConfig{Username: "alice"}
	if _, err := r.jellyfinAccount(context.Background(), res, c, apps.NewJellyfin(broken.URL), "pw", "pw"); err == nil {
		t.Fatalf("a 500 from the sign-in was reported as a missing account\n%s", Summary(res))
	}
	if s := stepByName(t, res, "jellyfin/login"); s.Result != "failed" {
		t.Fatalf("jellyfin/login = %+v, want failed", s)
	}
}

func TestThePersonAccountIsCheckedNotCreated(t *testing.T) {
	r := &Runner{}
	c := &JellyfinConfig{Username: "alice"}
	res := &Result{}
	if err := r.jellyfinPerson(context.Background(), res, c, jellyfinUsers(t, "alice", "s5n")); err != nil {
		t.Fatalf("jellyfinPerson: %v", err)
	}
	if step := stepByName(t, res, "jellyfin/person"); step.Result != "ok" || step.Detail != "alice" {
		t.Fatalf("jellyfin/person = %+v, want ok alice", step)
	}
}

// The rewriter renames nothing, so a source server keeps its account names: the Job
// reports them and creates nothing beyond the automation account.
func TestAMigratedServersAccountsAreReported(t *testing.T) {
	r := &Runner{}
	c := &JellyfinConfig{Username: "alice"}
	res := &Result{}
	if err := r.jellyfinPerson(context.Background(), res, c, jellyfinUsers(t, "bob", "s5n")); err != nil {
		t.Fatalf("jellyfinPerson: %v", err)
	}
	step := stepByName(t, res, "jellyfin/person")
	if step.Result != stepPending {
		t.Fatalf("jellyfin/person = %+v, want pending", step)
	}
	for _, want := range []string{"alice", "bob", "s5n"} {
		if !strings.Contains(step.Detail, want) {
			t.Errorf("the detail does not name %q: %s", want, step.Detail)
		}
	}
}
