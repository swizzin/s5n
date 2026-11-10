package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeReviewer answers the review with a scripted verdict per verb and records
// what it was asked. A write is anything that is not a review: this fake has no
// such method, so a test that reaches the write has to say so itself.
type fakeReviewer struct {
	allow map[string]bool // verb -> allowed; a missing verb is allowed
	err   error
	asked []Access
}

func (f *fakeReviewer) Allowed(_ context.Context, a Access) (bool, error) {
	f.asked = append(f.asked, a)
	if f.err != nil {
		return false, f.err
	}
	if allowed, ok := f.allow[a.Verb]; ok {
		return allowed, nil
	}
	return true, nil
}

// TestAccessChecksAsksTheVerbsTheWriteNeeds pins the one thing the preflights
// used to get wrong: a server-side apply is authorized as create on a new
// object and patch on an existing one, so "apply" is two questions.
func TestAccessChecksAsksTheVerbsTheWriteNeeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Access
		want []string
	}{
		{
			name: "a plain create is one question",
			in:   Access{Verb: VerbCreate, Resource: "namespaces"},
			want: []string{"create"},
		},
		{
			name: "an apply is create and patch",
			in:   Access{Verb: VerbApply, Resource: "gateways", Namespace: "s5n-gateway"},
			want: []string{"create", "patch"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := tc.in.Checks()
			if len(checks) != len(tc.want) {
				t.Fatalf("%s: Checks() = %d questions, want %d", tc.in.Verb, len(checks), len(tc.want))
			}
			for i, verb := range tc.want {
				if checks[i].Verb != verb {
					t.Fatalf("%s: question %d asks %q, want %q", tc.in.Verb, i, checks[i].Verb, verb)
				}
				if checks[i].Resource != tc.in.Resource || checks[i].Namespace != tc.in.Namespace {
					t.Fatalf("%s: question %d changed the object: %+v", tc.in.Verb, i, checks[i])
				}
			}
		})
	}
}

// TestPreflightRefusesWhenPatchIsDenied is the reviewer's finding as a test: an
// identity that may create but not patch the object box is about to server-side
// apply has to be refused before the write, not after the plan was confirmed.
func TestPreflightRefusesWhenPatchIsDenied(t *testing.T) {
	r := &fakeReviewer{allow: map[string]bool{VerbCreate: true, VerbPatch: false}}
	err := Preflight(context.Background(), r, []Access{
		{Verb: VerbApply, Group: "gateway.networking.k8s.io", Resource: "gateways", Namespace: "s5n-gateway", What: "gateways"},
	})
	if err == nil {
		t.Fatal("an identity that cannot patch the Gateway was allowed to apply it")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot patch gateways") {
		t.Fatalf("refusal does not name the denied verb and object: %v", err)
	}
	if !strings.Contains(msg, "box needs cluster admin") || !strings.Contains(msg, "box bootstrap") {
		t.Fatalf("refusal lost its operator-facing wording: %v", err)
	}
	if len(r.asked) != 2 || r.asked[0].Verb != VerbCreate || r.asked[1].Verb != VerbPatch {
		t.Fatalf("questions asked = %+v, want create then patch", r.asked)
	}
}

// TestPreflightAllowsWhenBothVerbsAreAllowed is the other half: a cluster-admin
// identity is not refused by the stricter question.
func TestPreflightAllowsWhenBothVerbsAreAllowed(t *testing.T) {
	r := &fakeReviewer{}
	if err := Preflight(context.Background(), r, []Access{
		{Verb: VerbApply, Resource: "namespaces", What: "namespaces"},
	}); err != nil {
		t.Fatalf("Preflight = %v, want nil", err)
	}
	if len(r.asked) != 2 {
		t.Fatalf("questions asked = %d, want create and patch", len(r.asked))
	}
}

// TestAllowedRefusesAPartlyAnsweredAccess covers the callers that want a
// yes/no rather than a sentence: `box apply` asks whether it may create the
// seedbox namespace, and a create-only identity has not answered.
func TestAllowedRefusesAPartlyAnsweredAccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb map[string]bool
		want bool
	}{
		{name: "create only is not an apply", verb: map[string]bool{VerbPatch: false}, want: false},
		{name: "both verbs allow it", verb: map[string]bool{}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Allowed(context.Background(), &fakeReviewer{allow: tc.verb}, Access{Verb: VerbApply, Resource: "namespaces"})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeniedNamesEveryMissingRight keeps the probe's sentence honest: it is
// built from the same loop, so a patch-only denial is reported as patch.
func TestDeniedNamesEveryMissingRight(t *testing.T) {
	r := &fakeReviewer{allow: map[string]bool{VerbCreate: false, VerbPatch: true}}
	got, err := Denied(context.Background(), r, []Access{
		{Verb: VerbApply, Resource: "namespaces", What: "namespaces"},
		{Verb: VerbApply, Group: "rbac.authorization.k8s.io", Resource: "clusterroles", What: "cluster roles"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "create namespaces, create cluster roles"
	if strings.Join(got, ", ") != want {
		t.Fatalf("Denied = %q, want %q", strings.Join(got, ", "), want)
	}
}

// TestPreflightReportsAReviewError keeps a failed review (an RBAC-blind API
// server, a dead connection) distinct from a denial: box cannot tell the
// operator "you are not allowed" when it did not get an answer.
func TestPreflightReportsAReviewError(t *testing.T) {
	r := &fakeReviewer{err: errors.New("connection refused")}
	err := Preflight(context.Background(), r, []Access{{Verb: VerbCreate, Resource: "namespaces"}})
	if err == nil || !strings.Contains(err.Error(), "SelfSubjectAccessReview") {
		t.Fatalf("err = %v, want a review failure", err)
	}
}
