package kube

import (
	"context"
	"fmt"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The verbs box asks a SelfSubjectAccessReview about.
const (
	VerbCreate = "create"
	VerbPatch  = "patch"
	// VerbApply is how box writes: server-side apply.
	//
	// It is deliberately not an RBAC verb. The API server authorizes a
	// server-side apply as create when the object has to be made and as patch
	// when it already exists, so asking only for create admits an identity
	// that cannot patch and lets the first re-run fail after the plan was
	// confirmed. Every box write is an apply: a second `box bootstrap`
	// patches the Gateway, the Certificate and the HTTPRoute the first one
	// created, and `box apply` patches the release's objects.
	VerbApply = "apply"
)

// Access is one permission box needs for what it is about to write: may this
// kubeconfig's identity do Verb on Group/Resource in Namespace (empty means
// cluster-scoped, and Group empty means the core group)?
type Access struct {
	Verb      string
	Group     string
	Resource  string
	Namespace string
	// What names the permission in box's refusal, e.g. "cert-manager
	// Certificates in s5n-gateway".
	What string
}

// Checks is what the API server is actually asked for this access: one
// question per verb the write needs. Apply is create and patch; every other
// verb is itself.
func (a Access) Checks() []Access {
	if a.Verb != VerbApply {
		return []Access{a}
	}
	out := make([]Access, 0, 2)
	for _, verb := range []string{VerbCreate, VerbPatch} {
		c := a
		c.Verb = verb
		out = append(out, c)
	}
	return out
}

// Reviewer answers a SelfSubjectAccessReview as the kubeconfig's identity.
// The platform's client, `box apply`'s storage client and bootstrap's prober
// all answer it, so the question and its verbs are written once.
type Reviewer interface {
	Allowed(ctx context.Context, a Access) (bool, error)
}

// SSAR is the Reviewer over a client-go clientset, so the review body is built
// in one place.
func SSAR(cs kubernetes.Interface) Reviewer { return clientset{cs} }

type clientset struct{ cs kubernetes.Interface }

func (c clientset) Allowed(ctx context.Context, a Access) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:      a.Verb,
				Group:     a.Group,
				Resource:  a.Resource,
				Namespace: a.Namespace,
			},
		},
	}
	out, err := c.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}

// Allowed reports whether every question this access asks is permitted. A
// denial is an answer, not an error: the caller writes the sentence.
func Allowed(ctx context.Context, r Reviewer, a Access) (bool, error) {
	for _, c := range a.Checks() {
		ok, err := r.Allowed(ctx, c)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// Denied asks every access and returns one "verb what" phrase per missing
// permission, in the order asked. It is the one check loop: callers share the
// questions and the verbs and write their own refusal, because a probe's
// sentence ("this identity cannot …") and a write's ("box needs cluster
// admin …") are different things to different readers.
func Denied(ctx context.Context, r Reviewer, access []Access) ([]string, error) {
	var denied []string
	for _, a := range access {
		for _, c := range a.Checks() {
			ok, err := r.Allowed(ctx, c)
			if err != nil {
				return nil, err
			}
			if !ok {
				denied = append(denied, c.Verb+" "+c.What)
			}
		}
	}
	return denied, nil
}

// Preflight is box's write gate: it refuses unless the kubeconfig may do
// everything in access, and it is the last read before the confirm and the
// first write.
func Preflight(ctx context.Context, r Reviewer, access []Access) error {
	denied, err := Denied(ctx, r, access)
	if err != nil {
		return fmt.Errorf("pre-flight: SelfSubjectAccessReview: %w", err)
	}
	if len(denied) == 0 {
		return nil
	}
	return fmt.Errorf("box needs cluster admin, and this kubeconfig cannot %s: run `box bootstrap` as root, or with a cluster-admin kubeconfig",
		strings.Join(denied, ", "))
}
