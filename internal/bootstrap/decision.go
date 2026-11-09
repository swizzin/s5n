package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	// Prompt asks which GatewayClass to use when more than one is Accepted.
	// Nil means unattended: the decision then refuses and names the setting,
	// because choosing an implementation is the operator's call, never box's.
	Prompt ClassChooser
}

// Options carries the explicit cluster and platform modes from the CLI, plus
// the Gateway decisions that are not modes: which class to attach to, and
// whether a class that publishes no feature list may be used anyway.
type Options struct {
	Kubeconfig    string // --kubeconfig; empty when not given
	KubeconfigEnv string // KUBECONFIG; empty when not set
	Platform      PlatformConfig
	// GatewayClass is platform.gateway.className: the GatewayClass to use
	// when several are Accepted. Empty lets box use the only Accepted one, or
	// ask.
	GatewayClass string
	// AssumeGatewayFeatures is platform.gateway.assumeFeatures: use a class
	// that publishes no supportedFeatures list. Empty is the safe default:
	// box stops rather than using an implementation it cannot check.
	AssumeGatewayFeatures bool
	// DryRun is box bootstrap --dry-run: probe nothing remote and change
	// nothing. A clean host gets the install plan printed and stops there,
	// because the cluster to adopt does not exist and must not be probed.
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

	// 1. An explicit cluster: use it or fail, never fall back.
	if path := firstKubeconfig(opts.Kubeconfig, opts.KubeconfigEnv); path != "" {
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
	for _, path := range d.Source.Candidates() {
		kc, err := d.Source.Read(path)
		if err != nil {
			continue // absent candidate, not a skip
		}
		probe, err := d.NewProber(kc)
		if err != nil {
			skips = append(skips, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if _, err := probe.Version(ctx); err != nil {
			skips = append(skips, describeSkip(path, "version", err))
			continue
		}
		if err := probe.CanAdmin(ctx); err != nil {
			skips = append(skips, describeSkip(path, "privileges", err))
			continue
		}
		return d.useCluster(ctx, opts, kc, path, SourceExternal, "cluster on this host"), nil
	}

	// 3. Something present but not usable. Docker and containerd are excluded
	// from the blockers: they also run on hosts with no Kubernetes, which are
	// exactly the clean k0s targets. They stay as notes, and a runtime holding
	// an API port is caught by the listening-port finding.
	blockers, notes := classifyFindings(d.Host.Present())
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
	// branch can reach the installer's download.
	plan := k0sPlan()
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
			Reason:       fmt.Sprintf("clean host: k0s %s would be installed (dry run)", K0sVersion),
		}, nil
	}
	res = d.adoptInstalled(ctx, opts)
	res.Skips = skips
	res.Notes = describeNotes(notes)
	return res, nil
}

// adoptInstalled is the second half of the clean-host path. k0s is up, so
// there is a cluster: the same probe every other path uses decides its
// distribution, its platform layer and its Gateway class, and only then is it
// recorded. A cluster box started but cannot reach is never written down as one
// box manages, and it is never probed for capabilities before it answers.
func (d *Decision) adoptInstalled(ctx context.Context, opts Options) Result {
	kc, err := d.Source.Read(K0sAdminConfig)
	if err != nil {
		return stop(fmt.Sprintf("read the kubeconfig k0s wrote (%s): %v", K0sAdminConfig, err))
	}
	res := d.useCluster(ctx, opts, kc, K0sAdminConfig, SourceK0sManaged, "the cluster box just installed")
	if res.Action != ActionUseCluster {
		return res
	}
	res.Action = ActionInstall
	res.Reason = fmt.Sprintf("clean host: k0s %s installed; using %s (%s)", K0sVersion, K0sAdminConfig, res.Distribution)
	return res
}

// useCluster validates an existing cluster and, when it is usable, probes the
// platform layer, computes the result and records it.
func (d *Decision) useCluster(ctx context.Context, opts Options, kc []byte, path string, source Source, how string) Result {
	probe, err := d.NewProber(kc)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s: %v", how, path, err))
	}
	version, err := probe.Version(ctx)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s did not answer /version: %v", how, path, describeRemote(err)))
	}
	if err := probe.CanAdmin(ctx); err != nil {
		return stop(fmt.Sprintf("%s at %s answers /version (%s) but this identity cannot manage it: %v", how, path, version.GitVersion, describeRemote(err)))
	}
	cluster, err := probe.Describe(ctx)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s: %v", how, path, describeRemote(err)))
	}
	return d.finishCluster(ctx, opts, probe, version, cluster, path, source, how)
}

// finishCluster computes distribution, refuses a cluster v1 does not support,
// then computes platform decisions and the record.
func (d *Decision) finishCluster(ctx context.Context, opts Options, probe Prober, version Version, cluster Cluster, path string, source Source, how string) Result {
	dist := DetectDistribution(version.GitVersion, cluster)
	if problem := supportProblem(dist, cluster.Nodes); problem != "" {
		return stop(fmt.Sprintf("%s at %s: %s", how, path, problem))
	}
	plat, err := probe.Platform(ctx)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s: platform probe: %v", how, path, describeRemote(err)))
	}
	components, conflict := decidePlatform(opts.Platform, plat)
	res := Result{
		Action:       ActionUseCluster,
		Source:       source,
		Kubeconfig:   path,
		Distribution: dist,
		Nodes:        cluster.Nodes,
		Platform:     components,
	}
	if conflict != "" {
		res.Action = ActionStop
		res.Reason = conflict
		return res
	}
	choice, problem := d.gatewayChoice(opts, plat, gatewayComponent(components))
	if problem != "" {
		res.Action = ActionStop
		res.Reason = problem
		return res
	}
	res.Gateway = choice
	rec := &Record{
		Source:                source,
		Kubeconfig:            path,
		Distribution:          dist,
		Nodes:                 cluster.Nodes,
		Platform:              opts.Platform,
		GatewayClass:          choice.Class,
		AssumeGatewayFeatures: opts.AssumeGatewayFeatures,
	}
	if err := d.Records.Save(rec); err != nil {
		res.Action = ActionStop
		res.Reason = fmt.Sprintf("record cluster: %v", err)
		return res
	}
	res.Reason = fmt.Sprintf("using %s at %s (%s)", source, path, dist)
	return res
}

// useRecord short-circuits a re-run: an external cluster's lifecycle is never
// touched, only its reachability checked; a k0s-managed cluster's start and
// upgrade belong to box update, a later phase. A record written by an older box
// can name a cluster v1 no longer supports, so support is re-checked before any
// probing.
func (d *Decision) useRecord(ctx context.Context, rec *Record) Result {
	if problem := supportProblem(rec.Distribution, rec.Nodes); problem != "" {
		return stop(fmt.Sprintf("recorded %s cluster at %s: %s", rec.Source, rec.Kubeconfig, problem))
	}
	switch rec.Source {
	case SourceExternal:
		kc, err := d.Source.Read(rec.Kubeconfig)
		if err != nil {
			return stop(fmt.Sprintf("recorded external cluster kubeconfig %s: %v", rec.Kubeconfig, err))
		}
		probe, err := d.NewProber(kc)
		if err != nil {
			return stop(fmt.Sprintf("recorded external cluster at %s: %v", rec.Kubeconfig, err))
		}
		if _, err := probe.Version(ctx); err != nil {
			return stop(fmt.Sprintf("recorded external cluster at %s is unreachable: %v", rec.Kubeconfig, describeRemote(err)))
		}
		res := Result{
			Action:       ActionUseCluster,
			Source:       SourceExternal,
			Kubeconfig:   rec.Kubeconfig,
			Distribution: rec.Distribution,
			Nodes:        rec.Nodes,
			Platform:     platformFromConfig(rec.Platform),
			Gateway:      recordedGateway(rec),
			Reason:       fmt.Sprintf("using recorded external cluster at %s (%s); lifecycle never touched", rec.Kubeconfig, rec.Distribution),
		}
		return res
	case SourceK0sManaged:
		return Result{
			Action:       ActionUseManaged,
			Source:       SourceK0sManaged,
			Kubeconfig:   rec.Kubeconfig,
			Distribution: rec.Distribution,
			Nodes:        rec.Nodes,
			Platform:     platformFromConfig(rec.Platform),
			Gateway:      recordedGateway(rec),
			Reason:       "recorded k0s-managed cluster; k0s is started and upgraded by box update (a later phase)",
		}
	default:
		return stop(fmt.Sprintf("record has unknown source %q", rec.Source))
	}
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

// recordedGateway is the class a re-run attaches to. A record written before
// box recorded one came from a box that only ever wrote class `eg`, so that is
// the upgrade path; managed mode owns its implementation and always writes it.
func recordedGateway(rec *Record) GatewayChoice {
	if rec.Platform.Gateway == ModeManaged || rec.GatewayClass == "" {
		return GatewayChoice{Class: EnvoyGatewayClass, BoxInstalls: rec.Platform.Gateway == ModeManaged}
	}
	return GatewayChoice{Class: rec.GatewayClass}
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
