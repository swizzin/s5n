package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
)

// TestRollbackMapsItsFlagsToTheOptions: the app is what the command line names,
// and the two switches of its own sit over apply's flags -- a rollback ends in
// an apply, so it must not grow a second set of file paths to keep in step.
func TestRollbackMapsItsFlagsToTheOptions(t *testing.T) {
	var flags *rollbackFlags
	parse(t, "rollback", func(fs *flag.FlagSet) { flags = addRollbackFlags(fs) },
		"--pin", "--yes", "--plain",
		"--grant", "/etc/example/seedboxes/alice.yaml",
		"--values", "/home/alice/.config/box/values.yaml")

	opts := flags.options("sonarr")
	if opts.App != "sonarr" {
		t.Fatalf("app = %q, want the one the operator named", opts.App)
	}
	if !opts.Pin {
		t.Fatal("--pin did not reach the options: the image would be held, not pinned")
	}
	if !*flags.yes || !*flags.plain {
		t.Fatalf("yes = %v, plain = %v, want both", *flags.yes, *flags.plain)
	}
	if opts.Apply.GrantPath != "/etc/example/seedboxes/alice.yaml" || opts.Apply.ValuesPath != "/home/alice/.config/box/values.yaml" {
		t.Fatalf("files = %q, %q; a rollback reads apply's two files", opts.Apply.GrantPath, opts.Apply.ValuesPath)
	}
	if got, want := opts.Apply.ChartRef(), (release.ChartRef{Repo: release.DefaultChartRepo}); got != want {
		t.Fatalf("chart = %+v, want the default repository: a rollback installs the release that is there", got)
	}
	// --chart-version is inherited from apply and refused by the command, so
	// --help has to say what it does rather than promise an install.
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	addRollbackFlags(fs)
	f := fs.Lookup("chart-version")
	if f == nil || !strings.Contains(f.Usage, "not used") {
		t.Fatalf("--chart-version usage = %v, want it to say the flag is not used", f)
	}
}
