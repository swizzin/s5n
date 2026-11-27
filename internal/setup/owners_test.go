package setup

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// ---- the account that owns the files ----------------------------------------

// dataWith puts one file in the data path, owned by uid:gid.
func dataWith(h *fakeHost, uid, gid int) {
	const path = "/srv/s5n/data"
	h.listing[path] = []DirEntry{{Name: "downloads", IsDir: true}}
	h.owners[path] = owner{uid, gid}
}

// grantMap is the grant init wrote, decoded.
func grantMap(t *testing.T, h *harness) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(h.read(h.grantPath)), &doc); err != nil {
		t.Fatalf("the grant is not yaml: %v", err)
	}
	return doc
}

// grantID is one id from the grant init wrote.
func grantID(t *testing.T, h *harness, key string) int {
	t.Helper()
	n, ok := intAt(grantMap(t, h), key)
	if !ok {
		t.Fatalf("the grant has no %s:\n%s", key, h.read(h.grantPath))
	}
	return n
}

// An install must not change who owns a data path that holds files: the account is
// the answer, and sudo's ids are the fallback for an empty path.
func TestFilesOwnerComesFromTheDataPath(t *testing.T) {
	h := newHarness(t)
	dataWith(h.host, 1500, 1600)
	opts := h.unattended()
	opts.Apps = []string{"alpha"}
	opts.PUID, opts.PGID = 1000, 1000

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if got := grantID(t, h, "global.puid"); got != 1500 {
		t.Fatalf("puid = %d, want the data path's owner 1500", got)
	}
	if got := grantID(t, h, "global.pgid"); got != 1600 {
		t.Fatalf("pgid = %d, want the data path's group 1600", got)
	}
}

// An empty data path is nobody's yet, so what sudo carried is the answer.
func TestFilesOwnerFallsBackToTheInvokingUser(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{"alpha"}
	opts.PUID, opts.PGID = 1000, 1001

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if got, want := grantID(t, h, "global.puid"), 1000; got != want {
		t.Fatalf("puid = %d, want the invoking user's %d", got, want)
	}
	if got, want := grantID(t, h, "global.pgid"), 1001; got != want {
		t.Fatalf("pgid = %d, want the invoking group %d", got, want)
	}
}

// --puid/--pgid are the operator saying who the files belong to, which box does not
// override from a directory.
func TestFilesFlagsBeatTheDataOwner(t *testing.T) {
	h := newHarness(t)
	dataWith(h.host, 1500, 1600)
	opts := h.unattended()
	opts.Apps = []string{"alpha"}
	opts.PUID, opts.PGID, opts.PUIDSet, opts.PGIDSet = 1000, 1000, true, true

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if got, want := grantID(t, h, "global.puid"), 1000; got != want {
		t.Fatalf("puid = %d, want the flag's %d", got, want)
	}
}

// An installed box keeps its owner: reading the data path would let a stray
// root-owned file re-chown the whole release.
func TestFilesOwnerOnAReRunIsTheGrants(t *testing.T) {
	h := newHarness(t)
	write(t, h.grantPath, reRunGrant)
	dataWith(h.host, 1500, 1600)
	h.prompt.inputs = []string{"old.example.com"}
	h.prompt.multis = [][]string{{"alpha"}}
	opts := h.options()
	opts.PUID, opts.PGID = 1500, 1600

	if _, err := h.run(opts); err != nil {
		t.Fatalf("re-run: %v\n%s", err, h.out)
	}
	if got, want := grantID(t, h, "global.puid"), 1000; got != want {
		t.Fatalf("puid = %d, want the grant's %d", got, want)
	}
}

// uid 0 is never the answer, since the apps run unprivileged, so an unattended run
// names the flags instead of installing a box that cannot work.
func TestRootOwnedDataPathIsRefusedUnattended(t *testing.T) {
	h := newHarness(t)
	dataWith(h.host, 0, 0)
	opts := h.unattended()
	opts.Apps = []string{"alpha"}
	opts.PUID, opts.PGID = 0, 0

	_, err := h.run(opts)
	if err == nil {
		t.Fatalf("a root-owned data path was accepted:\n%s", h.out)
	}
	for _, want := range []string{"--puid", "--pgid", "/srv/s5n/data"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
	if h.exists(h.grantPath) {
		t.Fatal("a refused run wrote the grant")
	}
}

// On a terminal the operator knows whose files those are: the same state is a
// question, not a refusal.
func TestRootOwnedDataPathAsksForTheOwnerGuided(t *testing.T) {
	h := newHarness(t)
	dataWith(h.host, 0, 0)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	opts := h.options()
	opts.PUID, opts.PGID = 0, 0
	// The owner questions come between the paths and the app list.
	h.prompt.inputs = append(h.prompt.inputs, "1500", "1600")

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if got, want := grantID(t, h, "global.puid"), 1500; got != want {
		t.Fatalf("puid = %d, want the answered %d", got, want)
	}
	if got, want := grantID(t, h, "global.pgid"), 1600; got != want {
		t.Fatalf("pgid = %d, want the answered %d", got, want)
	}
	if !contains(h.prompt.events, "input:global.puid") || !contains(h.prompt.events, "input:global.pgid") {
		t.Fatalf("the owner was not asked for: %v", h.prompt.events)
	}
}

// box never follows a symlink when it walks a seedbox's files, so the owner beyond
// one is not the box's answer: the path has no data, the invoking user's ids are
// used, and the link is said out loud.
func TestASymlinkedDataPathIsNoData(t *testing.T) {
	h := newHarness(t)
	h.host.links["/srv/s5n/data"] = true
	// What the link points at, which box must not read.
	h.host.dirs["/srv/elsewhere"] = true
	h.host.owners["/srv/elsewhere"] = owner{1500, 1600}
	h.host.listing["/srv/elsewhere"] = []DirEntry{{Name: "downloads", IsDir: true}}
	opts := h.unattended()
	opts.Apps = []string{"alpha"}
	opts.PUID, opts.PGID = 1000, 1000

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if got, want := grantID(t, h, "global.puid"), 1000; got != want {
		t.Fatalf("puid = %d, want the invoking user's %d: the link's target is not read", got, want)
	}
	if !strings.Contains(h.out.String(), "symlink") {
		t.Fatalf("the link was not named:\n%s", h.out)
	}
}

// ---- the web login ---------------------------------------------------------

// The account that owns the files is the login page's default -- still only a
// default: the answer is the operator's.
func TestLoginDefaultsToTheAccountThatOwnsTheFiles(t *testing.T) {
	h := newHarness(t)
	h.decision.UserLookup = func(uid int) (string, bool) {
		if uid == 1000 {
			return "core", true
		}
		return "", false
	}
	opts := h.options()
	opts.Login = ""
	opts.DefaultLogin = "someoneelse"
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if len(h.prompt.asked) == 0 {
		t.Fatal("nothing was asked")
	}
	var loginDefault string
	for _, ask := range h.prompt.asked {
		if strings.HasPrefix(ask, "login=") {
			loginDefault = strings.TrimPrefix(ask, "login=")
		}
	}
	if loginDefault != "core" {
		t.Fatalf("login default = %q, want the account that owns uid 1000", loginDefault)
	}
}

// A uid with no account is not a login name, so sudo's user stands as the default.
func TestLoginFallsBackWhenTheUidHasNoAccount(t *testing.T) {
	h := newHarness(t)
	h.decision.UserLookup = func(int) (string, bool) { return "", false }
	opts := h.options()
	opts.Login = ""
	opts.DefaultLogin = "someoneelse"
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	for _, ask := range h.prompt.asked {
		if strings.HasPrefix(ask, "login=") {
			if got := strings.TrimPrefix(ask, "login="); got != "someoneelse" {
				t.Fatalf("login default = %q, want the old default", got)
			}
			return
		}
	}
	t.Fatalf("the login was not asked for: %v", h.prompt.asked)
}

// One is the web login, the other the file owner: a box where they differ must not
// look like a box where they agree, so both are shown.
func TestGuidedInitShowsTheLoginAndTheFileOwner(t *testing.T) {
	h := newHarness(t)
	h.decision.UserLookup = func(uid int) (string, bool) {
		if uid == 1000 {
			return "core", true
		}
		return "", false
	}
	opts := h.options()
	opts.Login = "liara"
	opts.DefaultLogin = "liara"
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	out := h.out.String()
	if !strings.Contains(out, "owner: core (uid 1000, gid 1000)") {
		t.Fatalf("the file owner is not shown:\n%s", out)
	}
}

// bootstrap and init never create an OS user: the files belong to an existing
// account, and only `box adduser` makes one. PathHost is init's whole write surface,
// so the rule is checked here: it has the probes and two writes, and nothing that
// could add an account.
func TestTheHostSeamCannotCreateAnOSUser(t *testing.T) {
	want := map[string]bool{
		"Stat": true, "Lstat": true, "StatFS": true, "MountInfo": true,
		"ReadDir": true, "MkdirAll": true, "Lchown": true,
	}
	typ := reflect.TypeOf((*PathHost)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !want[name] {
			t.Fatalf("PathHost has %s: init must not gain a way to change the host's accounts (only box adduser creates an OS user)", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Fatalf("PathHost lost %s", name)
	}
}
