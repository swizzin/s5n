package release

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeAnchor is root's host-CA step: the read apply makes around its diff and
// the write it makes before the release moves, recorded in the shared event log
// so a test can prove where it landed.
type fakeAnchor struct {
	// line is what Plan answers: the plan's one line, or "" for no work.
	line      string
	planErr   error
	ensureErr error

	plans   []AnchorPlan
	ensured []AnchorPlan
	events  *[]string
	// namespaceReady is asked at Ensure: a first install has no namespace when
	// the plan is made, and the step must not run until this run made one.
	namespaceReady func() bool
	namespaceThere []bool
}

func (f *fakeAnchor) Plan(_ context.Context, plan AnchorPlan) (string, error) {
	f.plans = append(f.plans, plan)
	if f.planErr != nil {
		return "", f.planErr
	}
	return f.line, nil
}

func (f *fakeAnchor) Ensure(_ context.Context, plan AnchorPlan) error {
	if f.namespaceReady != nil {
		f.namespaceThere = append(f.namespaceThere, f.namespaceReady())
	}
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured = append(f.ensured, plan)
	if f.events != nil {
		*f.events = append(*f.events, "anchor")
	}
	return nil
}

// The anchor is one ConfigMap for the whole seedbox, not a per-host object, and
// it is written before the release moves: an app that has to verify the login's
// certificate reads it as its pod starts.
func TestApplyWritesTheAnchorBeforeTheRelease(t *testing.T) {
	f := newFakes(t)
	f.anchor = &fakeAnchor{line: "write the host CA ConfigMap testbox/s5n-host-ca", events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.anchor.plans) != 1 || len(f.anchor.ensured) != 1 {
		t.Fatalf("anchor planned %v and wrote %v, want once each", f.anchor.plans, f.anchor.ensured)
	}
	plan := f.anchor.ensured[0]
	if plan.Domain != "box.example.com" || plan.Namespace != "testbox" {
		t.Fatalf("anchor plan = %+v, want the box's host and its namespace", plan)
	}
	if a, b := eventIndex(f.events, "anchor"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want the anchor before the release", f.events)
	}
	if !res.AnchorWrote {
		t.Fatalf("result does not say the anchor was written: %+v", res)
	}
	if !strings.Contains(f.out.String(), "write the host CA ConfigMap testbox/s5n-host-ca") {
		t.Fatalf("the plan did not show the anchor it would write:\n%s", f.out)
	}
}

// TestTheAnchorFollowsTheIssuerThisRunWrites: init hands its own answer for the
// box's host name to the step, and it arrives unchanged. It is the input that
// makes a first init right -- there is no Certificate in the cluster then, and
// the run's decision is the only thing the anchor can follow.
func TestTheAnchorFollowsTheIssuerThisRunWrites(t *testing.T) {
	f := newFakes(t)
	f.anchor = &fakeAnchor{line: "write the host CA ConfigMap testbox/s5n-host-ca", events: &f.events}
	f.anchorIssuer = "selfsigned"

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.anchor.plans) != 1 {
		t.Fatalf("anchor planned %v, want once", f.anchor.plans)
	}
	if got := f.anchor.plans[0].Issuer; got != "selfsigned" {
		t.Fatalf("anchor plan issuer = %q, want the issuer this run writes", got)
	}
}

// TestAUserApplyDoesNoAnchorWork: `box apply` hands apply no anchor step at
// all, so there is nothing to read and nothing to write -- a user's run never
// touches the ConfigMap, and never needs to read cert-manager's namespace.
func TestAUserApplyDoesNoAnchorWork(t *testing.T) {
	f := newFakes(t)
	f.rel.values, f.rel.exists = deployedValues(t, f), true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if res.AnchorWrote {
		t.Fatal("a run with no anchor step says it wrote the anchor")
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("a run with no work did not say so:\n%s", f.out)
	}
}

// An anchor that is the only work left does not churn the release: the revision
// users would roll back to stays where it is.
func TestTheAnchorAloneMakesNoReleaseRevision(t *testing.T) {
	f := newFakes(t)
	f.rel.values, f.rel.exists = deployedValues(t, f), true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true
	f.anchor = &fakeAnchor{line: "write the host CA ConfigMap testbox/s5n-host-ca", events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.anchor.ensured) != 1 {
		t.Fatalf("the anchor wrote %v, want once", f.anchor.ensured)
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none: only the anchor was missing", f.rel.upgrades)
	}
	if res.Applied {
		t.Fatal("result says the release was applied")
	}
	if !res.AnchorWrote {
		t.Fatal("result does not say the anchor was written")
	}
}

// A run with nothing to do about the anchor does not ask it to write, and its
// plan says nothing: a steady-state apply stays quiet.
func TestNoAnchorWorkIsNotAStep(t *testing.T) {
	f := newFakes(t)
	f.rel.values, f.rel.exists = deployedValues(t, f), true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true
	f.anchor = &fakeAnchor{events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.anchor.ensured) != 0 {
		t.Fatalf("the anchor wrote %v, want nothing", f.anchor.ensured)
	}
	if res.AnchorWrote {
		t.Fatal("result says the anchor was written")
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("a run with no work did not say so:\n%s", f.out)
	}
}

// A refused anchor stops the run before the release moves, like the per-host
// step: an app whose trust is not in place is an app the release would start
// against nothing. On a first install the namespace exists by then -- the
// anchor lives in it, so it cannot be written before -- and nothing is
// installed on top of the failure.
func TestAFailedAnchorLeavesTheReleaseAlone(t *testing.T) {
	f := newFakes(t)
	f.anchor = &fakeAnchor{
		line:      "write the host CA ConfigMap testbox/s5n-host-ca",
		ensureErr: errors.New("writing the host CA into testbox: the cluster refused it"),
		events:    &f.events,
	}

	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "the cluster refused it") {
		t.Fatalf("error = %v, want the step's own failure", err)
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none", f.rel.upgrades)
	}
	if len(f.store.applied) != 0 {
		t.Fatalf("storage was written before the anchor stopped the run: %v", f.store.applied)
	}
}

// A first install has no namespace when the plan is made: the run creates it,
// and the anchor is written after that -- before the release, so an app that
// verifies the login's certificate starts with it -- and never before the
// per-host step, whose writes are in another namespace.
func TestTheAnchorIsWrittenOnceTheNamespaceExists(t *testing.T) {
	f := newFakes(t)
	f.anchor = &fakeAnchor{
		line:           "write the host CA ConfigMap testbox/s5n-host-ca",
		events:         &f.events,
		namespaceReady: func() bool { return f.store.nsExists },
	}

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.anchor.namespaceThere) != 1 || !f.anchor.namespaceThere[0] {
		t.Fatalf("the anchor was written with no namespace: %v", f.anchor.namespaceThere)
	}
	if f.store.nsCreates != 1 {
		t.Fatalf("namespace creates = %d, want this run to have made one", f.store.nsCreates)
	}
	if ns, a, h := eventIndex(f.events, "namespace"), eventIndex(f.events, "anchor"), eventIndex(f.events, "helm"); ns < 0 || a < 0 || h < 0 || ns > a || a > h {
		t.Fatalf("events = %v, want the namespace, then the anchor, then the release", f.events)
	}
}
