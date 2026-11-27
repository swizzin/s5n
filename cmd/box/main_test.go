package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
	"github.com/swizzin/s5n/internal/update"
)

const (
	testChartRepo    = "oci://127.0.0.1:5000/charts/s5n"
	testChartVersion = "1.2.3"
)

// testKubeconfig is enough for client-go to build a client: box does not contact
// the cluster when it opens a release handle.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user: {}
`

func TestTheUpdatesHelmWaitOutlastsTheStartupBudget(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := updateHelm(kubeconfig, "testbox")
	if err != nil {
		t.Fatalf("updateHelm: %v", err)
	}
	if h.Timeout != 35*time.Minute {
		t.Fatalf("update's Helm wait = %v, want the health gate's %v", h.Timeout, update.DefaultHealthWait)
	}
	if !h.HooksOnly {
		t.Fatal("update's Helm waits for the release too: a crashlooping pod holds it for the whole budget")
	}
	applied, err := release.NewHelm(kubeconfig, "testbox", release.ReleaseName)
	if err != nil {
		t.Fatalf("apply's helm: %v", err)
	}
	if applied.Timeout != 0 {
		t.Fatalf("apply's Helm wait = %v, want Helm's own default", applied.Timeout)
	}
	if applied.HooksOnly {
		t.Fatal("apply's Helm waits for hooks only: nothing else gates the release it installs")
	}
}

// recordingCharts stands in for the registry: it records the reference each
// command asks for, which is the thing that must not lose a flag.
type recordingCharts struct {
	refs []release.ChartRef
}

func (r *recordingCharts) Load(_ context.Context, ref release.ChartRef) (release.Chart, error) {
	r.refs = append(r.refs, ref)
	return release.Chart{Name: "s5n", Version: testChartVersion}, nil
}

func parse(t *testing.T, name string, add func(*flag.FlagSet), args ...string) {
	t.Helper()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	add(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

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

func TestApplyPassesItsChartFlagsToTheOptions(t *testing.T) {
	var flags *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { flags = addApplyFlags(fs) },
		"--chart-repo", testChartRepo, "--chart-version", testChartVersion)

	want := release.ChartRef{Repo: testChartRepo, Version: testChartVersion}
	if got := flags.options().ChartRef(); got != want {
		t.Fatalf("apply asked for %+v, want %+v", got, want)
	}
}

func TestApplyAndRollbackInstallWithTheChartsHooks(t *testing.T) {
	var apply *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { apply = addApplyFlags(fs) })
	if opts := apply.options(); opts.SkipHooks || opts.RunHooks {
		t.Fatalf("box apply's options = %+v, want the chart's hooks left to the apply", opts)
	}

	var rollback *rollbackFlags
	parse(t, "rollback", func(fs *flag.FlagSet) { rollback = addRollbackFlags(fs) }, "sonarr")
	if opts := rollback.options("sonarr"); opts.Apply.SkipHooks || opts.Apply.RunHooks {
		t.Fatalf("box rollback's options = %+v, want the chart's hooks left to the apply", opts.Apply)
	}
}

func TestAStepsApplyIsTheOptionsUpdateRuns(t *testing.T) {
	var flags *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { flags = addUpdateFlags(fs) }, "--check")

	held := func(context.Context, *seedbox.Seedbox, release.Chart, map[string]any) (map[string]any, error) {
		return nil, nil
	}
	step := release.Options{GrantPath: "/tmp/grant.yaml", ValuesPath: "/tmp/values.yaml", Holds: held}
	run := flags.runOptions(step)
	if !run.Check {
		t.Fatal("--check did not reach the options the steps run with")
	}
	if run.Apply.GrantPath != "/tmp/grant.yaml" || run.Apply.ValuesPath != "/tmp/values.yaml" {
		t.Fatalf("the steps would apply %q/%q, want the files this run read", run.Apply.GrantPath, run.Apply.ValuesPath)
	}
	if run.Apply.Holds == nil {
		t.Fatal("the steps run without the holds file: a step would undo the rollback it had just made")
	}
}

func TestUpdatePassesItsFlagsToTheOptions(t *testing.T) {
	var flags *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { flags = addUpdateFlags(fs) },
		"--chart-repo", testChartRepo, "--chart-version", testChartVersion,
		"--grant", "/tmp/grant.yaml", "--values", "/tmp/values.yaml", "--check")

	opts := flags.options()
	if got, want := opts.Apply.ChartRef(), (release.ChartRef{Repo: testChartRepo, Version: testChartVersion}); got != want {
		t.Fatalf("update asked for %+v, want %+v", got, want)
	}
	if opts.Apply.GrantPath != "/tmp/grant.yaml" || opts.Apply.ValuesPath != "/tmp/values.yaml" {
		t.Fatalf("update would apply %q/%q, want the files it was given", opts.Apply.GrantPath, opts.Apply.ValuesPath)
	}
	if !opts.Check {
		t.Fatal("--check did not reach the options")
	}
}

func TestUpdateKeepsItsRecordBesideTheClusterRecord(t *testing.T) {
	var defaults *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { defaults = addUpdateFlags(fs) })
	if got, want := defaults.statePath(), update.FilePath(bootstrap.RecordPath); got != want {
		t.Fatalf("update's record = %s, want %s beside bootstrap's own", got, want)
	}

	record := filepath.Join(t.TempDir(), "cluster.yaml")
	var flags *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { flags = addUpdateFlags(fs) }, "--record", record)
	if got, want := flags.statePath(), filepath.Join(filepath.Dir(record), "update.json"); got != want {
		t.Fatalf("update's record = %s, want %s", got, want)
	}
}

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

// Every chart box pulls is verified before it is read: the source must carry the key's
// verifier and the page its signature is published on. A source built without them either
// refuses everything (a box with no key) or reads an unsigned page (no business at all).
func TestTheChartSourceVerifiesWhatItPulls(t *testing.T) {
	previous := chartReleases
	t.Cleanup(func() { chartReleases = previous })
	chartReleases = "http://127.0.0.1:8080/releases"

	for name, src := range map[string]release.ChartSource{
		"apply, init and migrate": chartSource(&say.Plain{W: io.Discard}),
		"update":                  updateChartSource(&say.Plain{W: io.Discard}),
	} {
		oci, ok := src.(*release.OCIChart)
		if !ok {
			t.Fatalf("%s: the chart source is %T, want the registry", name, src)
		}
		if oci.VerifyChart == nil {
			t.Fatalf("%s: the chart source would install a chart it cannot verify", name)
		}
		if oci.ReleasesURL != chartReleases {
			t.Fatalf("%s: the chart source reads signatures from %q, want %q", name, oci.ReleasesURL, chartReleases)
		}
		// The verifier is the key compiled into box, not a stub that says yes: a
		// signature nobody made is refused.
		bogus := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		err := oci.VerifyChart(testChartVersion, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", []byte(bogus))
		if err == nil {
			t.Fatalf("%s: the chart source verified a signature nobody made", name)
		}
		if !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("%s: the refusal is %q, want the signature's refusal", name, err)
		}
	}
}

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

func TestDevFlagsAreNotInAnyHelp(t *testing.T) {
	commands := []struct {
		name string
		add  func(*flag.FlagSet)
	}{
		{"bootstrap and init", func(fs *flag.FlagSet) { addInitFlags(fs) }},
		{"apply", func(fs *flag.FlagSet) { addApplyFlags(fs) }},
		{"rollback", func(fs *flag.FlagSet) { addRollbackFlags(fs) }},
	}
	for _, cmd := range commands {
		t.Run(cmd.name, func(t *testing.T) {
			fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
			cmd.add(fs)
			var b strings.Builder
			fs.SetOutput(&b)
			fs.PrintDefaults()
			help := b.String()
			for _, name := range []string{"acme-server", "dns-file", "paths-in-node", "ca-file"} {
				if strings.Contains(help, name) {
					t.Errorf("%s help lists the dev flag %q:\n%s", cmd.name, name, help)
				}
				if fs.Lookup(name) != nil {
					t.Errorf("%s registers the dev flag %q, which --help prints whatever the usage says", cmd.name, name)
				}
			}
		})
	}
}

func TestTakePathsInNode(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
		rest []string
	}{
		{"absent", []string{"--yes", "--grant", "/tmp/grant.yaml"}, false, []string{"--yes", "--grant", "/tmp/grant.yaml"}},
		{"double dash", []string{"--yes", "--paths-in-node"}, true, []string{"--yes"}},
		{"single dash", []string{"-paths-in-node", "--yes"}, true, []string{"--yes"}},
		{"equals true", []string{"--paths-in-node=true"}, true, nil},
		{"equals false", []string{"--paths-in-node=false"}, false, nil},
		{"after a positional", []string{"stray", "--paths-in-node"}, true, []string{"stray"}},
		{"before a positional", []string{"--paths-in-node", "stray"}, true, []string{"stray"}},
		{"last one wins", []string{"--paths-in-node", "--paths-in-node=false"}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := takePathsInNode(tc.args)
			if err != nil {
				t.Fatalf("takePathsInNode: %v", err)
			}
			if got != tc.want {
				t.Fatalf("set = %v, want %v", got, tc.want)
			}
			if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
				t.Fatalf("rest = %v, want %v", rest, tc.rest)
			}
		})
	}
	if _, _, err := takePathsInNode([]string{"--paths-in-node=maybe"}); err == nil {
		t.Fatal("a value that is not a boolean was accepted")
	}
}

func TestPathsInNodeReachesTheDecisions(t *testing.T) {
	spellings := [][]string{
		{"--paths-in-node"},
		{"-paths-in-node"},
		{"--paths-in-node=true"},
	}
	chart := release.Chart{Name: "s5n", Version: testChartVersion}
	login := setup.Login{Host: "box.example.com", Name: "alice", Password: "not-a-real-password"}

	for _, args := range spellings {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// init and bootstrap: the switch is stripped, the rest is parsed,
			// and the flags carry it into the login decision.
			dev, rest, err := takeInitDevFlags(append(append([]string{}, args...), "--domain", "box.example.com"))
			if err != nil {
				t.Fatalf("takeInitDevFlags: %v", err)
			}
			var initF *initFlags
			parse(t, "init", func(fs *flag.FlagSet) { initF = addInitFlags(fs) }, rest...)
			initF.takeDev(dev)
			if !loginOptions(chart, login, initF.pathsInNode).PathsInNode {
				t.Errorf("init: %v did not reach the login decision", args)
			}

			// apply: the same switch, into the options the release client is
			// handed. The storage check reads Options.PathsInNode.
			set, rest, err := takePathsInNode(append(append([]string{}, args...), "--values", "/tmp/values.yaml"))
			if err != nil {
				t.Fatalf("takePathsInNode: %v", err)
			}
			var applyF *applyFlags
			parse(t, "apply", func(fs *flag.FlagSet) { applyF = addApplyFlags(fs) }, rest...)
			applyF.pathsInNode = set
			if !applyF.options().PathsInNode {
				t.Errorf("apply: %v did not reach the storage decision", args)
			}
		})
	}

	// Without the switch both decisions stay false: an operator's apply must
	// still stat the directories it is about to write into.
	var initF *initFlags
	parse(t, "init", func(fs *flag.FlagSet) { initF = addInitFlags(fs) }, "--domain", "box.example.com")
	if loginOptions(chart, login, initF.pathsInNode).PathsInNode {
		t.Error("init skipped the login's storage without being asked to")
	}
	var applyF *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { applyF = addApplyFlags(fs) }, "--values", "/tmp/values.yaml")
	if applyF.options().PathsInNode {
		t.Error("apply skipped its directory check without being asked to")
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
	ok, err := confirm.Ask("apply s5n in the seedbox namespace?", false)
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

func TestConfirmerAutoConfirmsWhenTheCallerSaysYes(t *testing.T) {
	withStdin(t, mustOpen(t, os.DevNull))
	confirm := confirmer(&say.Plain{W: io.Discard}, true)
	ok, err := confirm.Ask("install the platform layer?", false)
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

func TestOnlyApplySetsTheSnapshotStep(t *testing.T) {
	var applyF *applyFlags
	parse(t, "apply", func(fs *flag.FlagSet) { applyF = addApplyFlags(fs) })
	if applyF.options().Snapshots != nil {
		t.Fatal("apply's flags carry a snapshot step: the command that knows the grant sets it")
	}

	var updateF *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { updateF = addUpdateFlags(fs) })
	if updateF.options().Apply.Snapshots != nil {
		t.Fatal("update's steps would inherit a snapshot step; each step copies its own")
	}

	var rollbackF *rollbackFlags
	parse(t, "rollback", func(fs *flag.FlagSet) { rollbackF = addRollbackFlags(fs) })
	if rollbackF.options("sonarr").Apply.Snapshots != nil {
		t.Fatal("a rollback's apply would copy over the snapshot it is restoring")
	}

	// And the step apply does set is off the package that holds the cluster
	// client, so cmd/box imports no k8s.io package itself.
	hook := applySnapshots("")
	if hook == nil || hook.OpenRelease == nil || hook.OpenCluster == nil || hook.OpenSnapshots == nil {
		t.Fatalf("apply's snapshot step is incomplete: %+v", hook)
	}
}

func TestEveryVersionSpellingPrintsTheVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		if !versionFlag(arg) {
			t.Fatalf("box %s does not print the version", arg)
		}
	}
	for _, arg := range []string{"bootstrap", "init", "apply", "-version", "--v", ""} {
		if versionFlag(arg) {
			t.Fatalf("box %s was read as a version question", arg)
		}
	}
}
