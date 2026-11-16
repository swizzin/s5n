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
// ready, which of the apps a step moved keep exiting, and how often each of
// their containers has restarted.
type Health struct {
	// Ready is every StatefulSet of the release rolled out on its current
	// generation with all its replicas ready and updated. It is the gate's
	// pass, and the only thing it asks about the release as a whole.
	Ready bool
	// Exiting is the moved apps that keep exiting, sorted by app. They are
	// the ones box cannot wait out: waiting only adds restarts.
	Exiting []Exiting
	// Restarts is the highest restart count of each moved app's container,
	// across its pods and instances. An app with no pod yet is absent rather
	// than zero. box reads it once after a failed apply, to tell an image
	// Helm gave up on from a release that failed for some other reason: a
	// moved app that restarted at all deserves the health gate's watch.
	Restarts map[string]int32
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
	health.Restarts = restarts
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
		want := replicasOf(set)
		if set.Status.ReadyReplicas != want || set.Status.UpdatedReplicas != want {
			return false
		}
	}
	return true
}

// replicasOf is how many replicas one StatefulSet asks for. A set with no
// replicas field runs one, which is what the chart leaves when it does not say.
func replicasOf(set *appsv1.StatefulSet) int32 {
	if set.Spec.Replicas != nil {
		return *set.Spec.Replicas
	}
	return 1
}

// Readiness reads every app of the release once, for the timer: whether all of
// its pods are ready, and a short reason when one is not. It is the same app
// key the plan and the rollback use, so a reading and a step name an app the
// same way.
//
// This is the "is anything wrong now" question, not the health gate's "did this
// step break it": nothing moved, so every app is asked about. An app the values
// disabled has no StatefulSet and is not here at all; one box scaled to zero
// while it copies has nothing that should be running, so it is ready.
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

// podReady is a pod's Ready condition: the app has opened its port.
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

// appRestarts is how many times one pod's app container has exited. The chart
// names that container after the app key (workloads.yaml), so that is the one
// the count is read from; a pod whose container is named something else falls
// back to its first, so a chart that renames it still gets a count rather than
// silence. Init containers are the chart's own (the wiring and permission
// steps) and are not the app, so their restarts are not counted.
func appRestarts(pod *corev1.Pod) int32 {
	name := appContainer(pod, pod.Labels[appLabel])
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name {
			return status.RestartCount
		}
	}
	return 0
}

// appContainer is the pod's app container: the one the chart names after the
// app key, or the first when the chart names it something else. Init containers
// are the chart's own and are never the app.
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

// Running is one app as the cluster runs it: the image each instance's
// StatefulSet asks for, and the digests the pods report for it.
//
// Both halves are needed because a values pin may be a tag alone. The chart
// then names no digest, so the reference says which release of the app was
// asked for and nothing about the bytes; the bytes are in the pods' imageID,
// which is what the kubelet pulled.
type Running struct {
	// Images is each instance's image as its StatefulSet asks for it, keyed
	// by the StatefulSet's name. An instance scaled to zero keeps its
	// StatefulSet, so it is still here.
	Images map[string]snapshot.ImageRef
	// Digests is the digest each instance's pods report, sorted and without
	// repeats. An instance with no pod, or whose container has not started,
	// has no entry: the caller decides whether that is answerable.
	Digests map[string][]string
}

// Running reads every app of the release: the image each of its StatefulSets
// asks for and, from the pods, the digest that reference resolved to. It is how
// an apply knows what it moves an app from: the values file and Helm's stored
// values are what was asked for, never what runs.
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
		// A pod of the wiring Job or of an init container carries the
		// release label and no app key, and an app whose StatefulSet is gone
		// is not one this release runs: neither has an image to report.
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

// podDigest is the digest a pod reports for the app container: the part after
// the `@` in containerStatuses[].imageID. Kubernetes spells it with the
// repository in front (and, from older runtimes, a `docker-pullable://`
// scheme), so only the tail is the digest.
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

// parseImage splits an image reference the way Kubernetes spells it --
// [registry[:port]/]repository[:tag][@digest] -- into the three parts box
// compares. The tag is the colon after the last slash, because the registry's
// port is a colon before it.
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

// sortedUnique is one instance's reported digests, sorted and without repeats:
// box's plan names the pair in the same order on every run, and a pod that
// restarted onto the same image reports one digest twice.
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

// appOf is the app key one StatefulSet belongs to. It is on the pod template;
// a set without it falls back to its own name label, which is the app for the
// default instance and only wrong for a second Arr instance.
func appOf(set *appsv1.StatefulSet) string {
	if app := set.Spec.Template.Labels[appLabel]; app != "" {
		return app
	}
	return set.Labels[nameLabel]
}
