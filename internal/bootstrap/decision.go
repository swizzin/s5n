package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/release"
)

// Decision runs box bootstrap's cluster detection and platform probes. It
// depends only on the interfaces in bootstrap.go, so the whole decision is
// testable without a cluster.
type Decision struct {
	Source    ConfigSource
	NewProber func(kubeconfig []byte) (Prober, error)
	Host      Host
	Records   RecordStore
	Install   Installer
	// Pins is the platform pins of the chart release box is installing
	// (charts/s5n/platform.yaml, read by internal/release). bootstrap installs
	// k0s at the release's pin, so it pulls the release before it touches the
	// host. It is a function because the pull is only needed on the one
	// branch that installs: a host that already has a cluster never asks, and
	// the caller -- `box bootstrap` hands the same release to init -- loads it
	// once for the whole command.
	Pins func(ctx context.Context) (release.Versions, error)
	// Prompt asks which GatewayClass to use when more than one is Accepted.
	// Nil means unattended: the decision then refuses and names the setting,
	// because choosing an implementation is the operator's call, never box's.
	Prompt ClassChooser
	// Net reports this host's default route and interfaces. It is read where
	// box installs k0s (to pin the node IP) and where box warns about a node
	// address it can see locally. Nil means the running host; a test fakes it.
	Net Net
}

// Options carries the explicit cluster and platform modes from the CLI, plus
// the Gateway decision that is not a mode: which class to attach to.
type Options struct {
	Kubeconfig    string // --kubeconfig; empty when not given
	KubeconfigEnv string // KUBECONFIG; empty when not set
	Platform      PlatformConfig
	// GatewayClass is platform.gateway.className: the Envoy Gateway
	// GatewayClass to use when several are Accepted. Empty lets box use the
	// only Accepted one, or ask.
	GatewayClass string
	// DryRun is box bootstrap --dry-run: probe no cluster and change
	// nothing. On a clean host it still reads the chart release, because the
	// k0s it would print comes from the release's pins. A clean host gets the
	// install plan printed and stops there, because the cluster to adopt does
	// not exist and must not be probed.
	DryRun bool
}

// Bootstrap decides which cluster to use, in PLAN.md's fixed order, and never
// installs a second cluster over a broken first one.
func (d *Decision) Bootstrap(ctx context.Context, opts Options) (Result, error) {
	res := Result{}

	// The record is read before any probing, so a re-run short-circuits the
	// probe-and-install path. A record that exists but cannot be read is a
	// fault, not a clean host: box must not install over what it cannot see.
	rec, err := d.Records.Load()
	if err != nil {
		return res, fmt.Errorf("read cluster record: %w", err)
	}

	// 1. An explicit cluster: use it or fail, never fall back. When a record
	// already exists it decides what an explicit path means. The record's own
	// kubeconfig is the recorded re-run (the e2e passes both flags every run,
	// and a k0s-managed record must stay managed), while a different file is a
	// move to another cluster that only the operator may make: stop before any
	// probe or write rather than replacing the record and resetting its
	// decisions.
	if path := firstKubeconfig(opts.Kubeconfig, opts.KubeconfigEnv); path != "" {
		if rec != nil {
			if filepath.Clean(path) == filepath.Clean(rec.Kubeconfig) {
				return d.useRecord(ctx, rec), nil
			}
			return stop(fmt.Sprintf(
				"explicit kubeconfig %s does not match the cluster record at %s, which names %s (%s): box will not switch this host to another cluster. Unset KUBECONFIG or drop --kubeconfig to use the recorded cluster, or remove %s deliberately to move box to another cluster.",
				path, d.Records.RecordPath(), rec.Kubeconfig, rec.Source, d.Records.RecordPath())), nil
		}
		kc, err := d.Source.Read(path)
		if err != nil {
			return stop(fmt.Sprintf("explicit kubeconfig %s: %v", path, err)), nil
		}
		return d.useCluster(ctx, opts, kc, path, SourceExternal, "explicit cluster"), nil
	}

	// A recorded cluster short-circuits the probe-and-install path on a re-run.
	if rec != nil {
		return d.useRecord(ctx, rec), nil
	}

	// 2. A cluster on this host, at a distribution's kubeconfig path.
	var skips []string
	var dead []Finding
	for _, cand := range d.Source.Candidates() {
		kc, err := d.Source.Read(cand.Path)
		if err != nil {
			continue // absent candidate, not a skip
		}
		probe, err := d.NewProber(kc)
		if err != nil {
			skips = append(skips, fmt.Sprintf("%s: %v", cand.Path, err))
			dead = appendDeadCandidate(dead, cand, err)
			continue
		}
		if _, err := probe.Version(ctx); err != nil {
			skips = append(skips, describeSkip(cand.Path, "version", err))
			dead = appendDeadCandidate(dead, cand, err)
			continue
		}
		if err := probe.CanAdmin(ctx); err != nil {
			skips = append(skips, describeSkip(cand.Path, "privileges", err))
			dead = appendDeadCandidate(dead, cand, err)
			continue
		}
		return d.useCluster(ctx, opts, kc, cand.Path, SourceExternal, "cluster on this host"), nil
	}

	// A cancelled run is not evidence about the host. Probing cut short by a
	// signal reaches here as a dead candidate ("version: context canceled"),
	// and the evidence below would read a host with a cluster as either broken
	// or clean -- the second one installs k0s on it. Report the cancellation
	// through the error path every failed command already has.
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 3. Something present but not usable. A distribution-owned kubeconfig that
	// exists but cannot be used is part of this evidence: the distribution
	// wrote it when it installed a cluster, so a dead one means that cluster is
	// still on the host. Docker and containerd are excluded
	// from the blockers: they also run on hosts with no Kubernetes, which are
	// exactly the clean k0s targets. They stay as notes, and a runtime holding
	// an API port is caught by the listening-port finding.
	blockers, notes := classifyFindings(d.Host.Present())
	blockers = append(blockers, dead...)
	if len(blockers) > 0 {
		res := stop("a cluster appears to be present on this host but is not usable through any kubeconfig:")
		res.Skips = skips
		res.Notes = describeNotes(notes)
		res.Reason += "\n  " + formatFindings(blockers) +
			"\nbox will not install a second cluster over a broken first one."
		return res, nil
	}

	// 4. A clean host: install k0s, then adopt the cluster box just started.
	// The plan carries the affirmative choice, so nothing that is not this
	// branch can reach the installer's download. Which k0s is the chart
	// release's pin: bootstrap pulls the release before it touches the host,
	// so a bump of the pin is a chart release rather than a new box.
	pins, err := d.platformPins(ctx)
	if err != nil {
		return stop(err.Error()), nil
	}
	// The node IP is chosen here, before anything is downloaded: a host whose
	// default route leaves through a tunnel is refused with the paragraph that
	// says what a node on it would do, and a host with no default route is
	// refused because there is nothing to pin.
	up, err := d.net().Uplink()
	if err != nil {
		return stop(fmt.Sprintf("box pins the node IP to this host's uplink before it installs k0s, and cannot find one: %v", err)), nil
	}
	if why := tunnelRefusal(up); why != "" {
		return stop(why), nil
	}
	plan := k0sPlan(pins, up)
	if err := d.Install.Install(ctx, plan); err != nil {
		return stop(fmt.Sprintf("k0s install failed: %v", err)), nil
	}
	if opts.DryRun {
		return Result{
			Action:       ActionInstall,
			Source:       SourceK0sManaged,
			Kubeconfig:   K0sAdminConfig,
			Distribution: DistK0s,
			Skips:        skips,
			Notes:        describeNotes(notes),
			Reason:       fmt.Sprintf("clean host: k0s %s would be installed (dry run)", plan.Version),
		}, nil
	}
	res = d.adoptInstalled(ctx, opts, plan.Version)
	res.Skips = skips
	res.Notes = describeNotes(notes)
	return res, nil
}

// net is the host's routes and links, or the real host's when none was given.
func (d *Decision) net() Net {
	if d.Net != nil {
		return d.Net
	}
	return LinuxNet{}
}

// platformPins is the release's pins, or the refusal that says what is missing.
// There is no fallback: a compiled-in version would be a second home for the
// platform, and this one is the release's.
func (d *Decision) platformPins(ctx context.Context) (release.Versions, error) {
	if d.Pins == nil {
		return release.Versions{}, errors.New("no chart release: bootstrap installs the k0s the release pins, and box was given no way to read it")
	}
	pins, err := d.Pins(ctx)
	if err != nil {
		return release.Versions{}, fmt.Errorf("the chart release's platform pins: %w", err)
	}
	if pins.K0s == "" {
		return release.Versions{}, errors.New("the chart release pins no k0s version: box installs the k0s its release names in charts/s5n/platform.yaml")
	}
	return pins, nil
}

// adoptInstalled is the second half of the clean-host path. k0s is up, so
// there is a cluster: the same probe every other path uses decides its
// distribution, its platform layer and its Gateway class, and only then is it
// recorded. A cluster box started but cannot reach is never written down as one
// box manages, and it is never probed for capabilities before it answers.
func (d *Decision) adoptInstalled(ctx context.Context, opts Options, k0s string) Result {
	kc, err := d.Source.Read(K0sAdminConfig)
	if err != nil {
		return stop(fmt.Sprintf("read the kubeconfig k0s wrote (%s): %v", K0sAdminConfig, err))
	}
	res := d.useCluster(ctx, opts, kc, K0sAdminConfig, SourceK0sManaged, "the cluster box just installed")
	if res.Action != ActionUseCluster {
		return res
	}
	res.Action = ActionInstall
	res.Reason = fmt.Sprintf("clean host: k0s %s installed; using %s (%s)", k0s, K0sAdminConfig, res.Distribution)
	res.Platform = freshClusterAddresses(res.Platform)
	return res
}

// freshClusterAddresses settles the address question on a cluster box has just
// started. k0s ships nothing that hands out LoadBalancer addresses, and nothing
// else has had a chance to be installed, so in auto mode the Gateway would only
// report AddressNotAssigned after the wait: MetalLB is planned as an install
// up front instead of a question. Only this run knows that. The record keeps
// the configured mode, so a later run asks the Gateway as usual, and external
// and managed modes are left as the operator set them.
func freshClusterAddresses(components []ComponentResult) []ComponentResult {
	out := append([]ComponentResult(nil), components...)
	for i, c := range out {
		if c.Component == ComponentLoadBalancer && c.Mode == ModeAuto && c.Action == "probe" {
			out[i].Action = "install"
			out[i].Reason = "box just installed k0s, which hands out no LoadBalancer addresses; box installs MetalLB"
		}
	}
	return out
}

// useCluster validates an existing cluster and, when it is usable, probes the
// platform layer, computes the result and records it.
func (d *Decision) useCluster(ctx context.Context, opts Options, kc []byte, path string, source Source, how string) Result {
	obs, res, ok := d.inspectCluster(ctx, kc, path, how)
	if !ok {
		return res
	}
	return d.finishCluster(ctx, opts, obs, identityOf(kc), path, source, how)
}

// clusterProbes is what the probe chain learned about a cluster before any
// capability probe or write: the live prober, the version it answered, the
// node count and distribution its evidence supports.
type clusterProbes struct {
	probe   Prober
	cluster Cluster
	dist    Distribution
}

// inspectCluster is the gate chain every path that uses a cluster runs, in the
// order the gates need their evidence: /version answers, the identity can
// manage the cluster, the node list is readable, and the distribution and node
// count are one box supports. It returns the refusal Result and false when a
// gate fires, so no caller can probe capabilities or write before it.
//
// A recorded cluster runs this same chain (see useRecord): a record is a cached
// decision, not evidence, and a cluster that has since gone multi-node or been
// replaced must be refused with the wording a freshly found cluster gets.
func (d *Decision) inspectCluster(ctx context.Context, kc []byte, path, how string) (clusterProbes, Result, bool) {
	var obs clusterProbes
	probe, err := d.NewProber(kc)
	if err != nil {
		return obs, stop(fmt.Sprintf("%s at %s: %v", how, path, err)), false
	}
	version, err := probe.Version(ctx)
	if err != nil {
		return obs, stop(fmt.Sprintf("%s at %s did not answer /version: %v", how, path, describeRemote(err))), false
	}
	if err := probe.CanAdmin(ctx); err != nil {
		return obs, stop(fmt.Sprintf("%s at %s answers /version (%s) but this identity cannot manage it: %v", how, path, version.GitVersion, describeRemote(err))), false
	}
	cluster, err := probe.Describe(ctx)
	if err != nil {
		return obs, stop(fmt.Sprintf("%s at %s: %v", how, path, describeRemote(err))), false
	}
	dist := DetectDistribution(version.GitVersion, cluster)
	if problem := supportProblem(dist, cluster.Nodes); problem != "" {
		return obs, stop(fmt.Sprintf("%s at %s: %s", how, path, problem)), false
	}
	return clusterProbes{probe: probe, cluster: cluster, dist: dist}, Result{}, true
}

// finishCluster probes the platform layer of a cluster inspectCluster accepted,
// then computes the platform decisions and the record. id is the identity of the
// cluster the kubeconfig reached, so the record says which cluster it describes
// and a re-run can refuse the file if it has been repointed since.
func (d *Decision) finishCluster(ctx context.Context, opts Options, obs clusterProbes, id clusterIdentity, path string, source Source, how string) Result {
	plat, err := obs.probe.Platform(ctx)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s: platform probe: %v", how, path, describeRemote(err)))
	}
	components, conflict := decidePlatform(opts.Platform, plat)
	res := Result{
		Action:       ActionUseCluster,
		Source:       source,
		Kubeconfig:   path,
		Distribution: obs.dist,
		Nodes:        obs.cluster.Nodes,
		Platform:     components,
		// A node whose address box can see on a tunnel of this host is a
		// warning, not a refusal: the cluster is not box's to change, and the
		// operator is the one who can pin its kubelet.
		Warnings: nodeIPWarnings(obs.cluster, d.net()),
	}
	if conflict != "" {
		res.Action = ActionStop
		res.Reason = conflict
		return res
	}
	choice, problem := d.gatewayChoice(opts, plat, gatewayComponent(components), obs.dist)
	if problem != "" {
		res.Action = ActionStop
		res.Reason = problem
		return res
	}
	res.Gateway = choice
	rec := &Record{
		Source:       source,
		Kubeconfig:   path,
		Server:       id.Server,
		CAHash:       id.CAHash,
		Distribution: obs.dist,
		Nodes:        obs.cluster.Nodes,
		Platform:     opts.Platform,
		GatewayClass: choice.Class,
	}
	if err := d.Records.Save(rec); err != nil {
		res.Action = ActionStop
		res.Reason = fmt.Sprintf("record cluster: %v", err)
		return res
	}
	res.Reason = fmt.Sprintf("using %s at %s (%s)", source, path, obs.dist)
	return res
}

// useRecord handles a re-run. The record is a cached decision, not evidence:
// it supplies which kubeconfig to read, who owns the lifecycle, the platform
// modes and the GatewayClass, and every fact the cluster can answer for itself
// is re-probed. A record is cheap to trust and expensive to be wrong about — a
// cluster that went multi-node, lost its class or lost its platform layer must
// stop or be repaired here, before anything is written, exactly as a freshly
// found cluster would.
//
// A record whose kubeconfig no longer answers stops, naming the record path; it
// never falls through to the candidate scan and the k0s installer, because an
// unreachable recorded cluster may still be there and installing over it would
// destroy a cluster box owns. One whose kubeconfig now reaches a different
// cluster stops before the probe too, naming the recorded and the current
// server: a file can be repointed, and the record's decisions are no more valid
// for the cluster behind it than for any other.
func (d *Decision) useRecord(ctx context.Context, rec *Record) Result {
	if problem := supportProblem(rec.Distribution, rec.Nodes); problem != "" {
		return stop(fmt.Sprintf("recorded %s cluster at %s: %s", rec.Source, rec.Kubeconfig, problem))
	}
	switch rec.Source {
	case SourceExternal, SourceK0sManaged:
	default:
		return stop(fmt.Sprintf("record has unknown source %q", rec.Source))
	}
	kc, err := d.Source.Read(rec.Kubeconfig)
	if err != nil {
		return stop(fmt.Sprintf("recorded %s cluster kubeconfig %s: %v", rec.Source, rec.Kubeconfig, err))
	}
	how := fmt.Sprintf("recorded %s cluster", rec.Source)
	if conflict := identityConflict(rec, identityOf(kc), d.Records.RecordPath()); conflict != "" {
		return stop(conflict)
	}
	obs, res, ok := d.inspectCluster(ctx, kc, rec.Kubeconfig, how)
	if !ok {
		return res
	}
	return d.finishRecorded(ctx, obs, rec, how)
}

// gatewayComponent is the Gateway's result, for the class decision that needs
// to know whether box installs its own implementation or uses one that is
// already there.
func gatewayComponent(components []ComponentResult) ComponentResult {
	for _, c := range components {
		if c.Component == ComponentGateway {
			return c
		}
	}
	return ComponentResult{Component: ComponentGateway}
}

// firstKubeconfig resolves the explicit cluster path: the flag wins, else the
// first entry of KUBECONFIG (which may be a path list).
func firstKubeconfig(flag, env string) string {
	if flag != "" {
		return flag
	}
	if env == "" {
		return ""
	}
	for _, p := range filepath.SplitList(env) {
		if p != "" {
			return p
		}
	}
	return ""
}

// describeRemote collapses ErrRemoteCluster to a "skipped: remote" phrase and
// redacts the rest, so the dry run never leaks a remote server address into a
// "fail" message it cannot act on anyway.
func describeRemote(err error) error {
	if errors.Is(err, ErrRemoteCluster) {
		return errors.New("skipped: remote cluster")
	}
	return err
}

func describeSkip(path, stage string, err error) string {
	if errors.Is(err, ErrRemoteCluster) {
		return fmt.Sprintf("%s: skipped: remote cluster", path)
	}
	return fmt.Sprintf("%s: %s: %v", path, stage, err)
}

// appendDeadCandidate marks a candidate whose kubeconfig exists but cannot be
// used as a blocker when a distribution owns that path. Only a distribution
// that installed a cluster writes its admin kubeconfig there, so a dead one is
// evidence of a broken installation box must not build a second cluster over.
// ~/.kube/config is not distribution-owned: it is often a remote cluster, and
// an unusable one is only a skip. A dry run's ErrRemoteCluster says nothing
// about the cluster's health, so it never blocks either.
func appendDeadCandidate(dead []Finding, cand Candidate, err error) []Finding {
	if !cand.DistributionOwned || errors.Is(err, ErrRemoteCluster) {
		return dead
	}
	return append(dead, Finding{Kind: "kubeconfig", Name: cand.Path})
}

func stop(reason string) Result {
	return Result{Action: ActionStop, Reason: reason}
}

func formatFindings(findings []Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("%s %s", f.Kind, f.Name))
	}
	return strings.Join(parts, ", ")
}

// describeNotes renders runtime findings as informational lines. They are
// reported to the operator, never acted on.
func describeNotes(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, fmt.Sprintf("%s running; not a cluster by itself", f.Name))
	}
	return out
}
