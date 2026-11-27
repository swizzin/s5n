package netproof

// ingress.go proves the direction the apps rely on: a policy denying all ingress
// is enforced, so nothing but the Gateway (or a peer, or the wiring Job) reaches
// a seedbox app. A rehearsal's egress lock is the other direction, in egress.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IngressPort is the port both targets listen on and both clients dial. It is
// above 1024, because a restricted pod holds no capability to bind lower.
const IngressPort = 18080

// The parts a proof pod or policy plays, in its one label.
const (
	roleControlTarget = "control-target"
	roleDeniedTarget  = "denied-target"
	roleControlClient = "control-client"
	roleDeniedClient  = "denied-client"
)

// IngressOptions is what the ingress proof needs.
type IngressOptions struct {
	// Namespace is the seedbox's namespace: where its apps will run, and where
	// the proof's pods and policy are written.
	Namespace string
	// Image is the s5n image the release runs, by tag and digest.
	Image string
	// Timeout bounds each pod, create to ended; it must outlast Settle.
	Timeout time.Duration
	// Settle is how long a pod keeps dialling (s5n probe --settle): a CNI
	// programs a new pod's policy a moment after it starts, so one dial would
	// read an enforcing cluster as one that ignores policy. Zero dials once.
	Settle time.Duration
	// Poll is how often a pod's status is read.
	Poll time.Duration
}

// IngressReport is what the ingress proof saw.
type IngressReport struct {
	// ControlReached is true when a pod reached a target no policy selects, so
	// the denied client's failure means something.
	ControlReached bool
	// ProbeReached is true when a pod reached a target a deny-all policy selects:
	// the CNI ignores policies, and ProveIngress returned an error.
	ProbeReached bool
	// Port is the port both targets listened on.
	Port int
	// CNI is the network implementation box could name, empty when it could not.
	CNI string
}

// ProveIngress writes two targets and one policy, then dials each target from its
// own pod: the control must open and the policed one must not. A nil error means
// this cluster enforces NetworkPolicy ingress. Every other answer says which of
// the two it was, because a cluster that ignores policy and a cluster whose pods
// cannot reach each other call for different fixes.
func ProveIngress(ctx context.Context, client objects, o IngressOptions) (IngressReport, error) {
	rep := IngressReport{Port: IngressPort}
	if err := o.check(); err != nil {
		return rep, err
	}
	rep.CNI = cniName(ctx, client)
	var pods []string
	var policy string
	// Pods before the policy: a policy removed first would open the target it
	// denies while the pod is still running.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range pods {
			_ = client.Delete(cleanupCtx, o.Namespace, name)
		}
		if policy != "" {
			_ = client.DeleteNetworkPolicy(cleanupCtx, o.Namespace, policy)
		}
	}()

	control, err := startPod(ctx, client, o.Timeout, o.Poll, listenerPod(o, roleControlTarget))
	if control.name != "" {
		pods = append(pods, control.name)
	}
	if err != nil {
		return rep, unrunnable(err)
	}
	denied, err := startPod(ctx, client, o.Timeout, o.Poll, listenerPod(o, roleDeniedTarget))
	if denied.name != "" {
		pods = append(pods, denied.name)
	}
	if err != nil {
		return rep, unrunnable(err)
	}
	np, err := client.CreateNetworkPolicy(ctx, denyPolicy(o.Namespace))
	if err != nil {
		return rep, fmt.Errorf("the network policy proof could not run: write its deny-all policy in %s: %w", o.Namespace, err)
	}
	policy = np.Name

	// The control first: without it a refusal would be this network's silence, not
	// a policy's absence.
	open, err := runPod(ctx, client, o.Timeout, o.Poll, dialPod(o, roleControlClient, control, "open"))
	if open.name != "" {
		pods = append(pods, open.name)
	}
	if err != nil {
		return rep, fmt.Errorf("the network policy proof could not run: %w", err)
	}
	switch open.exit {
	case exitAsAsked:
		rep.ControlReached = true
	case exitOpposite:
		return rep, fmt.Errorf("the network policy proof could not run: the probe pods could not reach each other at all -- one pod in %s could not reach another at %s that no policy selects, so a denial here would say nothing about this cluster's network plugin",
			o.Namespace, Target{Host: control.ip, Port: rep.Port})
	default:
		return rep, fmt.Errorf("the network policy proof could not run: the control pod exited %d (%s)", open.exit, resultDetail(open))
	}

	closed, err := runPod(ctx, client, o.Timeout, o.Poll, dialPod(o, roleDeniedClient, denied, "closed"))
	if closed.name != "" {
		pods = append(pods, closed.name)
	}
	if err != nil {
		return rep, fmt.Errorf("the network policy proof could not run: %w", err)
	}
	switch closed.exit {
	case exitAsAsked:
		// The pod reached nothing through the policy: the cluster enforces it.
	case exitOpposite:
		rep.ProbeReached = true
		return rep, fmt.Errorf("this cluster's network plugin does not enforce NetworkPolicy%s: a pod in %s reached a pod at %s that a policy denying all ingress selects, and the Arrs and qBittorrent rely on those policies to keep other pods out (their own login is at the edge, or off for private addresses). Use a plugin that enforces them -- Calico, Cilium, kube-router -- or let box install the cluster",
			cniSuffix(rep.CNI), o.Namespace, Target{Host: denied.ip, Port: rep.Port})
	default:
		return rep, fmt.Errorf("the network policy proof could not run: the probe pod exited %d (%s), so box cannot tell whether the policy denied the connection", closed.exit, resultDetail(closed))
	}
	return rep, nil
}

// unrunnable says the proof could not run, unless the image predates the proof:
// that is the chart's age, and saying the proof failed would blame the cluster.
func unrunnable(err error) error {
	if errors.Is(err, ErrNoProbe) {
		return err
	}
	return fmt.Errorf("the network policy proof could not run: %w", err)
}

// check refuses options that cannot describe a proof, before any pod exists.
func (o IngressOptions) check() error {
	var missing []string
	if strings.TrimSpace(o.Namespace) == "" {
		missing = append(missing, "namespace")
	}
	if strings.TrimSpace(o.Image) == "" {
		missing = append(missing, "image")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the network policy proof needs %s", strings.Join(missing, ", "))
	}
	return nil
}

// listenerPod is one target: it listens on IngressPort until box deletes it.
func listenerPod(o IngressOptions, role string) *corev1.Pod {
	command := []string{"/usr/local/bin/s5n", probeArg, "listen", "--port", fmt.Sprint(IngressPort)}
	return newPod(o.Namespace, o.Image, "s5n-probe-"+role+"-", map[string]string{proofLabel: role}, command)
}

// dialPod is one client: it dials one target and answers with its exit code.
func dialPod(o IngressOptions, role string, t target, want string) *corev1.Pod {
	command := dialCommand(Target{Host: t.ip, Port: IngressPort}, want, dialTimeout, o.Settle)
	return newPod(o.Namespace, o.Image, "s5n-probe-"+role+"-", map[string]string{proofLabel: role}, command)
}

// denyPolicy denies every source, every port, to the one target it selects. A
// policy that names no rule allows nothing.
func denyPolicy(namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "s5n-probe-deny-",
			Namespace:    namespace,
			Labels:       map[string]string{proofLabel: "deny-policy"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{proofLabel: roleDeniedTarget}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
		},
	}
}
