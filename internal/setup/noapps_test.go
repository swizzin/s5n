package setup

import (
	"bytes"
	"strings"
	"testing"
)

// emptyBox is what init says once about a box with no apps.
const emptyBox = "nothing runs yet"

// noApps is the grant's app section for a box with every app off.
func assertNoApps(t *testing.T, grant string) {
	t.Helper()
	for _, want := range []string{
		"alpha:\n    enabled: false",
		"beta:\n    enabled: false",
		"gamma:\n    enabled: false",
	} {
		if !strings.Contains(grant, want) {
			t.Fatalf("the grant is missing %q:\n%s", want, grant)
		}
	}
	if strings.Contains(grant, "enabled: true") {
		t.Fatalf("a box with no apps has one on:\n%s", grant)
	}
}

// Picking nothing is an answer: apps arrive by `box install` or migrate, so the
// picker's minimum is zero and init says once what an empty box means.
func TestGuidedInitAcceptsAnEmptySelection(t *testing.T) {
	h := newHarness(t)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{}}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init refused an empty app list: %v\n%s", err, h.out)
	}
	assertNoApps(t, h.read(h.grantPath))
	if len(res.Created) != 0 {
		t.Fatalf("an empty box created %v, want no app config directories", res.Created)
	}
	if got := strings.Count(h.out.String(), emptyBox); got != 1 {
		t.Fatalf("the empty box was explained %d times, want once:\n%s", got, h.out)
	}
	if !strings.Contains(h.out.String(), "box install") {
		t.Fatalf("the line does not say how apps get there:\n%s", h.out)
	}
}

// Unticking every app on a re-run writes enabled: false for each and changes
// nothing else, and says the same one line.
func TestGuidedReRunUnticksEveryApp(t *testing.T) {
	h := newHarness(t)
	write(t, h.grantPath, reRunGrant)
	h.prompt.inputs = []string{"old.example.com"}
	h.prompt.multis = [][]string{{}}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("re-run with every app unticked: %v\n%s", err, h.out)
	}
	if !res.ReRun {
		t.Fatal("an existing grant was treated as a fresh host")
	}
	assertNoApps(t, h.read(h.grantPath))
	if got := strings.Count(h.out.String(), emptyBox); got != 1 {
		t.Fatalf("the empty box was explained %d times, want once:\n%s", got, h.out)
	}
	// Only the app keys changed: the rest is the old bytes.
	if got, want := strings.Join(res.GrantChanged, ","), "apps.alpha.enabled"; got != want {
		t.Fatalf("grant keys changed = %s, want %s", got, want)
	}
}

// The word, not an empty string, is the explicit empty list: `--apps ""` stays "not
// given", which on a fresh box is still the refusal.
func TestUnattendedAppsNone(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{"none"}

	if _, err := h.run(opts); err != nil {
		t.Fatalf("--apps none was refused: %v\n%s", err, h.out)
	}
	assertNoApps(t, h.read(h.grantPath))
	if got := strings.Count(h.out.String(), emptyBox); got != 1 {
		t.Fatalf("the empty box was explained %d times, want once:\n%s", got, h.out)
	}
}

// `--apps ""` is a flag with nothing in it: the refusal names the flag and the word
// that is an answer.
func TestUnattendedEmptyAppsStillRefuses(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{""}

	_, err := h.run(opts)
	if err == nil {
		t.Fatal("an empty --apps was accepted on a fresh box")
	}
	if !strings.Contains(err.Error(), "--apps is needed") || !strings.Contains(err.Error(), "none") {
		t.Fatalf("refusal = %v, want it to name --apps and list none as an answer", err)
	}
	if h.exists(h.grantPath) {
		t.Fatal("a refused run wrote the grant")
	}
}

// "none" and an app cannot both be the answer: either alone is a box running
// something else.
func TestUnattendedNoneWithNames(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{"none,alpha"}

	_, err := h.run(opts)
	if err == nil {
		t.Fatal("--apps none,alpha was accepted")
	}
	for _, want := range []string{"none", "alpha"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
	if h.exists(h.grantPath) {
		t.Fatal("a refused run wrote the grant")
	}
}

// The plain prompt has no toggle, so an empty list needs a word for it -- only where
// the flow allows zero; with a minimum of one, "none" is not an answer.
func TestLinePromptAcceptsNoneWhenAnyAnswerWillDo(t *testing.T) {
	field := MultiSelect{
		Key:      "apps",
		Options:  []Option{{Value: "alpha", Label: "alpha"}, {Value: "beta", Label: "beta"}},
		Selected: []string{"alpha"},
	}
	out := &bytes.Buffer{}
	p := &LinePrompt{In: strings.NewReader("none\n"), Out: out}
	got, err := p.MultiSelect(field)
	if err != nil {
		t.Fatalf("MultiSelect: %v\n%s", err, out)
	}
	if len(got) != 0 {
		t.Fatalf("MultiSelect = %v, want no apps", got)
	}

	field.Min = 1
	p = &LinePrompt{In: strings.NewReader("none\n1\n"), Out: &bytes.Buffer{}}
	got, err = p.MultiSelect(field)
	if err != nil {
		t.Fatalf("MultiSelect: %v", err)
	}
	if len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("with a minimum of one, MultiSelect = %v, want the first app asked again for", got)
	}
}

// Enter keeps what is selected, and on a fresh box that is nothing.
func TestLinePromptAcceptsAnEmptyAnswerWhenNothingIsSelected(t *testing.T) {
	p := &LinePrompt{In: strings.NewReader("\n"), Out: &bytes.Buffer{}}
	got, err := p.MultiSelect(MultiSelect{Key: "apps", Options: []Option{{Value: "alpha", Label: "alpha"}}})
	if err != nil {
		t.Fatalf("MultiSelect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("MultiSelect = %v, want no apps", got)
	}
}

// A migrating init has every app off on purpose, so it passes Adopting and the
// empty-box line is not printed; the grant, the answers and every other line are
// init's own.
func TestAMigratingInitSaysNothingAboutAnEmptyBox(t *testing.T) {
	h := newHarness(t)
	opts := h.unattended()
	opts.Apps = []string{"none"}
	opts.Adopting = true

	res, err := h.run(opts)
	if err != nil {
		t.Fatalf("a migrating init with --apps none was refused: %v\n%s", err, h.out)
	}
	if !res.Applied {
		t.Fatal("the migrating init did not hand over to apply")
	}
	assertNoApps(t, h.read(h.grantPath))
	if strings.Contains(h.out.String(), emptyBox) {
		t.Fatalf("a migrating init told the operator nothing runs yet, and it is about to switch the plan's apps on:\n%s", h.out)
	}
}
