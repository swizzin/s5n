package update

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// crashloopRestarts is how many times a moved app's container has to exit
// since the step before box calls it a crashloop rather than an app that is
// still starting: docs/box.md, "An app that crashloops on its new image is
// rolled back by box" -- keeps exiting means the container has exited three
// times since the step. Three leaves an app that restarts once or twice and
// then serves its port alone, and still names a broken image long before the
// startup budget, which the kubelet would spend restarting it.
const crashloopRestarts int32 = 3

// Health is one reading of the release for the health gate: whether it is
// ready, and which of the apps a step moved keep exiting.
type Health struct {
	// Ready is every StatefulSet of the release rolled out on its current
	// generation with all its replicas ready and updated. It is the gate's
	// pass, and the only thing it asks about the release as a whole.
	Ready bool
	// Exiting is the moved apps that keep exiting, sorted by app. They are
	// the ones box cannot wait out: waiting only adds restarts.
	Exiting []Exiting
}

// Exiting is one moved app whose container keeps exiting, with the count that
// says so.
type Exiting struct {
	App      string
	Restarts int32
}

// Health reads the release once: whether every StatefulSet of it is ready, and
// whether any of the apps this step moved is exiting rather than slow to start.
//
// Only the moved apps are asked to keep exiting. An app the step did not touch
// kept its image, so its restarts are history and not this step's fault, and a
// step that moved nothing lists no pods at all.
func (w *Workloads) Health(ctx context.Context, moving []string) (Health, error) {
	sets, err := w.statefulSets(ctx)
	if err != nil {
		return Health{}, err
	}
	health := Health{Ready: rolledOut(sets)}
	if len(moving) == 0 {
		return health, nil
	}
	pods, err := w.pods(ctx)
	if err != nil {
		return Health{}, err
	}
	moved := make(map[string]bool, len(moving))
	for _, app := range moving {
		moved[app] = true
	}
	// The highest count across the app's pods: one instance crashlooping is
	// the app crashlooping, whatever its other instances are doing.
	restarts := make(map[string]int32, len(moving))
	for i := range pods {
		app := pods[i].Labels[appLabel]
		if !moved[app] {
			continue
		}
		if n := appRestarts(&pods[i]); n > restarts[app] {
			restarts[app] = n
		}
	}
	for app, n := range restarts {
		if n >= crashloopRestarts {
			health.Exiting = append(health.Exiting, Exiting{App: app, Restarts: n})
		}
	}
	sort.Slice(health.Exiting, func(i, j int) bool { return health.Exiting[i].App < health.Exiting[j].App })
	return health, nil
}

// rolledOut is the gate's pass over the release's workloads: every StatefulSet
// has rolled out its current generation with all its replicas ready. A release
// with no StatefulSets at all is ready, because there is nothing that can be
// unready.
func rolledOut(sets []appsv1.StatefulSet) bool {
	for i := range sets {
		set := &sets[i]
		if set.Status.ObservedGeneration < set.Generation {
			return false
		}
		want := int32(1)
		if set.Spec.Replicas != nil {
			want = *set.Spec.Replicas
		}
		if set.Status.ReadyReplicas != want || set.Status.UpdatedReplicas != want {
			return false
		}
	}
	return true
}

// appRestarts is how many times one pod's app container has exited. The chart
// names that container after the app key (workloads.yaml), so that is the one
// the count is read from; a pod whose container is named something else falls
// back to its first, so a chart that renames it still gets a count rather than
// silence. Init containers are the chart's own (the wiring and permission
// steps) and are not the app, so their restarts are not counted.
func appRestarts(pod *corev1.Pod) int32 {
	if len(pod.Spec.Containers) == 0 {
		return 0
	}
	app := pod.Spec.Containers[0].Name
	for _, container := range pod.Spec.Containers {
		if container.Name == pod.Labels[appLabel] {
			app = container.Name
			break
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == app {
			return status.RestartCount
		}
	}
	return 0
}

func (w *Workloads) statefulSets(ctx context.Context) ([]appsv1.StatefulSet, error) {
	selector := labels.SelectorFromSet(labels.Set{instanceLabel: w.release}).String()
	list, err := w.cs.AppsV1().StatefulSets(w.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list StatefulSets in %s: %w", w.namespace, err)
	}
	return list.Items, nil
}

// pods is every pod of the release, instances included.
func (w *Workloads) pods(ctx context.Context) ([]corev1.Pod, error) {
	selector := labels.SelectorFromSet(labels.Set{instanceLabel: w.release}).String()
	list, err := w.cs.CoreV1().Pods(w.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", w.namespace, err)
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
