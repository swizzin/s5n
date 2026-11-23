package setup

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// ---- the account that owns the files ----------------------------------------

// dataWith puts one file in the data path, owned by uid:gid: the state this
// rule is about (a data path that already holds somebody's files).
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

// TestFilesOwnerComesFromTheDataPath: an install must not change who owns a data
// path that already holds files -- the account is the answer, and sudo's ids are
// only the fallback for a path with nothing in it.
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

// TestFilesOwnerFallsBackToTheInvokingUser: an empty data path is nobody's yet,
// so what sudo carried is the answer.
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

// TestFilesFlagsBeatTheDataOwner: --puid/--pgid are the operator saying who the
// files belong to, and that is not a guess box overrides from a directory.
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

// TestFilesOwnerOnAReRunIsTheGrants: an installed box keeps its owner. Reading
// the data path would let a stray root-owned file re-chown the whole release.
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

// TestRootOwnedDataPathIsRefusedUnattended: uid 0 is never the answer -- the apps
// run unprivileged and could not write a root-owned tree -- so an unattended run
// stops and names the flags rather than installing a box that cannot work.
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

// TestRootOwnedDataPathAsksForTheOwnerGuided: on a terminal the operator knows
// whose files those are, so the same state is a question instead of a refusal.
func TestRootOwnedDataPathAsksForTheOwnerGuided(t *testing.T) {
	h := newHarness(t)
	dataWith(h.host, 0, 0)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	opts := h.options()
	opts.PUID, opts.PGID = 0, 0
	// The two owner questions are asked between the paths and the app list.
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

// TestASymlinkedDataPathIsNoData: box never follows a symlink when it walks a
// seedbox's files on the host, so the owner on the other side of a link is not
// the box's answer -- the path has no data for this rule, the invoking user's
// ids are used, and the link is said out loud.
func TestASymlinkedDataPathIsNoData(t *testing.T) {
	h := newHarness(t)
	h.host.links["/srv/s5n/data"] = true
	// What the link points at, which box must not read: it is a directory full
	// of somebody else's files.
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

// TestLoginDefaultsToTheAccountThatOwnsTheFiles: the box's files belong to an
// OS account, so that account's name is the login page's default. Still just a
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

// TestLoginFallsBackWhenTheUidHasNoAccount: a uid with no account in the host's
// database is not a login name, so the old default (sudo's user) stands.
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

// TestGuidedInitShowsTheLoginAndTheFileOwner: one is the web login, the other is
// who the files belong to; a box where they differ must not look like a box
// where they agree. Both are shown, in the run's own record style.
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
	if !strings.Contains(out, "files: core (uid 1000, gid 1000)") {
		t.Fatalf("the file owner is not shown:\n%s", out)
	}
}

// TestTheHostSeamCannotCreateAnOSUser: bootstrap and init never create an OS
// user -- the files belong to an account that already exists, and only
// `box adduser` makes one. PathHost is the whole of init's write surface, so the
// rule is checked where it could be broken: the interface has the probes and two
// writes, and nothing that could add an account.
func TestTheHostSeamCannotCreateAnOSUser(t *testing.T) {
	want := map[string]bool{
		"Stat": true, "Lstat": true, "StatFS": true, "MountInfo": true,
		"ReadDir": true, "MkdirAll": true, "Chown": true,
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
