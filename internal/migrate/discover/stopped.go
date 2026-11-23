package discover

// stopped.go is cutover's proof: discovery reads templates and needs no pod, but
// "this app is stopped" is two facts, and only one of them is in the workload.
// A Deployment at replicas 0 can still have a pod on its way out, and a pod on
// its way out is still writing; a cutover that copied configs under it would
// copy a database mid-write.
//
// So this file reads the workloads and the pods the source holds now -- the same
// two reads discovery itself starts from -- and answers, per planned app,
// whether the workload has replicas and whether any pod of it is left.
// Terminating counts as running, because the process has not exited yet, and a
// pod is one of a workload's own by the one rule the read already carries
// (Workload.Owns).
//
// Nothing here stops anything: box never stops a source (docs/box.md, "box
// never stops or resets a source"). What it returns is what the operator has to
// do, as a command to double-check.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// Running is one planned app that has not stopped.
type Running struct {
	// App is box's instance name for it.
	App string
	// Kind, Namespace and Name are the source's workload, as the read carries
	// it: what the stop command has to name.
	Kind      string
	Namespace string
	Name      string
	// Replicas is the workload's replica count, as it stands. It is -1 for a
	// kind that has no count of its own: a DaemonSet runs one pod per node,
	// and a bare Pod is the pod.
	Replicas int
	// Pods are the pods still matching the workload's template, Terminating
	// ones included.
	Pods []string
}

// Why says what is still running, in the one line the refusal prints.
func (r Running) Why() string {
	var parts []string
	if r.Replicas > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", r.Replicas, plural(r.Replicas, "replica")))
	}
	if len(r.Pods) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s left", len(r.Pods), plural(len(r.Pods), "pod")))
	}
	if len(parts) == 0 {
		return "running"
	}
	return strings.Join(parts, ", ")
}

// StopCommand is the command that stops it: printed for the operator to run and
// double-check, never run by box.
//
// A kind with a replica count is scaled to zero. A DaemonSet and a bare Pod have
// no count to scale, so they are deleted -- and that is safe, which is what
// StopNote says: the read a refusal keeps is a survey, and the survey holds the
// manifest.
func (r Running) StopCommand() string {
	where := ""
	if r.Namespace != "" {
		where = "-n " + r.Namespace + " "
	}
	object := strings.ToLower(r.Kind) + "/" + r.Name
	switch r.Kind {
	case kindDaemonSet, kindPod:
		return "kubectl " + where + "delete " + object
	}
	return "kubectl " + where + "scale " + object + " --replicas=0"
}

// StopNote is what the operator has to know about that command, empty when
// there is nothing to add: a delete cannot be undone with a scale, so it says
// where the manifest is. A refusal writes the survey before it prints a
// command, so the manifest is always there by the time this is read.
func (r Running) StopNote() string {
	switch r.Kind {
	case kindDaemonSet, kindPod:
		return "safe to delete: the survey this run wrote holds its manifest, so it can be put back"
	}
	return ""
}

// Unstopped is every planned app that is still running, sorted by box's name
// for it. An empty answer is the proof cutover needs.
//
// It refuses rather than guesses: an app whose workload the read does not carry,
// an app from a source kind box cannot prove yet, and a saved read of manifests
// (which has no pods behind it) are all errors naming what is wrong. A cutover
// that silently treated any of them as stopped would copy a live app's config.
func Unstopped(ctx context.Context, src Source, p *plan.Plan) ([]Running, error) {
	if p == nil {
		return nil, fmt.Errorf("no plan: cutover proves the apps a plan names")
	}
	if src == nil {
		return nil, fmt.Errorf("no source: proving a source stopped means reading the workloads and pods it still has, and this run cannot")
	}
	// A saved read answers with the pods it was saved with, and a directory
	// saved without them answers none -- which looks exactly like a source that
	// has stopped. The proof is the reason cutover exists, so it is refused
	// here as well as by its caller.
	if _, saved := src.(*Directory); saved {
		return nil, fmt.Errorf("a saved read of manifests cannot prove a source stopped: it carries no live pods, so run cutover against the cluster")
	}
	wls, err := src.Workloads(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := src.Pods(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []Running
	for _, name := range names {
		app := p.Apps[name]
		if app.Kind != plan.Kubernetes {
			return nil, fmt.Errorf("apps.%s ran on %s, and cutover can only prove a Kubernetes app stopped yet; the stop proof for a %s source lands next", name, app.Kind, app.Kind)
		}
		workload, ok := workloadForApp(wls, app)
		if !ok {
			return nil, fmt.Errorf("apps.%s: the read carries no workload named %s in %s, so whether it is stopped cannot be proved: run cutover against the cluster the plan was read from",
				name, app.Source, where(app.Namespace))
		}
		r := Running{App: name, Kind: workload.Kind, Namespace: app.Namespace, Name: app.Source, Replicas: workload.Replicas}
		for _, pod := range pods {
			if workload.Owns(pod) {
				r.Pods = append(r.Pods, pod.Name)
			}
		}
		sort.Strings(r.Pods)
		if r.Replicas > 0 || len(r.Pods) > 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// workloadForApp finds the app's workload in what the read carries. The plan
// records the platform ("kubernetes"), not the kind, because a Deployment where
// a StatefulSet belonged is a fine thing to migrate; the read is kind-neutral
// for the same reason, so the object is looked for by its namespace and its name.
func workloadForApp(wls []Workload, app plan.App) (Workload, bool) {
	for _, wl := range wls {
		if wl.Namespace == app.Namespace && wl.Name == app.Source {
			return wl, true
		}
	}
	return Workload{}, false
}

// plural picks the word for a count.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
