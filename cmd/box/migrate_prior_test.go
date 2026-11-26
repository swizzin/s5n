package main

// migrate_prior_test.go drives cutover's second read: the refusal with nothing
// earlier on record, the diff against the read that is on record, and the two
// things a planned app can be there -- gone, which stops the run, or new, which is
// kept and named.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/discover"
	"github.com/swizzin/s5n/internal/migrate/plan"
)

// readPlan is the plan this saved source reads as, settled the way a cutover
// settles it: the plan an earlier snapshot of the same source would hold, before
// a test edits it.
func readPlan(t *testing.T, o cutoverOptions) *plan.Plan {
	t.Helper()
	read, err := readSource(context.Background(), o.discoverOptions, archive.ModeFinal)
	if err != nil {
		t.Fatalf("read the source: %v", err)
	}
	o.Work = t.TempDir()
	if err := settleRead(context.Background(), o.discoverOptions, &read); err != nil {
		t.Fatalf("settle the read: %v", err)
	}
	// A copy through the file format: the plan a snapshot holds is the plan read
	// back, and the test edits that one.
	raw, err := yaml.Marshal(read.plan)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse(raw)
	if err != nil {
		t.Fatalf("the plan does not read back: %v", err)
	}
	return p
}

// writePrior writes one survey of p in dir, taken at when, and returns its path.
// It is the earlier read a cutover is asked to compare against.
func writePrior(t *testing.T, dir string, when time.Time, p *plan.Plan) string {
	t.Helper()
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, bundle.FileName(archive.ModeSurvey, when))
	if _, err := bundle.Write(path, bundle.Snapshot{
		Mode:   archive.ModeSurvey,
		Plan:   raw,
		Report: []byte("an earlier read of this source\n"),
		Box:    testChartVersion,
		Now:    func() time.Time { return when },
	}); err != nil {
		t.Fatalf("write the earlier read: %v", err)
	}
	return path
}

// editPlan is p with one change, as a fresh plan: the earlier read a test writes
// is never the same value the run holds.
func editPlan(t *testing.T, p *plan.Plan, change func(*plan.Plan)) *plan.Plan {
	t.Helper()
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := plan.Parse(raw)
	if err != nil {
		t.Fatalf("the edited plan does not read back: %v", err)
	}
	change(out)
	return out
}

func TestCutoverRefusesTheFirstReadUnattended(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.NoPriorDiscover = false
	out := filepath.Join(t.TempDir(), "final.tar.gz")
	o.Out = out
	o.Replace = true
	reporter, text := recording()

	err := cutover(context.Background(), o, reporter, proofOf())
	if !errors.Is(err, errCutoverPrior) {
		t.Fatalf("cutover = %v, want the first-read refusal", err)
	}
	for _, want := range []string{
		"this is the first read of this source",
		"sonarr (from sonarr, in media)",
		"invisible to this read",
		"survey: ",
		"box migrate discover",
		"--no-prior-discover",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
	// Nothing apply could run was written, and the read it did take is a survey.
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused cutover wrote the final snapshot: %v", err)
	}
	reads, err := bundle.Reads(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || reads[0].Mode != archive.ModeSurvey {
		t.Fatalf("the refusal kept %d snapshots (%v), want one survey", len(reads), reads)
	}
}

func TestCutoverFirstReadGoesAheadWithNoPriorDiscover(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.NoPriorDiscover = true
	out := filepath.Join(t.TempDir(), "final.tar.gz")
	o.Out = out
	o.Replace = true
	reporter, text := recording()

	if err := cutover(context.Background(), o, reporter, proofOf()); !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v, want the replace path's stop", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the run wrote no final snapshot: %v", err)
	}
	if !strings.Contains(text.String(), "going ahead because --no-prior-discover") {
		t.Errorf("the run does not say why it went ahead:\n%s", text)
	}
}

func TestCutoverAsksOnTheFirstRead(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.NoPriorDiscover = false
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Replace = true
	asker := &fakeSelect{no: true}
	o.Prompt = asker
	reporter, text := recording()

	if err := cutover(context.Background(), o, reporter, proofOf()); !errors.Is(err, errCutoverPrior) {
		t.Fatalf("cutover = %v, want the first-read refusal", err)
	}
	if len(asker.confirms) != 1 {
		t.Fatalf("asked %d questions, want the one about this read", len(asker.confirms))
	}
	if asker.confirms[0].Default {
		t.Error("an empty enter would cut over from a read nothing was compared against")
	}
	if !strings.Contains(asker.confirms[0].Prompt, "invisible to this read") {
		t.Errorf("the question does not say what the risk is: %q", asker.confirms[0].Prompt)
	}
	for _, want := range []string{"this is the first read of this source", "not cutting over, then.", "box migrate discover"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
}

func TestCutoverAnswersYesOnTheFirstRead(t *testing.T) {
	manifests, root := discoverySource(t)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.NoPriorDiscover = false
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Replace = true
	asker := &fakeSelect{}
	o.Prompt = asker
	reporter, _ := recording()

	if err := cutover(context.Background(), o, reporter, proofOf()); !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v, want the replace path's stop", err)
	}
	if len(asker.confirms) != 1 {
		t.Errorf("asked %d questions, want one", len(asker.confirms))
	}
}

func TestCutoverStopsOnAnAppTheEarlierReadHad(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.NoPriorDiscover = true // the flag never skips the diff
	out := filepath.Join(dir, "final.tar.gz")
	o.Out = out
	o.Replace = true

	// The earlier read of this same source saw a Radarr as well.
	earlier := editPlan(t, readPlan(t, o), func(p *plan.Plan) {
		p.Apps["radarr"] = plan.App{
			Kind: plan.Kubernetes, Source: "radarr", Config: "/srv/apps/radarr", Namespace: "media",
			Ports: []int{7878},
		}
	})
	prior := writePrior(t, dir, time.Now().Add(-time.Hour), earlier)

	reporter, text := recording()
	err := cutover(context.Background(), o, reporter, proofOf())
	if !errors.Is(err, errCutoverPrior) {
		t.Fatalf("cutover = %v, want the diff's refusal", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused cutover wrote the final snapshot: %v", err)
	}
	for _, want := range []string{
		"earlier read: " + filepath.Base(prior),
		"neither read names a source host",
		"radarr (it was radarr in media)",
		"cutover stops rather than migrate a source that lost one",
		"survey: ",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
	// Two reads are on record now: the earlier one, and the one this refusal kept.
	reads, err := bundle.Reads(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 {
		t.Errorf("%d reads on record, want the earlier one and the survey this run kept", len(reads))
	}
}

func TestCutoverKeepsAnAppNewSinceTheEarlierRead(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(dir, "final.tar.gz")
	o.Replace = true

	earlier := editPlan(t, readPlan(t, o), func(p *plan.Plan) {
		// The earlier read had only qBittorrent: the two Arrs are new since.
		for name := range p.Apps {
			if name != "qbittorrent" {
				delete(p.Apps, name)
			}
		}
		// And with them the library mapping, whose every directory became a
		// category of an app that read does not have.
		p.Data.Folders = nil
	})
	writePrior(t, dir, time.Now().Add(-time.Hour), earlier)

	reporter, text := recording()
	if err := cutover(context.Background(), o, reporter, proofOf()); !errors.Is(err, errCutoverStopped) {
		t.Fatalf("cutover = %v, want the replace path's stop", err)
	}
	if !strings.Contains(text.String(), "new since the earlier read (kept)") {
		t.Errorf("the run does not name what is new:\n%s", text)
	}
	if !strings.Contains(text.String(), "sonarr (from sonarr, in media)") {
		t.Errorf("the run does not say which app is new:\n%s", text)
	}
}

func TestFindPriorReadMatchesOnHostAndNamespaces(t *testing.T) {
	manifests, root := discoverySource(t)
	dir := t.TempDir()
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	base := readPlan(t, o)
	// A snapshot's name is to the second, so the times a test compares are too.
	when := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for _, tt := range []struct {
		host string
		ns   []string
		at   time.Time
	}{
		{"elsewhere", []string{"media"}, when},
		{"box", []string{"media", "other"}, when.Add(time.Minute)},
		{"box", nil, when.Add(2 * time.Minute)},
		{"box", []string{"media"}, when.Add(3 * time.Minute)},
	} {
		scoped := editPlan(t, base, func(p *plan.Plan) {
			p.Source.Host = tt.host
			p.Source.Namespaces = tt.ns
		})
		writePrior(t, dir, tt.at, scoped)
	}
	// A file that is not a plan box can read is stepped over, not the answer.
	if err := os.WriteFile(filepath.Join(dir, bundle.FileName(archive.ModeSurvey, time.Now())),
		[]byte("not a snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		host string
		ns   []string
		want time.Time
	}{
		{"same host and scope", "box", []string{"media"}, when.Add(3 * time.Minute)},
		{"every namespace", "box", nil, when.Add(2 * time.Minute)},
		{"another host", "other", []string{"media"}, time.Time{}},
		{"another scope", "box", []string{"elsewhere"}, time.Time{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			read := migrationRead{plan: editPlan(t, base, func(p *plan.Plan) {
				p.Source.Host = tt.host
				p.Source.Namespaces = tt.ns
			})}
			prior, err := findPriorRead(dir, read)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want.IsZero() {
				if prior != nil {
					t.Fatalf("matched %s, want no earlier read of this source", filepath.Base(prior.path))
				}
				return
			}
			if prior == nil {
				t.Fatal("found no earlier read of this source")
			}
			if got := nameStampOf(t, prior.path); !got.UTC().Equal(tt.want.UTC()) {
				t.Errorf("matched the read taken at %s, want %s", got.Format(time.RFC3339), tt.want.Format(time.RFC3339))
			}
		})
	}
}

// nameStampOf is when a snapshot's name says it was taken, so a test can say which
// file the search chose.
func nameStampOf(t *testing.T, path string) time.Time {
	t.Helper()
	reads, err := bundle.Reads(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reads {
		if r.Path == path {
			return r.Taken
		}
	}
	t.Fatalf("%s is not in its directory", path)
	return time.Time{}
}

func TestTwoRefusalsInOneSecondKeepTwoReads(t *testing.T) {
	manifests, root := discoverySource(t)
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	o := cutoverOptionsFor(t, manifests)
	o.Root = root
	o.Out = filepath.Join(t.TempDir(), "final.tar.gz")
	o.Now = func() time.Time { return at }
	running := discover.Running{
		App: "sonarr", Kind: "Deployment", Namespace: "media", Name: "sonarr",
		Replicas: 1, Pods: []string{"sonarr-abc"},
	}
	for i := 0; i < 2; i++ {
		reporter, _ := recording()
		if err := cutover(context.Background(), o, reporter, proofOf(running)); !errors.Is(err, errCutoverStillRunning) {
			t.Fatalf("cutover %d = %v, want the still-running refusal", i+1, err)
		}
	}
	reads, err := bundle.Reads(filepath.Dir(o.Out))
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 2 {
		t.Fatalf("%d reads on record, want both refusals' surveys", len(reads))
	}
	for _, r := range reads {
		if r.Mode != archive.ModeSurvey {
			t.Errorf("a refusal kept a %q snapshot, want a survey", r.Mode)
		}
	}
	if !reads[0].Taken.After(reads[1].Taken) {
		t.Errorf("the two reads are named %s and %s, want the second one later", reads[1].Taken, reads[0].Taken)
	}
}
