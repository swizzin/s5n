package update

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
)

const (
	// instanceLabel is the chart's release label (s5n.labels in _helpers.tpl):
	// every object of one seedbox carries it.
	instanceLabel = "app.kubernetes.io/instance"
	// nameLabel is the chart's object name label. On a StatefulSet it is the
	// instance (`sonarr`, `radarr-4k`), which is why update identifies the app
	// from the pod template below rather than from this.
	nameLabel = "app.kubernetes.io/name"
	// appLabel is the app key on the pod template (workloads.yaml). It is the
	// same for every instance of one app, so one scale covers an Arr's HD and
	// 4K instances together.
	appLabel = "s5n.io/app"
)

// Workloads is Cluster over client-go: the StatefulSets of one release in one
// namespace. It is built after the grant names the namespace, which is why it
// takes it rather than a kubeconfig alone.
type Workloads struct {
	namespace string
	release   string
	cs        kubernetes.Interface
}

// NewWorkloads builds the client for one seedbox's release.
func NewWorkloads(kubeconfig, namespace, releaseName string) (*Workloads, error) {
	if namespace == "" {
		return nil, fmt.Errorf("no namespace: the grant names the release namespace")
	}
	cfg, err := kube.Config(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("workload client for %s: %w", namespace, err)
	}
	return NewWorkloadsFor(namespace, releaseName, cs), nil
}

// NewWorkloadsFor is NewWorkloads over an existing clientset, which is what the
// tests drive.
func NewWorkloadsFor(namespace, releaseName string, cs kubernetes.Interface) *Workloads {
	return &Workloads{namespace: namespace, release: releaseName, cs: cs}
}

// Scale sets one app's replicas through the scale subresource, on every
// StatefulSet of the release whose pod template names that app. An app with no
// StatefulSet here (the values just enabled it) is not an error: there is
// nothing of it to stop, and the apply that follows creates it.
func (w *Workloads) Scale(ctx context.Context, app string, replicas int32) error {
	sets, err := w.statefulSets(ctx)
	if err != nil {
		return err
	}
	for i := range sets {
		if appOf(&sets[i]) != app {
			continue
		}
		scale, err := w.cs.AppsV1().StatefulSets(w.namespace).GetScale(ctx, sets[i].Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read scale of %s: %w", sets[i].Name, err)
		}
		scale.Spec.Replicas = replicas
		if _, err := w.cs.AppsV1().StatefulSets(w.namespace).UpdateScale(ctx, sets[i].Name, scale, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("scale %s to %d: %w", sets[i].Name, replicas, err)
		}
	}
	return nil
}

// Gone reports whether no pod of the app is left. The pods carry the release
// and app labels (the chart's s5n.labels plus s5n.io/app), so this is one list
// per app, instances included.
func (w *Workloads) Gone(ctx context.Context, app string) (bool, error) {
	selector := labels.SelectorFromSet(labels.Set{
		instanceLabel: w.release,
		appLabel:      app,
	}).String()
	pods, err := w.cs.CoreV1().Pods(w.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, fmt.Errorf("list pods of %s: %w", app, err)
	}
	return len(pods.Items) == 0, nil
}

// Healthy reports whether every StatefulSet of the release has rolled out its
// current generation and has all its replicas ready. A release with no
// StatefulSets at all is healthy: there is nothing that can be unready.
func (w *Workloads) Healthy(ctx context.Context) (bool, error) {
	sets, err := w.statefulSets(ctx)
	if err != nil {
		return false, err
	}
	for i := range sets {
		set := &sets[i]
		if set.Status.ObservedGeneration < set.Generation {
			return false, nil
		}
		want := int32(1)
		if set.Spec.Replicas != nil {
			want = *set.Spec.Replicas
		}
		if set.Status.ReadyReplicas != want || set.Status.UpdatedReplicas != want {
			return false, nil
		}
	}
	return true, nil
}

func (w *Workloads) statefulSets(ctx context.Context) ([]appsv1.StatefulSet, error) {
	selector := labels.SelectorFromSet(labels.Set{instanceLabel: w.release}).String()
	list, err := w.cs.AppsV1().StatefulSets(w.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list StatefulSets in %s: %w", w.namespace, err)
	}
	return list.Items, nil
}

// appOf is the app key one StatefulSet belongs to. It is on the pod template;
// a set without it falls back to its own name label, which is the app for the
// default instance and only wrong for a second Arr instance.
func appOf(set *appsv1.StatefulSet) string {
	if app := set.Spec.Template.Labels[appLabel]; app != "" {
		return app
	}
	return set.Labels[nameLabel]
}
