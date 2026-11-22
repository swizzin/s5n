// Package apply is the write half of `box migrate`: it adopts a source install
// into a seedbox box has installed.
//
// The order is fixed and every step refuses before it writes: the plan and the
// seedbox are read, every problem is collected, each app's config is copied into
// a staging tree and checked there, the operator sees the whole change once, and
// only then does anything move. A rewriter's Check pass writes nothing, and no
// app's Write runs until every app's Check has passed, so one uncovered path in
// the last app stops the migration before its first write (docs/box.md,
// "Shown before it's done").
//
// The source is never written. It is copied, the copy is rewritten, and the copy
// is what the release runs: the source stays the rollback the operator tears
// down after the box has proved itself.
//
// What lives here is everything testable without a cluster: the staging copy,
// the preflight, the state file, the summary, the two passes, the data renames,
// the move into place, the ownership seed and the switch-on. cmd/box builds the
// dependencies (Env) and hands over to `box apply` afterwards. The one piece
// that talks to a cluster is the Job runner (runner.go), and it is written
// against a clientset so a fake drives it.
package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// ErrCancelled says the operator answered no, or could not be asked. Nothing
// has been written at that point: the confirm is the last read-only step.
var ErrCancelled = errors.New("cancelled")

// Env is everything apply touches outside the plan directory, injected so every
// refusal and every write is testable without a cluster or an installed box.
type Env struct {
	// ConfigRoot and DataRoot are the grant's two host paths. ConfigRoot is
	// where each instance's directory goes (the claim's subPath), DataRoot is
	// the tree the release mounts at /data and the one a rename happens in.
	ConfigRoot, DataRoot string
	// Configs is the directory holding one config tree per planned app, as the
	// snapshot carries them: `<Configs>/<instance>/` is what this run stages
	// and rewrites. The plan's own `config:` key is where the source kept it
	// and is informational: apply never reads the source host.
	Configs string
	// PUID and PGID are the values' global.puid and global.pgid: everything
	// staged is owned by them, because the pod's user is not box's.
	PUID, PGID int
	// Chown hands one path of a staged config tree to the seedbox's user,
	// never following a link (own.go). Nil means os.Lchown; a test that
	// cannot change an owner records the calls instead.
	Chown func(path string, uid, gid int) error
	// Targets is where every instance answers inside the release (chart.go,
	// Targets).
	Targets map[string]rewrite.Endpoint
	// Run executes a tool inside an app's pinned image. nil when this caller
	// cannot run one: the one rewriter that needs it (Plex) then refuses with
	// its own message, and preflight says so before anything is staged.
	Run rewrite.Runner
	// Enabled reports whether the seedbox already switches an instance on.
	Enabled func(instance string) bool
	// Owned is the release's wiring ownership record. nil on a run that only
	// wants the files moved (the tests, and a box whose migration cannot seed
	// it): an app that adopted a connection then stops the run rather than
	// dropping the ids on the floor.
	Owned OwnedStore
	// Secret is the release Secret, where each app's own API key is written so
	// the chart hands the app the key it has today (keys.go). nil on a run
	// whose apps have no key of their own.
	Secret KeyStore
	// Values turns an instance on in the seedbox's files.
	Values ValuesEditor
	// Confirm shows the summary and asks the one question. A nil Confirm is
	// refused by Run: an unattended run has to say --yes, which cmd/box turns
	// into a Confirm that answers yes without printing anything.
	Confirm func(summary Summary) (bool, error)
	// Catalog is the app names of the chart release being applied into: what
	// says whether this box can migrate an app at all. It is the one field
	// this package adds to the design's Env, because the chart is not part of
	// the plan and applying into a chart that has no such app is a refusal
	// before the first write rather than a surprise after it.
	Catalog []string
	// Rehearsal says this run applies a rehearsal snapshot: the release is
	// locked (the box's grant carries the flag, so its egress policy has no
	// internet), the data is neither adopted nor renamed, and a rewriter whose
	// app would act on the network with nothing else to stop it writes its
	// state quiet.
	Rehearsal bool
	// Carried are the notes about what a snapshot does not hold of an app's
	// config, built by the caller that opened it: the confirm shows each once,
	// with the app it belongs to.
	Carried []Carried
}

// OwnedStore and ValuesEditor are the two halves of the seedbox this package
// does not own. OwnedStore is in owned.go; ValuesEditor is the other.

// ValuesEditor turns one instance on in the seedbox's files.
//
// It is called only for instances the values do not already enable, and its
// implementation edits the files the way `box init` does: apps.<app>.enabled,
// and for an app that takes instances an entry in apps.<app>.instances. Both
// are grant keys (internal/seedbox/grant.go), so the implementation writes the
// grant, not the user's values file.
type ValuesEditor interface {
	Enable(instance string) error
}

// Summary is everything the operator sees before anything moves.
type Summary struct {
	// Instances are the plan's apps, sorted by name.
	Instances []InstanceSummary
	// Renames are the data directories apply would rename, source to contract
	// name (plan.Data.Renames).
	Renames []string
	// Defaults are the plan's own decisions: the rules discovery wrote and
	// nobody answered (plan.Defaults). A default is never silent, so the
	// confirm screen names every one of them.
	Defaults []string
	// Outside are the source's workloads box has no app for: they are not
	// migrated, and the operator brings them back by hand.
	Outside []string
	// Rehearsal says this run is a rehearsal: nothing leaves the box and no
	// data is renamed. It is a property of the run, not of the plan.
	Rehearsal bool
	// Stopped is how many torrents this run writes stopped, which is the one
	// change no config file shows.
	Stopped int
	// DefaultRules are the plan's own decisions in a form a grouped screen can
	// count them by: Defaults is the same list as the lines a report prints.
	DefaultRules []plan.Default
	// Carried are the notes about the part of a config a snapshot does not
	// hold, in the app's own words: Plex's is preferences and databases only.
	Carried []Carried
}

// Carried is one app's note about what the snapshot did not carry.
type Carried struct {
	App  string
	Note string
}

// InstanceSummary is one app's part of the summary.
type InstanceSummary struct {
	App     string
	Staged  Staged
	Changes []rewrite.Change
	Notes   []string
	// Left are the values the app stores and box will not touch, with their
	// reasons: a client it never dials, a client of a type box does not run,
	// a folder history never reads again. They stop nothing, so they are
	// shown before the confirm rather than after the run.
	Left []rewrite.Left
	// Accounts are the app's own account rows this run renames to box's account
	// (rewrite.Account): the app's single account, which the init container then
	// signs in as, so it is box's rather than the source owner's.
	Accounts []rewrite.Account
	// Enable says this run switches the instance on.
	Enable bool
	// Keys are the release Secret's key names for the keys this instance keeps:
	// names only, because a key is never printed.
	Keys []string
	// Owned are the connections that will be adopted by id.
	Owned []rewrite.Owned
	// Stopped is how many of this app's stored objects this run writes stopped:
	// a rehearsal's torrents.
	Stopped int
}

// Lines renders the summary as rows: one per change, note, rename, instance
// switched on, adopted connection, default the plan took and outside workload,
// in that order. What box leaves as it is comes right after the changes, which
// is where the operator looks for what will not move.
func (s Summary) Lines() []string {
	var rows []string
	if s.Rehearsal {
		rows = append(rows, "rehearsal: nothing leaves this box, and no data is renamed")
	}
	for _, in := range s.Instances {
		if in.Staged.Files > 0 {
			rows = append(rows, fmt.Sprintf("%s: copy %d files, %s", in.App, in.Staged.Files, bytesText(in.Staged.Bytes)))
		}
		for _, c := range in.Changes {
			rows = append(rows, fmt.Sprintf("%s: %s %s %s %d", in.App, c.File, c.Where, c.What, c.Count))
		}
		for _, a := range in.Accounts {
			rows = append(rows, fmt.Sprintf("%s: account %q becomes %s, box's own; sign in through the box login", in.App, a.From, a.To))
		}
		for _, n := range in.Notes {
			rows = append(rows, fmt.Sprintf("%s: note: %s", in.App, n))
		}
	}
	for _, c := range s.Carried {
		rows = append(rows, c.App+": "+c.Note)
	}
	for _, in := range s.Instances {
		for _, l := range in.Left {
			rows = append(rows, fmt.Sprintf("%s: left as it is: %s (%s)", in.App, l.What, l.Why))
		}
	}
	for _, r := range s.Renames {
		rows = append(rows, "data: rename "+r)
	}
	if s.Stopped > 0 {
		rows = append(rows, fmt.Sprintf("%d torrents written stopped", s.Stopped))
	}
	for _, in := range s.Instances {
		if in.Enable {
			rows = append(rows, "enable: "+in.App)
		}
	}
	for _, in := range s.Instances {
		for _, key := range in.Keys {
			rows = append(rows, fmt.Sprintf("%s: keep its API key (%s)", in.App, key))
		}
	}
	for _, in := range s.Instances {
		for _, o := range in.Owned {
			rows = append(rows, fmt.Sprintf("%s: adopt %s %q (id %s) for %s", in.App, o.Kind, o.Name, ownedID(o), o.Target))
		}
	}
	for _, d := range s.Defaults {
		rows = append(rows, "default: "+d)
	}
	for _, o := range s.Outside {
		rows = append(rows, "not migrated: "+o)
	}
	return rows
}

// Result is what one run did, for the caller to report and a test to assert.
type Result struct {
	PlanID string
	// Done says an earlier run of this same plan finished every step, so this
	// run did nothing; the caller goes straight to the apply.
	Done    bool
	Staged  []Staged
	Reports []rewrite.Report
	Summary Summary
	// Renamed are the data directories this run renamed.
	Renamed []string
	// Moved are the instances this run moved into the config root.
	Moved []string
	// Keys are the release-Secret key names this run wrote, never their values.
	Keys []string
	// Owned are the connections this run seeded into owned.json.
	Owned []rewrite.Owned
	// Enabled are the instances this run switched on.
	Enabled []string
	// Notes are lines the operator only learns after the writes: a note a
	// Write pass reported, and anything a later step could not do. The caller
	// prints them when the run returns.
	Notes []string
}

// problems is one refusal with every problem found in it. A plan and a
// seedbox are each a page an operator edits, so one pass has to say everything
// that is wrong rather than the first thing.
type problems struct {
	lead  string
	items []string
}

func (p *problems) Error() string {
	if len(p.items) == 1 {
		return p.lead + ": " + p.items[0]
	}
	return p.lead + ":\n  " + strings.Join(p.items, "\n  ")
}

// migration is one run's state: the plan, its environment, the staging
// directory and the record of what is already done.
type migration struct {
	p   *plan.Plan
	env Env
	id  string
	// dir is `<config>/.box/migrate/<plan-id>`: staging, the runner's files
	// and the state record.
	dir string
	st  *State
	res Result
	// checks is every app's Check report, which the Write pass is held to.
	checks map[string]rewrite.Report
	// keys are the API keys read out of the staged copies, waiting for step 8,
	// and keyNames is the same set as names for the summary.
	keys     []keptKey
	keyNames []string
}

// Run performs steps 2 to 10 of a migration. A plan whose every step is already
// recorded returns Done without touching anything, which is what makes a second
// run of the same plan a no-op instead of a re-rename.
func Run(ctx context.Context, p *plan.Plan, planBytes []byte, env Env) (Result, error) {
	res := Result{PlanID: PlanID(planBytes)}
	if env.Confirm == nil {
		return res, errors.New("no confirm: migrate shows every change and asks once before it writes, so a run without a way to ask is not a run")
	}
	m := &migration{p: p, env: env, id: PlanID(planBytes), checks: map[string]rewrite.Report{}}
	m.res.PlanID = m.id
	if err := Preflight(p, planBytes, env); err != nil {
		return res, err
	}
	m.dir = stateDir(env.ConfigRoot, m.id)
	st, err := loadState(m.dir)
	if err != nil {
		return res, err
	}
	st.Plan = PlanDigest(planBytes)
	m.st = st
	if st.Values {
		res.Done = true
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := m.stage(ctx); err != nil {
		return m.res, err
	}
	if err := m.check(ctx); err != nil {
		return m.res, err
	}
	if err := m.readKeys(); err != nil {
		return m.res, err
	}
	if err := m.show(); err != nil {
		return m.res, err
	}
	if err := m.write(ctx); err != nil {
		return m.res, err
	}
	if err := m.own(); err != nil {
		return m.res, err
	}
	if err := m.renameData(); err != nil {
		return m.res, err
	}
	if err := m.move(); err != nil {
		return m.res, err
	}
	if err := m.keepKeys(ctx); err != nil {
		return m.res, err
	}
	if err := m.adoptOwned(ctx); err != nil {
		return m.res, err
	}
	if err := m.enable(); err != nil {
		return m.res, err
	}
	m.cleanStaging()
	return m.res, nil
}

// Preflight is step 1: every problem the plan and the seedbox have together,
// before anything is staged. It writes nothing, so a refusal leaves the host
// exactly as it was and the source's own tree untouched.
func Preflight(p *plan.Plan, planBytes []byte, env Env) error {
	if p == nil {
		return errors.New("no plan: migrate applies a plan written by `box migrate discover`")
	}
	var items []string
	id := PlanID(planBytes)
	dir := stateDir(env.ConfigRoot, id)
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	if err := otherPlan(filepath.Join(env.ConfigRoot, ".box", "migrate"), id, PlanDigest(planBytes)); err != nil {
		items = append(items, err.Error())
	}
	if !isDir(env.ConfigRoot) {
		items = append(items, fmt.Sprintf("storage.config.path %s is not a directory: migrate adopts into the config root an installed seedbox already has, so run `box init` first", env.ConfigRoot))
	}
	items = append(items, preflightData(p, env, st)...)
	names := planApps(p)
	for _, name := range names {
		items = append(items, preflightApp(p, env, st, name)...)
	}
	if len(items) > 0 {
		return &problems{lead: "this plan cannot be applied to this seedbox", items: items}
	}
	return nil
}

// preflightData checks the data root and the two directories a rename moves.
func preflightData(p *plan.Plan, env Env, st *State) []string {
	d := p.Data
	if d == nil {
		return nil
	}
	if env.Rehearsal {
		// A rehearsal adopts no data: it never renames, and the copy may not
		// have the source's tree at all. Torrents start stopped precisely so
		// that nothing reads it back, and the paths in the configs are still
		// rewritten to where box would find the data on a real run.
		return nil
	}
	root := path.Clean(env.DataRoot)
	if root != path.Clean(d.Root) {
		return []string{fmt.Sprintf("data.root is %s and this seedbox's storage.data.path is %s: a migration adopts the data where it is, so the plan's root has to be the grant's path", d.Root, env.DataRoot)}
	}
	var items []string
	for _, pair := range [][2]string{{d.Downloads, plan.DownloadsDir}, {d.Media, plan.MediaDir}} {
		from, to := filepath.Join(env.DataRoot, pair[0]), filepath.Join(env.DataRoot, pair[1])
		if has(st.Renamed, pair[0]) {
			// An earlier run of this plan made the rename; the source name is
			// gone by design.
			continue
		}
		if !isDir(from) {
			items = append(items, fmt.Sprintf("data: %s is not a directory: the plan says the source downloaded into %s under %s", from, pair[0], env.DataRoot))
			continue
		}
		if pair[0] == pair[1] || !exists(to) {
			continue
		}
		// The box's own /data/{downloads,media}/<category> directories are
		// empty, and the rename removes them: only a target with a file
		// anywhere under it is the operator's data, and moving that by hand is
		// the refusal's business.
		if empty, err := emptyTree(to); err != nil || !empty {
			items = append(items, fmt.Sprintf("data: %s already exists and is not empty, and %s has to become %s: move its contents into %s by hand first",
				to, pair[0], pair[1], from))
		}
	}
	return items
}

// preflightApp checks one instance: that this box can rewrite the app at all,
// that the seedbox is not already running it, that its source config is there,
// and that the directory it moves into is free.
func preflightApp(p *plan.Plan, env Env, st *State, name string) []string {
	app := p.Apps[name]
	catalog := CatalogApp(name, env.Catalog)
	if catalog == "" {
		return []string{fmt.Sprintf("apps.%s: no app in this chart's catalog is named that (the catalog has %s)", name, strings.Join(env.Catalog, ", "))}
	}
	var items []string
	if _, ok := rewrite.For(catalog); !ok {
		items = append(items, fmt.Sprintf("apps.%s: this box has no rewriter for %s, so its config cannot be rewritten (it has %s); a plan naming an app box cannot read cannot be applied",
			name, catalog, strings.Join(rewrite.Apps(), ", ")))
	}
	if _, ok := keyed(catalog); ok && env.Secret == nil {
		items = append(items, fmt.Sprintf("apps.%s: %s keeps its API key in its own config, and this run has no release Secret to keep that key in", name, catalog))
	}
	if NeedsRunner(catalog) && env.Run == nil {
		items = append(items, fmt.Sprintf("apps.%s: %s's rewriter runs a tool from the app's own pinned image, and this run has no way to start one", name, catalog))
	}
	src := filepath.Join(env.Configs, name)
	if !isDir(src) {
		items = append(items, fmt.Sprintf("apps.%s: the snapshot holds no config for %s (%s is not a directory); apply rewrites the configs the snapshot carries, and the plan says the source kept them at %s", name, name, src, app.Config))
	}
	dst := filepath.Join(env.ConfigRoot, name)
	if !has(st.Moved, name) {
		if env.Enabled != nil && env.Enabled(name) {
			items = append(items, fmt.Sprintf("apps.%s: this seedbox already runs %s; migrate adopts into a seedbox whose apps are off, so run `box init` with %s left off and apply again", name, name, name))
		}
		if exists(dst) {
			if empty, err := emptyDir(dst); err != nil || !empty {
				items = append(items, fmt.Sprintf("apps.%s: %s is not empty; the plan moves %s's config there, so move what is in it first", name, dst, name))
			}
		}
	}
	if ep, ok := env.Targets[name]; !ok || ep.Port == 0 {
		items = append(items, fmt.Sprintf("apps.%s: the release has no endpoint for it, so its own port and URL base cannot be set", name))
	}
	return items
}

// NeedsRunner reports whether an app's rewriter can only work with a runner:
// the rewriter says so itself (rewrite.RunnerUser), so this package names no
// app. Preflight refuses a plan naming one on a run that cannot start images,
// before anything is staged.
func NeedsRunner(app string) bool {
	r, ok := rewrite.For(app)
	if !ok {
		return false
	}
	u, ok := r.(rewrite.RunnerUser)
	return ok && u.NeedsRunner()
}

// planApps is the plan's instance names, sorted.
func planApps(p *plan.Plan) []string {
	out := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// stage is step 2: the whole of every source config directory, verbatim, into
// the staging tree.
func (m *migration) stage(ctx context.Context) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", m.dir, err)
	}
	for _, name := range planApps(m.p) {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := m.staging(name)
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("clear %s: %w", dst, err)
		}
		src := filepath.Join(m.env.Configs, name)
		staged, err := copyTree(dst, src, m.env.PUID, m.env.PGID)
		if err != nil {
			return fmt.Errorf("stage %s from %s: %w", name, src, err)
		}
		staged.App = name
		m.res.Staged = append(m.res.Staged, staged)
	}
	return nil
}

// staging is one instance's staged config directory.
func (m *migration) staging(name string) string { return filepath.Join(m.dir, name) }

// check is step 3: every app's Check pass, all of them, before any Write.
func (m *migration) check(ctx context.Context) error {
	var items []string
	for _, name := range planApps(m.p) {
		if err := ctx.Err(); err != nil {
			return err
		}
		app := CatalogApp(name, m.env.Catalog)
		rw, ok := rewrite.For(app)
		if !ok {
			return fmt.Errorf("apps.%s: no rewriter for %s", name, app)
		}
		rep, err := rw.Check(ctx, m.input(name))
		if err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
		rep.App = name
		m.checks[name] = rep
		m.res.Reports = append(m.res.Reports, rep)
		items = append(items, settleItems(rep)...)
	}
	if len(items) > 0 {
		return &problems{
			lead:  "the plan does not settle everything these configs say, and nothing has been written outside " + m.dir,
			items: items,
		}
	}
	return nil
}

// settleItems is every gap and unclassified address one report found, each with
// the plan key that would settle it.
func settleItems(rep rewrite.Report) []string {
	var items []string
	for _, u := range rep.Uncovered {
		where := u.App
		if u.File != "" {
			where += " " + u.File
		}
		if u.Key != "" {
			where += " " + u.Key
		}
		items = append(items, fmt.Sprintf("%s: %s no rule covers; add a rule for %s to apps.%s.paths", where, u.Path, u.Path, u.App))
	}
	for _, a := range rep.Unclassified {
		items = append(items, fmt.Sprintf("%s: %s:%d no host rule settles; add {host: %s, port: %d, app: <instance>} or {host: %s, leave: true} to hosts",
			addressWhere(a), a.Host, a.Port, a.Host, a.Port, a.Host))
	}
	return items
}

// addressWhere names one unclassified address' place.
func addressWhere(a rewrite.Address) string {
	where := a.App
	if a.File != "" {
		where += " " + a.File
	}
	if a.Where != "" {
		where += " " + a.Where
	}
	return where
}

// input is what one app's rewriter gets: the staged directory, the plan, the
// endpoints and the runner.
func (m *migration) input(name string) rewrite.Input {
	return rewrite.Input{
		App:       name,
		Dir:       m.staging(name),
		Plan:      m.p,
		Targets:   m.env.Targets,
		Runs:      CatalogRuns(m.p, m.env.Catalog),
		Run:       m.env.Run,
		Rehearsal: m.env.Rehearsal,
	}
}

// show is step 4: the summary, and the one question.
func (m *migration) show() error {
	m.res.Summary = m.summary()
	ok, err := m.env.Confirm(m.res.Summary)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCancelled
	}
	return nil
}

// summary is the whole change, from the Check reports: nothing here reads a
// file, so the operator's review is of what apply is about to do.
func (m *migration) summary() Summary {
	s := Summary{
		Outside:      append([]string(nil), m.p.Outside...),
		Defaults:     m.p.Defaults(),
		DefaultRules: m.p.DefaultRules(),
		Carried:      append([]Carried(nil), m.env.Carried...),
		Rehearsal:    m.env.Rehearsal,
	}
	// A rehearsal renames nothing (renameData), so its summary lists nothing
	// to rename.
	if m.p.Data != nil && !m.env.Rehearsal {
		for _, pair := range m.p.Data.Renames() {
			if has(m.st.Renamed, pair[0]) {
				continue
			}
			s.Renames = append(s.Renames, pair[0]+" to "+pair[1])
		}
	}
	for _, name := range planApps(m.p) {
		rep := m.checks[name]
		in := InstanceSummary{
			App:      name,
			Changes:  rep.Changes,
			Accounts: rep.Accounts,
			Notes:    rep.Notes,
			Left:     rep.Left,
			Owned:    rep.Owned,
			Stopped:  rep.Stopped,
			Enable:   !m.alreadyEnabled(name),
		}
		for _, st := range m.res.Staged {
			if st.App == name {
				in.Staged = st
			}
		}
		for _, k := range m.keys {
			if k.instance == name {
				in.Keys = append(in.Keys, k.name)
			}
		}
		s.Instances = append(s.Instances, in)
	}
	for _, rep := range m.checks {
		s.Stopped += rep.Stopped
	}
	return s
}

// alreadyEnabled reports whether the seedbox switches an instance on already.
func (m *migration) alreadyEnabled(name string) bool {
	return m.env.Enabled != nil && m.env.Enabled(name)
}

// write is step 5: every app's Write pass, held to the report its Check made.
func (m *migration) write(ctx context.Context) error {
	for _, name := range planApps(m.p) {
		if err := ctx.Err(); err != nil {
			return err
		}
		app := CatalogApp(name, m.env.Catalog)
		rw, _ := rewrite.For(app)
		rep, err := rw.Write(ctx, m.input(name))
		if err != nil {
			return fmt.Errorf("rewrite %s: %w", name, err)
		}
		rep.App = name
		if why, same := sameReport(m.checks[name], rep); !same {
			return fmt.Errorf("%s: the write pass changed something its check did not report (%s); nothing else has been rewritten, so the staged copy in %s can be looked at",
				name, why, m.staging(name))
		}
		m.res.Notes = append(m.res.Notes, rep.Notes...)
	}
	return nil
}

// sameReport compares a Check and a Write report: the same changes, counted the
// same way, and the same adopted connections.
func sameReport(check, write rewrite.Report) (string, bool) {
	a, b := flattenChanges(check.Changes), flattenChanges(write.Changes)
	if len(a) != len(b) {
		return fmt.Sprintf("its check reported %d changes and the write made %d", len(a), len(b)), false
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Sprintf("the first difference is %q", b[i]), false
		}
	}
	owned := func(rs []rewrite.Owned) []string {
		out := make([]string, 0, len(rs))
		for _, o := range rs {
			out = append(out, fmt.Sprintf("%s %d %s %q %s", o.Kind, o.ID, o.StringID, o.Name, o.Target))
		}
		sort.Strings(out)
		return out
	}
	x, y := owned(check.Owned), owned(write.Owned)
	if strings.Join(x, "\n") != strings.Join(y, "\n") {
		return "the adopted connections differ", false
	}
	if check.Stopped != write.Stopped {
		return fmt.Sprintf("its check counts %d torrents to stop and the write stopped %d", check.Stopped, write.Stopped), false
	}
	accounts := func(rs []rewrite.Account) []string {
		out := make([]string, 0, len(rs))
		for _, a := range rs {
			out = append(out, a.From+" -> "+a.To)
		}
		sort.Strings(out)
		return out
	}
	if got, want := strings.Join(accounts(write.Accounts), "\n"), strings.Join(accounts(check.Accounts), "\n"); got != want {
		return "the accounts it renames differ from the ones its check reported", false
	}
	return "", true
}

// flattenChanges renders changes as one sorted line each, so two reports with
// the same counts in another order compare equal.
func flattenChanges(changes []rewrite.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, fmt.Sprintf("%s %s %s %d", c.File, c.Where, c.What, c.Count))
	}
	sort.Strings(out)
	return out
}

// renameData is step 6: the two directories under the data root, renamed to the
// names box's contract uses. A rename is the only write migrate makes to the
// data, and it is a rename because that is what keeps the hardlinks.
func (m *migration) renameData() error {
	if m.p.Data == nil || m.env.Rehearsal {
		// A rehearsal never renames: the source may be live, and the data may
		// not even be on this host.
		return nil
	}
	for _, pair := range m.p.Data.Renames() {
		from, to := filepath.Join(m.env.DataRoot, pair[0]), filepath.Join(m.env.DataRoot, pair[1])
		if has(m.st.Renamed, pair[0]) {
			continue
		}
		if !exists(from) && exists(to) {
			// A re-run of a run that renamed but did not record it: the
			// target is what this step leaves behind, so it is done.
			if err := m.recordRenamed(pair[0]); err != nil {
				return err
			}
			continue
		}
		if exists(to) {
			// A box's own release makes /data/downloads/<category> for every
			// Arr it runs, so a target of empty directories is the box's, not
			// the operator's data: it goes, and the source is renamed into the
			// name. A target with a file anywhere under it stops the run.
			empty, err := emptyTree(to)
			if err != nil {
				return fmt.Errorf("inspect %s: %w", to, err)
			}
			if !empty {
				return fmt.Errorf("%s appeared while this migration ran and is not empty; what %s has to become is a rename of it, so move that directory's contents into %s by hand", to, pair[0], from)
			}
			if err := removeEmptyTree(to); err != nil {
				return fmt.Errorf("remove the empty %s: %w", to, err)
			}
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("rename %s to %s: %w", from, to, err)
		}
		if err := m.recordRenamed(pair[0]); err != nil {
			return err
		}
		m.res.Renamed = append(m.res.Renamed, pair[0]+" to "+pair[1])
	}
	return nil
}

// recordRenamed writes one rename into the state before the next step starts.
func (m *migration) recordRenamed(name string) error {
	m.st.Renamed = append(m.st.Renamed, name)
	return saveState(m.dir, m.st)
}

// move is step 7: each staged config becomes the instance's directory under the
// config root.
func (m *migration) move() error {
	for _, name := range planApps(m.p) {
		if has(m.st.Moved, name) {
			continue
		}
		src, dst := m.staging(name), filepath.Join(m.env.ConfigRoot, name)
		if exists(dst) {
			empty, err := emptyDir(dst)
			if err != nil {
				return fmt.Errorf("inspect %s: %w", dst, err)
			}
			if !empty {
				return fmt.Errorf("%s appeared while this migration ran and is not empty; migrate moves %s there and will not replace it", dst, name)
			}
			if err := os.Remove(dst); err != nil {
				return fmt.Errorf("remove the empty %s: %w", dst, err)
			}
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s into %s: %w", src, dst, err)
		}
		m.st.Moved = append(m.st.Moved, name)
		if err := saveState(m.dir, m.st); err != nil {
			return err
		}
		m.res.Moved = append(m.res.Moved, name)
	}
	return nil
}

// adoptOwned is step 9: the adopted connections go into the release's ownership
// record, so wiring takes them over instead of creating a second one.
func (m *migration) adoptOwned(ctx context.Context) error {
	if m.st.Owned {
		return nil
	}
	if m.env.Owned == nil {
		for _, rep := range m.res.Reports {
			if len(rep.Owned) > 0 {
				return fmt.Errorf("%s: this run adopted connections and has no ownership record to put them in, and a release that cannot adopt them by id creates a second download client beside the one it migrated", rep.App)
			}
		}
		return nil
	}
	raw, err := m.env.Owned.Read(ctx)
	if err != nil {
		return fmt.Errorf("read owned.json: %w", err)
	}
	rec, seeded, err := seedOwned(raw, m.res.Reports, m.apps())
	if err != nil {
		return err
	}
	if len(seeded) > 0 {
		data, err := encodeOwned(rec)
		if err != nil {
			return err
		}
		if err := m.env.Owned.Write(ctx, data); err != nil {
			return fmt.Errorf("write owned.json: %w", err)
		}
	}
	m.res.Owned = seeded
	m.st.Owned = true
	return saveState(m.dir, m.st)
}

// apps is the instance-to-catalog-app map the ownership seed needs.
func (m *migration) apps() map[string]string {
	out := make(map[string]string, len(m.p.Apps))
	for name := range m.p.Apps {
		out[name] = CatalogApp(name, m.env.Catalog)
	}
	return out
}

// enable is step 10: every instance the seedbox does not switch on yet is
// switched on, in the files, through the editor that splices rather than
// rewriting.
func (m *migration) enable() error {
	if m.env.Values == nil {
		return errors.New("no values editor: a migration switches the instances it moved on in the seedbox's files, and this run cannot")
	}
	for _, name := range planApps(m.p) {
		if m.alreadyEnabled(name) {
			continue
		}
		if err := m.env.Values.Enable(name); err != nil {
			return fmt.Errorf("enable %s: %w", name, err)
		}
		m.res.Enabled = append(m.res.Enabled, name)
	}
	m.st.Values = true
	return saveState(m.dir, m.st)
}

// cleanStaging removes the staged copies and the runner's files once every step
// is recorded. The record stays: it is what makes a re-run a no-op and what
// stops a second plan from starting over a migration that did not finish.
func (m *migration) cleanStaging() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "state.json" {
			continue
		}
		os.RemoveAll(filepath.Join(m.dir, e.Name()))
	}
}

// has reports whether list holds name.
func has(list []string, name string) bool {
	for _, got := range list {
		if got == name {
			return true
		}
	}
	return false
}

// bytesText renders a byte count the way the summary says it.
func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	v, exp := float64(n)/unit, 0
	for v >= unit && exp < 3 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", v, []string{"KiB", "MiB", "GiB", "TiB"}[exp])
}

// ownedID is an adopted connection's id, whichever kind it is.
func ownedID(o rewrite.Owned) string {
	if o.StringID != "" {
		return o.StringID
	}
	return fmt.Sprintf("%d", o.ID)
}
