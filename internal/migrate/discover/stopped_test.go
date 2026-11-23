package discover

// stopped_test.go proves cutover's stop proof: an app is stopped only when its
// workload has no replicas *and* no pod of it is left, a Terminating pod counts
// as running, every kind discovery reads can be proved stopped and has a stop
// command that names the source's own workload, and a read that cannot prove a
// stop refuses instead of guessing. The source is a list of workloads and a list
// of pods, so no cluster is involved.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// stoppedSource is a source read with nothing behind it but the workloads and
// pods a read would have carried.
type stoppedSource struct {
	wls  []Workload
	pods []Pod
	err  error
}

func (s stoppedSource) Workloads(context.Context) ([]Workload, error) { return s.wls, s.err }
func (s stoppedSource) Services(context.Context) ([]Service, error)   { return nil, nil }
func (s stoppedSource) Ingresses(context.Context) ([]Ingress, error)  { return nil, nil }
func (s stoppedSource) Pods(context.Context) ([]Pod, error)           { return s.pods, s.err }
func (s stoppedSource) Nodes(context.Context) ([]Node, error)         { return nil, nil }
func (s stoppedSource) Raw(context.Context) (map[string][]byte, error) {
	return nil, nil
}

// workload is one planned app's workload, as the read carries it: the kind and
// the two things a stop proof asks (the replica count and what its pods carry).
func workload(kind, name string, replicas int, selector map[string]string) Workload {
	return Workload{
		Namespace: "media",
		Name:      name,
		Kind:      kind,
		Labels:    selector,
		Selector:  selector,
		Replicas:  replicas,
	}
}

// runningPod is one pod the source holds, carrying app's labels.
func runningPod(app, name string) Pod {
	return Pod{Namespace: "media", Name: name, Labels: map[string]string{"app": app}}
}

// stoppedPlan is one app, sonarr, in namespace media.
func stoppedPlan() *plan.Plan {
	return &plan.Plan{Version: 2, Apps: map[string]plan.App{
		"sonarr": {Kind: plan.Kubernetes, Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media"},
	}}
}

// TestStoppedIsNoReplicasAndNoPod: the two halves of "stopped", one case each,
// over every kind discovery reads.
func TestStoppedIsNoReplicasAndNoPod(t *testing.T) {
	selector := map[string]string{"app": "sonarr"}
	tests := []struct {
		name     string
		wls      []Workload
		pods     []Pod
		want     int // how many apps are still running
		wantWhy  string
		wantPods int
	}{
		{
			name: "a Deployment at zero with no pod is stopped",
			wls:  []Workload{workload(kindDeployment, "sonarr", 0, selector)},
		},
		{
			name: "a StatefulSet at zero with no pod is stopped",
			wls:  []Workload{workload(kindStatefulSet, "sonarr", 0, selector)},
		},
		{
			// A DaemonSet has no count of its own: its pods are what says it
			// is running.
			name: "a DaemonSet with no pod is stopped",
			wls:  []Workload{workload(kindDaemonSet, "sonarr", -1, selector)},
		},
		{
			// A bare Pod is the pod: there is the workload and there is the
			// pod, so an app that ran as one is stopped only when it is gone.
			name: "a bare Pod that is gone is stopped",
			wls:  []Workload{workload(kindPod, "sonarr", -1, selector)},
		},
		{
			name: "a bare Pod that is there is running",
			wls:  []Workload{workload(kindPod, "sonarr", -1, selector)},
			pods: []Pod{runningPod("sonarr", "sonarr")},
			want: 1,
			// The pod is the workload, so it is found by its name and not by
			// the labels alone.
			wantWhy:  "1 pod left",
			wantPods: 1,
		},
		{
			name:    "a replica is running",
			wls:     []Workload{workload(kindDeployment, "sonarr", 2, selector)},
			want:    1,
			wantWhy: "2 replicas",
		},
		{
			// The case the whole file exists for: scaled to zero, and a pod is
			// still there, on its way out.
			name:     "a Terminating pod counts as running",
			wls:      []Workload{workload(kindDeployment, "sonarr", 0, selector)},
			pods:     []Pod{{Namespace: "media", Name: "sonarr-abc", Labels: selector, Terminating: true}},
			want:     1,
			wantWhy:  "1 pod left",
			wantPods: 1,
		},
		{
			name:     "replicas and a pod are both named",
			wls:      []Workload{workload(kindDeployment, "sonarr", 1, selector)},
			pods:     []Pod{runningPod("sonarr", "sonarr-abc")},
			want:     1,
			wantWhy:  "1 replica, 1 pod left",
			wantPods: 1,
		},
		{
			name: "a DaemonSet's pod is running",
			wls:  []Workload{workload(kindDaemonSet, "sonarr", -1, selector)},
			pods: []Pod{runningPod("sonarr", "sonarr-x2b7c")},
			want: 1, wantWhy: "1 pod left", wantPods: 1,
		},
		{
			// Another app's pod in the same namespace is not this app's.
			name: "another app's pod does not count",
			wls:  []Workload{workload(kindDeployment, "sonarr", 0, selector)},
			pods: []Pod{runningPod("radarr", "radarr-abc")},
		},
		{
			// A pod in another namespace with the same labels is not this
			// app's either.
			name: "a pod in another namespace does not count",
			wls:  []Workload{workload(kindDeployment, "sonarr", 0, selector)},
			pods: []Pod{{Namespace: "other", Name: "sonarr-abc", Labels: selector}},
		},
		{
			// A workload nothing selects cannot claim someone else's pods.
			name: "a workload with no selector owns no pod",
			wls:  []Workload{workload(kindDeployment, "sonarr", 0, nil)},
			pods: []Pod{runningPod("sonarr", "sonarr-abc")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Unstopped(context.Background(), stoppedSource{wls: tt.wls, pods: tt.pods}, stoppedPlan())
			if err != nil {
				t.Fatalf("Unstopped: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("Unstopped = %d running app(s), want %d: %+v", len(got), tt.want, got)
			}
			if tt.want == 0 {
				return
			}
			if got[0].Why() != tt.wantWhy {
				t.Errorf("why = %q, want %q", got[0].Why(), tt.wantWhy)
			}
			if len(got[0].Pods) != tt.wantPods {
				t.Errorf("pods = %v, want %d of them", got[0].Pods, tt.wantPods)
			}
		})
	}
}

// TestTheStopCommandNamesTheSource: what the operator is told to run is the
// source's own workload, in its own namespace. A kind with a count is scaled; a
// DaemonSet and a bare Pod, which have none, are deleted, and the line says why
// that is safe.
func TestTheStopCommandNamesTheSource(t *testing.T) {
	selector := map[string]string{"app": "sonarr"}
	tests := []struct {
		kind     string
		want     string
		wantNote string
	}{
		{kindDeployment, "kubectl -n media scale deployment/sonarr --replicas=0", ""},
		{kindStatefulSet, "kubectl -n media scale statefulset/sonarr --replicas=0", ""},
		{kindReplicaSet, "kubectl -n media scale replicaset/sonarr --replicas=0", ""},
		{kindDaemonSet, "kubectl -n media delete daemonset/sonarr", "holds its manifest"},
		{kindPod, "kubectl -n media delete pod/sonarr", "holds its manifest"},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			wls := []Workload{workload(tt.kind, "sonarr", 1, selector)}
			// A DaemonSet and a bare Pod are running through their pod, and
			// a bare Pod is the workload itself, so its name is the workload's.
			pod := "sonarr-abc"
			if tt.kind == kindDaemonSet || tt.kind == kindPod {
				wls = []Workload{workload(tt.kind, "sonarr", -1, selector)}
			}
			if tt.kind == kindPod {
				pod = "sonarr"
			}
			got, err := Unstopped(context.Background(), stoppedSource{
				wls:  wls,
				pods: []Pod{runningPod("sonarr", pod)},
			}, stoppedPlan())
			if err != nil {
				t.Fatalf("Unstopped: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("Unstopped = %+v, want sonarr still running", got)
			}
			if got[0].StopCommand() != tt.want {
				t.Errorf("stop command = %q, want %q", got[0].StopCommand(), tt.want)
			}
			note := got[0].StopNote()
			if tt.wantNote == "" && note != "" {
				t.Errorf("a scale carries the note %q, and scaling needs no warning", note)
			}
			if tt.wantNote != "" && !strings.Contains(note, tt.wantNote) {
				t.Errorf("the delete's note = %q, want it to say %q", note, tt.wantNote)
			}
		})
	}
}

// TestTheAppsAreSorted: a refusal reads the same way twice, so the apps are in
// box's own name order.
func TestTheAppsAreSorted(t *testing.T) {
	p := &plan.Plan{Version: 2, Apps: map[string]plan.App{
		"sonarr": {Kind: plan.Kubernetes, Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media"},
		"radarr": {Kind: plan.Kubernetes, Source: "radarr", Config: "/srv/apps/radarr", Namespace: "media"},
	}}
	got, err := Unstopped(context.Background(), stoppedSource{wls: []Workload{
		workload(kindDeployment, "sonarr", 1, map[string]string{"app": "sonarr"}),
		workload(kindDeployment, "radarr", 1, map[string]string{"app": "radarr"}),
	}}, p)
	if err != nil {
		t.Fatalf("Unstopped: %v", err)
	}
	if len(got) != 2 || got[0].App != "radarr" || got[1].App != "sonarr" {
		t.Errorf("apps are %+v, want radarr and sonarr in that order", got)
	}
}

// TestStoppedRefusesWhatItCannotProve: a proof that guessed is worse than a
// refusal, because the guess is what copies a live app's config.
func TestStoppedRefusesWhatItCannotProve(t *testing.T) {
	selector := map[string]string{"app": "sonarr"}
	tests := []struct {
		name string
		src  Source
		p    *plan.Plan
		want string
	}{
		{
			name: "the read does not carry the workload",
			src:  stoppedSource{},
			p:    stoppedPlan(),
			want: "no workload named sonarr",
		},
		{
			name: "a source kind cutover cannot prove yet",
			src:  stoppedSource{},
			p: &plan.Plan{Version: 2, Apps: map[string]plan.App{
				"sonarr": {Kind: plan.Docker, Source: "sonarr", Config: "/srv/apps/sonarr"},
			}},
			want: "ran on docker",
		},
		{
			name: "no source at all",
			src:  nil,
			p:    stoppedPlan(),
			want: "no source",
		},
		{
			name: "no plan at all",
			src:  stoppedSource{wls: []Workload{workload(kindDeployment, "sonarr", 0, selector)}},
			p:    nil,
			want: "no plan",
		},
		{
			// A directory of saved manifests answers with the pods it was
			// saved with, and one saved without them answers none: that looks
			// exactly like a source that stopped.
			name: "a saved read",
			src:  NewDirectory(fstest.MapFS{}),
			p:    stoppedPlan(),
			want: "a saved read",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Unstopped(context.Background(), tt.src, tt.p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Unstopped error = %v, want it to name %q", err, tt.want)
			}
		})
	}
}

// TestStoppedPassesTheReadsFailureOn: a read that failed is the run's error, not
// "nothing is running".
func TestStoppedPassesTheReadsFailureOn(t *testing.T) {
	want := errors.New("the API server refused")
	src := stoppedSource{err: want}
	if _, err := Unstopped(context.Background(), src, stoppedPlan()); !errors.Is(err, want) {
		t.Fatalf("Unstopped error = %v, want the read's own failure", err)
	}
}
