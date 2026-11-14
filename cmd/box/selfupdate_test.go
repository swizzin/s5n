package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/selfupdate"
)

// These tests are about the decision, in order: what `box update` does about the
// box it is running as before it touches a chart. selfupdate's own tests prove
// that a release verifies, a signature is refused and a binary is replaced
// atomically; here the package's entry points are fakes, so what is observed is
// which one was asked, with what, and whether the chart work followed.

// setEuid makes this process root, or not, for the length of one test: the file
// owner is what decides whether box may replace itself, and a test cannot become
// root.
func setEuid(t *testing.T, uid int) {
	t.Helper()
	previous := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = previous })
}

// plainOutput is the transcript an operator without a terminal reads.
func plainOutput() (say.Reporter, *bytes.Buffer) {
	var out bytes.Buffer
	return &say.Plain{W: &out}, &out
}

// noUpdate and noCheck fail the test if they are reached: a user's box, a
// development build and a re-executed run must not ask to replace the binary at
// all.
func noUpdate(t *testing.T) func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
	return func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		t.Error("the self-update ran when it must not")
		return selfupdate.Result{}, errors.New("unexpected update")
	}
}

func noCheck(t *testing.T) func(context.Context, selfupdate.Options) (selfupdate.Available, error) {
	return func(context.Context, selfupdate.Options) (selfupdate.Available, error) {
		t.Error("the release page was checked when it must not be")
		return selfupdate.Available{}, errors.New("unexpected check")
	}
}

func noReexec(t *testing.T) func(string, []string, []string, string) error {
	return func(string, []string, []string, string) error {
		t.Error("the process was handed over when it must not be")
		return errors.New("unexpected reexec")
	}
}

// runBox carries the step out the way runUpdate does: through updateRun, with
// the chart work counted.
func runBox(t *testing.T, b *boxUpdate) (charts int) {
	t.Helper()
	err := updateRun{Box: b, Charts: func(context.Context) { charts++ }}.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return charts
}

func TestUpdateMapsTheBoxFlagsToTheOptions(t *testing.T) {
	previous := packaged
	packaged = "apt"
	t.Cleanup(func() { packaged = previous })

	var f *updateFlags
	parse(t, "update", func(fs *flag.FlagSet) { f = addUpdateFlags(fs) },
		"--hold-box", "--rollback-box", "--values", "/home/alice/.config/box/values.yaml")

	opts := f.selfUpdateOptions("http://127.0.0.1:8080")
	if !opts.Hold {
		t.Fatal("--hold-box did not reach Options.Hold: the box would be replaced anyway")
	}
	if !*f.rollbackBox {
		t.Fatal("--rollback-box is not read by the command")
	}
	if opts.Version != version {
		t.Fatalf("Options.Version = %q, want the version this box runs, %q", opts.Version, version)
	}
	if opts.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("Options.BaseURL = %q, want the release page the development switch names", opts.BaseURL)
	}
	if opts.Packaged != "apt" {
		t.Fatalf("Options.Packaged = %q, want the build's package manager: a packaged box would overwrite itself", opts.Packaged)
	}
	// The release-page switch is a development one, taken out of the arguments
	// before the FlagSet sees them, so --help cannot offer it.
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	addUpdateFlags(fs)
	if fs.Lookup("box-releases") != nil {
		t.Fatal("--box-releases is registered: it is a development switch and must not be in --help")
	}
	if f := fs.Lookup("hold-box"); f == nil || !strings.Contains(f.Usage, "newer") {
		t.Fatalf("--hold-box usage = %v, want it to say what it holds back", f)
	}
}

func TestUpdateInstallsANewerBoxInsteadOfDoingTheChartWork(t *testing.T) {
	setEuid(t, 0)
	out, transcript := plainOutput()
	var order []string
	b := &boxUpdate{
		Options: selfupdate.Options{Version: "0.4.0"},
		Args:    []string{"update", "--values", "/etc/box/values.yaml"},
		Env:     []string{"PATH=/usr/local/bin:/usr/bin", "HOME=/root"},
		Out:     out,
		Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
			order = append(order, "update")
			return selfupdate.Result{From: "0.4.0", To: "0.5.0", Path: "/usr/local/bin/box", Previous: "/usr/local/bin/box.previous", Changed: true}, nil
		},
		Reexec: func(path string, args, env []string, version string) error {
			order = append(order, "reexec")
			if path != "/usr/local/bin/box" {
				t.Errorf("re-exec of %q, want the binary the update installed", path)
			}
			if want := []string{"update", "--values", "/etc/box/values.yaml"}; !slices.Equal(args, want) {
				t.Errorf("re-exec arguments %q, want the process's own, %q: the command that triggered the update has to run again", args, want)
			}
			if want := []string{"PATH=/usr/local/bin:/usr/bin", "HOME=/root"}; !slices.Equal(env, want) {
				t.Errorf("re-exec environment %q, want the process's own, %q", env, want)
			}
			if version != "0.5.0" {
				t.Errorf("re-exec marked the new process %q, want 0.5.0: without it the new box updates itself again", version)
			}
			// A real exec never returns; nil is the test standing in for it, and
			// the run has to treat it as "the process is gone".
			return nil
		},
	}

	charts := runBox(t, b)
	if want := []string{"update", "reexec"}; !slices.Equal(order, want) {
		t.Fatalf("the run did %q, want %q: the box is replaced first, and the chart work belongs to the new binary", order, want)
	}
	if charts != 0 {
		t.Fatalf("the chart work ran %d times in the replaced process, want 0", charts)
	}
	if !strings.Contains(transcript.String(), "box updated from 0.4.0 to 0.5.0") {
		t.Fatalf("transcript %q does not say the box was updated", transcript.String())
	}
}

// closeRecorder is a reporter that notes when it is closed, so a test can see
// whether the terminal was handed back before the process was replaced.
type closeRecorder struct {
	say.Reporter
	order *[]string
}

func (c closeRecorder) Close() { *c.order = append(*c.order, "close") }

func TestUpdateClosesTheReporterBeforeTheNewBoxTakesOver(t *testing.T) {
	// An exec never returns, so the reporter cmd/box defers closing is never
	// closed on this path unless the step closes it: the "box updated" line
	// could stay buffered and the terminal stay painted under the new process.
	setEuid(t, 0)
	plain, _ := plainOutput()
	var order []string
	b := &boxUpdate{
		Options: selfupdate.Options{Version: "0.4.0"},
		Out:     closeRecorder{Reporter: plain, order: &order},
		Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
			return selfupdate.Result{From: "0.4.0", To: "0.5.0", Path: "/usr/local/bin/box", Changed: true}, nil
		},
		Reexec: func(string, []string, []string, string) error {
			order = append(order, "reexec")
			return nil
		},
	}
	runBox(t, b)
	if want := []string{"close", "reexec"}; !slices.Equal(order, want) {
		t.Fatalf("the run did %q, want %q: the terminal is handed back before the process is replaced", order, want)
	}
}

func TestUpdateRunsTheChartWorkWhenTheBoxCannotBeReplaced(t *testing.T) {
	// A box that cannot replace itself is still a box that moves the seedbox, and
	// the chart update then runs with the releases this box meets min-box for,
	// which is what it did before this step existed.
	cases := []struct {
		name  string
		build func(t *testing.T, out say.Reporter) *boxUpdate
		want  string
	}{
		{
			name: "the release page is unreachable",
			build: func(t *testing.T, out say.Reporter) *boxUpdate {
				return &boxUpdate{Options: selfupdate.Options{Version: "0.4.0"}, Out: out,
					Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
						return selfupdate.Result{}, errors.New("fetching https://example.com/releases/latest: 503 Service Unavailable")
					}}
			},
			want: "not updating box: fetching https://example.com/releases/latest: 503 Service Unavailable",
		},
		{
			name: "this build has no signing key",
			build: func(t *testing.T, out say.Reporter) *boxUpdate {
				return &boxUpdate{Options: selfupdate.Options{Version: "0.4.0"}, Out: out,
					Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
						return selfupdate.Result{}, errors.New("this box was built without a signing key, so it cannot verify a box release and will not update itself: replace it by hand")
					}}
			},
			want: "not updating box: this box was built without a signing key",
		},
		{
			name: "the release does not verify",
			build: func(t *testing.T, out say.Reporter) *boxUpdate {
				return &boxUpdate{Options: selfupdate.Options{Version: "0.4.0"}, Out: out,
					Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
						return selfupdate.Result{}, errors.New("updating box 0.4.0 to 0.5.0: sha256sums.txt for box-v0.5.0 does not verify against the signing key compiled into this box")
					}}
			},
			want: "not updating box: updating box 0.4.0 to 0.5.0: sha256sums.txt for box-v0.5.0 does not verify",
		},
		{
			name: "box came from a package",
			build: func(t *testing.T, out say.Reporter) *boxUpdate {
				// The real refusal: selfupdate refuses a packaged binary before
				// it asks the release page anything, so this needs no network.
				return &boxUpdate{Options: selfupdate.Options{Version: "0.4.0", Packaged: "apt", Geteuid: func() int { return 0 }},
					Out: out, Update: selfupdate.Update}
			},
			want: "not updating box: this box was installed by a package manager (apt), so the package owns the binary: upgrade it with apt, not with box",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEuid(t, 0)
			out, transcript := plainOutput()
			b := tc.build(t, out)
			b.Args = []string{"update"}
			if charts := runBox(t, b); charts != 1 {
				t.Fatalf("the chart work ran %d times, want once: a box that cannot replace itself still updates the seedbox", charts)
			}
			if !strings.Contains(transcript.String(), tc.want) {
				t.Fatalf("transcript %q does not say %q", transcript.String(), tc.want)
			}
			if strings.Count(transcript.String(), "\n") != 1 {
				t.Fatalf("transcript %q is not one line: the cause is said once", transcript.String())
			}
		})
	}
}

func TestUpdateStopsWhenTheInstalledBoxWillNotStart(t *testing.T) {
	// A replaced binary that will not run is the one case that does not fall
	// through to the chart work: the seedbox would move under a box that is no
	// longer the one on disk, and the run would say it continued when it had not.
	setEuid(t, 0)
	out, _ := plainOutput()
	b := &boxUpdate{
		Options: selfupdate.Options{Version: "0.4.0"},
		Out:     out,
		Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
			return selfupdate.Result{From: "0.4.0", To: "0.5.0", Path: "/usr/local/bin/box", Changed: true}, nil
		},
		Reexec: func(string, []string, []string, string) error {
			return errors.New("permission denied")
		},
	}
	charts := 0
	err := updateRun{Box: b, Charts: func(context.Context) { charts++ }}.run(context.Background())
	if err == nil {
		t.Fatal("a box that could not start the binary it installed carried on")
	}
	if charts != 0 {
		t.Fatalf("the chart work ran %d times, want none", charts)
	}
	if want := "box 0.5.0 is installed at /usr/local/bin/box, but starting it failed (permission denied)"; !strings.Contains(err.Error(), want) {
		t.Fatalf("the run stopped with %q, want %q", err, want)
	}
	if !strings.Contains(err.Error(), "run `box update` again") {
		t.Fatalf("the run stopped with %q, want it to say what to do next", err)
	}
}

func TestUpdateNamesTheHeldRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result selfupdate.Result
		want   string
	}{
		{
			name:   "a newer box is held",
			result: selfupdate.Result{From: "0.4.0", To: "0.5.0", Path: "/usr/local/bin/box", Held: true},
			want:   "box 0.5.0 is out; --hold-box is set, so it is not installed",
		},
		{
			name:   "nothing newer is out",
			result: selfupdate.Result{From: "0.4.0", To: "0.4.0", Path: "/usr/local/bin/box"},
			want:   "box 0.4.0 is the newest release; --hold-box is set, so the self-update stays off",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEuid(t, 0)
			out, transcript := plainOutput()
			b := &boxUpdate{
				Options: selfupdate.Options{Version: "0.4.0", Hold: true},
				Out:     out,
				Update: func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
					return tc.result, nil
				},
				Reexec: noReexec(t),
			}
			if charts := runBox(t, b); charts != 1 {
				t.Fatalf("the chart work ran %d times, want once: a hold is about box, not about the seedbox", charts)
			}
			if !strings.Contains(transcript.String(), tc.want) {
				t.Fatalf("transcript %q does not say %q", transcript.String(), tc.want)
			}
		})
	}
}

func TestUpdateChecksNothingWhenItIsOnlyACheck(t *testing.T) {
	// `box update --check` writes nothing, and replacing the binary would be the
	// one thing an operator asking what would happen must not get.
	setEuid(t, 0)
	out, transcript := plainOutput()
	b := &boxUpdate{
		Options:   selfupdate.Options{Version: "0.4.0"},
		CheckOnly: true,
		Out:       out,
		Update:    noUpdate(t),
		Check: func(context.Context, selfupdate.Options) (selfupdate.Available, error) {
			return selfupdate.Available{Current: "0.4.0", Latest: "0.5.0", Newer: true}, nil
		},
	}
	if charts := runBox(t, b); charts != 1 {
		t.Fatalf("the chart work ran %d times, want once: --check still plans the chart update", charts)
	}
	if want := "box 0.5.0 is out; --check, so this run installs nothing"; !strings.Contains(transcript.String(), want) {
		t.Fatalf("transcript %q does not say %q", transcript.String(), want)
	}
}

func TestUpdateNeverReplacesAUsersBox(t *testing.T) {
	setEuid(t, 1000)
	out, transcript := plainOutput()
	b := &boxUpdate{
		Options: selfupdate.Options{Version: "0.4.0"},
		Out:     out,
		Update:  noUpdate(t),
		Check: func(context.Context, selfupdate.Options) (selfupdate.Available, error) {
			return selfupdate.Available{Current: "0.4.0", Latest: "0.5.0", Newer: true}, nil
		},
	}
	if charts := runBox(t, b); charts != 1 {
		t.Fatalf("the chart work ran %d times, want once: a user's box still updates their seedbox", charts)
	}
	if want := "box 0.5.0 is out; root's next `box update` installs it"; !strings.Contains(transcript.String(), want) {
		t.Fatalf("transcript %q does not say %q", transcript.String(), want)
	}
}

func TestUpdateReportsAFailedCheckWithoutStopping(t *testing.T) {
	setEuid(t, 1000)
	out, transcript := plainOutput()
	b := &boxUpdate{
		Options: selfupdate.Options{Version: "0.4.0"},
		Out:     out,
		Update:  noUpdate(t),
		Check: func(context.Context, selfupdate.Options) (selfupdate.Available, error) {
			return selfupdate.Available{}, errors.New("fetching https://example.com/releases/latest: 503 Service Unavailable")
		},
	}
	if charts := runBox(t, b); charts != 1 {
		t.Fatalf("the chart work ran %d times, want once", charts)
	}
	if want := "not checking for a newer box: fetching https://example.com/releases/latest: 503"; !strings.Contains(transcript.String(), want) {
		t.Fatalf("transcript %q does not say %q", transcript.String(), want)
	}
}

func TestRollbackBoxDoesNoChartWork(t *testing.T) {
	setEuid(t, 0)
	out, transcript := plainOutput()
	var replaced string
	b := &boxUpdate{
		Options:     selfupdate.Options{},
		RollbackBox: true,
		Out:         out,
		Update:      noUpdate(t),
		Check:       noCheck(t),
		Executable: func(selfupdate.Options) (string, error) {
			return "/usr/local/bin/box", nil
		},
		Rollback: func(path string) error {
			replaced = path
			return nil
		},
	}
	if charts := runBox(t, b); charts != 0 {
		t.Fatalf("the chart work ran %d times, want none: --rollback-box is box only", charts)
	}
	if replaced != "/usr/local/bin/box" {
		t.Fatalf("rollback put %q back, want the binary box runs from", replaced)
	}
	if want := "put the previous box back at /usr/local/bin/box"; !strings.Contains(transcript.String(), want) {
		t.Fatalf("transcript %q does not say %q", transcript.String(), want)
	}
}

func TestRollbackBoxNeedsRoot(t *testing.T) {
	setEuid(t, 1000)
	out, _ := plainOutput()
	b := &boxUpdate{
		RollbackBox: true,
		Out:         out,
		Rollback: func(string) error {
			t.Error("a user's box rolled root's box back")
			return nil
		},
	}
	err := updateRun{Box: b, Charts: func(context.Context) { t.Error("a refused rollback ran chart work") }}.run(context.Background())
	if err == nil {
		t.Fatal("--rollback-box ran as a user")
	}
	if !strings.Contains(err.Error(), "run it as root") {
		t.Fatalf("--rollback-box refused with %q, want it to say to run it as root", err)
	}
}

func TestRollbackBoxReportsWhatFailed(t *testing.T) {
	setEuid(t, 0)
	out, _ := plainOutput()
	b := &boxUpdate{
		RollbackBox: true,
		Out:         out,
		Executable:  func(selfupdate.Options) (string, error) { return "/usr/local/bin/box", nil },
		Rollback: func(string) error {
			return errors.New("there is no box.previous beside /usr/local/bin/box, so there is no previous box to go back to")
		},
	}
	err := updateRun{Box: b, Charts: func(context.Context) { t.Error("a failed rollback ran chart work") }}.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no box.previous") {
		t.Fatalf("run returned %v, want the rollback's own refusal", err)
	}
}

func TestUpdateSkipsTheSelfUpdateForADevBuild(t *testing.T) {
	for _, build := range []string{"dev", ""} {
		t.Run(build, func(t *testing.T) {
			setEuid(t, 0)
			out, transcript := plainOutput()
			b := &boxUpdate{
				Options: selfupdate.Options{Version: build},
				Out:     out,
				Update:  noUpdate(t),
				Check:   noCheck(t),
			}
			if charts := runBox(t, b); charts != 1 {
				t.Fatalf("the chart work ran %d times, want once: a development build still updates the seedbox", charts)
			}
			if want := "skipping the self-update"; !strings.Contains(transcript.String(), want) {
				t.Fatalf("transcript %q does not say %q", transcript.String(), want)
			}
		})
	}
}

func TestUpdateSkipsTheSelfUpdateInTheReexecutedProcess(t *testing.T) {
	setEuid(t, 0)
	t.Setenv(selfupdate.ReexecEnv, "0.5.0")
	out, transcript := plainOutput()
	b := &boxUpdate{
		Options: selfupdate.Options{Version: "0.5.0"},
		Out:     out,
		Update:  noUpdate(t),
		Check:   noCheck(t),
	}
	if charts := runBox(t, b); charts != 1 {
		t.Fatalf("the chart work ran %d times, want once", charts)
	}
	// Silent: the process that installed this box already said so.
	if transcript.Len() != 0 {
		t.Fatalf("a re-executed run said %q, want nothing", transcript.String())
	}
}
