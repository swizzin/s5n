// Package netproof proves what a cluster's CNI does with NetworkPolicy, which no
// API server can answer: the seedbox's apps are reachable only through policies
// (the Arrs run External auth and qBittorrent skips its login for private
// ranges), and a rehearsal's egress lock is a policy too. A proof runs pods of
// the release's own image inside its namespace and reads their exit codes, with
// a control in front: a cluster that ignores policy must never be reported as
// one whose probe could not run, nor the other way round.
package netproof

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// The exit codes `s5n probe` reports (cmd/s5n/probe.go), the contract between
// this package and the binary, pinned from both sides.
const (
	exitAsAsked  = 0
	exitOpposite = 3
	exitBroken   = 4
)

// probeArg is the subcommand every probe pod runs, and the name of the container
// that runs it.
const probeArg = "probe"

// proofLabel marks the pods and policies a proof creates, so neither a person nor
// a later run mistakes one for an app.
const proofLabel = "s5n.io/netproof"

// Target is one address a probe dials.
type Target struct {
	Host string
	Port int
}

// ParseTarget reads a host:port back, as the grant records it.
func ParseTarget(s string) (Target, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return Target{}, fmt.Errorf("%q is not host:port: %w", s, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 || host == "" {
		return Target{}, fmt.Errorf("%q is not host:port", s)
	}
	return Target{Host: host, Port: n}, nil
}

// String is the address as the probe takes it.
func (t Target) String() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// objects is the half of a clientset these proofs use, so a fake drives every
// path without a cluster.
type objects interface {
	Create(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	Get(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	Delete(ctx context.Context, namespace, name string) error
	List(ctx context.Context, namespace string) (*corev1.PodList, error)
	Services(ctx context.Context, namespace string) (*corev1.ServiceList, error)
	CreateNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error)
	DeleteNetworkPolicy(ctx context.Context, namespace, name string) error
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

func (c Cluster) CreateNetworkPolicy(ctx context.Context, policy *networkingv1.NetworkPolicy) (*networkingv1.NetworkPolicy, error) {
	return c.Client.NetworkingV1().NetworkPolicies(policy.Namespace).Create(ctx, policy, metav1.CreateOptions{})
}

func (c Cluster) DeleteNetworkPolicy(ctx context.Context, namespace, name string) error {
	return c.Client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c Cluster) Delete(ctx context.Context, namespace, name string) error {
	grace := int64(0)
	opts := metav1.DeleteOptions{GracePeriodSeconds: &grace}
	return c.Client.CoreV1().Pods(namespace).Delete(ctx, name, opts)
}

func (c Cluster) List(ctx context.Context, namespace string) (*corev1.PodList, error) {
	return c.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
}

// Services lists a namespace's Services.
func (c Cluster) Services(ctx context.Context, namespace string) (*corev1.ServiceList, error) {
	return c.Client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
}

// newPod is one probe pod: the release's own image (already pulled, and what the
// image policy blesses), the restricted context every app has, a label naming its
// part, and the command that answers.
func newPod(namespace, image, prefix string, labels map[string]string, command []string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: prefix,
			Namespace:    namespace,
			Labels:       labels,
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
				Name:            probeArg,
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         command,
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

// dialCommand is a probe pod that dials one address until --want holds.
func dialCommand(target Target, want string, timeout, settle time.Duration) []string {
	command := []string{
		"/usr/local/bin/s5n", probeArg, "tcp",
		"--address", target.String(),
		"--want", want,
		"--timeout", timeout.String(),
	}
	if settle > 0 {
		command = append(command, "--settle", settle.String())
	}
	return command
}

// result is one probe pod's answer: its name and its container's exit code.
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

// runPod creates one probe pod and waits for it to finish. A pod that never runs
// (a bad image, a refusing namespace) is an error: a proof needs its pods to have
// had their say.
func runPod(ctx context.Context, client objects, timeout, poll time.Duration, pod *corev1.Pod) (result, error) {
	out, err := client.Create(ctx, pod)
	if err != nil {
		return result{}, fmt.Errorf("create the %s probe pod in %s: %w", pod.GenerateName, pod.Namespace, err)
	}
	timeout, poll = bounds(timeout, poll)
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
		next, err := client.Get(ctx, pod.Namespace, out.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return result{name: out.Name}, fmt.Errorf("the %s probe pod was deleted while box waited for it", out.Name)
			}
			return result{name: out.Name}, fmt.Errorf("read the %s probe pod: %w", out.Name, err)
		}
		out = next
	}
}

// target is a listener that may already be reachable: its pod and its address.
type target struct {
	name string
	ip   string
}

// startPod creates a listening probe pod and waits until it is running with an
// address. Unlike runPod it waits for a pod that is meant never to end: a dial
// needs an address, and a proof that dials an empty one proves nothing.
func startPod(ctx context.Context, client objects, timeout, poll time.Duration, pod *corev1.Pod) (target, error) {
	out, err := client.Create(ctx, pod)
	if err != nil {
		return target{}, fmt.Errorf("create the %s probe pod in %s: %w", pod.GenerateName, pod.Namespace, err)
	}
	timeout, poll = bounds(timeout, poll)
	deadline := time.NewTimer(timeout + 30*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if ip, ok := listening(out); ok {
			return target{name: out.Name, ip: ip}, nil
		}
		if exit, done := finished(out); done {
			return target{name: out.Name}, fmt.Errorf("the %s probe pod ended (%d, %s) before it listened", out.Name, exit.code, resultDetail(result{exit: exit.code, message: exit.message}))
		}
		select {
		case <-ctx.Done():
			return target{name: out.Name}, ctx.Err()
		case <-deadline.C:
			return target{name: out.Name}, fmt.Errorf("the %s probe pod was not running after %s", out.Name, timeout+30*time.Second)
		case <-tick.C:
		}
		next, err := client.Get(ctx, pod.Namespace, out.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return target{name: out.Name}, fmt.Errorf("the %s probe pod was deleted while box waited for it", out.Name)
			}
			return target{name: out.Name}, fmt.Errorf("read the %s probe pod: %w", out.Name, err)
		}
		out = next
	}
}

// listening reports a pod's address once its container is up.
func listening(pod *corev1.Pod) (string, bool) {
	if pod.Status.PodIP == "" {
		return "", false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil || cs.Ready {
			return pod.Status.PodIP, true
		}
	}
	return "", false
}

// bounds are the waits a proof falls back to when a caller names none.
func bounds(timeout, poll time.Duration) (time.Duration, time.Duration) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	return timeout, poll
}

// exit is one container's terminated state.
type exit struct {
	code    int32
	message string
}

// finished reports whether a pod has ended, and with what. A pod that cannot
// start is a failure, or box waits a whole timeout for something that never
// runs.
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

// instanceSelector is the label pair the chart selects a release's pods by.
func instanceSelector(release string) string {
	return "app.kubernetes.io/instance=" + release
}

// cniName names the network implementation from its kube-system pods, when box
// recognises them; only used in a refusal, so empty is fine.
func cniName(ctx context.Context, client objects) string {
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
