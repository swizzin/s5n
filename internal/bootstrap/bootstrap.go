// Package bootstrap decides which Kubernetes cluster box should use, in PLAN.md's
// fixed order, and probes the platform layer. Every dependency is a small
// interface, so the decision is testable without a cluster; the real (read-only)
// implementations live here beside the logic.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/swizzin/s5n/internal/hostroot"
	"github.com/swizzin/s5n/internal/say"
)

// Action is what box decided to do about the cluster.
type Action string

const (
	// ActionUseCluster: an existing, manageable cluster will be used.
	ActionUseCluster Action = "use-cluster"
	// ActionUseManaged: the recorded k0s-managed cluster will be used; its lifecycle
	// belongs to box update.
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

// Distribution is the distribution box detected: it selects presets and lets box
// refuse a managed cluster by name. The chart itself is distribution-agnostic.
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
	// GitVersion is the server version with its distribution suffix: "v1.31.3+k3s1",
	// "v1.30.5-eks-123abc".
	GitVersion string
}

// Cluster is what the decision learns about a cluster it will use.
type Cluster struct {
	Nodes            int
	HasKubeadmConfig bool
	NodeLabels       map[string]string
	// NodeIPs are the InternalIPs the nodes registered, read for the one warning they
	// can give: a node on a tunnel this host can see (see nodeip.go).
	NodeIPs []string
}

// Finding is evidence that a cluster exists on this host but is not usable through
// any discovered kubeconfig.
type Finding struct {
	Kind string // "process", "unit", "binary", "port", "kubeconfig"
	Name string // "kubelet", "k3s.service", "/usr/local/bin/k0s", "6443"
}

// InstallPlan is what box installs on a clean host.
type InstallPlan struct {
	// Version and SHA256 are the k0s release the chart pins and its sha256, filled
	// from k0sPlan() for this architecture. One pin in one file: the installer refuses
	// a plan with no checksum and verifies the download against this one.
	Version string
	SHA256  string
	// Arch is the k0s build's GOARCH (amd64 or arm64).
	Arch string
	// CleanHost is set only by the branch that found no usable cluster: the installer
	// refuses a plan without it, so a wrong path cannot install.
	CleanHost bool
	// NodeIP is the address the node registers with, Uplink the link it came from. box
	// chooses it before the first byte is fetched (see nodeip.go); the installer
	// refuses a plan without it, because kube-router derives every pod's MTU from it
	// and leaving it to the host ends a node on a tunnel's 1280.
	NodeIP string
	Uplink Uplink
	// ConfigPath and ConfigText are the k0s config the installer writes: k0s has no
	// install flag for spec.api.address, so this is the only way to pin the API to the
	// same address. k0sPlan builds it, so the decision says what it contains.
	ConfigPath string
	ConfigText string
}

// Result is the full decision with its human-readable reasoning.
type Result struct {
	Action       Action
	Source       Source
	Kubeconfig   string
	Distribution Distribution
	Nodes        int
	Platform     []ComponentResult
	// Gateway is the class box's own Gateway attaches to: a decision about the
	// cluster, not about the plan.
	Gateway GatewayChoice
	// Skips records why each discovered candidate kubeconfig was not used.
	Skips []string
	// Notes records host evidence that is not a reason to stop: a Docker or containerd
	// daemon no cluster port depends on.
	Notes []string
	// Warnings records what box saw and will not change: a node whose InternalIP sits
	// on a tunnel this host can see, which gives every pod that tunnel's MTU.
	Warnings []string
	// Firewall is what box found: whether firewalld runs, the cluster's pod and
	// service networks the platform plan will trust, or the warning for a host whose
	// firewall is not box's to change.
	Firewall FirewallState
	Reason   string
}

// ErrRemoteCluster is returned by a prober refusing a cluster it does not consider
// local: a dry run never probes a remote production cluster.
var ErrRemoteCluster = errors.New("remote cluster: not probed")

// ErrNotExist marks a kubeconfig that is simply absent. It is fs.ErrNotExist so
// callers can errors.Is it.
var ErrNotExist = fs.ErrNotExist

// Candidate is one kubeconfig path box will try. DistributionOwned marks the paths a
// distribution writes for its cluster: one that is unusable is evidence of a broken
// installation, not a stray file. ~/.kube/config is often a remote cluster, so an
// unusable one is only a skip.
type Candidate struct {
	Path              string
	DistributionOwned bool
}

// ConfigSource reads kubeconfig files. Candidates lists where distributions put
// their admin kubeconfig; a candidate counts only after the prober validates it.
type ConfigSource interface {
	Candidates() []Candidate
	Read(path string) ([]byte, error)
}

// Prober validates a cluster and probes its capabilities, all read-only. The
// LoadBalancer capability cannot be settled that way -- it is box's own Gateway that
// gets an address, and it exists only after bootstrap writes it -- so the real prober
// reports it not probed and the platform decision settles it after the confirm.
type Prober interface {
	// Version is the API server's version; an error means the kubeconfig does not
	// reach a live cluster.
	Version(ctx context.Context) (Version, error)
	// CanAdmin is nil when the identity can create namespaces, CRDs and cluster
	// roles, else an error naming what was denied.
	CanAdmin(ctx context.Context) error
	// Describe answers what the decision needs once the cluster is good: node count
	// and distribution evidence.
	Describe(ctx context.Context) (Cluster, error)
	// Platform probes the platform layer's four components.
	Platform(ctx context.Context) (PlatformProbe, error)
}

// Host reports evidence that a cluster is on this host but unusable. Read-only.
type Host interface {
	Present() []Finding
}

// RecordStore persists box's cluster record, hostroot.RecordPath by default.
type RecordStore interface {
	// Load returns (nil, nil) when no record exists. An existing record that
	// cannot be read is an error: box must not install over what it cannot see.
	Load() (*Record, error)
	Save(*Record) error
	// RecordPath reports where the record lives: a refusal naming it names the file an
	// operator removes deliberately, and --record can move it.
	RecordPath() string
}

// Installer provisions k0s on a clean host: download the pinned release, verify it,
// install it, start a single-node controller, wait for the API. The only part of
// bootstrap that changes the host.
type Installer interface {
	Install(ctx context.Context, plan InstallPlan) error
}

// conditionTrue is a condition's status when it holds: the API spells conditions as
// strings.
const conditionTrue = "True"

// K0sAdminConfig is where k0s writes the admin kubeconfig box records and uses.
const K0sAdminConfig = "/var/lib/k0s/pki/admin.conf"

// RecordPath is where box records what it decided.
const RecordPath = hostroot.RecordPath

// FormatResult renders the decision for a human, as --dry-run prints it.
func FormatResult(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "decision: %s\n", res.Action)
	switch res.Action {
	case ActionStop:
		writeSkips(&b, res.Skips)
		writeNotes(&b, res.Notes)
		writeWarnings(&b, res.Warnings)
		fmt.Fprintf(&b, "%s\n", res.Reason)
		return b.String()
	case ActionInstall:
		fmt.Fprintf(&b, "source: %s\n", res.Source)
		fmt.Fprintf(&b, "distribution: %s\n", res.Distribution)
		fmt.Fprintf(&b, "kubeconfig: %s\n", res.Kubeconfig)
		writeSkips(&b, res.Skips)
		writeNotes(&b, res.Notes)
		writeWarnings(&b, res.Warnings)
		fmt.Fprintf(&b, "%s\n", res.Reason)
		return b.String()
	}
	// use-cluster / use-managed
	fmt.Fprintf(&b, "source: %s\n", res.Source)
	fmt.Fprintf(&b, "kubeconfig: %s\n", res.Kubeconfig)
	fmt.Fprintf(&b, "distribution: %s\n", res.Distribution)
	fmt.Fprintf(&b, "nodes: %d\n", res.Nodes)
	writeSkips(&b, res.Skips)
	writeWarnings(&b, res.Warnings)
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

// writeWarnings prints what box saw and will not change, with the `!` mark: a note is
// evidence that does not matter here, a warning is something the operator must fix
// where box does not write.
func writeWarnings(b *strings.Builder, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(b, "%s %s\n", say.MarkNeeds, w)
	}
}

// ReportResult is what `box bootstrap` says about the cluster it found, through the
// output seam: one row for the decision, the evidence in the log. --dry-run still
// prints FormatResult, because a dry run exists to write the decision out.
func ReportResult(res Result, r say.Reporter) {
	if res.Action == ActionStop {
		reportStop(res, r)
		return
	}
	r.Rows([]say.Row{clusterRow(res)})
	for _, w := range res.Warnings {
		r.Warn(w)
	}
	r.Detail("decision: " + string(res.Action))
	r.Detail("source: " + string(res.Source))
	if res.Kubeconfig != "" {
		r.Detail("kubeconfig: " + res.Kubeconfig)
	}
	r.Detail("distribution: " + string(res.Distribution))
	if res.Action != ActionInstall {
		r.Detail(fmt.Sprintf("nodes: %d", res.Nodes))
	}
	for _, line := range res.Skips {
		r.Detail("skipped: " + line)
	}
	for _, note := range res.Notes {
		r.Detail("note: " + note)
	}
	for _, c := range res.Platform {
		r.Detail(fmt.Sprintf("platform %s: %s -> %s (%s)", c.Component, c.Mode, c.Action, c.Reason))
	}
	if res.Gateway.Class != "" {
		line := fmt.Sprintf("gateway class: %s", res.Gateway.Class)
		if res.Gateway.ControllerName != "" {
			line += " (" + res.Gateway.ControllerName + ")"
		}
		if res.Gateway.BoxInstalls {
			line += ", installed by box"
		}
		r.Detail(line)
	}
	r.Detail(res.Reason)
}

// clusterRow is the line a bootstrap starts with: what the cluster is and what box
// does about it.
func clusterRow(res Result) say.Row {
	dist := string(res.Distribution)
	if dist == "" {
		dist = "a cluster"
	}
	switch res.Action {
	case ActionInstall:
		return say.Row{Mark: say.MarkDone, Facts: []string{"clean host", "box installs k0s"}}
	case ActionUseManaged:
		return say.Row{Mark: say.MarkDone, Facts: []string{dist + ", " + plural(res.Nodes, "node", "nodes"), "box manages it"}}
	default:
		return say.Row{Mark: say.MarkDone, Facts: []string{dist + ", " + plural(res.Nodes, "node", "nodes"), "box uses it as it is"}}
	}
}

// reportStop draws a refusal: the fact on the `✗` row, the decision's paragraphs
// under it two columns in. Skipped candidates are the log's.
func reportStop(res Result, r say.Reporter) {
	lines := strings.Split(strings.TrimRight(res.Reason, "\n"), "\n")
	rows := []say.Row{{Mark: say.MarkFailed, Facts: []string{strings.TrimSpace(lines[0])}}}
	for _, line := range lines[1:] {
		if text := strings.TrimSpace(line); text != "" {
			// A continuation carries a space for a mark: two columns in.
			rows = append(rows, say.Row{Mark: " ", Facts: []string{text}})
		}
	}
	r.Rows(rows)
	for _, w := range res.Warnings {
		r.Warn(w)
	}
	for _, line := range res.Skips {
		r.Detail("skipped: " + line)
	}
	for _, note := range res.Notes {
		r.Detail("note: " + note)
	}
}

// plural is a count and the word for it: one node, two nodes.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
