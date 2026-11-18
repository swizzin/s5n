package discover

// keep_test.go covers the keep path's pre-flight: what a source cluster has that
// stops box installing onto it, and what a source with none of it looks like.

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestBlockersNameWhatHoldsBoxsPortsAndWhatWouldRewriteIt: an ingress controller
// holding the node's 80/443, a container binding a host port, and an auto-updater
// are each named with a command, because box changes nothing on a cluster it did
// not install.
func TestBlockersNameWhatHoldsBoxsPortsAndWhatWouldRewriteIt(t *testing.T) {
	ingress := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "traefik", Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Name: "web", Port: 80}, {Name: "websecure", Port: 443}},
		},
	}
	// A ClusterIP Service on 443 is a Service's own port inside the cluster: it
	// holds none of the node's, and is not the front door.
	internal := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "panel", Namespace: "media"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Port: 443}}},
	}
	hostPort := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ingress-bare", Namespace: "media"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "ingress",
			Image: "example.com/ingress:2",
			Ports: []corev1.ContainerPort{{ContainerPort: 80, HostPort: 80}},
		}}},
	}
	watched := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sonarr", Namespace: "media",
			Annotations: map[string]string{"keel.sh/policy": "force"},
		},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sonarr", Image: "lscr.io/linuxserver/sonarr:4.0.14"}},
		}}},
	}
	updater := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "keel", Namespace: "media"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "keel", Image: "ghcr.io/keelhq/keel:0.20.0"}},
		}}},
	}
	c := NewClusterFor(fake.NewSimpleClientset(ingress, internal, hostPort, watched, updater), nil)
	blockers, err := c.Blockers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, b := range blockers {
		lines = append(lines, b.String())
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"kube-system/traefik: this LoadBalancer Service answers on 80",
		"kubectl -n kube-system get service traefik -o wide",
		"media/ingress-bare: ingress binds hostPort 80",
		`media/sonarr: keel.sh/policy is "force"`,
		"media/keel: keel is running in this namespace",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the pre-flight does not say %q:\n%s", want, joined)
		}
	}
	// One line per blocker: the Service answers on two ports and is still one
	// thing to deal with, and a ClusterIP on 443 is nothing at all.
	if got := countContaining(lines, "kube-system/traefik"); got != 1 {
		t.Errorf("traefik appears %d times, want one line", got)
	}
	if strings.Contains(joined, "media/panel") {
		t.Errorf("a ClusterIP Service on 443 was read as holding the node's port:\n%s", joined)
	}
	// The same list twice: a cluster read the same way is the same answer.
	again, err := c.Blockers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(blockers) {
		t.Errorf("%d blockers on a re-read, %d the first time", len(again), len(blockers))
	}
}

// TestBlockersOfAClusterBoxMayKeep: a source with nothing on the node's front door
// and no updater is a cluster box may install onto, and an empty list is that
// answer rather than a failure.
func TestBlockersOfAClusterBoxMayKeep(t *testing.T) {
	sonarr := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "sonarr", Namespace: "media"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sonarr", Image: "lscr.io/linuxserver/sonarr:4.0.14"}},
		}}},
	}
	qbt := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "qbittorrent", Namespace: "media"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Port: 8080}}},
	}
	c := NewClusterFor(fake.NewSimpleClientset(sonarr, qbt), nil)
	blockers, err := c.Blockers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %v, want none", blockers)
	}
}

// countContaining counts the lines naming one object.
func countContaining(lines []string, what string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, what) {
			n++
		}
	}
	return n
}
