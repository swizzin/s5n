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

	mkdirs []string
	chowns []string
	reads  []string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		dirs:    map[string]bool{string(filepath.Separator): true},
		files:   map[string]int64{},
		owners:  map[string]owner{},
		listing: map[string][]DirEntry{},
		mounts:  []Mount{{Major: 8, Minor: 1, Point: "/", FSType: "ext4"}},
		free:    1600 * 1024 * 1024 * 1024,
	}
}

func (h *fakeHost) Stat(path string) (FileInfo, error) {
	if h.dirs[path] {
		o := h.owners[path]
		if o == (owner{}) {
			o = owner{1000, 1000}
		}
		return FileInfo{IsDir: true, Mode: 0o755, UID: o.uid, GID: o.gid}, nil
	}
	if _, ok := h.files[path]; ok {
		return FileInfo{Mode: 0o644}, nil
	}
	return FileInfo{}, fs.ErrNotExist
}

func (h *fakeHost) StatFS(string) (FSInfo, error) { return FSInfo{Free: h.free}, nil }

func (h *fakeHost) MountInfo() ([]Mount, error) { return h.mounts, nil }

func (h *fakeHost) ReadDir(path string, max int) ([]DirEntry, bool, error) {
	if !h.dirs[path] {
		return nil, false, fs.ErrNotExist
	}
	h.reads = append(h.reads, path)
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

func (h *fakeHost) Chown(path string, uid, gid int) error {
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
	// confirmDefaults is the starting answer of every Confirm, in order, so a
	// test can prove the safe choice is the one an empty enter takes.
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

// chartAt is the chart box would hand init: the two files a question's default
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
	// logins is every login the hand-over carried, and loginErr fails it: the
	// hook runs after apply's confirm, so a test can prove both orders.
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
		// The login is installed by internal/login; these tests only need to
		// know what init decided, and that the hook ran where apply runs it.
		Login: func(_ context.Context, l Login) error {
			h.logins = append(h.logins, l)
			return h.loginErr
		},
		Apply: func(ctx context.Context, opts ApplyOptions) error {
			h.applied = append(h.applied, opts)
			// The real hand-over runs the login hook here: after its own
			// confirm and before the release. The fake does the same.
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

// options are the defaults every test starts from: a first run whose answers
// come from the prompts, not the flags.
func (h *harness) options() Options {
	return Options{
		Chart:      chartAt(h.t, h.chartDir),
		GrantPath:  h.grantPath,
		ValuesPath: h.valuesPath,
		Namespace:  "seedbox",
		PUID:       1000,
		PGID:       1000,
		// The login is a question like the others; the flag is its starting
		// answer, so a test that says nothing about it gets this one.
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

// TestFirstRunAsksTheQuestionsInOrder: domain, data path, config path, the app
// list, then each selected app's questions, then the hand-over. The grant gets
// every decision; a values answer equal to the chart's default is not written,
// because a file that only repeats the defaults is noise.
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
	// The greeting was answered with the value the chart already defaults to,
	// so there is nothing to write and no file to create.
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

// TestReRunSplicesOnlyTheChangedKey: a hand-edited pair of files, one answer
// changed. The rest of both files must come out byte for byte, comments and odd
// indentation included, and a second run with the same answers must not write at
// all.
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

// TestCancelWritesNothing: the files are written after the last question, so a
// "no" anywhere leaves the host alone -- no files, no directories, no apply.
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

// TestUnattendedNamesTheMissingFlag: with --yes nothing is asked, and a missing
// answer is an error naming the flag rather than a prompt nobody sees.
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

// TestGuidedMissingPathSaysWhereItWouldLand: the unmounted-disk case. The path
// is missing, the nearest ancestor is on /, and that has to be said before the
// operator says yes.
func TestGuidedMissingPathSaysWhereItWouldLand(t *testing.T) {
	h := newHarness(t)
	delete(h.host.dirs, "/srv/s5n/data")
	h.prompt.inputs = []string{"box.example.com", "/srv/newbox/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}

	// The first run declines the creation.
	_, err := h.run(h.options())
	if err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if len(h.prompt.notes) != 1 {
		t.Fatalf("notes = %v, want one path report", h.prompt.notes)
	}
	note := h.prompt.notes[0]
	for _, want := range []string{
		"/srv/newbox/data",
		"does not exist yet",
		"device 8:1, mounted at / (ext4)",
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

// TestBothMissingPathsAreAskedAboutSeparately: data and config are two
// volumes, so a run where both are missing asks two questions, in
// data-then-config order, each naming its own path, each starting on the safe
// answer. Answering the first does not answer the second.
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
	if len(h.prompt.confirmDefaults) != 2 || h.prompt.confirmDefaults[0] || h.prompt.confirmDefaults[1] {
		t.Fatalf("confirm defaults = %v, want both to start on the safe no", h.prompt.confirmDefaults)
	}
	for _, want := range []string{"/srv/s5n/data", "/srv/s5n/config"} {
		if !contains(res.Created, want) {
			t.Fatalf("created = %v, want %s", res.Created, want)
		}
	}
}

// TestDecliningEitherMissingPathStopsBeforeAnythingIsWritten: the second
// path's no is as final as the first's, and the error names which one.
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

// TestExistingPathSummaryIsCheapAndBounded: one bounded directory read, no walk.
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
		"owner 1000:1000",
		"4 entries at the top level: downloads, media, lost+found, note.txt",
		"1 file, 2.0 KiB",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("the report is missing %q:\n%s", want, note)
		}
	}
	// One read per path, and none of them a walk.
	if len(h.host.reads) != 2 || h.host.reads[0] != "/srv/s5n/data" {
		t.Fatalf("ReadDir calls = %v, want the two paths once each", h.host.reads)
	}

	// A directory with more names than the cap reports the cap, not a walk.
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

// TestTransientAnswerReachesApplyAndNoFile: the claim token's case. It is asked
// after the hand-over begins, it reaches the apply, and neither file has it.
func TestTransientAnswerReachesApplyAndNoFile(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha", "beta"}}
	// The first secret is the login's password question (empty: box makes
	// one), the second is the claim token.
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

// TestUnattendedTransientIsAFlag: a required transient answer has no file to
// live in, so --yes needs --set-transient, and a typo must not be ignored.
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

// TestUnattendedTwiceIsSilent: the same answers must splice nothing the second
// time -- the property the kind e2e asserts on the files it compares.
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

// TestAppsFlagNamesTheCatalog: a typo in --apps is refused with the list.
func TestAppsFlagNamesTheCatalog(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{"alpha,gamms"}
	_, err := h.run(opts)
	if err == nil || !strings.Contains(err.Error(), "gamms") || !strings.Contains(err.Error(), "alpha, beta") {
		t.Fatalf("error = %v, want the bad name and the catalog's list", err)
	}
}

// TestSettingsDirsFollowTheInstances: one directory per enabled instance, named
// as the chart names the workload, because that is the subPath the pod mounts.
func TestSettingsDirsFollowTheInstances(t *testing.T) {
	chart := chartAt(t, "testdata/chart")
	base, err := seedbox.Base(chart)
	if err != nil {
		t.Fatal(err)
	}
	got := settingsDirs("/srv/s5n/config", []string{"alpha", "beta"}, base)
	want := "/srv/s5n/config/alpha,/srv/s5n/config/beta"
	if strings.Join(got, ",") != want {
		t.Fatalf("settingsDirs = %v, want %s (the disabled 4k instance is not one)", got, want)
	}
}

// TestPathPairIsRefused: the two volumes must not overlap.
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

// TestMountForPicksTheLongestPrefix: /srv/s5n/data is not on /srv/s5.
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

// TestParseMountInfo reads the real format, including an escaped space.
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

// TestHumanBytes keeps the report readable at every scale it will meet.
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
// indentation in one section, and a comment between a key and its value.
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

// reRunValues is the user's own file: comments and a four-space indent that a
// splice must not disturb.
const reRunValues = `# my tuning, not root's
apps:
    alpha:
        # it says this when it starts
        greeting: "hi"
`

// stubResolver answers one fixed lookups map, so init's issuer decision never
// depends on the machine a test runs on.
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

// tls.issuer in the values file forces the issuer, even on a first run where
// the chart's defaults are the baseline. It is the operator's file, not the
// chart's, so init reads it directly.
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

// The resolution is init's default: a public answer gets Let's Encrypt, and
// the line says so.
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
	if !strings.Contains(h.out.String(), "box.example.com resolves to 203.0.113.7: using Let's Encrypt") {
		t.Fatalf("init did not explain the issuer:\n%s", h.out)
	}
}

// TestTheHandOverCarriesEachHostsIssuer: init decides each name's issuer from
// its own DNS answer, and the apply it hands over compares every Certificate
// that already answers with that decision. tls.issuer is empty in this run, so
// the map is the only thing that can carry it -- and the names must be the ones
// init decided, including an app served at a host of its own.
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
