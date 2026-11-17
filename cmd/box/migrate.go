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
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/release"
)

// migrateUsage is what a bad invocation and `box migrate` print. It is the
// command surface: three halves of one migration, each of which says so in the
// files it writes or refuses to.
const migrateUsage = `usage: box migrate <command> [flags]

commands:
  discover    read a source install and write a survey of it; changes nothing
  cutover     take the final snapshot on the source host, then install or adopt
  apply       install or adopt from a snapshot

box migrate discover [--rehearsal] [--out <file>] [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>] [--chart-repo <ref>] [--chart-version <tag>]
box migrate cutover [--replace-cluster] [--out <file>] [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>] [--chart-repo <ref>] [--chart-version <tag>] [--values <file>] [--yes]
box migrate apply [<snapshot>] [--values <file>] [--grant <file>] [--kubeconfig <file>] [--yes] [--plain] [--chart-repo <ref>] [--chart-version <tag>]

A snapshot is one file in ` + bundle.Dir + `/ (or where --out names it). discover writes a
survey -- the plan, the report and the source's manifests, no configs -- and --rehearsal
adds the configs a locked rehearsal applies. cutover writes the final snapshot apply runs.
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
	out := newOutput(terminalSet(false), true, "")
	defer out.Close()
	text, err := migrateDiscover(ctx, o)
	if text != "" {
		out.Line(text)
	}
	if err != nil {
		out.Error(err)
		os.Exit(1)
	}
}

// migrationRead is one read of a source: the plan, the bytes it will be written
// as, what discovery could not migrate, and the text the operator sees.
type migrationRead struct {
	plan *plan.Plan
	// src is what the plan was read from. The stop proof and the reset recipe
	// ask it the two things a plan cannot hold -- the pods that are left and
	// the distribution that installed the cluster -- over the same read.
	src discover.Source
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
func migrateDiscover(ctx context.Context, o discoverOptions) (string, error) {
	mode := archive.ModeSurvey
	if o.Rehearsal {
		mode = archive.ModeRehearsal
	}
	read, err := readSource(ctx, o)
	if err != nil {
		return read.text, err
	}
	// The file is named before it is written, so the report inside it can name
	// it: the operator reads one page that says where the snapshot is.
	path := snapshotPath(o, mode)
	read.text = discoveryReport(read.plan, read.report, path, nil)
	if _, err := writeSnapshot(path, mode, o, read); err != nil {
		return read.text, err
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
	variants, err := o.variants(ctx)
	if err != nil {
		read.text = discoveryReport(nil, read.report, "", err)
		return read, err
	}
	p, rep, derr := discover.Discover(ctx, src, discover.Options{
		Namespaces: o.Namespaces,
		Variants:   variants,
		// The host's filesystem, read-only: every open below is a read.
		ConfigFS: os.DirFS(root),
	})
	read.plan, read.report = p, rep
	if derr != nil {
		read.text = discoveryReport(p, rep, "", derr)
		return read, derr
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		read.text = discoveryReport(p, rep, "", fmt.Errorf("write the plan: %w", err))
		return read, err
	}
	// A plan box cannot read back is worse than no plan: apply trusts it with
	// the data root and every instance's config, and a field the encoder
	// dropped would be a rule that silently does nothing.
	back, err := plan.Parse(raw)
	if err != nil {
		read.text = discoveryReport(p, rep, "", fmt.Errorf("the plan does not read back: %w", err))
		return read, err
	}
	if !reflect.DeepEqual(p, back) {
		err := errors.New("the plan does not read back as the plan discovery built; this is a bug in box")
		read.text = discoveryReport(p, rep, "", err)
		return read, err
	}
	read.raw = raw
	read.text = discoveryReport(p, rep, "", nil)
	return read, nil
}

// snapshotPath is where one run writes: --out when the operator named a file,
// otherwise bundle.Dir under the name its mode and the time give it.
func snapshotPath(o discoverOptions, mode archive.Mode) string {
	if strings.TrimSpace(o.Out) != "" {
		return o.Out
	}
	return filepath.Join(bundle.Dir, bundle.FileName(mode, o.clock()))
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
			s.Configs = append(s.Configs, bundle.Config{
				Instance: name,
				Dir:      filepath.Join(root, read.plan.Apps[name].Config),
			})
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

// variants is the chart's variants.yaml: the blessed image list a source image
// is mapped onto a variant through. It is read from the release this run
// installs -- parsed by internal/admission, the one parser of that file -- so a
// plan cannot name a variant the release it is applied onto does not have.
//
// A chart without the file is refused rather than read as "no variants": the
// chart owns the file, and a plan that named no variant would quietly put every
// app on the chart's default image, which is the silent default migrate exists
// to avoid.
func (o discoverOptions) variants(ctx context.Context) ([]byte, error) {
	charts := &chartRelease{flags: o.chart, source: o.Charts}
	chart, err := charts.load(ctx)
	if err != nil {
		return nil, err
	}
	if len(chart.Variants) == 0 {
		return nil, fmt.Errorf("chart %s ships no variants.yaml, so no source image can be mapped onto one of its blessed variants", chart.Ref)
	}
	return chart.Variants, nil
}

// discoveryReport is report.txt: what the plan holds, and everything discovery
// would not migrate. It is printed and kept, so the operator reads one page
// before apply writes one byte.
func discoveryReport(p *plan.Plan, rep discover.Report, out string, failure error) string {
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
	return b.String()
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
