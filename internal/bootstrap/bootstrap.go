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

	"github.com/swizzin/s5n/internal/say"
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
// presets and lets box refuse a managed cluster by name; the chart itself is
// distribution-agnostic.
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

// InstallPlan is what box installs on a clean host.
type InstallPlan struct {
	// Version and SHA256 are the pinned release and its checksum, filled from
	// the policy in policy.go for this binary's architecture. The installer
	// re-resolves the pin and refuses a plan that names anything else.
	Version string
	SHA256  string
	// Arch is the k0s build's GOARCH (amd64 or arm64).
	Arch string
	// CleanHost is set only by the branch that found no usable cluster and
	// chose to install. The installer refuses a plan without it, so an
	// install cannot happen because a caller took the wrong path by accident.
	CleanHost bool
}

// Result is the full decision with its human-readable reasoning.
type Result struct {
	Action       Action
	Source       Source
	Kubeconfig   string
	Distribution Distribution
	Nodes        int
	Platform     []ComponentResult
	// Gateway is the class box's own Gateway attaches to. It is chosen here
	// because it is a decision about the cluster, not about the plan.
	Gateway GatewayChoice
	// Skips records why each discovered candidate kubeconfig was not used.
	Skips []string
	// Notes records host evidence that is not, by itself, a reason to stop:
	// a Docker or containerd daemon that no cluster port depends on.
	Notes  []string
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
// are read-only with respect to cluster state. The LoadBalancer capability
// cannot be settled that way -- it is box's own Gateway that has to get an
// address, and the Gateway only exists after `box bootstrap` writes it -- so
// the real prober reports it as not probed and the platform decision settles
// it after the confirm.
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
	// RecordPath reports where the record lives. A refusal that hinges on the
	// record names the file an operator would remove deliberately, and --record
	// can move it off the default.
	RecordPath() string
}

// Installer provisions k0s on a clean host: it downloads the pinned release,
// verifies it, installs it and starts a single-node controller, then waits for
// the API to answer. It is the only part of bootstrap that changes the host.
type Installer interface {
	Install(ctx context.Context, plan InstallPlan) error
}

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
		writeNotes(&b, res.Notes)
		fmt.Fprintf(&b, "%s\n", res.Reason)
		return b.String()
	case ActionInstall:
		fmt.Fprintf(&b, "source: %s\n", res.Source)
		fmt.Fprintf(&b, "distribution: %s\n", res.Distribution)
		fmt.Fprintf(&b, "kubeconfig: %s\n", res.Kubeconfig)
		writeSkips(&b, res.Skips)
		writeNotes(&b, res.Notes)
		fmt.Fprintf(&b, "%s\n", res.Reason)
		return b.String()
	}
	// use-cluster / use-managed
	fmt.Fprintf(&b, "source: %s\n", res.Source)
	fmt.Fprintf(&b, "kubeconfig: %s\n", res.Kubeconfig)
	fmt.Fprintf(&b, "distribution: %s\n", res.Distribution)
	fmt.Fprintf(&b, "nodes: %d\n", res.Nodes)
	writeSkips(&b, res.Skips)
	if len(res.Platform) > 0 {
		fmt.Fprintf(&b, "platform:\n")
		for _, c := range res.Platform {
			fmt.Fprintf(&b, "  %s: %s -> %s (%s)\n", c.Component, c.Mode, c.Action, c.Reason)
		}
	}
	if res.Gateway.Class != "" {
		line := fmt.Sprintf("gateway class: %s", res.Gateway.Class)
		if res.Gateway.ControllerName != "" {
			line += " (" + res.Gateway.ControllerName + ")"
		}
		if res.Gateway.BoxInstalls {
			line += ", installed by box"
		}
		fmt.Fprintf(&b, "%s\n", line)
	}
	if res.Gateway.FeaturesChecked {
		fmt.Fprintf(&b, "gateway features: %s is supported\n", FeaturePathRewrite)
	} else if res.Gateway.Assumed {
		fmt.Fprintf(&b, "gateway features: GatewayClass %s publishes none; continuing because platform.gateway.assumeFeatures is set\n", res.Gateway.Class)
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

func writeNotes(b *strings.Builder, notes []string) {
	for _, n := range notes {
		fmt.Fprintf(b, "note: %s\n", n)
	}
}

// ReportResult is FormatResult through the output seam: the same facts, stated
// as events so a live renderer can lay them out as rows and a wrapped list.
// Under say.Plain it is byte-for-byte FormatResult, which is what the tests and
// every piped run read; TestReportResultMatchesFormatResult pins that.
func ReportResult(res Result, r say.Reporter) {
	switch res.Action {
	case ActionStop:
		r.Fact("decision", string(res.Action))
		reportSkips(r, res.Skips)
		reportNotes(r, res.Notes)
		r.Line(res.Reason)
		return
	case ActionInstall:
		r.Fact("decision", string(res.Action))
		r.Fact("source", string(res.Source))
		r.Fact("distribution", string(res.Distribution))
		r.Fact("kubeconfig", res.Kubeconfig)
		reportSkips(r, res.Skips)
		reportNotes(r, res.Notes)
		// The facts above already name the source, the distribution and the
		// kubeconfig; the reason says why, in one muted sentence instead of a
		// second raw line. The plain transcript is unchanged: Plain.Muted and
		// Plain.Line print the same words.
		r.Muted(res.Reason)
		return
	}
	// use-cluster / use-managed
	r.Fact("decision", string(res.Action))
	r.Fact("source", string(res.Source))
	r.Fact("kubeconfig", res.Kubeconfig)
	r.Fact("distribution", string(res.Distribution))
	r.Fact("nodes", fmt.Sprintf("%d", res.Nodes))
	reportSkips(r, res.Skips)
	if len(res.Platform) > 0 {
		rows := make([]string, 0, len(res.Platform))
		for _, c := range res.Platform {
			rows = append(rows, fmt.Sprintf("%s: %s -> %s (%s)", c.Component, c.Mode, c.Action, c.Reason))
		}
		r.List("platform", rows)
	}
	if res.Gateway.Class != "" {
		line := fmt.Sprintf("gateway class: %s", res.Gateway.Class)
		if res.Gateway.ControllerName != "" {
			line += " (" + res.Gateway.ControllerName + ")"
		}
		if res.Gateway.BoxInstalls {
			line += ", installed by box"
		}
		r.Line(line)
	}
	if res.Gateway.FeaturesChecked {
		r.Line(fmt.Sprintf("gateway features: %s is supported", FeaturePathRewrite))
	} else if res.Gateway.Assumed {
		r.Line(fmt.Sprintf("gateway features: GatewayClass %s publishes none; continuing because platform.gateway.assumeFeatures is set", res.Gateway.Class))
	}
	if res.Reason != "" {
		r.Line(res.Reason)
	}
}

func reportSkips(r say.Reporter, skips []string) {
	if len(skips) > 0 {
		r.List("skipped candidates", skips)
	}
}

func reportNotes(r say.Reporter, notes []string) {
	for _, n := range notes {
		r.Line("note: " + n)
	}
}
