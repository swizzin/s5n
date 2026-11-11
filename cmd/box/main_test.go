package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
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

// withStdin points the process's stdin at a file for one test, which is the
// only way to prove what `box apply < /dev/null` does.
func withStdin(t *testing.T, f *os.File) {
	t.Helper()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
}

// `box apply < /dev/null` runs with a stdin that is a character device but not a
// terminal. The old check treated the device as enough, so it printed the
// prompt and failed with "read the answer: EOF" instead of the documented
// refusal. The confirmer reads the terminal, so a redirection cannot answer a
// question nobody was asked.
func TestConfirmerRefusesAStdinThatIsNotATerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { devNull.Close() })
	if info, err := devNull.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		// The point of this test is that the older check would have accepted
		// this file; if it stops being a character device, say so rather than
		// passing for the wrong reason.
		t.Fatalf("%s is no longer a character device, so this test no longer proves the old check was wrong", os.DevNull)
	}
	withStdin(t, devNull)

	var out strings.Builder
	confirm := confirmer(&say.Plain{W: &out}, false)
	ok, err := confirm.Ask("apply s5n in the seedbox namespace?")
	if err == nil {
		t.Fatal("a question was answered by nobody")
	}
	if ok {
		t.Fatal("a question nobody could answer came back yes")
	}
	if !strings.Contains(err.Error(), "not a terminal: pass --yes to apply without a prompt") {
		t.Fatalf("err = %q, want the documented not-a-terminal refusal", err)
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("a prompt was printed where nobody can answer it:\n%s", out.String())
	}
}

// The platform layer's unattended rule is the opposite one and stays: with no
// terminal to ask on it does not refuse, because the caller passed yes for it.
// apply is the command that refuses, which is the policy difference the two
// call sites of confirmer keep.
func TestConfirmerAutoConfirmsWhenTheCallerSaysYes(t *testing.T) {
	withStdin(t, mustOpen(t, os.DevNull))
	confirm := confirmer(&say.Plain{W: io.Discard}, true)
	ok, err := confirm.Ask("install the platform layer?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ok {
		t.Fatal("the platform layer's unattended answer was refused")
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The login's image is the chart release's pin. box writes the Deployment
// itself, so this mapping is the only place the reference is chosen: a chart
// release that moves the login moves it here, and a constant in this file would
// fail this test.
func TestLoginImageComesFromTheChartRelease(t *testing.T) {
	image := "docker.io/example/authelia:1.0.0@sha256:" + strings.Repeat("e", 64)
	chart := release.Chart{Name: "s5n", Version: testChartVersion, Pins: release.Versions{Authelia: image}}

	opts := loginOptions(chart, setup.Login{
		Host:        "box.example.com",
		Name:        "alice",
		Password:    "not-a-real-password",
		NewPassword: true,
		Values:      map[string]any{"apps": map[string]any{}},
	}, true)

	if opts.Image != image {
		t.Fatalf("login image = %q, want the chart release's pin %q", opts.Image, image)
	}
	for name, got := range map[string]string{"host": opts.Host, "login": opts.Login, "password": opts.Password} {
		if got == "" {
			t.Errorf("%s: the answer init collected was dropped", name)
		}
	}
	if !opts.PathsInNode {
		t.Error("paths-in-node did not reach the login decision")
	}
	if !opts.NewPassword {
		t.Error("the new-password question did not reach the login decision")
	}
}

// One command pulls one release. bootstrap reads the k0s pin, the platform half
// reads the chart versions and init installs the seedbox from the same value, so
// the registry must be asked once: a second pull could settle on a newer
// release than the platform that was just installed.
func TestOneCommandPullsOneRelease(t *testing.T) {
	var f *initFlags
	parse(t, "bootstrap", func(fs *flag.FlagSet) { f = addInitFlags(fs) },
		"--chart-repo", testChartRepo, "--chart-version", testChartVersion)
	src := &recordingCharts{}
	charts := &chartRelease{flags: f.chart, out: &say.Plain{W: io.Discard}, source: src}
	for range []int{0, 1} { // bootstrap's k0s pin, then the platform's versions
		if _, err := charts.pins(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := charts.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.refs) != 1 {
		t.Fatalf("the chart source was asked %d times, want once: %+v", len(src.refs), src.refs)
	}
}
