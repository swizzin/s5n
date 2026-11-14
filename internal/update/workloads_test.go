package update

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// statefulSet is one app workload with the labels the chart writes: the
// release and instance on the object, the app key on the pod template.
func statefulSet(name, app string, generation, observed int64, ready, updated int32) *appsv1.StatefulSet {
	one := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "testbox",
			Generation: generation,
			Labels: map[string]string{
				instanceLabel: "s5n",
				nameLabel:     name,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					instanceLabel: "s5n",
					nameLabel:     name,
					appLabel:      app,
				}},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: observed,
			Replicas:           one,
			ReadyReplicas:      ready,
			UpdatedReplicas:    updated,
		},
	}
}

// TestScaleCoversEveryInstanceOfOneApp: an App's second instance is another
// StatefulSet with the same app key. Scaling the app stops both, and leaves
// every other app alone.
func TestScaleCoversEveryInstanceOfOneApp(t *testing.T) {
	cs := k8sfake.NewClientset(
		statefulSet("sonarr", "sonarr", 1, 1, 1, 1),
		statefulSet("sonarr-4k", "sonarr", 1, 1, 1, 1),
		statefulSet("radarr", "radarr", 1, 1, 1, 1),
	)
	var scaled []string
	// The fake client does not understand the scale subresource, so the two
	// verbs are served from the StatefulSet itself: this is what the real API
	// server does with them.
	cs.PrependReactor("get", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		named, _ := action.(interface{ GetName() string })
		obj, err := cs.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("statefulsets"), "testbox", named.GetName())
		if err != nil {
			return true, nil, err
		}
		set := obj.(*appsv1.StatefulSet)
		replicas := int32(1)
		if set.Spec.Replicas != nil {
			replicas = *set.Spec.Replicas
		}
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: set.Name, Namespace: set.Namespace},
			Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
		}, nil
	})
	cs.PrependReactor("update", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateAction)
		if update.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := update.GetObject().(*autoscalingv1.Scale)
		scaled = append(scaled, fmt.Sprintf("%s=%d", scale.Name, scale.Spec.Replicas))
		return true, scale, nil
	})
	w := NewWorkloadsFor("testbox", "s5n", cs)
	if err := w.Scale(context.Background(), "sonarr", 0); err != nil {
		t.Fatalf("Scale: %v", err)
	}
	want := []string{"sonarr=0", "sonarr-4k=0"}
	if !reflect.DeepEqual(scaled, want) {
		t.Fatalf("scaled = %v, want %v through the scale subresource", scaled, want)
	}

	// An app with no StatefulSet in the release (the values just enabled it)
	// is nothing to stop, not an error.
	if err := w.Scale(context.Background(), "plex", 0); err != nil {
		t.Fatalf("Scale of an app with no workload: %v", err)
	}
}

func TestGoneWaitsForEveryPod(t *testing.T) {
	cs := k8sfake.NewClientset()
	pod := func(name, app string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "testbox",
			Labels:    map[string]string{instanceLabel: "s5n", appLabel: app},
		}}
	}
	if _, err := cs.CoreV1().Pods("testbox").Create(context.Background(), pod("sonarr-0", "sonarr"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	w := NewWorkloadsFor("testbox", "s5n", cs)
	gone, err := w.Gone(context.Background(), "sonarr")
	if err != nil {
		t.Fatalf("Gone: %v", err)
	}
	if gone {
		t.Fatal("a running pod counts as gone")
	}
	if gone, err := w.Gone(context.Background(), "radarr"); err != nil || !gone {
		t.Fatalf("Gone(radarr) = %v, %v; want true, nil", gone, err)
	}
}

// containerRestarts is one container of a test pod and how many times it has
// exited since the step. The first one named is the pod's first container,
// which is what the fallback reads.
type containerRestarts struct {
	name     string
	restarts int32
}

// appPod is one pod of an app as the chart's workload leaves it: the release
// and app labels, a container per name, and the statuses the kubelet reports
// for them. The app's own container is named after the app key, as
// workloads.yaml names it.
func appPod(name, app string, containers ...containerRestarts) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "testbox",
		Labels:    map[string]string{instanceLabel: "s5n", appLabel: app},
	}}
	for _, container := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: container.name})
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:         container.name,
			RestartCount: container.restarts,
		})
	}
	return pod
}

// TestHealthIsPerGenerationAndReady: Ready is the gate's pass, and it is the
// rule the gate always had -- every StatefulSet on its current generation with
// all its replicas ready and updated.
func TestHealthIsPerGenerationAndReady(t *testing.T) {
	cases := []struct {
		name string
		sets []*appsv1.StatefulSet
		want bool
	}{
		{"all ready", []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 2, 2, 1, 1)}, true},
		{"nothing to run", nil, true},
		{"a pod not ready", []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 2, 2, 0, 1)}, false},
		{"old generation still ready", []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 2, 1, 1, 1)}, false},
		{"not rolled out", []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 2, 2, 1, 0)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := k8sfake.NewClientset()
			for _, set := range tc.sets {
				if _, err := cs.AppsV1().StatefulSets("testbox").Create(context.Background(), set, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := NewWorkloadsFor("testbox", "s5n", cs).Health(context.Background(), nil)
			if err != nil {
				t.Fatalf("Health: %v", err)
			}
			if got.Ready != tc.want {
				t.Fatalf("Ready = %v, want %v", got.Ready, tc.want)
			}
			if len(got.Exiting) != 0 {
				t.Fatalf("Exiting = %+v on a read that was asked about no app", got.Exiting)
			}
		})
	}
}

// TestHealthListsOnlyAnAppThatKeepsExiting: the third exit since the step is
// what makes it a crashloop; a first or second one is an app that restarted
// and then served its port.
func TestHealthListsOnlyAnAppThatKeepsExiting(t *testing.T) {
	cases := []struct {
		name     string
		restarts int32
		want     []Exiting
	}{
		{"one exit", 1, nil},
		{"two exits", 2, nil},
		{"three exits", 3, []Exiting{{App: "sonarr", Restarts: 3}}},
		{"eight exits", 8, []Exiting{{App: "sonarr", Restarts: 8}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := k8sfake.NewClientset(appPod("sonarr-0", "sonarr", containerRestarts{"sonarr", tc.restarts}))
			got, err := NewWorkloadsFor("testbox", "s5n", cs).Health(context.Background(), []string{"sonarr"})
			if err != nil {
				t.Fatalf("Health: %v", err)
			}
			if !reflect.DeepEqual(got.Exiting, tc.want) {
				t.Fatalf("Exiting = %+v, want %+v", got.Exiting, tc.want)
			}
			// The count itself is what a failed apply is re-read for: one exit
			// is the difference between "the image is broken" and "something
			// else failed", so Health reports it whatever the crashloop
			// threshold does.
			if got.Restarts["sonarr"] != tc.restarts {
				t.Fatalf("Restarts = %+v, want sonarr at %d", got.Restarts, tc.restarts)
			}
		})
	}
}

// TestHealthIgnoresAnAppTheStepDidNotMove: an app that kept its image cannot
// be this step's fault, whatever its restarts say.
func TestHealthIgnoresAnAppTheStepDidNotMove(t *testing.T) {
	cs := k8sfake.NewClientset(
		appPod("sonarr-0", "sonarr", containerRestarts{"sonarr", 0}),
		appPod("radarr-0", "radarr", containerRestarts{"radarr", 5}),
	)
	got, err := NewWorkloadsFor("testbox", "s5n", cs).Health(context.Background(), []string{"sonarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(got.Exiting) != 0 {
		t.Fatalf("Exiting = %+v, want nothing: only sonarr moved", got.Exiting)
	}
}

// TestHealthCountsAnAppOnceAcrossItsInstances: an Arr's second instance is
// another pod of the same app, and the count reported is the highest, because
// one instance crashlooping is the app crashlooping.
func TestHealthCountsAnAppOnceAcrossItsInstances(t *testing.T) {
	cs := k8sfake.NewClientset(
		appPod("sonarr-0", "sonarr", containerRestarts{"sonarr", 4}),
		appPod("sonarr-4k-0", "sonarr", containerRestarts{"sonarr", 7}),
	)
	got, err := NewWorkloadsFor("testbox", "s5n", cs).Health(context.Background(), []string{"sonarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	want := []Exiting{{App: "sonarr", Restarts: 7}}
	if !reflect.DeepEqual(got.Exiting, want) {
		t.Fatalf("Exiting = %+v, want %+v", got.Exiting, want)
	}
}

// TestHealthIgnoresInitContainers: the chart's init containers are its own
// wiring and permission steps, not the app. A restart there says nothing about
// the image the step moved to.
func TestHealthIgnoresInitContainers(t *testing.T) {
	pod := appPod("sonarr-0", "sonarr", containerRestarts{"sonarr", 0})
	pod.Spec.InitContainers = []corev1.Container{{Name: "permissions"}, {Name: "wiring"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "permissions", RestartCount: 9},
		{Name: "wiring", RestartCount: 9},
	}
	cs := k8sfake.NewClientset(pod)
	got, err := NewWorkloadsFor("testbox", "s5n", cs).Health(context.Background(), []string{"sonarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(got.Exiting) != 0 {
		t.Fatalf("Exiting = %+v, want nothing: init containers are the chart's", got.Exiting)
	}
}

// TestHealthFallsBackToTheFirstContainer: a chart that names the app's
// container something else still gets a count, and it is the first container
// the fallback reads -- not the highest of them, which could be a sidecar.
func TestHealthFallsBackToTheFirstContainer(t *testing.T) {
	cs := k8sfake.NewClientset(
		appPod("sonarr-0", "sonarr",
			containerRestarts{"server", 3},
			containerRestarts{"sidecar", 9},
		),
		appPod("radarr-0", "radarr",
			containerRestarts{"server", 9},
			containerRestarts{"sidecar", 1},
		),
	)
	w := NewWorkloadsFor("testbox", "s5n", cs)
	got, err := w.Health(context.Background(), []string{"sonarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if want := []Exiting{{App: "sonarr", Restarts: 3}}; !reflect.DeepEqual(got.Exiting, want) {
		t.Fatalf("Exiting = %+v, want %+v: the first container is the app's", got.Exiting, want)
	}
	// The same pod shape with the first container over the line is listed.
	got, err = w.Health(context.Background(), []string{"radarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if want := []Exiting{{App: "radarr", Restarts: 9}}; !reflect.DeepEqual(got.Exiting, want) {
		t.Fatalf("Exiting = %+v, want %+v", got.Exiting, want)
	}
}

// TestHealthSortsTheAppsItNames: two apps exiting are reported in one order,
// so the sentence and the record read the same on every run.
func TestHealthSortsTheAppsItNames(t *testing.T) {
	cs := k8sfake.NewClientset(
		appPod("sonarr-0", "sonarr", containerRestarts{"sonarr", 3}),
		appPod("radarr-0", "radarr", containerRestarts{"radarr", 5}),
	)
	got, err := NewWorkloadsFor("testbox", "s5n", cs).Health(context.Background(), []string{"sonarr", "radarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	want := []Exiting{{App: "radarr", Restarts: 5}, {App: "sonarr", Restarts: 3}}
	if !reflect.DeepEqual(got.Exiting, want) {
		t.Fatalf("Exiting = %+v, want %+v", got.Exiting, want)
	}
}
