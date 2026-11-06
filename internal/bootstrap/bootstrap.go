// Package bootstrap decides which Kubernetes cluster box should use, in the
// fixed order PLAN.md specifies, and probes the platform layer's capabilities.
// Every dependency is a small interface, so the whole decision is testable
// without a cluster; the real (read-only) implementations live in this package
// beside the logic.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Action is what box decided to do about the cluster.
type Action string

const (
	// ActionUseCluster: an existing, manageable cluster will be used.
	ActionUseCluster Action = "use-cluster"
	// ActionUseManaged: the recorded k0s-managed cluster will be used; its
	// lifecycle (start, upgrade) belongs to box update, a later phase.
	ActionUseManaged Action = "use-managed"
	// ActionStop: box refuses to proceed; Reason explains why.
	ActionStop Action = "stop"
	// ActionInstall: a clean host; installing k0s is planned.
	ActionInstall Action = "install"
)

// Source records who owns the cluster's lifecycle.
type Source string

const (
	SourceExternal   Source = "external"
	SourceK0sManaged Source = "k0s-managed"
)

// Distribution is the Kubernetes distribution box detected. It only selects
// presets; the chart itself is distribution-agnostic.
type Distribution string

const (
	DistVanilla Distribution = "vanilla"
	DistK0s     Distribution = "k0s"
	DistK3s     Distribution = "k3s"
	DistKubeadm Distribution = "kubeadm"
	DistEKS     Distribution = "eks"
	DistGKE     Distribution = "gke"
)

// Version is the subset of /version the decision needs.
type Version struct {
	// GitVersion carries the server version, including the distribution
	// suffix: "v1.31.3+k3s1", "v1.30.5-eks-123abc", "v1.29.2-gke.100".
	GitVersion string
}

// Cluster is what the decision learns about a cluster it will use.
type Cluster struct {
	Nodes            int
	HasKubeadmConfig bool
	NodeLabels       map[string]string
}

// Finding is one piece of evidence that a cluster already exists on this host
// but is not usable through any discovered kubeconfig.
type Finding struct {
	Kind string // "process", "unit", "binary", "port"
	Name string // "kubelet", "k3s.service", "/usr/local/bin/k0s", "6443"
}

// InstallPlan is what box would install on a clean host.
type InstallPlan struct {
	// Version is the pinned k0s release. SHA256 stays empty until the real
	// installer (a later Phase 2 step) downloads and verifies the archive;
	// nothing in this phase fetches or installs anything.
	Version string
	SHA256  string
}

// Result is the full decision with its human-readable reasoning.
type Result struct {
	Action               Action
	Source               Source
	Kubeconfig           string
	Distribution         Distribution
	Nodes                int
	StorageClassRequired bool
	Platform             []ComponentResult
	// Skips records why each discovered candidate kubeconfig was not used.
	Skips  []string
	Reason string
}

// ErrRemoteCluster is returned by a prober that refuses to touch a cluster it
// does not consider local. box bootstrap --dry-run uses it so a remote
// production cluster is never probed.
var ErrRemoteCluster = errors.New("remote cluster: not probed")

// ErrNotExist marks a kubeconfig that is simply absent. It is fs.ErrNotExist so
// callers can errors.Is it.
var ErrNotExist = fs.ErrNotExist

// ConfigSource reads kubeconfig files from the local filesystem. Candidates
// lists where distributions put their admin kubeconfig; a candidate only counts
// after the prober validates it.
type ConfigSource interface {
	Candidates() []string
	Read(path string) ([]byte, error)
}

// Prober validates a cluster and probes its capabilities. All of its methods
// are read-only with respect to cluster state, except that a full
// LoadBalancer probe would create a throwaway Service — that one the real
// prober reports as not probed rather than writing.
type Prober interface {
	// Version returns the API server's version; an error means the kubeconfig
	// does not reach a live cluster.
	Version(ctx context.Context) (Version, error)
	// CanAdmin is nil when the identity can create namespaces, CRDs and
	// cluster roles; otherwise it returns an error naming what was denied.
	CanAdmin(ctx context.Context) error
	// Describe answers the cluster questions the decision needs once the
	// cluster is known good: node count and distribution evidence.
	Describe(ctx context.Context) (Cluster, error)
	// Platform probes the platform layer's four components.
	Platform(ctx context.Context) (PlatformProbe, error)
}

// Host reports evidence that a cluster is already installed on this host but
// not usable through any discovered kubeconfig. Read-only.
type Host interface {
	Present() []Finding
}

// RecordStore persists /etc/swizzin/cluster.yaml.
type RecordStore interface {
	// Load returns (nil, nil) when no record exists. An existing record that
	// cannot be read is an error: box must not install over what it cannot see.
	Load() (*Record, error)
	Save(*Record) error
}

// Installer provisions k0s on a clean host. In this phase the real
// implementation only prints what it would do; nothing installs anything.
type Installer interface {
	Install(ctx context.Context, plan InstallPlan) error
}

// K0sVersion is the k0s release box installs on a clean host. Pinned: the real
// installer (a later phase) downloads exactly this release and verifies it.
const K0sVersion = "v1.31.3+k0s.0"

// K0sAdminConfig is where k0s writes the admin kubeconfig box records and uses.
const K0sAdminConfig = "/var/lib/k0s/pki/admin.conf"

// RecordPath is where box records what it decided.
const RecordPath = "/etc/swizzin/cluster.yaml"

// FormatResult renders the decision for a human. It is what box bootstrap
// --dry-run prints.
func FormatResult(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "decision: %s\n", res.Action)
	switch res.Action {
	case ActionStop:
		writeSkips(&b, res.Skips)
		fmt.Fprintf(&b, "%s\n", res.Reason)
		return b.String()
	case ActionInstall:
		fmt.Fprintf(&b, "source: %s\n", res.Source)
		fmt.Fprintf(&b, "distribution: %s\n", res.Distribution)
		fmt.Fprintf(&b, "kubeconfig: %s\n", res.Kubeconfig)
		fmt.Fprintf(&b, "%s\n", res.Reason)
		return b.String()
	}
	// use-cluster / use-managed
	fmt.Fprintf(&b, "source: %s\n", res.Source)
	fmt.Fprintf(&b, "kubeconfig: %s\n", res.Kubeconfig)
	fmt.Fprintf(&b, "distribution: %s\n", res.Distribution)
	fmt.Fprintf(&b, "nodes: %d\n", res.Nodes)
	if res.StorageClassRequired {
		fmt.Fprintf(&b, "storage class required: yes (multi-node; hostPath is single-node only)\n")
	} else {
		fmt.Fprintf(&b, "storage class required: no\n")
	}
	writeSkips(&b, res.Skips)
	if len(res.Platform) > 0 {
		fmt.Fprintf(&b, "platform:\n")
		for _, c := range res.Platform {
			fmt.Fprintf(&b, "  %s: %s -> %s (%s)\n", c.Component, c.Mode, c.Action, c.Reason)
		}
	}
	if res.Reason != "" {
		fmt.Fprintf(&b, "%s\n", res.Reason)
	}
	return b.String()
}

func writeSkips(b *strings.Builder, skips []string) {
	if len(skips) == 0 {
		return
	}
	fmt.Fprintf(b, "skipped candidates:\n")
	for _, s := range skips {
		fmt.Fprintf(b, "  %s\n", s)
	}
}
