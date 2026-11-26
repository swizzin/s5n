package platform

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestPodCIDRsAreTheClustersOwnNetwork: the value an app that trusts a proxy's
// forwarded address needs is the pod network, read from the Nodes that own it.
// A node naming none is skipped, and the same range named twice is one entry.
func TestPodCIDRsAreTheClustersOwnNetwork(t *testing.T) {
	k := &Kubernetes{cs: fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{PodCIDR: "10.244.1.0/24"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: corev1.NodeSpec{PodCIDR: "10.244.0.0/24"}},
		// A node the cluster has not allocated a range to yet.
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: corev1.NodeSpec{PodCIDR: "10.244.1.0/24"}},
	)}
	got, err := k.podCIDRs(context.Background())
	if err != nil {
		t.Fatalf("pod CIDRs: %v", err)
	}
	want := []string{"10.244.0.0/24", "10.244.1.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pod CIDRs = %v, want %v (sorted, one per range)", got, want)
	}
}

// TestNoPodCIDRMeansNoGuess: a cluster that names none is an empty answer, not
// a made-up network. The caller leaves the app's setting alone.
func TestNoPodCIDRMeansNoGuess(t *testing.T) {
	k := &Kubernetes{cs: fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
	)}
	got, err := k.podCIDRs(context.Background())
	if err != nil {
		t.Fatalf("pod CIDRs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("pod CIDRs = %v, want none", got)
	}
}
