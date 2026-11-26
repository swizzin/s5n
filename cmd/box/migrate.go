package main

// migrate.go is `box migrate`: the read half and the dispatch. `discover` reads
// an install box did not make and writes a snapshot -- plan, manifests, report,
// and a rehearsal's config trees -- changing nothing. A snapshot is one archive
// (internal/archive, internal/migrate/bundle) in /var/lib/s5n/migrate/, named
// for its time and mode; a survey holds no configs, so a rehearsal or final
// snapshot is the one that carries them (docs/box.md, "migrate").

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
	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/migrate/report"
	"github.com/swizzin/s5n/internal/migrate/space"
	"github.com/swizzin/s5n/internal/migrate/work"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
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

box migrate discover [--rehearsal] [--plex-minimal] [--out <file>] [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>] [--plain] [--chart-repo <ref>] [--chart-version <tag>]
box migrate cutover [--replace-cluster] [--no-prior-discover] [--plex-minimal] [--out <file>] [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>] [--chart-repo <ref>] [--chart-version <tag>] [--values <file>] [--yes] [--plain]
box migrate apply [<snapshot>] [--values <file>] [--grant <file>] [--kubeconfig <file>] [--yes] [--plain] [--chart-repo <ref>] [--chart-version <tag>]

A snapshot is one file in ` + bundle.Dir + `/ (or where --out names it). discover writes a
survey -- the plan, the report and the source's manifests, no configs -- and --rehearsal
adds the configs a locked rehearsal applies. cutover writes the final snapshot apply runs.

A snapshot carries Plex's whole data -- its metadata, artwork and thumbnails, which is
hours of the machine's time it does not have to build again -- when there is room for it
wherever it is written: in the snapshot's directory, and on a host that is both ends of
the migration, beside the seedbox's config too. Where there is not, it carries Plex's
preferences and databases, says so with the numbers, and Plex builds the rest again.
--plex-minimal asks for that, whatever the room.

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

// The command names, spelled once.
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

// discoverOptions is what discover and cutover share to read one source.
type discoverOptions struct {
	// Out is the snapshot file; empty means bundle.Dir with a name from mode
	// and time.
	Out        string
	FromDir    string
	Kubeconfig string
	Namespaces []string
	Root       string
	// chart is the release this run installs: its blessed variants map source
	// images. cutover points it at its apply's flags, so both name one release.
	chart *chartFlags
	// Charts is where that release comes from; nil is the published repository.
	Charts release.ChartSource
	// Rehearsal copies configs in and may only be applied locked.
	Rehearsal bool
	// Box is the writing version, as the manifest records it.
	Box string
	// Now is the clock behind the name and manifest; nil is time.Now.
	Now func() time.Time
	// ValuesPath is the values this read is for: its categories are the ones the
	// release renders. Empty is the chart's own values.
	ValuesPath string
	// Prompt asks the defaults Settle took; nil takes them all and says so.
	Prompt setup.Prompter
	// Plain is --plain: line prompts even on a terminal.
	Plain bool
	// PlexMinimal carries Plex's preferences and databases only; otherwise its
	// whole data goes when there is room.
	PlexMinimal bool
	// TargetConfig is this host's seedbox config path: a same-host cutover
	// writes two copies on one filesystem. Empty when there is no box to ask.
	TargetConfig string
	// Disk and CarrySize answer the room questions; nil uses the real ones.
	Disk      space.Disk
	CarrySize func(instance, dir string) (int64, error)
	// Work is the private directory for database copies, beside the snapshot
	// (internal/migrate/work): a source's config can be gigabytes.
	Work string
}

func (o discoverOptions) clock() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// addDiscoverFlags registers the shared read flags and returns the namespaces.
func addDiscoverFlags(fs *flag.FlagSet, o *discoverOptions, outUsage string) *namespaces {
	fs.StringVar(&o.Out, "out", "", outUsage)
	fs.StringVar(&o.FromDir, "from-dir", "", "read the source's manifests from this directory instead of a cluster")
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "explicit kubeconfig for the source cluster, or fail with no fallback")
	fs.StringVar(&o.Root, "root", "/", "the host root the apps' config directories are read under")
	fs.BoolVar(&o.PlexMinimal, "plex-minimal", false, "carry Plex's preferences and databases only, and let it rebuild its metadata and artwork")
	var ns namespaces
	fs.Var(&ns, "namespace", "limit discovery to this source namespace (repeatable; default every namespace)")
	return &ns
}

func runMigrateDiscover(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("migrate discover", flag.ExitOnError)
	o := discoverOptions{Box: version}
	ns := addDiscoverFlags(fs, &o, "the snapshot file to write (default: "+bundle.Dir+"/migrate-<time>-survey.tar.gz)")
	// The read names its release the same way the install it leads to does.
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
		// With a terminal, ask about the defaults; without one, take them and
		// say so.
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

// migrationRead is one read: plan, bytes, gaps, unsettled defaults and text.
type migrationRead struct {
	plan *plan.Plan
	// src holds the pods left and the installing distribution, for the stop
	// proof and reset recipe.
	src discover.Source
	// settled is what the checks could not settle, for the report and questions.
	settled apply.Settled
	// chart is the release this plan is for: variants, catalog and endpoints.
	chart release.Chart
	// raw is the plan's YAML, carried in the snapshot.
	raw    []byte
	report discover.Report
	// doc is the report the snapshot carries as report.txt and writes beside it.
	doc report.Doc
	// text is doc as text, which is what a snapshot's report.txt entry holds.
	text string
}

// setReport builds the report; screen and archive share one document.
func (r *migrationRead) setReport(mode archive.Mode, snapshot string, o discoverOptions, failure error) {
	r.doc = readReport(*r, mode, snapshot, o.clock(), o.Box, failure)
	r.text = r.doc.Text()
}

// migrateDiscover is the command without the exit; the returned text is the
// report, carried even on an error.
func migrateDiscover(ctx context.Context, o discoverOptions, out say.Reporter) (string, error) {
	mode := archive.ModeSurvey
	if o.Rehearsal {
		mode = archive.ModeRehearsal
	}
	// Working directory beside the snapshot, never in $TMPDIR.
	dir, done, err := work.New(filepath.Dir(snapshotPath(o, mode)))
	if err != nil {
		return "", err
	}
	defer done()
	o.Work = dir
	read, err := readSource(ctx, o, mode)
	if err != nil {
		return read.text, err
	}
	// Check the very bytes the snapshot will carry; anything unsettled is asked
	// about or written as a default.
	if err := settleRead(ctx, o, &read); err != nil {
		read.setReport(mode, "", o, err)
		return read.text, err
	}
	if err := askDefaults(&read, out, o); err != nil {
		read.setReport(mode, "", o, err)
		return read.text, err
	}
	// Name the file before writing, so the report inside can name it.
	path := freeSnapshotPath(o, mode)
	// Settle what the snapshot carries before writing; the decision goes into
	// the plan apply reads.
	short, err := decideSnapshotCarry(&read, o, path, mode)
	if err != nil {
		read.setReport(mode, "", o, err)
		return read.text, err
	}
	// What the snapshot leaves out is part of the page the operator reads.
	if mode != archive.ModeSurvey {
		read.report.Notes = append(read.report.Notes, carryLines(read.plan)...)
		read.report.Notes = append(read.report.Notes, short...)
	}
	if err := encodeRead(&read); err != nil {
		read.setReport(mode, "", o, err)
		return read.text, err
	}
	read.setReport(mode, path, o, nil)
	wrote, err := writeSnapshot(path, mode, o, read)
	if err != nil {
		return read.text, err
	}
	read.digest("box migrate discover", out)
	out.Detail("wrote " + wrote.Path + " (" + string(mode) + ")")
	// End with the count of decisions box took; the report names each reason.
	read.defaultsRow(out)
	return read.text, nil
}

// readSource reads the source and builds the plan; nothing is written.
func readSource(ctx context.Context, o discoverOptions, mode archive.Mode) (migrationRead, error) {
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
	// The blessed variants are the chart's; no variants means no plan, rather
	// than every app silently on the default image.
	chart, err := o.loadChart(ctx)
	if err != nil {
		read.setReport(mode, "", o, err)
		return read, err
	}
	if len(chart.Variants) == 0 {
		err := fmt.Errorf("chart %s ships no variants.yaml, so no source image can be mapped onto one of its blessed variants", chart.Ref)
		read.setReport(mode, "", o, err)
		return read, err
	}
	read.chart = chart
	// Ask the release for its categories: the library renames onto them.
	categories, err := readCategories(o, chart)
	if err != nil {
		read.setReport(mode, "", o, err)
		return read, err
	}
	p, rep, derr := discover.Discover(ctx, src, discover.Options{
		Namespaces: o.Namespaces,
		Variants:   chart.Variants,
		Categories: categories,
		// The host's filesystem, read-only.
		ConfigFS: os.DirFS(root),
		// A database is copied before opening, into the run's working directory.
		TempDir: o.Work,
	})
	read.plan, read.report = p, rep
	if derr != nil {
		read.setReport(mode, "", o, derr)
		return read, derr
	}
	return read, nil
}

// readCategories is each instance's library directory from this read's values.
// A read that cannot work them out refuses: every directory would keep the
// source's name and the Arrs would get a second root folder.
func readCategories(o discoverOptions, chart release.Chart) (map[string]string, error) {
	values, err := seedbox.Pending(seedbox.Chart{Values: chart.Values, Schema: chart.Schema}, o.ValuesPath)
	if err != nil {
		return nil, fmt.Errorf("the values this read is for name no categories: %w", err)
	}
	return apply.Categories(values), nil
}

// settleRead runs every rewriter's check over the configs and writes what they
// could not settle as defaults with reasons: a gap reaching apply would be a
// refusal after the downtime. The checks read the bytes the snapshot copies.
func settleRead(ctx context.Context, o discoverOptions, read *migrationRead) error {
	in, err := settleInput(read, o.Root, o.Work)
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

// snapshotPath is --out, or bundle.Dir with mode and time.
func snapshotPath(o discoverOptions, mode archive.Mode) string {
	if strings.TrimSpace(o.Out) != "" {
		return o.Out
	}
	return filepath.Join(bundle.Dir, bundle.FileName(mode, o.clock()))
}

// freeSnapshotPath moves a name carried to the next second rather than refusing
// over the clock; --out is the operator's file and is never renamed.
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

// writeSnapshot writes one read; only a rehearsal or final carries configs, so
// a survey can be taken on a running source.
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
			// An app carrying more than state travels as its plan says; every
			// other app's whole tree goes in.
			if carry, ok := discover.SnapshotCarry(name, read.plan.Apps[name].Carry == plan.CarryFull); ok {
				app.Sub = carry.Sub
				if home, err := plex.Home(app.Dir); err == nil {
					app.Sub = home
				}
				if carry.Full {
					app.Skip = carry.Skip
				} else {
					app.Files = make([]bundle.File, len(carry.Files))
					for i, f := range carry.Files {
						app.Files[i] = bundle.File{Path: f.Path, Required: f.Required}
					}
				}
			}
			s.Configs = append(s.Configs, app)
		}
	}
	wrote, err := bundle.Write(path, s)
	if err != nil {
		return wrote, err
	}
	// The same report beside the file, to read where it was taken.
	if err := report.Write(report.Name(path), read.doc); err != nil {
		return wrote, err
	}
	return wrote, nil
}

// decideSnapshotCarry writes into the plan whether each oversized app travels
// whole or as state, and returns the line for an app that did not fit, with
// numbers. A survey settles the answer without measuring.
func decideSnapshotCarry(read *migrationRead, o discoverOptions, path string, mode archive.Mode) ([]string, error) {
	if read.plan == nil {
		return nil, nil
	}
	root := o.Root
	if root == "" {
		root = "/"
	}
	var places []space.Place
	var short []string
	for _, name := range sortedKeys(read.plan.Apps) {
		// Only an app carrying more than state has a scope choice.
		if _, ok := discover.SnapshotCarry(name, true); !ok {
			continue
		}
		app := read.plan.Apps[name]
		full := !o.PlexMinimal
		var why string
		if full && mode != archive.ModeSurvey {
			dir := filepath.Join(root, app.Config)
			size, err := o.carrySize(name, dir)
			if err != nil {
				return nil, fmt.Errorf("%s: its whole data would be carried, and box cannot size %s: %w", name, dir, err)
			}
			if places == nil {
				if places, err = o.writePlaces(path); err != nil {
					return nil, err
				}
			}
			if v := space.Choose(size, places); !v.Fits() {
				full = false
				why = fmt.Sprintf("%s: its whole data needs %s and %s is free at %s; carried its state instead, so Plex builds its artwork, thumbnails and detection again",
					name, space.Human(v.Need), space.Human(v.Short.Free), v.Short.Path)
			}
		}
		if full {
			app.Carry = plan.CarryFull
		} else {
			app.Carry = plan.CarryMinimal
		}
		read.plan.Apps[name] = app
		if why != "" {
			short = append(short, why)
		}
	}
	return short, nil
}

// writePlaces is the filesystems a full carry needs; a same-host cutover counts
// its two copies on one filesystem twice.
func (o discoverOptions) writePlaces(snapshot string) ([]space.Place, error) {
	dir := filepath.Dir(snapshot)
	free, err := o.freeAt(dir)
	if err != nil {
		return nil, fmt.Errorf("the room a snapshot needs at %s: %w", dir, err)
	}
	places := []space.Place{{Path: dir, Free: free, Copies: 1}}
	if strings.TrimSpace(o.TargetConfig) == "" {
		return places, nil
	}
	same, err := o.disk().Same(dir, o.TargetConfig)
	if err != nil {
		return nil, fmt.Errorf("the room a snapshot needs beside %s: %w", o.TargetConfig, err)
	}
	if same {
		places[0].Copies = 2
		return places, nil
	}
	free, err = o.freeAt(o.TargetConfig)
	if err != nil {
		return nil, fmt.Errorf("the room a snapshot needs at %s: %w", o.TargetConfig, err)
	}
	return append(places, space.Place{Path: o.TargetConfig, Free: free}), nil
}

// freeAt is a filesystem's free space through the caller's answer.
func (o discoverOptions) freeAt(path string) (int64, error) { return o.disk().Free(path) }

// disk is the filesystems this read asks about, or the host's.
func (o discoverOptions) disk() space.Disk {
	if o.Disk != nil {
		return o.Disk
	}
	return space.Real{}
}

// carrySize is a full carry's bytes through the caller's answer.
func (o discoverOptions) carrySize(instance, dir string) (int64, error) {
	if o.CarrySize != nil {
		return o.CarrySize(instance, dir)
	}
	size, _, err := discover.SnapshotSize(instance, dir)
	return size, err
}

// discoverSource reads a saved directory if given, else the live cluster.
func discoverSource(o discoverOptions) (discover.Source, error) {
	if o.FromDir != "" {
		return discover.NewDirectory(os.DirFS(o.FromDir)), nil
	}
	// Reuse bootstrap's cluster, as apply does; an explicit path wins.
	kc, err := release.ResolveKubeconfig(o.Kubeconfig, os.Getenv("KUBECONFIG"), bootstrap.FilesystemRecords{Path: bootstrap.RecordPath})
	if err != nil {
		return nil, err
	}
	return discover.NewCluster(kc, o.Namespaces)
}

// loadChart is the release this plan is for. internal/admission and
// internal/catalog parse it, so a plan cannot name a variant or app the release
// lacks.
func (o discoverOptions) loadChart(ctx context.Context) (release.Chart, error) {
	return (&chartRelease{flags: o.chart, source: o.Charts}).load(ctx)
}

// settleInput is Settle's apps, source directories, targets and work directory.
func settleInput(read *migrationRead, root, workDir string) (apply.SettleInput, error) {
	var in apply.SettleInput
	chart := read.chart
	cat, err := catalog.Parse(chart.Ref+"/"+catalog.FileName, chart.Catalog, chart.Values, chart.Schema)
	if err != nil {
		return in, err
	}
	// The chart's values are what a rewritten address points at; the source host
	// has no seedbox files to ask.
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
		Work:    workDir,
		// The source's own directories, the bytes apply will rewrite; a survey
		// still checks, so it reports what apply would have refused.
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

// askDefaults asks each unsettled address, or takes every default and says so
// with the count; the report and run log keep each reason (rules 4 and 7).
func askDefaults(read *migrationRead, out say.Reporter, o discoverOptions) error {
	if read.settled.Empty() {
		return nil
	}
	if o.Prompt == nil {
		for _, line := range read.settled.Defaults().Lines() {
			out.Detail(line)
		}
		return nil
	}
	asker := &settleAsker{out: out, prompt: o.Prompt, apps: sortedKeys(read.plan.Apps)}
	decided, err := apply.Decide(read.plan, read.settled, asker.ask)
	if err != nil {
		return err
	}
	read.settled = decided
	return nil
}

// encodeRead refuses a plan that does not read back: a dropped field would be a
// rule that silently does nothing.
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

// carryLines is one line per app carrying more than state, in report and screen;
// a survey says nothing.
func carryLines(p *plan.Plan) []string {
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
		carry, ok := discover.SnapshotCarry(name, p.Apps[name].Carry == plan.CarryFull)
		if ok && carry.Note != "" {
			out = append(out, carry.Note)
		}
	}
	return out
}
