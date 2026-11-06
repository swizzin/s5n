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
}

// Options carries the explicit cluster and platform modes from the CLI.
type Options struct {
	Kubeconfig    string // --kubeconfig; empty when not given
	KubeconfigEnv string // KUBECONFIG; empty when not set
	Platform      PlatformConfig
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

	// 4. A clean host: install k0s.
	plan := InstallPlan{Version: K0sVersion}
	if err := d.Install.Install(ctx, plan); err != nil {
		return stop(fmt.Sprintf("k0s install failed: %v", err)), nil
	}
	rec = &Record{
		Source:       SourceK0sManaged,
		Kubeconfig:   K0sAdminConfig,
		Distribution: DistK0s,
		Nodes:        1,
		Platform:     opts.Platform,
	}
	if err := d.Records.Save(rec); err != nil {
		return stop(fmt.Sprintf("record the install: %v", err)), nil
	}
	res = Result{
		Action:       ActionInstall,
		Source:       SourceK0sManaged,
		Kubeconfig:   K0sAdminConfig,
		Distribution: DistK0s,
		Nodes:        1,
		Skips:        skips,
		Notes:        describeNotes(notes),
		Reason:       "clean host: k0s install planned (the platform layer is a later bootstrap step)",
	}
	return res, nil
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

// finishCluster computes distribution, platform decisions and the record.
func (d *Decision) finishCluster(ctx context.Context, opts Options, probe Prober, version Version, cluster Cluster, path string, source Source, how string) Result {
	dist := DetectDistribution(version.GitVersion, cluster)
	plat, err := probe.Platform(ctx)
	if err != nil {
		return stop(fmt.Sprintf("%s at %s: platform probe: %v", how, path, describeRemote(err)))
	}
	components, conflict := decidePlatform(opts.Platform, plat)
	res := Result{
		Action:               ActionUseCluster,
		Source:               source,
		Kubeconfig:           path,
		Distribution:         dist,
		Nodes:                cluster.Nodes,
		StorageClassRequired: storageClassRequired(cluster.Nodes),
		Platform:             components,
	}
	if conflict != "" {
		res.Action = ActionStop
		res.Reason = conflict
		return res
	}
	rec := &Record{
		Source:       source,
		Kubeconfig:   path,
		Distribution: dist,
		Nodes:        cluster.Nodes,
		Platform:     opts.Platform,
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
// upgrade belong to box update, a later phase.
func (d *Decision) useRecord(ctx context.Context, rec *Record) Result {
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
			Action:               ActionUseCluster,
			Source:               SourceExternal,
			Kubeconfig:           rec.Kubeconfig,
			Distribution:         rec.Distribution,
			Nodes:                rec.Nodes,
			StorageClassRequired: storageClassRequired(rec.Nodes),
			Platform:             platformFromConfig(rec.Platform),
			Reason:               fmt.Sprintf("using recorded external cluster at %s (%s); lifecycle never touched", rec.Kubeconfig, rec.Distribution),
		}
		return res
	case SourceK0sManaged:
		return Result{
			Action:               ActionUseManaged,
			Source:               SourceK0sManaged,
			Kubeconfig:           rec.Kubeconfig,
			Distribution:         rec.Distribution,
			Nodes:                rec.Nodes,
			StorageClassRequired: storageClassRequired(rec.Nodes),
			Platform:             platformFromConfig(rec.Platform),
			Reason:               "recorded k0s-managed cluster; k0s is started and upgraded by box update (a later phase)",
		}
	default:
		return stop(fmt.Sprintf("record has unknown source %q", rec.Source))
	}
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
