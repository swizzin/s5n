// Package lock proves a rehearsal box's egress lock before any app starts.
//
// A NetworkPolicy is an instruction to the CNI, not something the API server
// enforces. A cluster whose CNI ignores egress rules would run a rehearsal as
// the live box — announcing to trackers, querying indexers with the source's
// keys, joining IRC — and every other part of the lock would still look right.
// So box proves it: after the release is installed locked with every app
// switched off, and before adoption switches any on, it runs two pods in the
// seedbox's namespace.
//
//   - the control pod carries no release label, so no policy of the release
//     selects it, and it must reach an in-cluster address outside the
//     namespace (a DNS pod's metrics port, which the release's egress policy
//     does not admit);
//   - the probe pod carries the release's instance label, so the egress
//     policy selects it, and it must fail to reach the same address.
//
// The control is what makes the probe's failure mean something. A namespace
// with a stricter policy of its own, or a network that cannot reach the target,
// fails the control too, and box refuses saying the proof could not run — never
// "the lock works", which would be a lie in the one direction that matters.
// A second check dials a public address from the labelled pod: it must not
// reach it either, but a host with no internet looks identical, so the
// in-cluster control is the proof and the public check only ever adds a
// failure (the lock is absent) or a note.
//
// Both pods use the s5n image the release already runs, which is the image the
// chart's blessed-image policy admits; they carry the Pod Security "restricted"
// security context every app does; and they are deleted whatever happens.
package lock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// The exit codes `s5n probe` reports (cmd/s5n/probe.go). They are the contract
// between this package and the binary, so they are named once on each side and
// the probe's test pins its half.
const (
	exitAsAsked  = 0
	exitOpposite = 3
	exitBroken   = 4
)

// Target is one address a probe dials.
type Target struct {
	Host string
	Port int
}

// String is the address as the probe takes it.
func (t Target) String() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// Options is what the proof needs to know. Everything the pods run comes from
// the release being applied: the namespace it lives in, its instance label and
// the image it already pulls.
type Options struct {
	// Namespace is the seedbox's own namespace: the pods run there, so the
	// release's policy is the one that selects the probe.
	Namespace string
	// Release is the Helm release name the chart labels every pod with
	// (app.kubernetes.io/instance). The probe carries it; the control does not.
	Release string
	// Image is the s5n image the release runs (the wiring Job's), by tag and
	// digest as the chart resolves it.
	Image string
	// Target is the in-cluster address the lock must deny. It is outside the
	// seedbox's namespace and outside its policy's DNS and pod rules.
	Target Target
	// Internet is a public address to check as well. A zero port skips it.
	Internet Target
	// Timeout bounds each pod, from create to a terminated state.
	Timeout time.Duration
	// Poll is how often the pod's status is read.
	Poll time.Duration
}

// Report is what the proof saw, for the receipt and for a test.
type Report struct {
	// ControlReached is true when an unlabelled pod in the namespace reached
	// Target: the address is reachable, so the probe's failure below means
	// something.
	ControlReached bool
	// ProbeReached is true when a pod carrying the release's label reached
	// Target: the lock is not enforced, and Prove returned an error.
	ProbeReached bool
	// Target is the address both pods dialled.
	Target Target
	// InternetChecked says the public check ran.
	InternetChecked bool
	// InternetReached is true when the labelled pod reached the public
	// address: the lock is absent, and Prove returned an error.
	InternetReached bool
	// Note is one line the caller may print: what a host without internet
	// looks like, or why the public check did not run.
	Note string
	// CNI is the network implementation box could name from the cluster, empty
	// when it could not. It only ever appears in a refusal.
	CNI string
}

// pods is the half of a clientset this package uses, so a fake drives every
// path without a cluster.
type pods interface {
	Create(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	Get(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	Delete(ctx context.Context, namespace, name string) error
	List(ctx context.Context, namespace string) (*corev1.PodList, error)
}

// Cluster is the clientset adapter.
type Cluster struct {
	Client kubernetes.Interface
}

func (c Cluster) Create(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	return c.Client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func (c Cluster) Get(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.Client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c Cluster) Delete(ctx context.Context, namespace, name string) error {
	grace := int64(0)
	opts := metav1.DeleteOptions{GracePeriodSeconds: &grace}
	return c.Client.CoreV1().Pods(namespace).Delete(ctx, name, opts)
}

func (c Cluster) List(ctx context.Context, namespace string) (*corev1.PodList, error) {
	return c.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
}

// Prove runs the control, the probe and, when asked, the public check.
//
// A nil error means the lock is in place: the control reached the target and
// the labelled probe did not. Every other answer is a refusal that names what
// box actually saw, because "could not prove it" and "it is not enforced" are
// different facts and only one of them is about this cluster's CNI.
func Prove(ctx context.Context, client pods, o Options) (Report, error) {
	rep := Report{Target: o.Target}
	if err := o.check(); err != nil {
		return rep, err
	}
	rep.CNI = cniName(ctx, client)
	var created []string
	defer func() {
		for _, name := range created {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = client.Delete(cleanupCtx, o.Namespace, name)
			cancel()
		}
	}()

	control, err := runPod(ctx, client, o, probePod(o, "control", nil, o.Target, "open"))
	if control.name != "" {
		created = append(created, control.name)
	}
	if err != nil {
		return rep, fmt.Errorf("the lock could not be proven: %w", err)
	}
	switch control.exit {
	case exitAsAsked:
		rep.ControlReached = true
	case exitOpposite:
		return rep, fmt.Errorf("the lock could not be proven: a pod without %s could not reach %s from %s, so this namespace refuses traffic box did not write (or the target does not answer); box cannot tell the lock from the namespace, so it will not rehearse here",
			instanceSelector(o.Release), o.Target, o.Namespace)
	default:
		return rep, fmt.Errorf("the lock could not be proven: the control pod exited %d (%s), so box could not reach the target with a pod the release's policy does not select", control.exit, resultDetail(control))
	}

	probe, err := runPod(ctx, client, o, probePod(o, "probe", map[string]string{"app.kubernetes.io/instance": o.Release}, o.Target, "closed"))
	if probe.name != "" {
		created = append(created, probe.name)
	}
	if err != nil {
		return rep, fmt.Errorf("the lock could not be proven: %w", err)
	}
	switch probe.exit {
	case exitAsAsked:
		// The labelled pod could not reach the target: the policy held.
	case exitOpposite:
		rep.ProbeReached = true
		return rep, fmt.Errorf("this cluster does not enforce NetworkPolicy%s: a pod carrying %s reached %s, and the release's egress policy denies it, so a rehearsal here would run live",
			cniSuffix(rep.CNI), instanceSelector(o.Release), o.Target)
	default:
		return rep, fmt.Errorf("the lock could not be proven: the probe pod exited %d (%s), so box cannot tell whether the policy denied the connection", probe.exit, resultDetail(probe))
	}

	if o.Internet.Port == 0 {
		return rep, nil
	}
	rep.InternetChecked = true
	inet, err := runPod(ctx, client, o, probePod(o, "internet", map[string]string{"app.kubernetes.io/instance": o.Release}, o.Internet, "closed"))
	if inet.name != "" {
		created = append(created, inet.name)
	}
	if err != nil {
		rep.Note = fmt.Sprintf("the public check did not run (%v); the in-cluster proof above is the one that counts", err)
		return rep, nil
	}
	if inet.exit == exitOpposite {
		rep.InternetReached = true
		return rep, fmt.Errorf("a pod carrying %s reached the public address %s, so the release's egress policy is not in place and a rehearsal here would reach the internet",
			instanceSelector(o.Release), o.Internet)
	}
	rep.Note = fmt.Sprintf("the public check also stayed closed (%s); on a host with no internet that looks the same, so the in-cluster control is the proof", o.Internet)
	return rep, nil
}

// check refuses an options set that cannot describe a proof, before any pod is
// created.
func (o Options) check() error {
	var missing []string
	if strings.TrimSpace(o.Namespace) == "" {
		missing = append(missing, "namespace")
	}
	if strings.TrimSpace(o.Release) == "" {
		missing = append(missing, "release")
	}
	if strings.TrimSpace(o.Image) == "" {
		missing = append(missing, "image")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the lock proof needs %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(o.Target.Host) == "" || o.Target.Port == 0 {
		return errors.New("the lock proof needs a target: an in-cluster address the release's egress policy denies")
	}
	return nil
}

// probePod is one probe: the image the release already runs, the Pod Security
// "restricted" context every app has, and the label that decides whether the
// release's policy selects it.
func probePod(o Options, role string, labels map[string]string, target Target, want string) *corev1.Pod {
	all := map[string]string{"s5n.io/rehearsal-probe": role}
	for k, v := range labels {
		all[k] = v
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "s5n-rehearsal-" + role + "-",
			Namespace:    o.Namespace,
			Labels:       all,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           o.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command: []string{
					"/usr/local/bin/s5n", "probe", "tcp",
					"--address", target.String(),
					"--want", want,
					"--timeout", timeout.String(),
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources: corev1.ResourceRequirements{},
			}},
		},
	}
}

// result is one probe pod's answer: its name, and the exit code its one
// container ended with.
type result struct {
	name    string
	exit    int32
	message string
}

func resultDetail(r result) string {
	if r.message != "" {
		return r.message
	}
	return "no message"
}

// runPod creates one probe pod and waits for it to finish. A pod that never
// runs — a bad image, a namespace that refuses it — is an error rather than an
// answer, because a proof needs both pods to have had their say.
func runPod(ctx context.Context, client pods, o Options, pod *corev1.Pod) (result, error) {
	out, err := client.Create(ctx, pod)
	if err != nil {
		return result{}, fmt.Errorf("create the %s probe pod in %s: %w", pod.GenerateName, o.Namespace, err)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	poll := o.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.NewTimer(timeout + 30*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if exit, done := finished(out); done {
			return result{name: out.Name, exit: exit.code, message: exit.message}, nil
		}
		select {
		case <-ctx.Done():
			return result{name: out.Name}, ctx.Err()
		case <-deadline.C:
			return result{name: out.Name}, fmt.Errorf("the %s probe pod did not finish within %s", out.Name, timeout+30*time.Second)
		case <-tick.C:
		}
		next, err := client.Get(ctx, o.Namespace, out.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return result{name: out.Name}, fmt.Errorf("the %s probe pod was deleted while box waited for it", out.Name)
			}
			return result{name: out.Name}, fmt.Errorf("read the %s probe pod: %w", out.Name, err)
		}
		out = next
	}
}

// exit is one container's terminated state.
type exit struct {
	code    int32
	message string
}

// finished reports whether a pod has ended, and with what. A pod that cannot
// start (an image that will not pull, a sandbox that cannot be created) is a
// usable answer here only as a failure: box would otherwise wait a whole
// timeout for something that will never run.
func finished(pod *corev1.Pod) (exit, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return exit{code: cs.State.Terminated.ExitCode, message: strings.TrimSpace(cs.State.Terminated.Message)}, true
		}
		if cs.State.Waiting != nil && permanent(cs.State.Waiting.Reason) {
			return exit{code: exitBroken, message: cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message}, true
		}
	}
	return exit{}, false
}

// permanent is a waiting reason that will not clear on its own.
func permanent(reason string) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
		return true
	}
	return false
}

// instanceSelector is the label pair the chart selects a release's pods by, as
// a refusal says it.
func instanceSelector(release string) string {
	return "app.kubernetes.io/instance=" + release
}

// cniName names the network implementation from the pods it runs in
// kube-system, when the names are ones box recognises. It is only ever used in
// a refusal, so empty ("box cannot name it") is a fine answer.
func cniName(ctx context.Context, client pods) string {
	list, err := client.List(ctx, "kube-system")
	if err != nil {
		return ""
	}
	for _, pod := range list.Items {
		name := strings.ToLower(pod.Name)
		for _, known := range []struct{ needle, name string }{
			{"kindnet", "kindnet"},
			{"kube-router", "kube-router"},
			{"calico", "Calico"},
			{"cilium", "Cilium"},
			{"flannel", "flannel"},
			{"antrea", "Antrea"},
			{"weave", "Weave Net"},
		} {
			if strings.Contains(name, known.needle) {
				return known.name
			}
		}
	}
	return ""
}

// cniSuffix is the refusal's parenthetical, or nothing when box cannot name the
// implementation.
func cniSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

// dnsPort is the CoreDNS pod's metrics listener. The release's egress policy
// admits the cluster's DNS pods on 53 only, so it is an address outside the
// release's namespace that the policy denies and that every cluster box
// supports runs: a control pod reaches it, a pod the policy selects does not.
const dnsPort = 9153

// DNSTarget is the in-cluster address the lock must deny: a ready cluster DNS
// pod's metrics port. It refuses when the cluster runs none, because a proof
// with no target is not a proof and box would rather stop than guess.
func DNSTarget(ctx context.Context, client pods) (Target, error) {
	list, err := client.List(ctx, "kube-system")
	if err != nil {
		return Target{}, fmt.Errorf("read the cluster's DNS pods: %w", err)
	}
	for _, pod := range list.Items {
		if pod.Labels["k8s-app"] != "kube-dns" && pod.Labels["app.kubernetes.io/name"] != "coredns" {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		return Target{Host: pod.Status.PodIP, Port: dnsPort}, nil
	}
	return Target{}, errors.New("this cluster runs no ready CoreDNS pod (k8s-app=kube-dns) in kube-system: box needs its metrics port as the address the release's egress policy must deny, and without one it cannot prove the lock")
}
