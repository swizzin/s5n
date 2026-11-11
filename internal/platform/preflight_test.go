package platform

import (
	"context"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/kube"
)

// verbCluster refuses one SSAR verb while allowing the rest. It is what the
// platform fake cannot express, and the whole point of the checks below: box
// writes by server-side apply, which the API server authorizes as create on a
// new object and patch on an existing one, so "create is allowed" is not an
// answer to "may box apply this".
type verbCluster struct {
	*fakeCluster
	denyVerb string
	verbs    []string
}

func (c *verbCluster) Allowed(ctx context.Context, a Access) (bool, error) {
	c.verbs = append(c.verbs, a.Verb)
	if a.Verb == c.denyVerb {
		return false, nil
	}
	return c.fakeCluster.Allowed(ctx, a)
}

// TestApplyRefusesAnIdentityThatCannotPatch is the reviewer's finding, end to
// end: an identity that may create every object but patch none of them used to
// pass the preflight, and the second run of `box bootstrap` then failed inside
// the first write. It has to be refused before the confirm and before any
// write, with the verb it is missing named.
func TestApplyRefusesAnIdentityThatCannotPatch(t *testing.T) {
	f := newFixture()
	cl := &verbCluster{fakeCluster: f.cl, denyVerb: kube.VerbPatch}
	f.d.Open = func() (Cluster, error) {
		f.opened++
		return cl, nil
	}

	_, err := f.d.Apply(context.Background(), allInstall)
	if err == nil {
		t.Fatal("an identity that cannot patch was allowed to apply")
	}
	if !strings.Contains(err.Error(), "cannot patch") {
		t.Fatalf("refusal does not name the missing verb: %v", err)
	}
	if !strings.Contains(err.Error(), "gateways") {
		t.Fatalf("refusal does not name an object box would apply: %v", err)
	}
	if f.confirm.asked != 0 {
		t.Fatal("box asked before it knew it could do the work")
	}
	if len(f.rel.installed) != 0 || len(f.cl.writes) != 0 {
		t.Fatalf("a refused preflight wrote: %d releases, %d applies", len(f.rel.installed), len(f.cl.writes))
	}
	if got := f.tr.String(); got != "" {
		t.Fatalf("a refused preflight did more than ask: %s", got)
	}
	// Create was asked and allowed, then patch was asked and refused: the
	// check covers both halves of an apply rather than only the first.
	if len(cl.verbs) < 2 || cl.verbs[0] != kube.VerbCreate || cl.verbs[1] != kube.VerbPatch {
		t.Fatalf("SSAR verbs = %v, want create then patch", cl.verbs)
	}
}

// TestPreflightAccessAsksWhatApplyNeeds keeps the list itself honest: every
// entry is an apply, because every object in the plan is written with
// server-side apply. A new entry written as "create" would be the same bug
// again, one object at a time.
func TestPreflightAccessAsksWhatApplyNeeds(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	access := preflightAccess(plan)
	if len(access) == 0 {
		t.Fatal("the platform plans writes but asks for no permissions")
	}
	for _, a := range access {
		if a.Verb != kube.VerbApply {
			t.Errorf("%s asks for %q; box applies it, so the review must ask for create and patch", a.What, a.Verb)
		}
		if a.What == "" {
			t.Errorf("%s has no operator-facing name", a.Resource)
		}
	}
}

// TestCertificateAccessAsksWhatApplyNeeds is the per-host TLS write: on every
// run after the first, the Certificate, the HTTPRoute and this host's Gateway
// listeners already exist, so the write the API server authorizes is a patch.
func TestCertificateAccessAsksWhatApplyNeeds(t *testing.T) {
	access := certificateAccess()
	if len(access) == 0 {
		t.Fatal("the TLS flow writes but asks for no permissions")
	}
	for _, a := range access {
		if a.Verb != kube.VerbApply {
			t.Errorf("%s asks for %q; box applies it, so the review must ask for create and patch", a.What, a.Verb)
		}
	}
}

// verbCertCluster refuses one SSAR verb, so the TLS flow's gate can be tested
// against an identity that can create but not patch.
type verbCertCluster struct {
	*fakeCertCluster
	denyVerb string
}

func (c *verbCertCluster) Allowed(ctx context.Context, a Access) (bool, error) {
	if a.Verb == c.denyVerb {
		return false, nil
	}
	return c.fakeCertCluster.Allowed(ctx, a)
}

// TestCertificateApplyRefusesAnIdentityThatCannotPatch: the per-host write is
// the one that failed first in the field, because it patches the platform's
// existing Gateway.
func TestCertificateApplyRefusesAnIdentityThatCannotPatch(t *testing.T) {
	cl := &verbCertCluster{fakeCertCluster: &fakeCertCluster{ready: true, serves: true}, denyVerb: kube.VerbPatch}
	d := &CertificateDecision{
		Host:   "box.example.test",
		Issuer: LetsEncryptIssuer,
		Open:   func() (CertificateCluster, error) { return cl, nil },
	}
	if _, err := d.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot patch") {
		t.Fatalf("want a refusal naming the missing verb, got %v", err)
	}
	if len(cl.writes) != 0 {
		t.Fatalf("a refused preflight wrote %d documents", len(cl.writes))
	}
}
