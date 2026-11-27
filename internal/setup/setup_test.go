package setup

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// ---- a host with no machine in it -------------------------------------------

type owner struct{ uid, gid int }

type fakeHost struct {
	dirs    map[string]bool
	files   map[string]int64
	owners  map[string]owner
	listing map[string][]DirEntry
	mounts  []Mount
	free    int64

	// links are paths that are a symlink of their own: Lstat reports them and never
	// looks at what they point to.
	links map[string]bool

	mkdirs []string
	chowns []string
	reads  []string
	// readMax is the limit each read was given: a walk shows up here as a read that
	// could have returned everything.
	readMax []int
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		dirs:    map[string]bool{string(filepath.Separator): true},
		files:   map[string]int64{},
		owners:  map[string]owner{},
		listing: map[string][]DirEntry{},
		links:   map[string]bool{},
		mounts:  []Mount{{Major: 8, Minor: 1, Point: "/", FSType: "ext4"}},
		free:    1600 * 1024 * 1024 * 1024,
	}
}

func (h *fakeHost) Stat(path string) (FileInfo, error) {
	if h.dirs[path] {
		// An unrecorded owner is an ordinary account; a recorded one is used as it is,
		// 0:0 included.
		o, ok := h.owners[path]
		if !ok {
			o = owner{1000, 1000}
		}
		return FileInfo{IsDir: true, Mode: 0o755, UID: o.uid, GID: o.gid}, nil
	}
	if _, ok := h.files[path]; ok {
		return FileInfo{Mode: 0o644}, nil
	}
	return FileInfo{}, fs.ErrNotExist
}

// Lstat is Stat with the link check: a link is reported as itself, with no
// contents.
func (h *fakeHost) Lstat(path string) (FileInfo, error) {
	if h.links[path] {
		return FileInfo{Symlink: true, Mode: 0o777}, nil
	}
	return h.Stat(path)
}

func (h *fakeHost) StatFS(string) (FSInfo, error) { return FSInfo{Free: h.free}, nil }

func (h *fakeHost) MountInfo() ([]Mount, error) { return h.mounts, nil }

func (h *fakeHost) ReadDir(path string, max int) ([]DirEntry, bool, error) {
	if !h.dirs[path] {
		return nil, false, fs.ErrNotExist
	}
	h.reads = append(h.reads, path)
	h.readMax = append(h.readMax, max)
	entries := h.listing[path]
	more := false
	if len(entries) > max {
		entries, more = entries[:max], true
	}
	return entries, more, nil
}

func (h *fakeHost) MkdirAll(path string, _ fs.FileMode) error {
	h.mkdirs = append(h.mkdirs, path)
	for p := path; p != "/" && p != "."; p = filepath.Dir(p) {
		h.dirs[p] = true
	}
	return nil
}

func (h *fakeHost) Lchown(path string, uid, gid int) error {
	h.chowns = append(h.chowns, path)
	h.owners[path] = owner{uid, gid}
	return nil
}

// ---- a prompter that records the conversation -------------------------------

type fakePrompt struct {
	inputs   []string
	secrets  []string
	confirms []bool
	multis   [][]string

	// cancelAt makes one method return ErrCancelled instead of an answer.
	cancelAt string

	events []string
	asked  []string // "key=default" for every Input, in order
	notes  []string
	// confirmDefaults is the starting answer of every Confirm, in order.
	confirmDefaults []bool
}

func (p *fakePrompt) pop(list *[]string) (string, bool) {
	if len(*list) == 0 {
		return "", false
	}
	v := (*list)[0]
	*list = (*list)[1:]
	return v, true
}

func (p *fakePrompt) Input(f Input) (string, error) {
	p.events = append(p.events, "input:"+f.Key)
	p.asked = append(p.asked, f.Key+"="+f.Default)
	if p.cancelAt == "input" {
		return "", ErrCancelled
	}
	value, ok := p.pop(&p.inputs)
	if !ok || value == "" {
		value = f.Default
	}
	if f.Validate != nil {
		if err := f.Validate(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func (p *fakePrompt) Secret(f Secret) (string, error) {
	p.events = append(p.events, "secret:"+f.Key)
	if p.cancelAt == "secret" {
		return "", ErrCancelled
	}
	value, ok := p.pop(&p.secrets)
	if !ok {
		return "", nil
	}
	return value, nil
}

func (p *fakePrompt) Confirm(f Confirm) (bool, error) {
	p.events = append(p.events, "confirm:"+f.Prompt)
	p.confirmDefaults = append(p.confirmDefaults, f.Default)
	if p.cancelAt == "confirm" {
		return false, ErrCancelled
	}
	if len(p.confirms) > 0 {
		v := p.confirms[0]
		p.confirms = p.confirms[1:]
		return v, nil
	}
	return f.Default, nil
}

func (p *fakePrompt) Select(f Select) (string, error) {
	p.events = append(p.events, "select:"+f.Key)
	if p.cancelAt == "select" {
		return "", ErrCancelled
	}
	return f.Default, nil
}

func (p *fakePrompt) MultiSelect(f MultiSelect) ([]string, error) {
	p.events = append(p.events, "multi:"+f.Key)
	if p.cancelAt == "multi" {
		return nil, ErrCancelled
	}
	if len(p.multis) > 0 {
		v := p.multis[0]
		p.multis = p.multis[1:]
		return v, nil
	}
	return f.Selected, nil
}

func (p *fakePrompt) Note(text string) error {
	p.notes = append(p.notes, text)
	return nil
}

// chartAt is the chart box hands init: the values and schema a question's default
// and every answer are read against. init never reads a chart itself.
func chartAt(t *testing.T, dir string) seedbox.Chart {
	t.Helper()
	values, err := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile(filepath.Join(dir, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return seedbox.Chart{Values: values, Schema: schema}
}

// ---- the harness -------------------------------------------------------------

type harness struct {
	t          *testing.T
	dir        string
	grantPath  string
	valuesPath string
	chartDir   string
	host       *fakeHost
	prompt     *fakePrompt
	out        *bytes.Buffer
	decision   *Decision

	applied   []ApplyOptions
	transient map[string]any
	applyErr  error
	// logins is every login the hand-over carried, and loginErr fails it, so a test
	// can prove the hook's order.
	logins   []Login
	loginErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:          t,
		dir:        dir,
		grantPath:  filepath.Join(dir, "grant.yaml"),
		valuesPath: filepath.Join(dir, "values.yaml"),
		chartDir:   "testdata/chart",
		host:       newFakeHost(),
		prompt:     &fakePrompt{},
		out:        &bytes.Buffer{},
	}
	h.host.dirs["/srv/s5n"] = true
	h.host.dirs["/srv/s5n/data"] = true
	h.host.dirs["/srv/s5n/config"] = true

	cat, err := catalog.Load(h.chartDir)
	if err != nil {
		t.Fatalf("load the fixture catalog: %v", err)
	}
	h.decision = &Decision{
		Host:     h.host,
		Prompt:   h.prompt,
		Catalog:  cat,
		Say:      &say.Plain{W: h.out},
		Resolver: stubResolver{},
		// internal/login installs the login; these tests only need what init decided
		// and that the hook ran where apply runs it.
		Login: func(_ context.Context, l Login) error {
			h.logins = append(h.logins, l)
			return h.loginErr
		},
		Apply: func(ctx context.Context, opts ApplyOptions) error {
			h.applied = append(h.applied, opts)
			// The real hand-over runs the login hook here: after its confirm and
			// before the release.
			if opts.Login != nil {
				if err := opts.Login(ctx); err != nil {
					return err
				}
			}
			if opts.AfterConfirm != nil {
				extra, err := opts.AfterConfirm(ctx)
				if err != nil {
					return err
				}
				h.transient = extra
			}
			return h.applyErr
		},
	}
	return h
}

// options are the defaults every test starts from: a first run answered from the
// prompts, not the flags.
func (h *harness) options() Options {
	return Options{
		Chart:      chartAt(h.t, h.chartDir),
		GrantPath:  h.grantPath,
		ValuesPath: h.valuesPath,
		Namespace:  "seedbox",
		PUID:       1000,
		PGID:       1000,
		// The login is a question like the others, and the flag is its starting
		// answer.
		Login: "alice",
	}
}

// unattended is a fully specified --yes run, the shape the kind e2e uses.
func (h *harness) unattended() Options {
	opts := h.options()
	opts.Unattended = true
	opts.Domain = "box.example.com"
	opts.DataPath = "/srv/s5n/data"
	opts.ConfigPath = "/srv/s5n/config"
	opts.Apps = []string{"alpha", "beta"}
	return opts
}

func (h *harness) run(opts Options) (Result, error) {
	return h.decision.Run(context.Background(), opts)
}

func (h *harness) read(path string) string {
	h.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func (h *harness) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- the tests ---------------------------------------------------------------

// Domain, data path, config path, the app list, each selected app's questions, then
// the hand-over. A values answer equal to the chart's default is not written: a file
// that only repeats the defaults is noise.
func TestFirstRunAsksTheQuestionsInOrder(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if res.ReRun {
		t.Fatal("a fresh host was treated as a re-run")
	}
	want := []string{
		"input:gateway.host", "input:storage.data.path", "input:storage.config.path",
		"multi:apps", "input:login", "secret:login.password", "input:apps.alpha.greeting",
	}
	if got := strings.Join(h.prompt.events, ","); got != strings.Join(want, ",") {
		t.Fatalf("questions were %s, want %s", got, strings.Join(want, ","))
	}
	if len(h.applied) != 1 {
		t.Fatalf("apply was called %d times, want 1", len(h.applied))
	}

	grant := h.read(h.grantPath)
	for _, want := range []string{
		"namespace: seedbox",
		"puid: 1000",
		"path: /srv/s5n/data",
		"path: /srv/s5n/config",
		"host: box.example.com",
		"alpha:\n    enabled: true",
		"beta:\n    enabled: false",
	} {
		if !strings.Contains(grant, want) {
			t.Fatalf("the grant is missing %q:\n%s", want, grant)
		}
	}
	// The greeting's answer is the chart's default: nothing to write, no file.
	if h.exists(h.valuesPath) {
		t.Fatalf("the values file was written with only defaults:\n%s", h.read(h.valuesPath))
	}
	if len(res.Created) != 1 || res.Created[0] != "/srv/s5n/config/alpha" {
		t.Fatalf("created = %v, want the one selected app's config directory", res.Created)
	}
	if len(h.host.chowns) == 0 {
		t.Fatal("nothing was chowned")
	}
}

// A first run with no --namespace puts the box in s5n-<login>, root's own
// included.
func TestTheSeedboxNamespaceFollowsTheLogin(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	opts := h.options()
	opts.Namespace = ""
	opts.Login = "alice"

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if grant := h.read(h.grantPath); !strings.Contains(grant, "namespace: s5n-alice") {
		t.Fatalf("the grant does not put alice's box in s5n-alice:\n%s", grant)
	}
}

// An explicit --namespace still wins on a first run.
func TestAnExplicitNamespaceWinsOnAFirstRun(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	opts := h.options()
	opts.Namespace = "s5n-other"
	opts.Login = "alice"

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if grant := h.read(h.grantPath); !strings.Contains(grant, "namespace: s5n-other") {
		t.Fatalf("the grant does not keep the explicit namespace:\n%s", grant)
	}
}

// A box installed before the s5n-<login> default keeps `seedbox`.
func TestAReRunKeepsTheGrantsNamespace(t *testing.T) {
	h := newHarness(t)
	write(t, h.grantPath, reRunGrant)
	write(t, h.valuesPath, reRunValues)
	h.prompt.inputs = []string{"old.example.com"}
	opts := h.options()
	opts.Namespace = "s5n-other"

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if !res.ReRun {
		t.Fatal("an existing grant was treated as a fresh host")
	}
	for _, key := range res.GrantChanged {
		if key == "namespace" {
			t.Fatalf("the re-run rewrote the namespace: %v", res.GrantChanged)
		}
	}
	if grant := h.read(h.grantPath); !strings.Contains(grant, "namespace: seedbox") {
		t.Fatalf("the re-run changed the namespace:\n%s", grant)
	}
}

// A login whose s5n-<login> is box's own namespace, or not a DNS-1123 label, is
// refused when it is given, naming the login and why.
func TestLoginsThatCannotBeASeedboxAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		login string
		want  []string
	}{
		{"box's own system namespace", "system", []string{`login "system"`, "s5n-system", "box's own"}},
		{"box's own gateway namespace", "gateway", []string{`login "gateway"`, "s5n-gateway", "box's own"}},
		{"not a label", "alice@example.com", []string{`login "alice@example.com"`, "s5n-alice@example.com", "lowercase letters, digits and dashes"}},
		{"too long", strings.Repeat("a", 60), []string{"s5n-", "at most 63 characters"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			opts := h.unattended()
			opts.Namespace = "" // a first run derives it, which is when the login names it
			opts.Login = tc.login
			_, err := h.run(opts)
			if err == nil {
				t.Fatalf("login %q was accepted", tc.login)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if h.exists(h.grantPath) || h.exists(h.valuesPath) {
				t.Fatal("a refused login wrote a file")
			}
		})
	}
}

// The namespace rule applies only where a namespace is derived from the login: a
// re-run's login may be any Authelia accepts (capitals, a dot, an @), and an
// explicit --namespace takes the login's place in the name.
func TestALoginOnlyHasToMakeANamespaceWhenItMakesOne(t *testing.T) {
	t.Run("a re-run keeps a grant's login and namespace", func(t *testing.T) {
		h := newHarness(t)
		write(t, h.grantPath, strings.Replace(reRunGrant, "login: alice", "login: Alice.B", 1))
		write(t, h.valuesPath, reRunValues)
		opts := h.unattended()
		opts.Login = "Alice.B"
		opts.SetTransient = map[string]string{"apps.beta.token": "tok-unattended"}
		res, err := h.run(opts)
		if err != nil {
			t.Fatalf("a re-run with an existing login was refused: %v\n%s", err, h.out)
		}
		if !res.ReRun {
			t.Fatal("an existing grant was treated as a fresh host")
		}
		if grant := h.read(h.grantPath); !strings.Contains(grant, "namespace: seedbox") {
			t.Fatalf("the re-run changed the namespace:\n%s", grant)
		}
	})
	t.Run("a first run with --namespace takes any login Authelia takes", func(t *testing.T) {
		h := newHarness(t)
		opts := h.unattended()
		opts.Login = "Alice.B"
		opts.Namespace = "s5n-alice"
		opts.SetTransient = map[string]string{"apps.beta.token": "tok-unattended"}
		if _, err := h.run(opts); err != nil {
			t.Fatalf("a login with an explicit namespace was refused: %v\n%s", err, h.out)
		}
		if grant := h.read(h.grantPath); !strings.Contains(grant, "namespace: s5n-alice") {
			t.Fatalf("the grant does not carry the explicit namespace:\n%s", grant)
		}
	})
}

// An explicit namespace is held to the same DNS-1123 label rule.
func TestAnExplicitNamespaceIsHeldToALabel(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Namespace = strings.Repeat("a", 64)
	_, err := h.run(opts)
	if err == nil {
		t.Fatal("a 64-character --namespace was accepted")
	}
	for _, want := range []string{"--namespace", "at most 63 characters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("a refused namespace wrote a file")
	}
}

func TestReRunSplicesOnlyTheChangedKey(t *testing.T) {
	h := newHarness(t)
	write(t, h.grantPath, reRunGrant)
	write(t, h.valuesPath, reRunValues)

	h.prompt.inputs = []string{"new.example.com"}
	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if !res.ReRun {
		t.Fatal("an existing grant was treated as a fresh host")
	}
	if got := strings.Join(res.GrantChanged, ","); got != "gateway.host" {
		t.Fatalf("grant keys changed = %s, want only gateway.host", got)
	}
	if len(res.ValuesChanged) != 0 {
		t.Fatalf("values keys changed = %v, want none", res.ValuesChanged)
	}
	want := strings.Replace(reRunGrant, "host: old.example.com", "host: new.example.com", 1)
	if got := h.read(h.grantPath); got != want {
		t.Fatalf("the grant is not the old bytes with one value replaced:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if got := h.read(h.valuesPath); got != reRunValues {
		t.Fatalf("the values file changed:\n--- got\n%s\n--- want\n%s", got, reRunValues)
	}
	// The question showed the file's current answers as its defaults.
	if h.prompt.asked[0] != "gateway.host=old.example.com" {
		t.Fatalf("domain default = %q, want the grant's old.example.com", h.prompt.asked[0])
	}

	// The same answers again: nothing to splice.
	h.prompt = &fakePrompt{}
	h.decision.Prompt = h.prompt
	again, err := h.run(h.options())
	if err != nil {
		t.Fatalf("second init: %v\n%s", err, h.out)
	}
	if len(again.GrantChanged) != 0 || len(again.ValuesChanged) != 0 {
		t.Fatalf("the second run spliced %v / %v, want nothing", again.GrantChanged, again.ValuesChanged)
	}
	if got := h.read(h.grantPath); got != want {
		t.Fatalf("the second run rewrote the grant:\n%s", got)
	}
	if got := h.read(h.valuesPath); got != reRunValues {
		t.Fatalf("the second run rewrote the values file:\n%s", got)
	}
}

// The files are written after the last question, so a cancel anywhere leaves the
// host alone: no files, no directories, no apply.
func TestCancelWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.cancelAt = "multi"

	_, err := h.run(h.options())
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
	if !strings.Contains(h.out.String(), "oh heck, nvm") {
		t.Fatalf("the voice is missing:\n%s", h.out)
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("a cancel wrote a file")
	}
	if len(h.host.mkdirs) != 0 || len(h.host.chowns) != 0 {
		t.Fatalf("a cancel touched directories: mkdirs=%v chowns=%v", h.host.mkdirs, h.host.chowns)
	}
	if len(h.applied) != 0 {
		t.Fatal("a cancel reached apply")
	}
}

// With --yes nothing is asked, so a missing answer is an error naming the flag.
func TestUnattendedNamesTheMissingFlag(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Domain = ""
	_, err := h.run(opts)
	if err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Fatalf("error = %v, want it to name --domain", err)
	}

	opts = h.unattended()
	opts.Apps = nil
	_, err = h.run(opts)
	if err == nil || !strings.Contains(err.Error(), "--apps") {
		t.Fatalf("error = %v, want it to name --apps", err)
	}

	opts = h.unattended()
	opts.DataPath = "/srv/not-mounted/data"
	_, err = h.run(opts)
	if err == nil || !strings.Contains(err.Error(), "--data-path") || !strings.Contains(err.Error(), "/srv/not-mounted/data") {
		t.Fatalf("error = %v, want it to name the path and the flag", err)
	}
	if h.exists(h.grantPath) {
		t.Fatal("a refused unattended run wrote the grant")
	}
	if len(h.applied) != 0 {
		t.Fatal("a refused unattended run reached apply")
	}
}

// The unmounted-disk case: the path is missing, its nearest ancestor is on /, and
// that is said before the operator says yes.
func TestGuidedMissingPathSaysWhereItWouldLand(t *testing.T) {
	h := newHarness(t)
	delete(h.host.dirs, "/srv/s5n/data")
	h.prompt.inputs = []string{"box.example.com", "/srv/newbox/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	// Yes is the default, so a decline is an explicit no.
	h.prompt.confirms = []bool{false}
	_, err := h.run(h.options())
	if err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if len(h.prompt.confirmDefaults) != 1 || !h.prompt.confirmDefaults[0] {
		t.Fatalf("confirm default = %v, want the question to start on yes", h.prompt.confirmDefaults)
	}
	if len(h.prompt.notes) != 1 {
		t.Fatalf("notes = %v, want one path report", h.prompt.notes)
	}
	note := h.prompt.notes[0]
	for _, want := range []string{
		"/srv/newbox/data",
		"new",
		"8:1",
		"root filesystem",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("the report is missing %q:\n%s", want, note)
		}
	}

	// Now say yes: it is created, owned by the box's ids.
	h.out.Reset()
	h.prompt = &fakePrompt{confirms: []bool{true}}
	h.decision.Prompt = h.prompt
	h.prompt.inputs = []string{"box.example.com", "/srv/newbox/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if !contains(res.Created, "/srv/newbox/data") {
		t.Fatalf("created = %v, want the new data directory", res.Created)
	}
	if h.host.owners["/srv/newbox/data"] != (owner{1000, 1000}) {
		t.Fatalf("the new directory's owner = %+v, want 1000:1000", h.host.owners["/srv/newbox/data"])
	}
}

// Two volumes, two questions: in data-then-config order, each naming its own path
// and starting on the safe answer. Answering the first does not answer the second.
func TestBothMissingPathsAreAskedAboutSeparately(t *testing.T) {
	h := newHarness(t)
	delete(h.host.dirs, "/srv/s5n/data")
	delete(h.host.dirs, "/srv/s5n/config")
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.prompt.confirms = []bool{true, true}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	want := []string{
		"confirm:create /srv/s5n/data on that filesystem?",
		"confirm:create /srv/s5n/config on that filesystem?",
	}
	var asked []string
	for _, e := range h.prompt.events {
		if strings.HasPrefix(e, "confirm:") {
			asked = append(asked, e)
		}
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("confirms asked:\n%s\nwant:\n%s", strings.Join(asked, "\n"), strings.Join(want, "\n"))
	}
	// Both start on create: init cannot finish on a missing path.
	if len(h.prompt.confirmDefaults) != 2 || !h.prompt.confirmDefaults[0] || !h.prompt.confirmDefaults[1] {
		t.Fatalf("confirm defaults = %v, want both to start on create", h.prompt.confirmDefaults)
	}
	for _, want := range []string{"/srv/s5n/data", "/srv/s5n/config"} {
		if !contains(res.Created, want) {
			t.Fatalf("created = %v, want %s", res.Created, want)
		}
	}
}

// The second path's no is as final as the first's, and the error names which one.
func TestDecliningEitherMissingPathStopsBeforeAnythingIsWritten(t *testing.T) {
	h := newHarness(t)
	delete(h.host.dirs, "/srv/s5n/data")
	delete(h.host.dirs, "/srv/s5n/config")
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.prompt.confirms = []bool{true, false}

	_, err := h.run(h.options())
	if err == nil || !strings.Contains(err.Error(), "not creating /srv/s5n/config: nothing was written") {
		t.Fatalf("error = %v, want the config path's refusal", err)
	}
	if len(h.host.mkdirs) != 0 || len(h.host.chowns) != 0 {
		t.Fatalf("a refusal touched directories: mkdirs=%v chowns=%v", h.host.mkdirs, h.host.chowns)
	}
	if h.exists(h.grantPath) || h.exists(h.valuesPath) {
		t.Fatal("a refusal wrote a file")
	}
	if len(h.applied) != 0 {
		t.Fatal("a refusal reached apply")
	}
}

// One bounded directory read, no walk.
func TestExistingPathSummaryIsCheapAndBounded(t *testing.T) {
	h := newHarness(t)
	h.host.listing["/srv/s5n/data"] = []DirEntry{
		{Name: "downloads", IsDir: true},
		{Name: "media", IsDir: true},
		{Name: "lost+found", IsDir: true},
		{Name: "note.txt", Size: 2048},
	}
	h.prompt.multis = [][]string{{"alpha"}}
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	note := h.prompt.notes[0]
	t.Logf("the report init shows:\n%s", note)
	for _, want := range []string{
		"4 entries at the top level: downloads, media, lost+found, note.txt",
		"1 file, 2.0 KiB",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("the report is missing %q:\n%s", want, note)
		}
	}
	// No read is a walk; the data path's extra one is the owner probe, one entry
	// rather than the tree.
	if got := strings.Join(h.host.reads, " "); got != "/srv/s5n/data /srv/s5n/data /srv/s5n/config" {
		t.Fatalf("ReadDir calls = %v, want the owner probe then the two paths", h.host.reads)
	}
	for i, max := range h.host.readMax {
		if max > summaryEntries {
			t.Fatalf("read %d of %s asked for %d entries: init does not walk the tree", i, h.host.reads[i], max)
		}
	}

	// More names than the cap reports the cap, not a walk.
	many := make([]DirEntry, 0, summaryEntries+50)
	for i := 0; i < summaryEntries+50; i++ {
		many = append(many, DirEntry{Name: "d", IsDir: true})
	}
	h.host.listing["/srv/s5n/data"] = many
	h.host.reads = nil
	h.prompt = &fakePrompt{inputs: []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}, multis: [][]string{{"alpha"}}}
	h.decision.Prompt = h.prompt
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(h.prompt.notes[0], "200+ entries at the top level") {
		t.Fatalf("a big directory was not capped:\n%s", h.prompt.notes[0])
	}
}

// The claim token's case: asked after the hand-over begins, it reaches the apply,
// and neither file has it.
func TestTransientAnswerReachesApplyAndNoFile(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha", "beta"}}
	// The first secret is the login's password (empty: box makes one), the second the
	// claim token.
	h.prompt.secrets = []string{"", "tok-do-not-write"}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if len(h.applied) != 1 {
		t.Fatalf("apply was called %d times", len(h.applied))
	}
	req := h.applied[0]
	if strings.Join(req.TransientKeys, ",") != "apps.beta.token" {
		t.Fatalf("transient keys = %v, want the catalog's", req.TransientKeys)
	}
	if req.AfterConfirm == nil {
		t.Fatal("no transient hook was handed to apply")
	}
	if h.transient["apps.beta.token"] != "tok-do-not-write" {
		t.Fatalf("transient answers = %v", h.transient)
	}
	if strings.Contains(h.read(h.grantPath), "tok-do-not-write") {
		t.Fatal("the transient answer is in the grant")
	}
	if h.exists(h.valuesPath) && strings.Contains(h.read(h.valuesPath), "tok-do-not-write") {
		t.Fatal("the transient answer is in the values file")
	}
	if len(res.ValuesChanged) != 0 {
		t.Fatalf("the transient answer was spliced: %v", res.ValuesChanged)
	}
	// The transient question is asked after the hand-over, not with the others.
	if got := h.prompt.events[len(h.prompt.events)-1]; got != "secret:apps.beta.token" {
		t.Fatalf("the last question was %s, want the transient one", got)
	}
}

// A required transient answer has no file to live in, so --yes needs
// --set-transient, and a typo must not be ignored.
func TestUnattendedTransientIsAFlag(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	_, err := h.run(opts)
	if err == nil || !strings.Contains(err.Error(), "--set-transient apps.beta.token") {
		t.Fatalf("error = %v, want it to name --set-transient", err)
	}

	h2 := newHarness(t)
	opts = h2.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok-unattended"}
	if _, err := h2.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h2.out)
	}
	if h2.transient["apps.beta.token"] != "tok-unattended" {
		t.Fatalf("transient answers = %v", h2.transient)
	}

	h3 := newHarness(t)
	opts = h3.unattended()
	opts.SetTransient = map[string]string{"apps.beta.tokn": "x"}
	if _, err := h3.run(opts); err == nil || !strings.Contains(err.Error(), "apps.beta.tokn") {
		t.Fatalf("error = %v, want the unknown transient key named", err)
	}
}

// The same answers splice nothing the second time, as the kind e2e asserts.
func TestUnattendedTwiceIsSilent(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}
	if _, err := h.run(opts); err != nil {
		t.Fatalf("first init: %v\n%s", err, h.out)
	}
	grant, values := h.read(h.grantPath), ""
	if h.exists(h.valuesPath) {
		values = h.read(h.valuesPath)
	}
	h.applied = nil

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("second init: %v\n%s", err, h.out)
	}
	if len(res.GrantChanged) != 0 || len(res.ValuesChanged) != 0 {
		t.Fatalf("the second run spliced %v / %v", res.GrantChanged, res.ValuesChanged)
	}
	if h.read(h.grantPath) != grant {
		t.Fatal("the second run rewrote the grant")
	}
	if h.exists(h.valuesPath) && h.read(h.valuesPath) != values {
		t.Fatal("the second run rewrote the values file")
	}
}

// A typo in --apps is refused with the list.
func TestAppsFlagNamesTheCatalog(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{"alpha,gamms"}
	_, err := h.run(opts)
	if err == nil || !strings.Contains(err.Error(), "gamms") || !strings.Contains(err.Error(), "alpha, beta") {
		t.Fatalf("error = %v, want the bad name and the catalog's list", err)
	}
}

// An install with every app off still records the Gateway's proxy address, the shape
// `box migrate apply` installs into (the plan's apps are switched on later, from the
// grant): an OIDC app rendered without it cannot reach the login at the box's name,
// so qui has nobody able to sign in and autobrr shows its own password form. The key
// is the box's, so the question is what the chart serves, not what is on now.
func TestTheGatewayAddressIsWrittenWithEveryAppOff(t *testing.T) {
	h := newHarness(t)
	h.decision.GatewayIP = func(context.Context) (string, error) { return "10.96.0.10", nil }
	h.decision.Schema = chartAt(t, h.chartDir).Schema
	opts := h.unattended()
	opts.Apps = []string{"none"}
	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	grant := h.read(h.grantPath)
	if !strings.Contains(grant, "gatewayIP: 10.96.0.10") {
		t.Fatalf("the grant carries no Gateway address, and this chart's OIDC apps need it:\n%s", grant)
	}
	// With an OIDC app on the write is the same, and a re-run changes nothing.
	h2 := newHarness(t)
	h2.decision.GatewayIP = h.decision.GatewayIP
	h2.decision.Schema = h.decision.Schema
	on := h2.unattended()
	on.SetTransient = map[string]string{"apps.beta.token": "tok"}
	if _, err := h2.run(on); err != nil {
		t.Fatalf("init with apps on: %v\n%s", err, h2.out)
	}
	res, err := h2.run(on)
	if err != nil {
		t.Fatalf("second init: %v\n%s", err, h2.out)
	}
	if len(res.GrantChanged) != 0 {
		t.Fatalf("the second run spliced %v", res.GrantChanged)
	}
	if !strings.Contains(h2.read(h2.grantPath), "gatewayIP: 10.96.0.10") {
		t.Fatalf("the re-run lost the Gateway address:\n%s", h2.read(h2.grantPath))
	}
}

// A chart without the key is never given it.
func TestNoGatewayAddressWithoutTheKey(t *testing.T) {
	h := newHarness(t)
	called := false
	h.decision.GatewayIP = func(context.Context) (string, error) { called = true; return "10.96.0.10", nil }
	h.decision.Schema = []byte(`{"type":"object","properties":{}}`)
	opts := h.unattended()
	opts.SetTransient = map[string]string{"apps.beta.token": "tok"}
	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if called {
		t.Fatal("the Gateway was read for a chart that has no auth.oidc.gatewayIP")
	}
	if strings.Contains(h.read(h.grantPath), "gatewayIP") {
		t.Fatalf("a key the chart does not declare was written:\n%s", h.read(h.grantPath))
	}
}

// One directory per enabled instance, named as the chart names the workload.
func TestSettingsDirsFollowTheInstances(t *testing.T) {
	chart := chartAt(t, "testdata/chart")
	base, err := seedbox.Base(chart)
	if err != nil {
		t.Fatal(err)
	}
	got := SettingsDirs("/srv/s5n/config", []string{"alpha", "beta"}, base)
	want := "/srv/s5n/config/alpha,/srv/s5n/config/beta"
	if strings.Join(got, ",") != want {
		t.Fatalf("settingsDirs = %v, want %s (the disabled 4k instance is not one)", got, want)
	}
}

// The two volumes must not overlap.
func TestPathPairIsRefused(t *testing.T) {
	cases := [][2]string{
		{"/srv/s5n/data", "/srv/s5n/data"},
		{"/srv/s5n/data", "/srv/s5n/data/config"},
		{"/srv/s5n/data/sub", "/srv/s5n/data"},
	}
	for _, c := range cases {
		if err := checkPathPair(c[0], c[1]); err == nil {
			t.Fatalf("%s and %s were accepted", c[0], c[1])
		}
	}
	if err := checkPathPair("/srv/s5n/data", "/srv/s5n/config"); err != nil {
		t.Fatalf("two sibling paths were refused: %v", err)
	}
	if err := checkPathPair("/srv/s5n/data", "/srv/s5n/data2"); err != nil {
		t.Fatalf("a prefix that is not a parent was refused: %v", err)
	}
}

// /srv/s5n/data is not on /srv/s5.
func TestMountForPicksTheLongestPrefix(t *testing.T) {
	mounts := []Mount{
		{Point: "/", FSType: "ext4"},
		{Point: "/srv/s5", FSType: "xfs"},
		{Point: "/srv/s5n/data", FSType: "zfs"},
	}
	cases := map[string]string{
		"/srv/s5n/data":    "zfs",
		"/srv/s5n/data/tv": "zfs",
		"/srv/s5n/config":  "ext4", // /srv/s5 is a prefix, not a parent
		"/srv/s5":          "xfs",
		"/srv/s5/x":        "xfs",
		"/home/alice":      "ext4",
		"/srv/s5x":         "ext4",
	}
	for path, want := range cases {
		m, ok := mountFor(mounts, path)
		if !ok || m.FSType != want {
			t.Fatalf("mountFor(%s) = %+v (%v), want %s", path, m, ok, want)
		}
	}
}

// The real format, including an escaped space.
func TestParseMountInfo(t *testing.T) {
	src := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro
36 22 0:32 / /srv/my\040disk rw,nosuid - zfs tank/srv rw,xattr
37 22 0:33 / /srv/s5n/data rw - fuse.mergerfs merged rw
`
	mounts, err := parseMountInfo(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 {
		t.Fatalf("mounts = %+v, want three", mounts)
	}
	if mounts[0].Major != 8 || mounts[0].Minor != 1 || mounts[0].FSType != "ext4" || mounts[0].Point != "/" {
		t.Fatalf("root mount = %+v", mounts[0])
	}
	if mounts[1].Point != "/srv/my disk" || mounts[1].FSType != "zfs" || mounts[1].Source != "tank/srv" {
		t.Fatalf("escaped mount = %+v", mounts[1])
	}
	if mounts[2].Point != "/srv/s5n/data" || mounts[2].FSType != "fuse.mergerfs" {
		t.Fatalf("mergerfs mount = %+v", mounts[2])
	}
}

// Sizes at every scale the report meets.
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:                           "512 B",
		1536:                          "1.5 KiB",
		1600 * 1024 * 1024:            "1.6 GiB",
		2 * 1024 * 1024 * 1024 * 1024: "2.0 TiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reRunGrant is root's file as a hand edit left it: a comment, four-space
// indentation, and a comment between a key and its value.
const reRunGrant = `# root's grant for this box, edited by hand
namespace: seedbox
login: alice

global:
    puid: 1000
    pgid: 1000

storage:
  data:
    path: /srv/s5n/data
  config:
    path: /srv/s5n/config

gateway:
  # the name this box answers to
  host: old.example.com

apps:
  alpha:
    enabled: true
  beta:
    enabled: false
  gamma:
    enabled: false
`

// reRunValues is the user's own file: comments and an indent a splice must not
// disturb.
const reRunValues = `# my tuning, not root's
apps:
    alpha:
        # it says this when it starts
        greeting: "hi"
`

// stubResolver answers a fixed map, so init's issuer decision never depends on the
// machine.
type stubResolver struct {
	addrs map[string][]string
	errs  map[string]error
}

func (s stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if err := s.errs[host]; err != nil {
		return nil, err
	}
	return s.addrs[host], nil
}

// tls.issuer in the values file forces the issuer even on a first run: it is the
// operator's file, so init reads it directly.
func TestForcedIssuerFromTheValuesFile(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(h.valuesPath, []byte("tls:\n  issuer: selfsigned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.decision.Resolver = stubResolver{addrs: map[string][]string{"box.example.com": {"203.0.113.7"}}}
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if res.Issuer != IssuerSelfSigned {
		t.Fatalf("issuer = %q, want the values file's selfsigned", res.Issuer)
	}
	if !strings.Contains(h.out.String(), "tls.issuer=selfsigned forces a self-signed certificate") {
		t.Fatalf("init did not say the issuer was forced:\n%s", h.out)
	}
}

// A public answer gets Let's Encrypt, and the line says so.
func TestIssuerFollowsTheDomain(t *testing.T) {
	h := newHarness(t)
	h.decision.Resolver = stubResolver{addrs: map[string][]string{"box.example.com": {"203.0.113.7"}}}
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if res.Issuer != IssuerLetsEncrypt {
		t.Fatalf("issuer = %q, want Let's Encrypt", res.Issuer)
	}
	// One host is one row: there is nothing to fold.
	if !strings.Contains(h.out.String(), "· box.example.com  Let's Encrypt · resolves to 203.0.113.7\n") {
		t.Fatalf("init did not explain the issuer:\n%s", h.out)
	}
}

// init decides each name's issuer from its own answer, and the apply compares every
// answering Certificate with that decision. tls.issuer is empty here, so the map is
// the only thing that carries it, for every name init decided.
func TestTheHandOverCarriesEachHostsIssuer(t *testing.T) {
	h := newHarness(t)
	h.decision.Resolver = stubResolver{addrs: map[string][]string{
		"box.example.com":       {"203.0.113.7"},
		"gamma.box.example.com": {"203.0.113.7"},
	}}
	opts := h.unattended()
	opts.Apps = []string{"alpha", "gamma"}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if len(h.applied) != 1 {
		t.Fatalf("the hand-over ran %d times, want once", len(h.applied))
	}
	got := h.applied[0].HostIssuers
	want := map[string]string{
		"box.example.com":       IssuerLetsEncrypt,
		"gamma.box.example.com": IssuerLetsEncrypt,
	}
	if len(got) != len(want) {
		t.Fatalf("the apply was handed %v, want %v", got, want)
	}
	for host, issuer := range want {
		if got[host] != issuer {
			t.Fatalf("issuer for %s = %q, want %q (%v)", host, got[host], issuer, got)
		}
	}
}
