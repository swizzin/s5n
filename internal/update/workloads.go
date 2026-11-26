package update

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/snapshot"
)

const (
	// instanceLabel is the chart's release label (s5n.labels in _helpers.tpl).
	instanceLabel = "app.kubernetes.io/instance"
	// nameLabel is the chart's object name label: the instance on a StatefulSet,
	// which is why the app comes from the pod template instead.
	nameLabel = "app.kubernetes.io/name"
	// appLabel is the app key on the pod template, the same for every instance.
	appLabel = "s5n.io/app"
)

// Workloads is Cluster over client-go: one release's StatefulSets in a
// namespace, named after the grant.
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

// Scale sets one app's replicas on every StatefulSet whose pod template names
// it. An app with none is not an error: the apply that follows creates it.
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

// Gone reports whether no pod of the app is left, instances included.
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

// crashloopRestarts is when a moved app counts as crashlooping rather than
// starting (docs/box.md, "An app that crashloops on its new image is rolled back
// by box"): three exits leaves one that restarts once or twice alone, and names
// a broken image long before the startup budget would.
const crashloopRestarts int32 = 3

// Health is one health-gate reading: whether the release is ready, and which
// moved apps keep exiting.
type Health struct {
	// Ready is every StatefulSet rolled out with all replicas ready.
	Ready bool
	// Exiting is the moved apps that keep exiting; waiting only adds restarts.
	Exiting []Exiting
	// Restarts is each moved app's highest restart count. box reads it once after
	// a failed apply: an app that restarted at all deserves the health gate.
	Restarts map[string]int32
}

// Exiting is one moved app whose container keeps exiting, with the count that
// says so.
type Exiting struct {
	App      string
	Restarts int32
}

// Health reads the release once: every StatefulSet ready, and any moved app
// exiting rather than slow. Only moved apps are asked: an untouched app's
// restarts are history, not this step's fault.
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
	// One instance crashlooping is the app crashlooping.
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
	health.Restarts = restarts
	return health, nil
}

// rolledOut: every StatefulSet has rolled out its generation with all replicas
// ready. No StatefulSets at all is ready.
func rolledOut(sets []appsv1.StatefulSet) bool {
	for i := range sets {
		set := &sets[i]
		if set.Status.ObservedGeneration < set.Generation {
			return false
		}
		want := replicasOf(set)
		if set.Status.ReadyReplicas != want || set.Status.UpdatedReplicas != want {
			return false
		}
	}
	return true
}

// replicasOf is one StatefulSet's replicas, one when unset.
func replicasOf(set *appsv1.StatefulSet) int32 {
	if set.Spec.Replicas != nil {
		return *set.Spec.Replicas
	}
	return 1
}

// Readiness reads every app for the timer: all pods ready, or a short reason.
// This asks "is anything wrong now", not the gate's "did this step break it", so
// every app is asked; an app scaled to zero has nothing to run and is ready.
func (w *Workloads) Readiness(ctx context.Context) (map[string]AppHealth, error) {
	sets, err := w.statefulSets(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[string]int32, len(sets))
	perApp := make(map[string][]appsv1.StatefulSet, len(sets))
	for i := range sets {
		app := appOf(&sets[i])
		if app == "" {
			continue
		}
		want[app] += replicasOf(&sets[i])
		perApp[app] = append(perApp[app], sets[i])
	}
	out := make(map[string]AppHealth, len(want))
	if len(want) == 0 {
		return out, nil
	}
	pods, err := w.pods(ctx)
	if err != nil {
		return nil, err
	}
	ready := make(map[string]int, len(want))
	restarts := make(map[string]int32, len(want))
	for i := range pods {
		app := pods[i].Labels[appLabel]
		if _, ok := want[app]; !ok {
			continue
		}
		if podReady(&pods[i]) {
			ready[app]++
		}
		if n := appRestarts(&pods[i]); n > restarts[app] {
			restarts[app] = n
		}
	}
	for app, n := range want {
		h := AppHealth{}
		switch {
		case n == 0:
			// Nothing should be running: not a fault.
			h.Ready = true
		case rolledOut(perApp[app]) && int32(ready[app]) >= n:
			h.Ready = true
		case restarts[app] > 0:
			h.Reason = fmt.Sprintf("restarting, %s", plural(int(restarts[app]), "restart", "restarts"))
		default:
			h.Reason = "not ready"
		}
		out[app] = h
	}
	return out, nil
}

// podReady is a pod's Ready condition.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// appRestarts is one pod's app-container exits. Init containers are the chart's
// and never the app, so their restarts do not count.
func appRestarts(pod *corev1.Pod) int32 {
	name := appContainer(pod, pod.Labels[appLabel])
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name {
			return status.RestartCount
		}
	}
	return 0
}

// appContainer is the container named for the app, else the first; init
// containers are never the app.
func appContainer(pod *corev1.Pod, app string) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == app {
			return container.Name
		}
	}
	return pod.Spec.Containers[0].Name
}

// Running is one app as the cluster runs it: each instance's asked-for image,
// and the digests its pods report. Both are needed because a tag-only pin names
// no bytes; the pods' imageID is what the kubelet pulled.
type Running struct {
	// Images is each instance's image as its StatefulSet asks for it.
	Images map[string]snapshot.ImageRef
	// Digests is each instance's reported digests; an instance with no started
	// pod has no entry, and the caller decides whether that is answerable.
	Digests map[string][]string
}

// Running reads each StatefulSet's asked-for image and its pods' digests: what
// runs, never what values or Helm asked for.
func (w *Workloads) Running(ctx context.Context) (map[string]Running, error) {
	sets, err := w.statefulSets(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Running, len(sets))
	for i := range sets {
		app := appOf(&sets[i])
		if app == "" {
			continue
		}
		run := out[app]
		if run.Images == nil {
			run.Images = make(map[string]snapshot.ImageRef, 1)
		}
		run.Images[sets[i].Name] = parseImage(containerImage(&sets[i], app))
		out[app] = run
	}
	if len(out) == 0 {
		return out, nil
	}
	pods, err := w.pods(ctx)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		// The wiring Job and init pods carry no app key; a gone StatefulSet
		// is not this release's app. Neither has an image to report.
		app := pods[i].Labels[appLabel]
		run, ok := out[app]
		if !ok {
			continue
		}
		digest := podDigest(&pods[i], app)
		if digest == "" {
			continue
		}
		if run.Digests == nil {
			run.Digests = make(map[string][]string, 1)
		}
		instance := pods[i].Labels[nameLabel]
		run.Digests[instance] = append(run.Digests[instance], digest)
		out[app] = run
	}
	for app, run := range out {
		for instance, digests := range run.Digests {
			run.Digests[instance] = sortedUnique(digests)
		}
		out[app] = run
	}
	return out, nil
}

// containerImage is the image one StatefulSet's app container asks for.
func containerImage(set *appsv1.StatefulSet, app string) string {
	containers := set.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return ""
	}
	for _, container := range containers {
		if container.Name == app {
			return container.Image
		}
	}
	return containers[0].Image
}

// podDigest is the part after `@` in imageID; the repository and an older
// `docker-pullable://` scheme are in front of it.
func podDigest(pod *corev1.Pod, app string) string {
	name := appContainer(pod, app)
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != name {
			continue
		}
		if at := strings.LastIndex(status.ImageID, "@"); at >= 0 {
			return status.ImageID[at+1:]
		}
	}
	return ""
}

// parseImage splits [registry[:port]/]repository[:tag][@digest]. The tag is the
// colon after the last slash, because the registry's port is before it.
func parseImage(ref string) snapshot.ImageRef {
	out := snapshot.ImageRef{}
	if at := strings.Index(ref, "@"); at >= 0 {
		out.Digest = ref[at+1:]
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		out.Tag = ref[colon+1:]
		ref = ref[:colon]
	}
	out.Repository = ref
	return out
}

// sortedUnique sorts and dedups an instance's digests: a restarted pod can
// report the same one twice.
func sortedUnique(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
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

// appOf is the app key from the pod template; without it, the name label.
func appOf(set *appsv1.StatefulSet) string {
	if app := set.Spec.Template.Labels[appLabel]; app != "" {
		return app
	}
	return set.Labels[nameLabel]
}
