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

	"github.com/swizzin/s5n/internal/snapshot"
)

// statefulSet has the chart's labels: release and instance, app on the template.
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

func TestScaleCoversEveryInstanceOfOneApp(t *testing.T) {
	cs := k8sfake.NewClientset(
		statefulSet("sonarr", "sonarr", 1, 1, 1, 1),
		statefulSet("sonarr-4k", "sonarr", 1, 1, 1, 1),
		statefulSet("radarr", "radarr", 1, 1, 1, 1),
	)
	var scaled []string
	// The fake client serves scale from the StatefulSet, as the API server does.
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

	// An app with no StatefulSet is nothing to stop.
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

// containerRestarts is one container and its exits since the step.
type containerRestarts struct {
	name     string
	restarts int32
}

// appPod is one pod with the chart's labels and a container per name.
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
			// One exit distinguishes a broken image from another failure, so the
			// count is reported whatever the crashloop threshold.
			if got.Restarts["sonarr"] != tc.restarts {
				t.Fatalf("Restarts = %+v, want sonarr at %d", got.Restarts, tc.restarts)
			}
		})
	}
}

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
	// The same pod shape with the first container over the line.
	got, err = w.Health(context.Background(), []string{"radarr"})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if want := []Exiting{{App: "radarr", Restarts: 9}}; !reflect.DeepEqual(got.Exiting, want) {
		t.Fatalf("Exiting = %+v, want %+v", got.Exiting, want)
	}
}

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

// runningSet is an app's StatefulSet with its image as the values left it.
func runningSet(name, app, image string) *appsv1.StatefulSet {
	set := statefulSet(name, app, 1, 1, 1, 1)
	set.Spec.Template.Spec.Containers = []corev1.Container{{Name: app, Image: image}}
	return set
}

// runningPod carries the kubelet's imageID, with its repository in front.
func runningPod(name, instance, app, imageID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "testbox",
			Labels: map[string]string{
				instanceLabel: "s5n",
				nameLabel:     instance,
				appLabel:      app,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: app, Image: "ignored"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: app, ImageID: "registry.example.com/seedbox/" + app + "@" + imageID},
		}},
	}
}

// readyPod is appPod with the Ready condition; a closed port has none.
func readyPod(name, app string, ready bool, restarts int32) *corev1.Pod {
	pod := appPod(name, app, containerRestarts{name: app, restarts: restarts})
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return pod
}

func TestReadinessIsEveryAppNotOnlyTheMovedOnes(t *testing.T) {
	cs := k8sfake.NewClientset(
		statefulSet("sonarr", "sonarr", 1, 1, 1, 1),
		statefulSet("radarr", "radarr", 1, 1, 1, 1),
		readyPod("sonarr-0", "sonarr", true, 0),
		readyPod("radarr-0", "radarr", false, 4),
	)
	w, err := NewWorkloadsFor("testbox", "s5n", cs).Readiness(context.Background())
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	want := map[string]AppHealth{
		"sonarr": {Ready: true},
		"radarr": {Ready: false, Reason: "restarting, 4 restarts"},
	}
	if !reflect.DeepEqual(w, want) {
		t.Fatalf("Readiness = %+v, want %+v", w, want)
	}
}

func TestReadinessSaysWhyAnAppIsNotReady(t *testing.T) {
	for _, c := range []struct {
		name string
		sets []*appsv1.StatefulSet
		pods []*corev1.Pod
		want AppHealth
	}{
		{
			name: "ready",
			sets: []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 1, 1, 1, 1)},
			pods: []*corev1.Pod{readyPod("sonarr-0", "sonarr", true, 0)},
			want: AppHealth{Ready: true},
		},
		{
			name: "running but not ready",
			sets: []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 1, 1, 1, 1)},
			pods: []*corev1.Pod{readyPod("sonarr-0", "sonarr", false, 0)},
			want: AppHealth{Ready: false, Reason: "not ready"},
		},
		{
			name: "restarting",
			sets: []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 1, 1, 1, 1)},
			pods: []*corev1.Pod{readyPod("sonarr-0", "sonarr", false, 7)},
			want: AppHealth{Ready: false, Reason: "restarting, 7 restarts"},
		},
		{
			name: "one restart is named in the singular",
			sets: []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 1, 1, 0, 1)},
			pods: []*corev1.Pod{readyPod("sonarr-0", "sonarr", false, 1)},
			want: AppHealth{Ready: false, Reason: "restarting, 1 restart"},
		},
		{
			name: "no pod yet",
			sets: []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 1, 1, 0, 1)},
			want: AppHealth{Ready: false, Reason: "not ready"},
		},
		{
			name: "a rollout that has not finished is not up",
			sets: []*appsv1.StatefulSet{statefulSet("sonarr", "sonarr", 2, 1, 1, 0)},
			pods: []*corev1.Pod{readyPod("sonarr-0", "sonarr", true, 0)},
			want: AppHealth{Ready: false, Reason: "not ready"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(c.sets)+len(c.pods))
			for _, s := range c.sets {
				objs = append(objs, s)
			}
			for _, p := range c.pods {
				objs = append(objs, p)
			}
			got, err := NewWorkloadsFor("testbox", "s5n", k8sfake.NewClientset(objs...)).Readiness(context.Background())
			if err != nil {
				t.Fatalf("Readiness: %v", err)
			}
			if !reflect.DeepEqual(got["sonarr"], c.want) {
				t.Fatalf("sonarr = %+v, want %+v", got["sonarr"], c.want)
			}
		})
	}
}

func TestReadinessKeepsASecondInstanceWithItsApp(t *testing.T) {
	cs := k8sfake.NewClientset(
		statefulSet("radarr", "radarr", 1, 1, 1, 1),
		statefulSet("radarr-4k", "radarr", 1, 1, 1, 1),
		readyPod("radarr-0", "radarr", true, 0),
		readyPod("radarr-4k-0", "radarr", false, 2),
	)
	got, err := NewWorkloadsFor("testbox", "s5n", cs).Readiness(context.Background())
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if h := got["radarr"]; h.Ready || h.Reason != "restarting, 2 restarts" {
		t.Fatalf("radarr = %+v, want not ready across both instances", h)
	}
}

func TestReadinessAcceptsAnAppScaledToZero(t *testing.T) {
	set := statefulSet("sonarr", "sonarr", 1, 1, 0, 0)
	zero := int32(0)
	set.Spec.Replicas = &zero
	got, err := NewWorkloadsFor("testbox", "s5n", k8sfake.NewClientset(set)).Readiness(context.Background())
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if h := got["sonarr"]; !h.Ready {
		t.Fatalf("a stopped app reads as %+v, want ready", h)
	}
}

// TestRunningReadsEachInstanceImageAndPodDigest pins the other reading beside it:
// Readiness and Running answer different questions and neither may take the
// other's shape.
func TestReadinessIsEmptyOnAReleaseWithNoWorkloads(t *testing.T) {
	got, err := NewWorkloadsFor("testbox", "s5n", k8sfake.NewClientset()).Readiness(context.Background())
	if err != nil {
		t.Fatalf("Readiness: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Readiness = %+v, want nothing", got)
	}
}

func TestRunningReadsEachInstanceImageAndPodDigest(t *testing.T) {
	cs := k8sfake.NewClientset(
		runningSet("sonarr", "sonarr", "registry.example.com/seedbox/sonarr:4.9.0"),
		runningSet("sonarr-4k", "sonarr", "registry.example.com/seedbox/sonarr:4.9.0"),
		runningSet("radarr", "radarr", "registry.example.com/seedbox/radarr:6.1.0@"+digestA),
		runningPod("sonarr-0", "sonarr", "sonarr", digestB),
		runningPod("sonarr-4k-0", "sonarr-4k", "sonarr", digestB),
		runningPod("radarr-0", "radarr", "radarr", digestA),
		// The wiring Job carries the release label and no app key, and a pod
		// of an app whose StatefulSet is gone is not one this release runs.
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "s5n-wiring-x", Namespace: "testbox",
			Labels: map[string]string{instanceLabel: "s5n"},
		}},
	)
	got, err := NewWorkloadsFor("testbox", "s5n", cs).Running(context.Background())
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	sonarr := got["sonarr"]
	wantSonarr := parseImage("registry.example.com/seedbox/sonarr:4.9.0")
	for instance, want := range map[string]snapshot.ImageRef{"sonarr": wantSonarr, "sonarr-4k": wantSonarr} {
		if sonarr.Images[instance] != want {
			t.Fatalf("sonarr.Images[%s] = %+v, want %+v", instance, sonarr.Images[instance], want)
		}
		if ds := sonarr.Digests[instance]; !reflect.DeepEqual(ds, []string{digestB}) {
			t.Fatalf("sonarr.Digests[%s] = %v, want %s", instance, ds, digestB)
		}
	}
	radarr := got["radarr"]
	if radarr.Images["radarr"].Digest != digestA || radarr.Images["radarr"].Tag != "6.1.0" {
		t.Fatalf("radarr image = %+v, want the pinned tag and digest", radarr.Images["radarr"])
	}
	if len(got) != 2 {
		t.Fatalf("Running reported %d apps, want sonarr and radarr: %+v", len(got), got)
	}
}

// TestParseImageSplitsARegistryReference covers every shape a chart's variants
// and a kubelet's imageID put in front of box: the registry's port is a colon
// before the tag's, so the tag is the colon after the last slash.
func TestParseImageSplitsARegistryReference(t *testing.T) {
	cases := []struct {
		ref  string
		want snapshot.ImageRef
	}{
		{"registry.example.com/seedbox/sonarr:4.9.0", snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.0"}},
		{"registry.example.com:5000/seedbox/sonarr:4.9.0", snapshot.ImageRef{Repository: "registry.example.com:5000/seedbox/sonarr", Tag: "4.9.0"}},
		{"registry.example.com/seedbox/sonarr:4.9.0@" + digestA, snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Tag: "4.9.0", Digest: digestA}},
		{"registry.example.com/seedbox/sonarr@" + digestA, snapshot.ImageRef{Repository: "registry.example.com/seedbox/sonarr", Digest: digestA}},
		{"registry.example.com:5000/sonarr", snapshot.ImageRef{Repository: "registry.example.com:5000/sonarr"}},
		{"sonarr", snapshot.ImageRef{Repository: "sonarr"}},
		{"", snapshot.ImageRef{}},
	}
	for _, tc := range cases {
		if got := parseImage(tc.ref); got != tc.want {
			t.Fatalf("parseImage(%q) = %+v, want %+v", tc.ref, got, tc.want)
		}
	}
}

// TestPodDigestAcceptsARuntimeScheme: an older runtime prefixes the imageID with
// docker-pullable://; the digest is still the part after the @.
func TestPodDigestAcceptsARuntimeScheme(t *testing.T) {
	pod := runningPod("sonarr-0", "sonarr", "sonarr", digestC)
	pod.Status.ContainerStatuses[0].ImageID = "docker-pullable://registry.example.com/seedbox/sonarr@" + digestC
	if got := podDigest(pod, "sonarr"); got != digestC {
		t.Fatalf("podDigest = %q, want %q", got, digestC)
	}
	// A container that has not started reports no imageID at all.
	pod.Status.ContainerStatuses[0].ImageID = ""
	if got := podDigest(pod, "sonarr"); got != "" {
		t.Fatalf("podDigest on a container that never started = %q, want empty", got)
	}
}
