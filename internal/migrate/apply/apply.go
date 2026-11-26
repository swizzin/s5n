// Package apply is the write half of `box migrate`: it adopts a source into an
// installed seedbox. Every step refuses before it writes -- plan and seedbox read,
// problems collected, configs staged and checked, one confirm -- and no app's
// Write runs until every Check passed, so one uncovered path stops the migration
// before its first write (docs/box.md, "Shown before it's done"). The source is
// never written: the copy is what the release runs, and the source stays the
// rollback. Only runner.go talks to a cluster, through a clientset a fake drives.
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
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/space"
)

// ErrCancelled says the operator answered no, or could not be asked. Nothing
// has been written at that point: the confirm is the last read-only step.
var ErrCancelled = errors.New("cancelled")

// Env is everything apply touches outside the plan directory, injected so every
// refusal and every write is testable without a cluster or an installed box.
type Env struct {
	// ConfigRoot and DataRoot are the grant's two host paths: an instance's
	// directory goes under the first, renames happen in the second.
	ConfigRoot, DataRoot string
	// Configs holds one config tree per app, as the snapshot carries them; the
	// plan's `config:` is informational, since apply never reads the source host.
	Configs string
	// PUID and PGID own everything staged: the pod's user is not box's.
	PUID, PGID int
	// Chown hands a staged path to the seedbox's user, never following a link
	// (own.go). Nil means os.Lchown.
	Chown func(path string, uid, gid int) error
	// Disk answers the room question (carry.go); nil means the host's own.
	Disk space.Disk
	// Bulk is the rest of a full carry's config, staged once from the snapshot
	// rather than extracted beside the checks; nil means Configs is all of it.
	Bulk Bulk
	// Targets is where every instance answers inside the release (chart.go,
	// Targets).
	Targets map[string]rewrite.Endpoint
	// Run executes a tool inside an app's pinned image. nil when this caller
	// cannot run one: the one rewriter that needs it (Plex) then refuses with
	Run rewrite.Runner
	// Enabled reports whether the seedbox already switches an instance on.
	Enabled func(instance string) bool
	// Owned is the release's wiring ownership record. nil on a run that only
	// wants the files moved (the tests, and a box whose migration cannot seed
	Owned OwnedStore
	// Secret is the release Secret, where each app's own API key is written so
	Secret KeyStore
	// Values turns an instance on in the seedbox's files.
	Values ValuesEditor
	// Confirm shows the summary and asks the one question; nil is refused, and
	// unattended runs say --yes, which cmd/box turns into an answering Confirm.
	Confirm func(summary Summary) (bool, error)
	// Catalog is the chart release's app names: applying into a chart without a
	// planned app is a refusal before the first write.
	Catalog []string
	// Image is the release's image for an app ("" when it has none), for the
	// confirm's version column (versions.go); nil leaves every row `?`.
	Image func(app string) string
	// Rehearsal applies a rehearsal snapshot: the release is locked, data is
	// neither adopted nor renamed, and a rewriter that would act on the network
	Rehearsal bool
	// deferred is Survey's mode: an app needing its own image is checked after the
	// install. Its row carries InstanceSummary.Deferred.
	deferred bool
}

// ValuesEditor turns one instance on in the seedbox's files, only when the values
// do not already enable it. It edits apps.<app>.enabled or adds an
// apps.<app>.instances entry -- both grant keys (internal/seedbox/grant.go) -- the
// way `box init` does. downloadCategory is the torrents' existing category, empty
// when the source said nothing and the instance gets its own.
type ValuesEditor interface {
	Enable(instance, downloadCategory string) error
}

// Summary is everything the operator sees before anything moves.
type Summary struct {
	// Instances are the plan's apps, sorted by name.
	Instances []InstanceSummary
	// Renames and MediaRenames are the data and library renames (plan.Data).
	Renames      []string
	MediaRenames []string
	// Defaults are the plan's own decisions (plan.Defaults), never silent: the
	// confirm names every one.
	Defaults []string
	// Outside are the source's workloads box has no app for.
	Outside []string
	// Rehearsal says nothing leaves the box and no data is renamed.
	Rehearsal bool
	// Stopped is how many torrents this run writes stopped.
	Stopped int
	// DefaultRules is Defaults in a form a grouped screen counts by.
	DefaultRules []plan.Default
	// Carried are the notes about what each staged config holds.
	Carried []Carried
}

// Carried is one app's note about what its staged config holds.
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
	// Left are the values box will not touch, with reasons; they stop nothing, so
	// they are shown before the confirm.
	Left []rewrite.Left
	// Accounts are the account rows renamed to box's (rewrite.Account).
	Accounts []rewrite.Account
	// Enable says this run switches the instance on.
	Enable bool
	// Keys are the release Secret's key names kept, never their values.
	Keys []string
	// Token is the release Secret's name for the Plex token, on the Plex row only.
	Token string
	// Owned are the connections that will be adopted by id.
	Owned []rewrite.Owned
	// Stopped is how many stored objects this run writes stopped.
	Stopped int
	// Deferred is the sentence a pre-install pass left instead of a check.
	Deferred string
	// Carry is what this run stages of a config that is more than its state.
	Carry string
	// Short names the numbers when the run carries less than the plan says for
	// lack of room; the confirm owes the operator that.
	Short string
	// Version is the source's image against this release's (versions.go).
	Version AppVersion
}

// Lines renders the summary as rows in reading order: changes, what box leaves,
// renames, switches, keys, adoptions, defaults, leftovers.
func (s Summary) Lines() []string {
	var rows []string
	if s.Rehearsal {
		rows = append(rows, "rehearsal: nothing leaves this box, and no data is renamed")
	}
	for _, in := range s.Instances {
		if in.Staged.Files > 0 {
			rows = append(rows, fmt.Sprintf("%s: copy %d files, %s", in.App, in.Staged.Files, bytesText(in.Staged.Bytes)))
		}
		if in.Carry != "" {
			rows = append(rows, fmt.Sprintf("%s: carries %s", in.App, in.Carry))
		}
		if in.Short != "" {
			rows = append(rows, in.Short)
		}
		if in.Deferred != "" {
			rows = append(rows, fmt.Sprintf("%s: %s", in.App, in.Deferred))
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
	for _, r := range s.MediaRenames {
		rows = append(rows, "data: rename media/"+r)
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
		if in.Token != "" {
			// Not an app's own key: the server token every Plex-dependent app
			// reads. Named as the token, never printed.
			rows = append(rows, fmt.Sprintf("%s: keep its Plex token (%s)", in.App, in.Token))
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
	// Done says an earlier run of this plan finished, so nothing was done.
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
	// Notes are lines the operator learns after the writes.
	Notes []string
}

// problems is one refusal with every problem in it: an operator fixes one page,
// not one line.
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

// migration is one run's state: plan, environment, staging and the record.
type migration struct {
	p   *plan.Plan
	env Env
	id  string
	// dir is `<config>/.box/migrate/<plan-id>`: staging and the state record.
	dir string
	st  *State
	res Result
	// checks is every app's Check report, which Write is held to.
	checks map[string]rewrite.Report
	// deferred are the apps whose rewriter needs the app's image, checked later.
	deferred map[string]bool
	// carry is what this run stages of a config that is more than its state,
	// settled before the first byte.
	carry map[string]carryDecision
	// keys are the API keys read from the staged copies; keyNames is for the
	// summary.
	keys     []keptKey
	keyNames []string
}

// Run performs steps 2 to 10. A plan whose steps are recorded returns Done
// untouched, so a second run is a no-op, not a re-rename.
func Run(ctx context.Context, p *plan.Plan, planBytes []byte, env Env) (Result, error) {
	res := Result{PlanID: PlanID(planBytes)}
	if env.Confirm == nil {
		return res, errors.New("no confirm: migrate shows every change and asks once before it writes, so a run without a way to ask is not a run")
	}
	m := &migration{p: p, env: env, id: PlanID(planBytes), checks: map[string]rewrite.Report{}, deferred: map[string]bool{}}
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

// StoppedBeforeWrite reports an error returned before the first write outside
// staging -- a preflight refusal or a Check gap -- which is the only case where
func StoppedBeforeWrite(err error) bool {
	var p *problems
	return errors.As(err, &p)
}

// Preflight is step 1: every problem found together, writing nothing.
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
		// A rehearsal adopts no data: nothing is renamed, torrents start stopped,
		// and config paths are still rewritten for a real run.
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
			// An earlier run made the rename; the source name is gone by design.
			continue
		}
		if !isDir(from) {
			items = append(items, fmt.Sprintf("data: %s is not a directory: the plan says the source downloaded into %s under %s", from, pair[0], env.DataRoot))
			continue
		}
		if pair[0] == pair[1] || !exists(to) {
			continue
		}
		// Only a target holding a file is the operator's data; an empty box-made
		// directory is removed by the rename.
		if empty, err := emptyTree(to); err != nil || !empty {
			items = append(items, fmt.Sprintf("data: %s already exists and is not empty, and %s has to become %s: move its contents into %s by hand first",
				to, pair[0], pair[1], from))
		}
	}
	// The library's own directories: one rename each, inside the media
	// directory, which moves this run carried out or not.
	for _, pair := range d.MediaRenames() {
		if has(st.Renamed, mediaKey(pair[0])) {
			continue
		}
		from, to, ok := mediaRename(env.DataRoot, d, pair)
		if !ok {
			if exists(to) {
				// A rename an earlier run made without recording it.
				continue
			}
			problems := fmt.Sprintf("data: %s is not a directory: the plan says %s becomes media/%s, so name the directory the source's library has",
				from, pair[0], pair[1])
			items = append(items, problems)
			continue
		}
		if !exists(to) {
			continue
		}
		if empty, err := emptyTree(to); err != nil || !empty {
			items = append(items, fmt.Sprintf("data: %s already exists and is not empty, and %s has to become %s: move its contents into %s by hand first",
				to, from, to, from))
		}
	}
	return items
}

// preflightApp checks one instance: that this box can rewrite the app at all,
// that the seedbox is not already running it, that its source config is there,
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
	if _, ok := keyed(catalog); ok && env.Secret == nil && !env.deferred {
		items = append(items, fmt.Sprintf("apps.%s: %s keeps its API key in its own config, and this run has no release Secret to keep that key in", name, catalog))
	}
	if NeedsRunner(catalog) && env.Run == nil && !env.deferred {
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
// app. Preflight refuses a plan naming one on a run that cannot start images,
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
// the staging tree; less, for an app whose config is more than its state, when
// this run finds no room for the whole of it (carry.go).
func (m *migration) stage(ctx context.Context) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", m.dir, err)
	}
	if err := m.decideCarry(); err != nil {
		return err
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
		staged, err := m.copyInto(name, dst, src)
		if err != nil {
			return fmt.Errorf("stage %s from %s: %w", name, src, err)
		}
		staged.App = name
		m.res.Staged = append(m.res.Staged, staged)
	}
	return nil
}

// copyInto is the copy one pass makes: a run stages what the release will run
// and owns it as the seedbox's user; a Survey pass only measures the same bytes,
// and the tree it counts is thrown away.
//
// Two apps are copied less than their whole config: one whose config is more than
// its state, whose staged copy is the tree this run decided it can hold
// (carry.go), and -- for the pass before an install -- the same app measured by a
// walk rather than copied, because a library of covers is tens of gigabytes and
// that pass stages into a working directory it removes.
func (m *migration) copyInto(name, dst, src string) (Staged, error) {
	treeApp := CatalogApp(name, m.env.Catalog) == appPlex
	carry := m.carryOf(name)
	if m.env.deferred {
		if treeApp {
			// The pass before an install stages nothing for this app: its check
			if m.env.Bulk != nil {
				return m.env.Bulk.Measure(name)
			}
			state := plex.Snapshot(false)
			if carry.Full {
				files, bytes, err := space.Tally(src, nil)
				if err != nil {
					return Staged{}, err
				}
				return Staged{Files: files, Bytes: bytes}, nil
			}
			names := make([]string, len(state.Files))
			for i, f := range state.Files {
				names[i] = f.Path
			}
			return measureFiles(dst, carriedDir(src, state.Sub), names)
		}
		return measureTree(dst, src)
	}
	if treeApp && !carry.Full {
		return stageTrimmed(dst, src, plex.Snapshot(false), m.env.PUID, m.env.PGID)
	}
	if treeApp && m.env.Bulk != nil {
		// The whole of it, once, out of the snapshot: the bulk was never
		return m.env.Bulk.Stage(name, dst, m.env.PUID, m.env.PGID)
	}
	return copyTree(dst, src, m.env.PUID, m.env.PGID)
}

// staging is one instance's staged config directory.
func (m *migration) staging(name string) string { return filepath.Join(m.dir, name) }

// check is step 3: every app's Check pass, all of them, before any Write. A
// Survey pass (before the install) leaves the apps whose rewriter runs the app's
// own image to the run after it; every other app is checked here exactly as it
// is there.
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
		if m.env.deferred && NeedsRunner(app) {
			// The check is a query through the app's pinned image, and there is
			m.deferred[name] = true
			continue
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
		Rehearsal:    m.env.Rehearsal,
	}
	for _, name := range planApps(m.p) {
		if note, _ := m.carried(name); note != "" {
			s.Carried = append(s.Carried, Carried{App: name, Note: note})
		}
	}
	// A rehearsal renames nothing (renameData), so its rows say what the real
	if m.p.Data != nil {
		for _, pair := range m.p.Data.Renames() {
			if has(m.st.Renamed, pair[0]) {
				continue
			}
			s.Renames = append(s.Renames, pair[0]+" to "+pair[1])
		}
		for _, pair := range m.p.Data.MediaRenames() {
			if has(m.st.Renamed, mediaKey(pair[0])) {
				continue
			}
			s.MediaRenames = append(s.MediaRenames, pair[0]+" to "+pair[1])
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
		if m.deferred[name] {
			in.Deferred = deferredSentence(CatalogApp(name, m.env.Catalog))
		}
		if _, phrase := m.carried(name); phrase != "" {
			in.Carry = phrase
		}
		in.Version = m.version(name)
		in.Short = m.carryOf(name).Short
		for _, st := range m.res.Staged {
			if st.App == name {
				in.Staged = st
			}
		}
		for _, k := range m.keys {
			if k.instance != name {
				continue
			}
			if k.name == plexTokenKey {
				in.Token = k.name
				continue
			}
			in.Keys = append(in.Keys, k.name)
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

// version is one app's version pair: the image the source ran, from the plan, and
// the image this release runs for it. A chart with no image for the app, or a
// caller with no chart, leaves the row `?` rather than judging a pair it cannot
// read.
func (m *migration) version(name string) AppVersion {
	app := CatalogApp(name, m.env.Catalog)
	release := ""
	if m.env.Image != nil && app != "" {
		release = m.env.Image(app)
	}
	return versionOf(name, m.p.Apps[name].Image, release)
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
		m.res.Notes = appendNotes(m.res.Notes, rep.Notes, m.checks[name].Notes)
	}
	return nil
}

// appendNotes adds the notes a Write pass reported and its Check did not: a
// note the confirm already showed, with the app it belongs to, is not printed
// again after the run, and a note only the write could know is.
func appendNotes(into, wrote, checked []string) []string {
	shown := make(map[string]bool, len(checked))
	for _, note := range checked {
		shown[note] = true
	}
	for _, note := range wrote {
		if shown[note] {
			continue
		}
		into = append(into, note)
	}
	return into
}

// sameReport compares a Check and a Write report: the same changes, counted the
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
func flattenChanges(changes []rewrite.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, fmt.Sprintf("%s %s %s %d", c.File, c.Where, c.What, c.Count))
	}
	sort.Strings(out)
	return out
}

// renameData is step 6: the two directories under the data root, renamed to the
// names box's contract uses, and then the library's own directories, renamed to
// the categories the release lays out. A rename is the only write migrate makes
// to the data, and it is a rename because that is what keeps the hardlinks.
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
			if err := m.removeEmptyTarget(to); err != nil {
				return err
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
	return m.renameLibrary()
}

// renameLibrary is the second half of step 6: every directory the source kept an
// instance's library in becomes the category box's release uses, inside the
// library's own directory.
//
// They come after the two contract renames, so whichever name the source gave
// the library, the folders move in the one box gives it. A parent a rename
// emptied -- the source's 4K/ holding 4K/Movies and 4K/TV -- goes too, and only
// when it is empty.
func (m *migration) renameLibrary() error {
	var parents []string
	for _, pair := range m.p.Data.MediaRenames() {
		key := mediaKey(pair[0])
		if has(m.st.Renamed, key) {
			continue
		}
		from, to, ok := mediaRename(m.env.DataRoot, m.p.Data, pair)
		if !ok {
			if exists(to) {
				// A rename an earlier run made without recording it: the
				// target is what this step leaves behind.
				if err := m.recordRenamed(key); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%s is not a directory, and the plan says %s becomes media/%s", from, pair[0], pair[1])
		}
		if exists(to) {
			if err := m.removeEmptyTarget(to); err != nil {
				return err
			}
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("rename %s to %s: %w", from, to, err)
		}
		if err := m.recordRenamed(key); err != nil {
			return err
		}
		m.res.Renamed = append(m.res.Renamed, "media/"+pair[0]+" to media/"+pair[1])
		if parent := filepath.Dir(from); parent != filepath.Dir(to) {
			parents = append(parents, parent)
		}
	}
	// Deepest first, so 4K/Movies and 4K/TV leaving 4K empty removes it once.
	sort.Slice(parents, func(i, j int) bool { return len(parents[i]) > len(parents[j]) })
	for _, parent := range parents {
		empty, err := emptyDir(parent)
		if err != nil || !empty {
			continue
		}
		_ = os.Remove(parent)
	}
	return nil
}

// removeEmptyTarget removes a rename's target when it holds nothing but empty
// directories, which is what a box's own release leaves behind, and refuses one
// that holds a file.
func (m *migration) removeEmptyTarget(to string) error {
	empty, err := emptyTree(to)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", to, err)
	}
	if !empty {
		return fmt.Errorf("%s appeared while this migration ran and is not empty; what has to become a rename of another directory, so move that directory's contents elsewhere by hand", to)
	}
	if err := removeEmptyTree(to); err != nil {
		return fmt.Errorf("remove the empty %s: %w", to, err)
	}
	return nil
}

// mediaKey is a library rename's name in the state record: the library
// directory's own path, so it can never collide with a contract directory's.
func mediaKey(from string) string { return plan.MediaDir + "/" + from }

// mediaRename is one library directory as apply moves it: the directory the
func mediaRename(dataRoot string, d *plan.Data, pair [2]string) (from, to string, ok bool) {
	base := filepath.Join(dataRoot, plan.MediaDir)
	if !isDir(filepath.Join(base, pair[0])) {
		base = filepath.Join(dataRoot, d.Media)
	}
	from = filepath.Join(base, pair[0])
	return from, filepath.Join(base, pair[1]), isDir(from)
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
		if err := m.env.Values.Enable(name, m.p.Apps[name].DownloadCategory); err != nil {
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
