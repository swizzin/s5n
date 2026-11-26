package release

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeAnchor records where the host-CA step landed relative to the release.
type fakeAnchor struct {
	// line is Plan's answer; "" means no work.
	line      string
	planErr   error
	ensureErr error

	plans   []AnchorPlan
	ensured []AnchorPlan
	events  *[]string
	// namespaceReady is asked at Ensure: a first install creates the namespace.
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

func TestOIDCClientsAreWrittenAfterTheNamespace(t *testing.T) {
	f := newFakes(t)
	f.anchor = &fakeAnchor{
		line:           "write Secret testbox/s5n-oidc-sonarr",
		events:         &f.events,
		namespaceReady: func() bool { return f.store.nsExists },
	}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.NamespaceCreated {
		t.Fatal("the fixture was supposed to be a first install: the namespace was there")
	}
	if len(f.anchor.ensured) != 1 {
		t.Fatalf("anchor wrote %d times, want once", len(f.anchor.ensured))
	}
	plan := f.anchor.ensured[0]
	if len(plan.OIDC) != 1 || plan.OIDC[0] != "sonarr" {
		t.Fatalf("anchor plan OIDC = %v, want the app the catalog marks: its client Secret is written by the same step", plan.OIDC)
	}
	if len(f.anchor.namespaceThere) != 1 || !f.anchor.namespaceThere[0] {
		t.Fatalf("the anchor step ran before the namespace existed (%v): a client Secret cannot be written there", f.anchor.namespaceThere)
	}
	if a, b := eventIndex(f.events, "anchor"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want the clients and the anchor before the release", f.events)
	}
}

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
