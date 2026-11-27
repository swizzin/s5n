package main

// netproof.go is the cluster check `box apply` makes before it installs an app on
// a cluster box did not install itself. The Arrs run External auth and
// qBittorrent skips its login for private addresses, so the chart's
// NetworkPolicies are the only thing keeping another pod out; a CNI that ignores
// them looks exactly like one that enforces them until a pod tries to get in.

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/kube"
	migrateapply "github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/netproof"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
)

// networkPolicyProof is apply's hook, built from the kubeconfig and the record.
func networkPolicyProof(kc, recordPath string, out say.Reporter) release.NetworkProof {
	records := bootstrap.FilesystemRecords{Path: recordPath}
	return func(ctx context.Context, namespace string, chart release.Chart, values map[string]any) error {
		return proveOnceNetworkPolicy(records, func() error {
			return proveNetworkPolicy(ctx, kc, namespace, chart, values, out)
		})
	}
}

// proveOnceNetworkPolicy decides from the record, proves, and keeps the answer.
// proof is a parameter so every path is tested without a cluster.
func proveOnceNetworkPolicy(records bootstrap.RecordStore, proof func() error) error {
	rec, err := records.Load()
	if err != nil {
		return err
	}
	if rec != nil && (rec.Source == bootstrap.SourceK0sManaged || rec.NetworkPolicyProved) {
		// k0s comes with kube-router, which enforces policy; and a cluster already
		// proved is not proved again by every apply.
		return nil
	}
	if err := proof(); err != nil {
		return err
	}
	if rec == nil {
		// No record is a cluster bootstrap never decided on, and apply writes none:
		// the next apply proves again rather than inventing what box chose.
		return nil
	}
	rec.NetworkPolicyProved = true
	return records.Save(rec)
}

// proveNetworkPolicy runs the ingress proof with the release's own image, which
// the image policy blesses and the cluster already has.
func proveNetworkPolicy(ctx context.Context, kc, namespace string, chart release.Chart, values map[string]any, out say.Reporter) error {
	image, err := migrateapply.WiringImage(chart.Variants, chart.AppVersion, values)
	if err != nil {
		return err
	}
	cfg, err := kube.Config(kc)
	if err != nil {
		return fmt.Errorf("kubeconfig %s: %w", kc, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("cluster client: %w", err)
	}
	h := out.Start(say.Op{
		Label: "proving the network policy",
		Plain: "network policy: proving a deny-all policy is enforced in " + namespace,
	})
	rep, err := netproof.ProveIngress(ctx, netproof.Cluster{Client: cs}, netproof.IngressOptions{
		Namespace: namespace,
		Image:     image,
		// Same shape as the lock proof: a CNI programs a new pod's policy a moment
		// after it starts, so each dial settles before its answer counts.
		Settle:  20 * time.Second,
		Timeout: 90 * time.Second,
		Poll:    time.Second,
	})
	if err != nil {
		h.Failed(err)
		return err
	}
	done := "network policy: enforced"
	if rep.CNI != "" {
		done += " (" + rep.CNI + ")"
	}
	h.Done(done)
	return nil
}
