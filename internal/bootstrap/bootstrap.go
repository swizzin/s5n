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

	"github.com/swizzin/s5n/internal/hostroot"
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
	// NodeIPs are the InternalIPs the nodes registered, in the API's order.
	// They are evidence about the cluster, not about its health: the decision
	// reads them for the one warning it can give -- a node whose address sits
	// on a tunnel this host can see (see nodeip.go).
	NodeIPs []string
}

// Finding is one piece of evidence that a cluster already exists on this host
// but is not usable through any discovered kubeconfig.
type Finding struct {
	Kind string // "process", "unit", "binary", "port", "kubeconfig"
	Name string // "kubelet", "k3s.service", "/usr/local/bin/k0s", "6443"
}

// InstallPlan is what box installs on a clean host.
type InstallPlan struct {
	// Version and SHA256 are the k0s release the chart pins and the sha256 the
	// same pins carry beside it, filled from k0sPlan() for this binary's
	// architecture. They are one pin in one file, so nothing here resolves the
	// release a second time; the installer refuses a plan that carries no
	// checksum, and verifies the download against this one.
	Version string
	SHA256  string
	// Arch is the k0s build's GOARCH (amd64 or arm64).
	Arch string
	// CleanHost is set only by the branch that found no usable cluster and
	// chose to install. The installer refuses a plan without it, so an
	// install cannot happen because a caller took the wrong path by accident.
	CleanHost bool
	// NodeIP is the address the node registers with, and Uplink is the link it
	// came from. box chooses it before the first byte is fetched (see
	// nodeip.go) and the installer refuses a plan without it: the address is
	// what kube-router derives every pod's MTU from, so leaving it to the
	// host is how a node ends up on a tunnel's 1280.
	NodeIP string
	Uplink Uplink
	// ConfigPath and ConfigText are the k0s config the installer writes on the
	// way in. k0s has no install flag for spec.api.address, so the config is
	// the only way to pin the API to the same address; the text is built by
	// k0sPlan so the decision, not the installer, says what it contains.
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
	// Gateway is the class box's own Gateway attaches to. It is chosen here
	// because it is a decision about the cluster, not about the plan.
	Gateway GatewayChoice
	// Skips records why each discovered candidate kubeconfig was not used.
	Skips []string
	// Notes records host evidence that is not, by itself, a reason to stop:
	// a Docker or containerd daemon that no cluster port depends on.
	Notes []string
	// Warnings records what box saw about the cluster that the operator should
	// know and box will not change: today, a node whose InternalIP sits on a
	// tunnel this host can see, which gives every pod that tunnel's MTU.
	Warnings []string
	// Firewall is what box found about the host's packet filter: whether
	// firewalld is running, the cluster's pod and service networks the
	// platform plan will trust, or the warning for a host whose firewall is
	// not box's to change.
	Firewall FirewallState
	Reason   string
}

// ErrRemoteCluster is returned by a prober that refuses to touch a cluster it
// does not consider local. box bootstrap --dry-run uses it so a remote
// production cluster is never probed.
var ErrRemoteCluster = errors.New("remote cluster: not probed")

// ErrNotExist marks a kubeconfig that is simply absent. It is fs.ErrNotExist so
// callers can errors.Is it.
var ErrNotExist = fs.ErrNotExist

// Candidate is one kubeconfig path box will try. DistributionOwned marks the
// paths a distribution writes for the cluster it installed: one that exists but
// is not usable is evidence of a broken installation, not a stray file. The
// user's ~/.kube/config is not distribution-owned; it is often a remote
// cluster, so an unusable one is only a skip.
type Candidate struct {
	Path              string
	DistributionOwned bool
}

// ConfigSource reads kubeconfig files from the local filesystem. Candidates
// lists where distributions put their admin kubeconfig; a candidate only counts
// after the prober validates it.
type ConfigSource interface {
	Candidates() []Candidate
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

// RecordStore persists box's cluster record, hostroot.RecordPath by default.
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

// conditionTrue is a condition's status when it holds: the API spells conditions
// as strings, and box reads three of them.
const conditionTrue = "True"

// K0sAdminConfig is where k0s writes the admin kubeconfig box records and uses.
const K0sAdminConfig = "/var/lib/k0s/pki/admin.conf"

// RecordPath is where box records what it decided.
const RecordPath = hostroot.RecordPath

// FormatResult renders the decision for a human. It is what box bootstrap
// --dry-run prints.
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

// writeWarnings prints what box saw but will not change. A warning is not a
// note: a note is host evidence that does not matter here, a warning is
// something wrong the operator has to fix somewhere box does not write, so it
// carries the `!` mark in the gutter rather than a word in front of it.
func writeWarnings(b *strings.Builder, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(b, "%s %s\n", say.MarkNeeds, w)
	}
}

// ReportResult is what `box bootstrap` says about the cluster it found, through
// the output seam: one row for the decision, and the evidence behind it in the
// log. FormatResult is still what --dry-run prints: a dry run's whole point is
// the decision written out, and it writes nothing else.
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

// clusterRow is the one line a bootstrap starts with: what the cluster is, and
// what box does about it. The distribution, the node count and the source are
// the three facts that decide everything after it.
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

// reportStop draws a refusal: the fact on the `✗` row, then whatever the
// decision said under it, two columns in, so the paragraphs a stop carries read
// as part of the row rather than as loose text. The candidates box skipped are
// the log's: a person acting on the refusal does not need them.
func reportStop(res Result, r say.Reporter) {
	lines := strings.Split(strings.TrimRight(res.Reason, "\n"), "\n")
	rows := []say.Row{{Mark: say.MarkFailed, Facts: []string{strings.TrimSpace(lines[0])}}}
	for _, line := range lines[1:] {
		if text := strings.TrimSpace(line); text != "" {
			// A continuation carries a space for a mark: two columns in, under
			// the text, which is where a wrapped line hangs.
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
