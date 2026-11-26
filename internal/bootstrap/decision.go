package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swizzin/s5n/internal/release"
)

// Decision runs box bootstrap's cluster detection and platform probes, over the
// interfaces in bootstrap.go, so the whole decision is testable without a cluster.
type Decision struct {
	Source    ConfigSource
	NewProber func(kubeconfig []byte) (Prober, error)
	Host      Host
	Records   RecordStore
	Install   Installer
	// Pins is the chart release's platform pins (charts/s5n/platform.yaml, read by
	// internal/release): bootstrap installs k0s at the release's pin, so it pulls the
	// release before touching the host. A function, because only the install branch
	// needs it, and cmd/box loads it once for the whole command.
	Pins func(ctx context.Context) (release.Versions, error)
	// Prompt asks which GatewayClass to use when more than one is Accepted. Nil
	// refuses and names the setting: choosing an implementation is the operator's.
	Prompt ClassChooser
	// Net reports this host's default route and interfaces, for the node IP box pins
	// and the warnings about an address it can see locally. Nil is the running host.
	Net Net
	// Firewall is the host's packet filter, read on every path that uses a cluster
	// and changed only where box installed k0s itself. Nil is Firewalld.
	Firewall Firewall
	// K0sConfigFile is where box reads back the k0s config it installed with, for
	// the networks the firewall trusts. Empty means K0sConfigPath.
	K0sConfigFile string
}

// Options carries the explicit cluster and platform modes from the CLI, plus
// the Gateway decision that is not a mode: which class to attach to.
type Options struct {
	Kubeconfig    string // --kubeconfig; empty when not given
	KubeconfigEnv string // KUBECONFIG; empty when not set
	Platform      PlatformConfig
	// GatewayClass is platform.gateway.className: the class to use when several are
	// Accepted. Empty uses the only one, or asks.
	GatewayClass string
	// DryRun is --dry-run: probe no cluster and change nothing. A clean host still
	// reads the release for its k0s pin, prints the install plan and stops, since
	// the cluster to adopt does not exist and must not be probed.
	DryRun bool
	// Preview decides which cluster box would use and what it would install, and
	// writes nothing: no k0s, no record, no firewall change. A flow whose one
	// question comes before its first write asks here, then decides again for real;
	// only what the preview showed is installed. A record is the one write it must
	// not make: it is what makes a host "have a box", so one written before the
	// answer would leave a host that said no looking installed.
	Preview bool
}

// Bootstrap decides which cluster to use, in PLAN.md's fixed order, and never
// installs a second cluster over a broken first one.
func (d *Decision) Bootstrap(ctx context.Context, opts Options) (Result, error) {
	res := Result{}

	// The record is read first, so a re-run short-circuits the probe-and-install path.
	// One that exists but cannot be read is a fault, not a clean host: box must not
	// install over what it cannot see.
	rec, err := d.Records.Load()
	if err != nil {
		return res, fmt.Errorf("read cluster record: %w", err)
	}

	// 1. An explicit cluster: use it or fail. With a record, its own kubeconfig is the
	// recorded re-run; any other file would move the host to another cluster, which only
	// the operator may do.
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

	// A cancelled run is not evidence about the host: probing cut short by a signal
	// reads as a dead candidate, and the evidence below would call a host with a
	// cluster broken or clean -- the second one installs k0s on it.
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// 3. Something present but not usable, a dead distribution-owned kubeconfig
	// included. Docker and containerd are notes: they also run on clean k0s targets.
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

	// 4. A clean host: install k0s, then adopt the cluster box just started. Only
	// this branch reaches the installer's download, and the k0s is the release's
	// pin, so a bump is a chart release rather than a new box.
	pins, err := d.platformPins(ctx)
	if err != nil {
		return stop(err.Error()), nil
	}
	// The node IP is chosen before anything is downloaded: a host whose default route
	// leaves through a tunnel is refused, with the paragraph on what a node there
	// would do, and a host with no default route has nothing to pin.
	up, err := d.net().Uplink()
	if err != nil {
		return stop(fmt.Sprintf("box pins the node IP to this host's uplink before it installs k0s, and cannot find one: %v", err)), nil
	}
	if why := tunnelRefusal(up); why != "" {
		return stop(why), nil
	}
	plan := k0sPlan(pins, up)
	if opts.Preview {
		// Nothing is downloaded, run or recorded: a host whose answer is no must be as
		// it was. The platform decision is the one a fresh cluster gives -- nothing on
		// it, no address layer -- so it can be made without the cluster.
		res := Result{
			Action:       ActionInstall,
			Source:       SourceK0sManaged,
			Kubeconfig:   K0sAdminConfig,
			Distribution: DistK0s,
			Platform:     previewComponents(opts.Platform),
			Gateway:      GatewayChoice{Class: EnvoyGatewayClass, ControllerName: EnvoyGatewayController, BoxInstalls: true},
			Skips:        skips,
			Notes:        describeNotes(notes),
			Reason:       fmt.Sprintf("clean host: k0s %s and the platform layer would be installed here", plan.Version),
		}
		res = d.withPlanFirewall(ctx, res, plan.ConfigText)
		return res, nil
	}
	if err := d.Install.Install(ctx, plan); err != nil {
		return stop(fmt.Sprintf("k0s install failed: %v", err)), nil
	}
	if opts.DryRun {
		res := Result{
			Action:       ActionInstall,
			Source:       SourceK0sManaged,
			Kubeconfig:   K0sAdminConfig,
			Distribution: DistK0s,
			Skips:        skips,
			Notes:        describeNotes(notes),
			Reason:       fmt.Sprintf("clean host: k0s %s would be installed (dry run)", plan.Version),
		}
		// A dry run never reaches the platform plan, which is where a real run says
		// what it does to the firewall, so it says it here.
		if d.firewall().Running(ctx) {
			res.Firewall.Active = true
			if pod, service, err := k0sNetworks([]byte(plan.ConfigText)); err == nil {
				res.Firewall.CIDRs = []string{pod, service}
				res.Notes = append(res.Notes, fmt.Sprintf("firewalld is running; the platform plan would add %s and %s to its trusted zone, permanently and now", pod, service))
			}
		}
		return res, nil
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

// firewall is the host's packet filter, or the real host's when none was given.
func (d *Decision) firewall() Firewall {
	if d.Firewall != nil {
		return d.Firewall
	}
	return Firewalld{}
}

// withFirewall fills a usable result's firewall half. Only a cluster box installed has
// networks box may trust: on one found or given, guessing at another operator's would be
// worse than saying so, so the plan carries the commands and warns.
func (d *Decision) withFirewall(ctx context.Context, res Result) Result {
	if res.Action == ActionStop || !d.firewall().Running(ctx) {
		return res
	}
	res.Firewall.Active = true
	if res.Source != SourceK0sManaged {
		res.Firewall.Warning = firewalldWarning()
		return res
	}
	path := d.K0sConfigFile
	if path == "" {
		path = K0sConfigPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		res.Firewall.Warning = fmt.Sprintf("firewalld is running and box could not read the k0s config it installed with (%s: %v): add the cluster's pod and service networks to firewalld's trusted zone by hand", path, err)
		return res
	}
	pod, service, err := k0sNetworks(raw)
	if err != nil {
		res.Firewall.Warning = fmt.Sprintf("firewalld is running and %s: add the cluster's pod and service networks to firewalld's trusted zone by hand", err)
		return res
	}
	res.Firewall.CIDRs = []string{pod, service}
	return res
}

// withPlanFirewall is withFirewall for a cluster that does not exist yet: the
// networks are the ones the k0s config box would install with. A preview reads the
// filter and changes nothing; the platform plan does that after the answer.
func (d *Decision) withPlanFirewall(ctx context.Context, res Result, configText string) Result {
	if !d.firewall().Running(ctx) {
		return res
	}
	res.Firewall.Active = true
	pod, service, err := k0sNetworks([]byte(configText))
	if err != nil {
		res.Firewall.Warning = fmt.Sprintf("firewalld is running and the k0s config box would install with cannot be read back (%v): add the cluster's pod and service networks to firewalld's trusted zone by hand", err)
		return res
	}
	res.Firewall.CIDRs = []string{pod, service}
	return res
}

// platformPins is the release's pins or the refusal: there is no fallback, since a
// compiled-in version would be a second home for the platform.
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

// adoptInstalled is the clean-host path's second half: k0s is up, so the same probe
// decides its distribution, platform layer and class, and only then is it recorded.
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

// freshClusterAddresses settles the address question on a cluster box just started: k0s
// hands out no LoadBalancer addresses, so auto mode would only report AddressNotAssigned
// after the wait and MetalLB is planned up front. The record keeps the mode.
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

// previewComponents is the platform decision for a cluster box would create: every
// capability absent, the address layer installed rather than asked.
func previewComponents(cfg PlatformConfig) []ComponentResult {
	empty := PlatformProbe{
		GatewayAPI:   OutcomeAbsent,
		LoadBalancer: OutcomeAbsent,
		Gateway:      OutcomeAbsent,
		CertManager:  OutcomeAbsent,
	}
	components, _ := decidePlatform(cfg, empty)
	return freshClusterAddresses(components)
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

// inspectCluster is the gate chain every path that uses a cluster runs, in the order the
// gates need their evidence: /version, admin rights, the node list, a supported
// distribution and node count. A gate returns the refusal and false, so nothing probes
// capabilities or writes first. A recorded cluster runs the same chain: a record is a
// cached decision, not evidence.
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

// finishCluster probes the accepted cluster's platform layer, decides the components and
// writes the record. id is the identity the kubeconfig reached, so a re-run can refuse a
// repointed file.
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
		// A node address box can see on a tunnel of this host is a warning, not a
		// refusal: the operator is who can pin its kubelet.
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
	if opts.Preview {
		// The decision, not a record of it: a record here would make a host whose
		// answer is no look like one box installed. The firewall is read, never
		// changed; the plan does that.
		res = d.withFirewall(ctx, res)
		res.Reason = fmt.Sprintf("%s at %s (%s): box would install the platform layer here", how, path, obs.dist)
		return res
	}
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
	res = d.withFirewall(ctx, res)
	res.Reason = fmt.Sprintf("using %s at %s (%s)", source, path, obs.dist)
	return res
}

// useRecord handles a re-run: the record says which kubeconfig, who owns the lifecycle,
// the modes and the class, and every fact the cluster can answer is re-probed.
//
// A record whose kubeconfig no longer answers stops, naming the record path, and never
// falls through to the installer: an unreachable cluster may still be there. One that
// reaches a different cluster stops before the probe, naming both servers.
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

// gatewayComponent is the Gateway's result, which the class decision needs to know
// whether box installs its own implementation.
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

// describeRemote collapses ErrRemoteCluster to "skipped: remote", so a dry run never
// leaks a remote server address into a message it cannot act on.
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

// appendDeadCandidate marks an unusable kubeconfig as a blocker when a distribution owns
// it: a dead admin kubeconfig is a broken installation box must not build over.
// ~/.kube/config is often remote, so an unusable one is only a skip.
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

// describeNotes renders runtime findings as lines the operator reads, never acts on.
func describeNotes(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, fmt.Sprintf("%s running; not a cluster by itself", f.Name))
	}
	return out
}
