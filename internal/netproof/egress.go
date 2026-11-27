package netproof

// egress.go is the rehearsal lock's proof: a pod carrying the release's label
// must not reach an in-cluster address the release's egress policy denies, while
// a pod without it must.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Options is what the egress proof needs, all of it from the release being applied.
type Options struct {
	// Namespace is the seedbox's own namespace, so the release's policy selects
	// the probe.
	Namespace string
	// Release is the Helm release name (app.kubernetes.io/instance); the probe
	// carries it, the control does not.
	Release string
	// Image is the s5n image the release runs, by tag and digest.
	Image string
	// Target is the in-cluster address the lock must deny.
	Target Target
	// Internet is a public address to check as well; a zero port skips it.
	Internet Target
	// Timeout bounds each pod, create to terminated; it must outlast Settle.
	Timeout time.Duration
	// Settle is how long a probe pod keeps dialling (s5n probe --settle): a CNI
	// programs a new pod's policy a moment after it starts, so one dial would
	// read an enforcing cluster as one that ignores policy. Zero dials once.
	Settle time.Duration
	// Poll is how often the pod's status is read.
	Poll time.Duration
}

// Report is what the egress proof saw, for the receipt and for a test.
type Report struct {
	// ControlReached is true when an unlabelled pod reached Target, so the
	// probe's failure means something.
	ControlReached bool
	// ProbeReached is true when a labelled pod reached Target: the lock is not
	// enforced, and ProveEgress returned an error.
	ProbeReached bool
	// Target is the address both pods dialled.
	Target Target
	// InternetChecked says the public check ran.
	InternetChecked bool
	// InternetReached is true when the labelled pod reached the public address.
	InternetReached bool
	// Note is one line the caller may print.
	Note string
	// CNI is the network implementation box could name, empty when it could not;
	// it only ever appears in a refusal.
	CNI string
}

// ProveEgress runs the control, the probe and, when asked, the public check. A
// nil error means the lock is in place: the control reached the target and the
// labelled probe did not. Every other answer names what box saw, because "could
// not prove it" and "not enforced" are different facts.
func ProveEgress(ctx context.Context, client objects, o Options) (Report, error) {
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

	control, err := runPod(ctx, client, o.Timeout, o.Poll, egressPod(o, "control", nil, o.Target, "open"))
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

	probe, err := runPod(ctx, client, o.Timeout, o.Poll, egressPod(o, "probe", map[string]string{"app.kubernetes.io/instance": o.Release}, o.Target, "closed"))
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
	inet, err := runPod(ctx, client, o.Timeout, o.Poll, egressPod(o, "internet", map[string]string{"app.kubernetes.io/instance": o.Release}, o.Internet, "closed"))
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

// check refuses options that cannot describe a proof, before any pod exists.
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

// dialTimeout is one attempt's limit: a denied connection is usually dropped, not
// refused, so this is how long a "closed" takes.
const dialTimeout = 2 * time.Second

// egressPod is one probe of the egress lock: the release's own command, and the
// label that decides whether the release's policy selects it.
func egressPod(o Options, role string, labels map[string]string, target Target, want string) *corev1.Pod {
	all := map[string]string{proofLabel: role}
	for k, v := range labels {
		all[k] = v
	}
	return newPod(o.Namespace, o.Image, "s5n-probe-"+role+"-", all, dialCommand(target, want, dialTimeout, o.Settle))
}

// dnsPort is CoreDNS's prometheus listener, which kube-dns exposes everywhere.
// The release's egress policy admits DNS on 53 only, so this is an address every
// cluster has that answers an unselected pod and that the lock must deny.
const dnsPort = 9153

// DNSTarget is the in-cluster address the lock must deny: the kube-dns Service's
// metrics port. The Service's address, not a CoreDNS pod's: the per-pod gate dials
// it for the life of the box, and a pod IP that went away would read as closed.
func DNSTarget(ctx context.Context, client objects) (Target, error) {
	list, err := client.Services(ctx, "kube-system")
	if err != nil {
		return Target{}, fmt.Errorf("read the cluster's DNS Service: %w", err)
	}
	for _, svc := range list.Items {
		if svc.Labels["k8s-app"] != "kube-dns" && svc.Name != "kube-dns" {
			continue
		}
		if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		for _, p := range svc.Spec.Ports {
			if p.Port == dnsPort {
				return Target{Host: svc.Spec.ClusterIP, Port: dnsPort}, nil
			}
		}
		return Target{}, fmt.Errorf("the cluster DNS Service kube-system/%s has no port %d: box needs CoreDNS's metrics port as the address the release's egress policy must deny, and without it box cannot prove the lock", svc.Name, dnsPort)
	}
	return Target{}, errors.New("this cluster has no DNS Service (k8s-app=kube-dns) in kube-system: box needs its metrics port as the address the release's egress policy must deny, and without one it cannot prove the lock")
}
