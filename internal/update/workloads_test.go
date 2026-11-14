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

func TestHealthyIsPerGenerationAndReady(t *testing.T) {
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
			got, err := NewWorkloadsFor("testbox", "s5n", cs).Healthy(context.Background())
			if err != nil {
				t.Fatalf("Healthy: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Healthy = %v, want %v", got, tc.want)
			}
		})
	}
}
