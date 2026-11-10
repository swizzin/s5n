package main

import (
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
)

const (
	testChartRepo    = "oci://127.0.0.1:5000/charts/s5n"
	testChartVersion = "1.2.3"
)

// recordingCharts stands in for the registry: it records the reference each
// command asks for, which is the thing that must not lose a flag.
type recordingCharts struct {
	refs []release.ChartRef
}

func (r *recordingCharts) Load(_ context.Context, ref release.ChartRef) (release.Chart, error) {
	r.refs = append(r.refs, ref)
	return release.Chart{Name: "s5n", Version: testChartVersion}, nil
}

func parse(t *testing.T, name string, add func(*flag.FlagSet), args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	add(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return fs
}

// TestBootstrapAndInitPassTheirChartFlagsToTheSource: bootstrap hands its flags
// to init in the same process, so both commands must ask for the release the
// operator named. A field dropped here is invisible until an e2e fails.
func TestBootstrapAndInitPassTheirChartFlagsToTheSource(t *testing.T) {
	for _, name := range []string{"bootstrap", "init"} {
		t.Run(name, func(t *testing.T) {
			var flags *initFlags
			parse(t, name, func(fs *flag.FlagSet) { flags = addInitFlags(fs) },
				"--chart-repo", testChartRepo, "--chart-version", testChartVersion)

			src := &recordingCharts{}
			if _, err := flags.chart.load(context.Background(), src); err != nil {
				t.Fatal(err)
			}
			want := release.ChartRef{Repo: testChartRepo, Version: testChartVersion}
			if len(src.refs) != 1 || src.refs[0] != want {
				t.Fatalf("the chart source was asked for %+v, want %+v", src.refs, want)
			}
		})
	}
}

// TestApplyPassesItsChartFlagsToTheOptions: apply loads its chart inside the
// Applier, and release.Options.ChartRef is the one mapping that reaches the
// source (pinned from the other side in the release package).
func TestApplyPassesItsChartFlagsToTheOptions(t *testing.T) {
	var flags *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { flags = addApplyFlags(fs) },
		"--chart-repo", testChartRepo, "--chart-version", testChartVersion)

	want := release.ChartRef{Repo: testChartRepo, Version: testChartVersion}
	if got := flags.options().ChartRef(); got != want {
		t.Fatalf("apply asked for %+v, want %+v", got, want)
	}
}

// TestChartFlagsDefaultToTheNewestRelease: with neither flag, the reference is
// the default repository and no version, which is what makes box pick the
// newest release it can install. The e2e's bootstrap runs this path.
func TestChartFlagsDefaultToTheNewestRelease(t *testing.T) {
	var initF *initFlags
	parse(t, "init", func(fs *flag.FlagSet) { initF = addInitFlags(fs) })
	if got, want := initF.chart.ref(), (release.ChartRef{Repo: release.DefaultChartRepo}); got != want {
		t.Fatalf("init default = %+v, want %+v", got, want)
	}

	var applyF *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { applyF = addApplyFlags(fs) })
	if got, want := applyF.options().ChartRef(), (release.ChartRef{Repo: release.DefaultChartRepo}); got != want {
		t.Fatalf("apply default = %+v, want %+v", got, want)
	}
}

// TestTakeDevFlag: a development switch is taken out of the arguments before
// the FlagSet sees them, in both forms, and never reaches --help. A missing
// value is a refusal rather than an empty override.
func TestTakeDevFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
		rest []string
	}{
		{"absent", []string{"--yes", "--domain", "box.example.test"}, "", []string{"--yes", "--domain", "box.example.test"}},
		{"equals", []string{"--yes", "--acme-server=https://pebble.example/dir"}, "https://pebble.example/dir", []string{"--yes"}},
		{"space", []string{"--acme-server", "https://pebble.example/dir", "--yes"}, "https://pebble.example/dir", []string{"--yes"}},
		{"single dash", []string{"-acme-server=https://pebble.example/dir"}, "https://pebble.example/dir", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := takeDevFlag(tc.args, "acme-server")
			if err != nil {
				t.Fatalf("takeDevFlag: %v", err)
			}
			if got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
			if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
				t.Fatalf("rest = %v, want %v", rest, tc.rest)
			}
		})
	}
	if _, _, err := takeDevFlag([]string{"--acme-server"}, "acme-server"); err == nil {
		t.Fatal("a missing value was accepted")
	}
}

// TestAcmeServerIsNotInBootstrapHelp: the dev flag is not registered, so the
// usage a person reads cannot mention it.
func TestAcmeServerIsNotInBootstrapHelp(t *testing.T) {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.String("yes", "", "confirm without asking")
	var b strings.Builder
	fs.SetOutput(&b)
	fs.PrintDefaults()
	if strings.Contains(b.String(), "acme-server") {
		t.Fatalf("bootstrap help lists the dev flag:\n%s", b.String())
	}
}
