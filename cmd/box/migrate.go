package main

// migrate.go is `box migrate`: the read half, and the dispatch.
//
// `discover` reads an install box did not make and writes a snapshot: the plan
// apply runs, the source's manifests, the report, and -- when it is rehearsing
// -- each planned app's config tree. It changes nothing: the source cluster is
// listed, the source's host files are read through a filesystem rooted where box
// thinks the host's / is, and every manifest read is kept so the plan can be
// rebuilt after the source is gone (docs/box.md, "migrate").
//
// A snapshot is one file (internal/archive's format, internal/migrate/bundle's
// shape) in /var/lib/s5n/migrate/, named for the time the source was read and
// the mode it was read in. A survey holds no configs, so it can be taken at any
// time on a running source; a rehearsal and a final snapshot carry them.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3" // only cmd/box may import it (AGENTS.md): the plan is a YAML file and cmd/s5n stays stdlib-only

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// migrateUsage is what a bad invocation and `box migrate` print. It is the
// command surface: three halves of one migration, each of which says so in the
// files it writes or refuses to.
const migrateUsage = `usage: box migrate <command> [flags]

commands:
  discover    read a source install and write a survey of it; changes nothing
  cutover     take the final snapshot on the source host, then install or adopt
  apply       install or adopt from a snapshot

box migrate discover [--rehearsal] [--out <file>] [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>] [--plain] [--chart-repo <ref>] [--chart-version <tag>]
box migrate cutover [--replace-cluster] [--no-prior-discover] [--out <file>] [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>] [--chart-repo <ref>] [--chart-version <tag>] [--values <file>] [--yes] [--plain]
box migrate apply [<snapshot>] [--values <file>] [--grant <file>] [--kubeconfig <file>] [--yes] [--plain] [--chart-repo <ref>] [--chart-version <tag>]

A snapshot is one file in ` + bundle.Dir + `/ (or where --out names it). discover writes a
survey -- the plan, the report and the source's manifests, no configs -- and --rehearsal
adds the configs a locked rehearsal applies. cutover writes the final snapshot apply runs.

cutover compares its read with the newest earlier snapshot of the same source (the host and
the namespaces in its plan): an app that read had and this one cannot find stops the run,
and one this read has and that one did not is new and kept. With no earlier read at all it
lists what it found and asks; unattended, it refuses and names 'box migrate discover', which
is the read to take while the source is whole. --no-prior-discover answers that question for
an operator who knows there is nothing earlier to compare against.

Both verbs that read a source run every rewriter's check over the configs they read, so a
plan carries a rule, with its reason, for everything the configs say and no rule settles:
a guided run asks about each one, and an unattended run takes it and says so.
`

// The commands, as names: the dispatch, the usage line and the messages they
// print all spell them once.
const (
	migrateDiscoverCmd = "discover"
	migrateCutoverCmd  = "cutover"
	migrateApplyCmd    = "apply"
)

func runMigrate(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, migrateUsage)
		os.Exit(2)
	}
	switch args[0] {
	case migrateDiscoverCmd:
		runMigrateDiscover(ctx, args[1:])
	case migrateCutoverCmd:
		runMigrateCutover(ctx, args[1:])
	case migrateApplyCmd:
		runMigrateApply(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "box: migrate has no command %q\n\n%s", args[0], migrateUsage)
		os.Exit(2)
	}
}

// namespaces is a repeatable --namespace.
type namespaces []string

func (n *namespaces) String() string { return strings.Join(*n, ",") }

func (n *namespaces) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("an empty namespace: name one, or leave --namespace out for every namespace")
	}
	*n = append(*n, v)
	return nil
}

// discoverOptions is what a read of a source needs: the same flags for discover
// and cutover, because they read one source the same way.
type discoverOptions struct {
	// Out is the snapshot file to write; empty means bundle.Dir with the name
	// its mode and the time give it.
	Out        string
	FromDir    string
	Kubeconfig string
	Namespaces []string
	Root       string
	// chart is the release this run installs. The read needs it for the one
	// thing a source cannot say: which of the chart's blessed variants a
	// source image already is. cutover points it at its apply's flags, so a
	// cutover and the install it hands over to name one release.
	chart *chartFlags
	// Charts is where that release comes from. Nil means the published
	// repository; a test hands over a fixed one.
	Charts release.ChartSource
	// Rehearsal makes discover's snapshot a rehearsal: the configs are copied
	// in, and the snapshot may only be applied locked.
	Rehearsal bool
	// Box is the version that wrote the snapshot, as the manifest records it.
	Box string
	// Now is the clock the snapshot's name and manifest come from. Empty means
	// time.Now: a test that compares two runs' names hands it one.
	Now func() time.Time
	// Prompt asks the defaults Settle took. Nil is an unattended run: every
	// default is taken and the run says so, through Say. A run with a terminal
	// on stdin builds one (runMigrateDiscover, runMigrateCutover); a test
	// hands over its own.
	Prompt setup.Prompter
	// Plain is --plain: ask with line prompts even on a terminal.
	Plain bool
}

func (o discoverOptions) clock() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// addDiscoverFlags registers what both verbs that read a source take, and
// returns the repeatable namespace the caller copies out after parsing.
func addDiscoverFlags(fs *flag.FlagSet, o *discoverOptions, outUsage string) *namespaces {
	fs.StringVar(&o.Out, "out", "", outUsage)
	fs.StringVar(&o.FromDir, "from-dir", "", "read the source's manifests from this directory instead of a cluster")
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "explicit kubeconfig for the source cluster, or fail with no fallback")
	fs.StringVar(&o.Root, "root", "/", "the host root the apps' config directories are read under")
	var ns namespaces
	fs.Var(&ns, "namespace", "limit discovery to this source namespace (repeatable; default every namespace)")
	return &ns
}

func runMigrateDiscover(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("migrate discover", flag.ExitOnError)
	o := discoverOptions{Box: version}
	ns := addDiscoverFlags(fs, &o, "the snapshot file to write (default: "+bundle.Dir+"/migrate-<time>-survey.tar.gz)")
	// The read needs the release for the blessed variants an image maps onto,
	// so discover names the chart the same way the install it leads to does.
	o.chart = addChartFlags(fs, chartVersionInstalls)
	rehearsal := fs.Bool("rehearsal", false, "copy every planned app's config into the snapshot, so it can be applied as a locked rehearsal")
	plain := fs.Bool("plain", false, "ask the defaults with plain line prompts, even on a terminal (for a flaky terminal or a screen reader)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	o.Namespaces = *ns
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "box: migrate discover takes no arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}
	o.Rehearsal = *rehearsal
	o.Plain = *plain
	if isTerminal(os.Stdin) {
		// A read with somebody at the terminal asks about the defaults it takes.
		// Without one it takes them all and says so: a question nobody can
		// answer is not a question.
		o.Prompt = newPrompt(terminalSet(o.Plain), startupTheme(""))
	}
	out := newOutput(terminalSet(o.Plain), false, "")
	defer out.Close()
	text, err := migrateDiscover(ctx, o, out)
	if err != nil {
		if text != "" {
			out.Line(text)
		}
		out.Error(err)
		os.Exit(1)
	}
}

// migrationRead is one read of a source: the plan, the bytes it will be written
// as, what discovery could not migrate, what its checks could not settle, and
// the text the operator sees.
type migrationRead struct {
	plan *plan.Plan
	// src is what the plan was read from. The stop proof and the reset recipe
	// ask it the two things a plan cannot hold -- the pods that are left and
	// the distribution that installed the cluster -- over the same read.
	src discover.Source
	// settled is what the checks could not settle, as Settle decided it: the
	// list report.txt carries and a guided run asks about.
	settled apply.Settled
	// chart is the release this read is writing a plan for: its blessed
	// variants, its catalog and the endpoints its values give every app.
	chart release.Chart
	// raw is the plan's YAML, which is what a snapshot carries and what apply's
	// plan id is a digest of.
	raw    []byte
	report discover.Report
	text   string
}

// migrateDiscover is the command without the exit, so a test drives it. The
// returned text is what it printed (and wrote into the snapshot's report.txt),
// which an error also carries: a run that stopped halfway still has something
// to say about what it read.
func migrateDiscover(ctx context.Context, o discoverOptions, out say.Reporter) (string, error) {
	mode := archive.ModeSurvey
	if o.Rehearsal {
		mode = archive.ModeRehearsal
	}
	read, err := readSource(ctx, o)
	if err != nil {
		return read.text, err
	}
	// Nothing unsettled reaches the install: the checks run here, on the very
	// bytes the snapshot will carry, and what they cannot settle is asked
	// about now or written down as a default.
	if err := settleRead(ctx, o, &read); err != nil {
		read.text = discoveryReport(read.plan, read.report, "", read.settled, err)
		return read.text, err
	}
	if err := askDefaults(&read, out, o); err != nil {
		read.text = discoveryReport(read.plan, read.report, "", read.settled, err)
		return read.text, err
	}
	// The file is named before it is written, so the report inside it can name
	// it: the operator reads one page that says where the snapshot is.
	path := freeSnapshotPath(o, mode)
	// What the snapshot does not carry is part of that page: Plex's metadata
	// stays on the source, and the operator reads that once, here.
	if mode != archive.ModeSurvey {
		read.report.Notes = append(read.report.Notes, skipLines(read.plan)...)
	}
	if err := encodeRead(&read); err != nil {
		read.text = discoveryReport(read.plan, read.report, "", read.settled, err)
		return read.text, err
	}
	read.text = discoveryReport(read.plan, read.report, path, read.settled, nil)
	wrote, err := writeSnapshot(path, mode, o, read)
	if err != nil {
		return read.text, err
	}
	out.Line(read.text)
	out.Success("wrote " + wrote.Path + " (" + string(mode) + ")")
	// The last screen names every default this run took: an address or a path
	// nobody answered is a decision box made, and the operator leaves the run
	// knowing which ones those were.
	if defaults := read.settled.Defaults(); !defaults.Empty() {
		out.List("defaults taken", defaults.Lines())
	}
	return read.text, nil
}

// readSource reads the source and builds the plan. Nothing is written: a run
// that stops here leaves the source and the host exactly as it was.
func readSource(ctx context.Context, o discoverOptions) (migrationRead, error) {
	var read migrationRead
	root := o.Root
	if root == "" {
		root = "/"
	}
	src, err := discoverSource(o)
	if err != nil {
		return read, err
	}
	read.src = src
	// The blessed variants are the chart's, so the read loads the release it is
	// writing a plan for. A chart that cannot be read stops the read: a plan
	// that names no variant would migrate every app onto the chart's default
	// image without saying so.
	chart, err := o.loadChart(ctx)
	if err != nil {
		read.text = discoveryReport(nil, read.report, "", nil, err)
		return read, err
	}
	if len(chart.Variants) == 0 {
		err := fmt.Errorf("chart %s ships no variants.yaml, so no source image can be mapped onto one of its blessed variants", chart.Ref)
		read.text = discoveryReport(nil, read.report, "", nil, err)
		return read, err
	}
	read.chart = chart
	p, rep, derr := discover.Discover(ctx, src, discover.Options{
		Namespaces: o.Namespaces,
		Variants:   chart.Variants,
		// The host's filesystem, read-only: every open below is a read.
		ConfigFS: os.DirFS(root),
	})
	read.plan, read.report = p, rep
	if derr != nil {
		read.text = discoveryReport(p, rep, "", nil, derr)
		return read, derr
	}
	return read, nil
}

// settleRead runs every rewriter's check over the configs this read saw and
// writes what they could not settle into the plan as defaults with reasons. The
// plan a run writes is Settle's plan: a gap that reached apply would be a
// refusal after the downtime, and this is where it becomes a decision instead.
//
// The checks read the source's own directories, which are the bytes a rehearsal
// or a final snapshot copies: apply then rewrites a copy of the same config, so
// the checks it repeats are the checks this run made.
func settleRead(ctx context.Context, o discoverOptions, read *migrationRead) error {
	in, err := settleInput(read, o.Root)
	if err != nil {
		return err
	}
	settledPlan, called, err := apply.Settle(ctx, read.plan, in)
	if err != nil {
		return err
	}
	read.plan, read.settled = settledPlan, called
	return nil
}

// snapshotPath is where one run writes: --out when the operator named a file,
// otherwise bundle.Dir under the name its mode and the time give it.
func snapshotPath(o discoverOptions, mode archive.Mode) string {
	if strings.TrimSpace(o.Out) != "" {
		return o.Out
	}
	return filepath.Join(bundle.Dir, bundle.FileName(mode, o.clock()))
}

// freeSnapshotPath is snapshotPath with the name made free. A snapshot's name
// carries the second it was taken, and two reads of one source can be taken in one
// second -- a cutover that refuses twice in a script, or an operator who answers a
// question and runs it again. The second one is a second later rather than a
// refusal, because a refusal there would be about the clock and not about the
// source. A file --out named is the operator's own and is never renamed: writing
// over their file stays a refusal.
func freeSnapshotPath(o discoverOptions, mode archive.Mode) string {
	path := snapshotPath(o, mode)
	if strings.TrimSpace(o.Out) != "" {
		return path
	}
	at := o.clock()
	for i := 0; i < 60; i++ {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path
		}
		path = filepath.Join(bundle.Dir, bundle.FileName(mode, at.Add(time.Duration(i+1)*time.Second)))
	}
	return path
}

// writeSnapshot writes one snapshot of a read at path. The configs travel only
// in a rehearsal or a final snapshot: a survey is the read alone, so it can be
// taken on a running source without copying a library's worth of config.
func writeSnapshot(path string, mode archive.Mode, o discoverOptions, read migrationRead) (bundle.Written, error) {
	s := bundle.Snapshot{
		Mode:   mode,
		Plan:   read.raw,
		Report: []byte(read.text),
		Source: read.report.Raw,
		Box:    o.Box,
		Now:    o.Now,
	}
	if mode != archive.ModeSurvey {
		root := o.Root
		if root == "" {
			root = "/"
		}
		names := make([]string, 0, len(read.plan.Apps))
		for name := range read.plan.Apps {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			app := bundle.Config{
				Instance: name,
				Dir:      filepath.Join(root, read.plan.Apps[name].Config),
			}
			// An app whose config is more than its state (Plex: a library of
			// covers beside its databases) travels as the files discovery's
			// table names. Every other app's whole tree goes in.
			if carry, ok := discover.SnapshotCarry(name); ok {
				app.Files = make([]bundle.File, len(carry.Files))
				for i, f := range carry.Files {
					app.Files[i] = bundle.File{Path: f.Path, Required: f.Required}
				}
			}
			s.Configs = append(s.Configs, app)
		}
	}
	return bundle.Write(path, s)
}

// discoverSource picks what discovery reads: a saved directory when the caller
// has one (the source may already be gone), otherwise the live cluster.
func discoverSource(o discoverOptions) (discover.Source, error) {
	if o.FromDir != "" {
		return discover.NewDirectory(os.DirFS(o.FromDir)), nil
	}
	// The cluster decision belongs to `box bootstrap`; a migration of a source
	// cluster reuses it exactly as apply reuses root's, and an explicit path
	// always wins.
	kc, err := release.ResolveKubeconfig(o.Kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		return nil, err
	}
	return discover.NewCluster(kc, o.Namespaces)
}

// loadChart is the release this read writes a plan for: its variants.yaml (the
// blessed image list a source image is mapped onto a variant through), its
// catalog, and the values whose ports and URL bases are where the box serves
// each app. It is parsed by internal/admission and internal/catalog, the one
// parser of each file, so a plan cannot name a variant or an app the release it
// is applied onto does not have.
func (o discoverOptions) loadChart(ctx context.Context) (release.Chart, error) {
	return (&chartRelease{flags: o.chart, source: o.Charts}).load(ctx)
}

// settleInput is what Settle is handed: this chart's apps, the source
// directories this read saw, and where the box serves every planned app.
func settleInput(read *migrationRead, root string) (apply.SettleInput, error) {
	var in apply.SettleInput
	chart := read.chart
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return in, err
	}
	// The chart's own values: the ports and URL bases this release gives every
	// app are what a rewritten address points at, and a read on the source host
	// has no seedbox's files to ask.
	var chartValues map[string]any
	if err := yaml.Unmarshal(chart.Values, &chartValues); err != nil {
		return in, fmt.Errorf("%s: read the chart's values: %w", chart.Ref, err)
	}
	targets, err := apply.Targets(chartValues, cat.Names(), sortedKeys(read.plan.Apps))
	if err != nil {
		return in, err
	}
	if root == "" {
		root = "/"
	}
	return apply.SettleInput{
		Catalog: cat.Names(),
		// The source's own directories: a rehearsal and a final snapshot copy
		// these, so the checks read the bytes apply will rewrite. A survey
		// copies nothing and still checks, which is what makes a read tell the
		// operator what apply would have refused.
		Configs: func(name string) string {
			config := read.plan.Apps[name].Config
			if config == "" {
				return ""
			}
			return filepath.Join(root, config)
		},
		Targets: targets,
	}, nil
}

// askDefaults shows what Settle could not settle and, when there is somebody to
// ask, has the operator answer each address. Unattended, it takes every default
// and says so with the same lines: a default is never silent, whichever run
// takes it.
func askDefaults(read *migrationRead, out say.Reporter, o discoverOptions) error {
	if read.settled.Empty() {
		return nil
	}
	asker := &settleAsker{out: out, prompt: o.Prompt, apps: sortedKeys(read.plan.Apps)}
	if o.Prompt == nil {
		out.List("nothing else to ask: taking every one of these defaults", read.settled.Lines())
		return nil
	}
	out.List("box cannot settle these by itself: these are the defaults it would take", read.settled.Lines())
	decided, err := apply.Decide(read.plan, read.settled, asker.ask)
	if err != nil {
		return err
	}
	read.settled = decided
	return nil
}

// encodeRead writes the plan this run will carry, and refuses one that does not
// read back: apply trusts it with the data root and every instance's config, and
// a field the encoder dropped would be a rule that silently does nothing.
func encodeRead(read *migrationRead) error {
	raw, err := yaml.Marshal(read.plan)
	if err != nil {
		return fmt.Errorf("write the plan: %w", err)
	}
	back, err := plan.Parse(raw)
	if err != nil {
		return fmt.Errorf("the plan does not read back: %w", err)
	}
	if !reflect.DeepEqual(read.plan, back) {
		return errors.New("the plan does not read back as the plan this run built; this is a bug in box")
	}
	read.raw = raw
	return nil
}

// discoveryReport is report.txt: what the plan holds, everything discovery
// would not migrate, and every decision box made that the operator did not:
// what apply will leave as it is, what it points where, and what only apply can
// check. It is printed and kept, so the operator reads one page before apply
// writes one byte.
func discoveryReport(p *plan.Plan, rep discover.Report, out string, settled apply.Settled, failure error) string {
	var b strings.Builder
	switch {
	case failure != nil:
		fmt.Fprintf(&b, "plan: not written: %v\n", failure)
	case out != "":
		fmt.Fprintf(&b, "plan: %s\n", out)
	}
	if p != nil {
		names := make([]string, 0, len(p.Apps))
		for name := range p.Apps {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			app := p.Apps[name]
			fmt.Fprintf(&b, "app: %s (from %s, config %s)\n", name, app.Source, app.Config)
		}
		if p.Data != nil {
			fmt.Fprintf(&b, "data: %s (downloads: %s, media: %s)\n", p.Data.Root, p.Data.Downloads, p.Data.Media)
		}
	}
	sections := []struct {
		title string
		rows  []string
	}{
		{"skipped", skippedRows(rep.Skipped)},
		{"ignored", ignoredRows(rep.Ignored)},
		{"notes", rep.Notes},
	}
	for _, s := range sections {
		if len(s.rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s:\n", s.title)
		for _, row := range s.rows {
			fmt.Fprintf(&b, "  %s\n", row)
		}
	}
	// The called-out list, exactly as Settle wrote it: one header per decision,
	// one line per host or path with its reason, and the places that hold it.
	if !settled.Empty() {
		b.WriteString("called out:\n")
		for _, line := range settled.Lines() {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// skipLines is what a read says about the parts of a config its snapshot does
// not carry: one line per planned app whose config is more than its state, said
// once, in the report the snapshot holds and on the screen the operator reads.
// Nothing is said on a survey, which carries no configs at all.
func skipLines(p *plan.Plan) []string {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		if carry, ok := discover.SnapshotCarry(name); ok && carry.Note != "" {
			out = append(out, carry.Note)
		}
	}
	return out
}

// skippedRows is what discovery did not migrate, with its reason.
func skippedRows(skipped []discover.Skipped) []string {
	out := make([]string, 0, len(skipped))
	for _, s := range skipped {
		out = append(out, s.Workload+": "+s.Why)
	}
	sort.Strings(out)
	return out
}

// ignoredRows is every mount that did not become a path rule.
func ignoredRows(ignored []discover.Ignored) []string {
	out := make([]string, 0, len(ignored))
	for _, i := range ignored {
		where := fmt.Sprintf("%s %s", i.App, i.Mount)
		if i.Host != "" {
			where += " (" + i.Host + ")"
		}
		where += " on " + i.Volume
		out = append(out, where+": "+i.Why)
	}
	sort.Strings(out)
	return out
}
