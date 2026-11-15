package main

// migrate.go is `box migrate`: the read half, and the dispatch.
//
// `discover` reads an install box did not make and writes the plan apply runs.
// It changes nothing: the source cluster is listed, the source's host files are
// read through a filesystem rooted where box thinks the host's / is, and every
// manifest read is saved beside the plan so the plan can be rebuilt after the
// source is gone (docs/box.md, "Discovery comes first and writes a plan").
//
// The plan directory is a document a person reads and edits: plan.yaml in the
// same shape `internal/migrate/plan` loads, source/ with every manifest the
// plan was built from, and report.txt with what discovery would not migrate.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3" // only cmd/box may import it (AGENTS.md): the plan is a YAML file and cmd/s5n stays stdlib-only

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/release"
)

// migrateUsage is what a bad invocation and `box migrate` print. It is the
// command surface: two halves of one migration, each of which says so in the
// files it writes or refuses to.
const migrateUsage = `usage: box migrate <command> [flags]

commands:
  discover    read a source install and write the plan migrate applies; changes nothing
  apply       adopt a source install into this seedbox from a plan

box migrate discover --out <dir> [--from-dir <manifests>] [--kubeconfig <file>] [--namespace <ns> ...] [--root <dir>]
box migrate apply <plan dir> [--source-root <dir>] [--grant <file>] [--values <file>] [--kubeconfig <file>] [--yes] [--plain] [--chart-repo <ref>] [--chart-version <tag>]
`

// The two halves, as names: the dispatch, the usage line and the messages they
// print all spell them once.
const (
	migrateDiscoverCmd = "discover"
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

// discoverOptions is `box migrate discover`, as flags.
type discoverOptions struct {
	Out        string
	FromDir    string
	Kubeconfig string
	Namespaces []string
	Root       string
}

func runMigrateDiscover(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("migrate discover", flag.ExitOnError)
	out := fs.String("out", "", "the plan directory to write (created; it has to be empty or absent)")
	fromDir := fs.String("from-dir", "", "read the source's manifests from this directory instead of a cluster")
	kubeconfig := fs.String("kubeconfig", "", "explicit kubeconfig for the source cluster, or fail with no fallback")
	root := fs.String("root", "/", "the host root the apps' config directories are read under")
	var ns namespaces
	fs.Var(&ns, "namespace", "limit discovery to this source namespace (repeatable; default every namespace)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "box: migrate discover takes no arguments: %s\n", strings.Join(fs.Args(), " "))
		os.Exit(2)
	}
	opts := discoverOptions{Out: *out, FromDir: *fromDir, Kubeconfig: *kubeconfig, Namespaces: ns, Root: *root}
	report, err := migrateDiscover(ctx, opts)
	if report != "" {
		fmt.Print(report)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(1)
	}
}

// migrateDiscover is the command without the exit, so a test drives it. The
// returned report is the text it printed (and wrote to report.txt), which an
// error also carries: a run that stopped halfway still has something to say
// about what it read.
func migrateDiscover(ctx context.Context, o discoverOptions) (string, error) {
	if strings.TrimSpace(o.Out) == "" {
		return "", errors.New("no plan directory: pass --out <dir>")
	}
	if err := planDirReady(o.Out); err != nil {
		return "", err
	}
	root := o.Root
	if root == "" {
		root = "/"
	}
	src, err := discoverSource(o)
	if err != nil {
		return "", err
	}
	p, rep, derr := discover.Discover(ctx, src, discover.Options{
		Namespaces: o.Namespaces,
		// The host's filesystem, read-only: every open below is a read.
		ConfigFS: os.DirFS(root),
	})
	text := discoveryReport(p, rep, o.Out, derr)
	if werr := writePlanDir(o.Out, p, rep, text); werr != nil {
		return text, errors.Join(derr, werr)
	}
	if derr != nil {
		return text, derr
	}
	return text, nil
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

// planDirReady creates the plan directory, or refuses one that holds something:
// the directory is the record of one plan, and writing a second into it would
// leave two plan.yaml files' worth of source manifests with one plan.
func planDirReady(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", dir, err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory: pass --out with the directory the plan should live in", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty: a plan directory holds one plan, its source manifests and its report, so write this one somewhere new", dir)
	}
	return nil
}

// writePlanDir writes the plan, the manifests it was built from and the report.
// Nothing is written when discovery found no plan: a failed run leaves the
// report alone, so the operator can read what it did find.
func writePlanDir(dir string, p *plan.Plan, rep discover.Report, text string) error {
	if err := writeFileMode(filepath.Join(dir, "report.txt"), []byte(text), 0o600); err != nil {
		return err
	}
	if p == nil {
		return nil
	}
	keys := make([]string, 0, len(rep.Raw))
	for key := range rep.Raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := filepath.Join(dir, "source", key+".json")
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(name), err)
		}
		if err := writeFileMode(name, rep.Raw[key], 0o600); err != nil {
			return err
		}
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("write the plan: %w", err)
	}
	name := filepath.Join(dir, "plan.yaml")
	if err := writeFileMode(name, raw, 0o600); err != nil {
		return err
	}
	// A plan box cannot read back is worse than no plan: apply trusts it with
	// the data root and every instance's config, and a field the encoder
	// dropped would be a rule that silently does nothing.
	back, err := plan.Load(name)
	if err != nil {
		return fmt.Errorf("%s does not read back: %w", name, err)
	}
	if !reflect.DeepEqual(p, back) {
		return fmt.Errorf("%s does not read back as the plan discovery built; this is a bug in box", name)
	}
	return nil
}

// writeFileMode writes one file with the mode the plan directory promises: a
// plan can name a host path, so it is 0600 like the seedbox's own files.
func writeFileMode(name string, data []byte, mode fs.FileMode) error {
	if err := os.WriteFile(name, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// discoveryReport is report.txt: what the plan holds, and everything discovery
// would not migrate. It is printed and kept, so the operator reads one page
// before apply writes one byte.
func discoveryReport(p *plan.Plan, rep discover.Report, out string, failure error) string {
	var b strings.Builder
	if failure != nil {
		fmt.Fprintf(&b, "plan: not written: %v\n", failure)
	} else {
		fmt.Fprintf(&b, "plan: %s\n", filepath.Join(out, "plan.yaml"))
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
