package main

// migrate_install.go is `box migrate apply` on a host with no box: bootstrap's
// cluster decision, bootstrap's platform apply, and init's own flow with the
// plan's answers plus the four a plan cannot know (domain, login, password,
// config path). A host with a record adopts into its existing box instead. Apps
// go in off: preflight requires it, and an app started before its config moves
// would never read the migrated one (docs/box.md).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/qbit"
	"github.com/swizzin/s5n/internal/migrate/work"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
)

// installFlags are the four answers a plan cannot know.
type installFlags struct {
	domain     *string
	login      *string
	password   *string
	configPath *string
	namespace  *string
	// record is the cluster record whose presence means "this host has a box"..
	record *string
	// pathsInNode is the e2e's switch: its paths are inside the node.
	pathsInNode bool
	// unitDir is where the timer units land; no systemctl runs.
	unitDir string
}

func addInstallFlags(fs *flag.FlagSet) *installFlags {
	return &installFlags{
		domain:     fs.String("domain", "", "the domain this box is reached at, e.g. box.example.com (asked on a terminal, required without --yes)"),
		login:      fs.String("login", "", "the account the login page admits (asked on a terminal, required without --yes)"),
		password:   fs.String("password-file", "", "read the login's password from this file, or from stdin with - (asked on a terminal, required without --yes)"),
		configPath: fs.String("config-path", "", "the host path the apps keep their settings on (asked on a terminal, required without --yes)"),
		namespace:  fs.String("box-namespace", "", "the namespace the seedbox is installed in (default s5n-<login> from --login)"),
		record:     fs.String("record", bootstrap.RecordPath, "where box records the cluster decision"),
	}
}

// boxInstalled: a record that cannot be read is an error, not "no box" --
// installing over what box cannot see would put a second seedbox beside it.
func boxInstalled(recordPath string) (bool, error) {
	rec, err := bootstrap.FilesystemRecords{Path: recordPath}.Load()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", recordPath, err)
	}
	return rec != nil, nil
}

// installGuided is init's rule: a terminal and no --yes.
func installGuided(o *migrateApplyFlags) bool { return !*o.yes && isTerminal(os.Stdin) }

// installTarget is the namespace, or "" when init will ask for the login.
func installTarget(inst *installFlags) string {
	if ns := strings.TrimSpace(*inst.namespace); ns != "" {
		return ns
	}
	login := strings.TrimSpace(*inst.login)
	if login == "" {
		return ""
	}
	ns, err := setup.NamespaceFor(login)
	if err != nil {
		// A bad login is init's refusal; this question does not need it.
		return ""
	}
	return ns
}

// migrateInstallSteps is a fresh host's pre-install half: decide, refuse, ask,
// install. The fields let a test watch that order without a host or cluster.
type migrateInstallSteps struct {
	// versions is the confirm's column, refused before the survey.
	versions []apply.AppVersion
	// survey is the summary and the read-only platform plan.
	survey func(ctx context.Context) (apply.Summary, *platform.Plan, error)
	// install is bootstrap, platform and init, with the apps off.
	install func(ctx context.Context) error
	// lead is the confirm's first line.
	lead string
}

// beforeInstall decides, refuses, shows and asks before installing; it returns
// the summary the adoption is held to.
func (s migrateInstallSteps) beforeInstall(ctx context.Context, ask asker, opts askOptions, target string, screen confirmScreen, out say.Reporter) (*apply.Summary, error) {
	if err := refuseDowngrades(out, s.versions, opts.AllowDowngrades, screen.Snapshot); err != nil {
		return nil, err
	}
	summary, plat, err := s.survey(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := migrateConfirm(out, ask, target, opts, plat, s.lead, screen)(summary)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apply.ErrCancelled
	}
	if err := s.install(ctx); err != nil {
		return nil, err
	}
	return &summary, nil
}

// migrationStepsWith builds the pre-install half for one run.
func migrationStepsWith(ctx context.Context, o *migrateApplyFlags, inst *installFlags, p *plan.Plan, opened *bundle.Opened, raw []byte, charts *chartRelease, chart release.Chart, cat *catalog.Catalog, rehearsal bool, out say.Reporter) (migrateInstallSteps, error) {
	// The plan's missing answers are the cheapest refusal, so they come first.
	answers, err := installAnswers(o, inst, p, installGuided(o))
	if err != nil {
		return migrateInstallSteps{}, err
	}
	// Read the apps and peer port once, so install and adoption cannot disagree.
	switchesOn := sortedKeys(p.Apps)
	sourcePort := sourceTorrentPort(p, opened)
	// Plan against chart before probing: the version column and an
	// unhonourable-plan refusal. values are a fresh install's.
	values, err := pendingValues(o, chart)
	if err != nil {
		return migrateInstallSteps{}, err
	}
	versions, err := planFacts(p, cat, values, chart)
	if err != nil {
		return migrateInstallSteps{}, err
	}
	// Preview the platform read-only, before staging: one this host cannot have
	// refuses before anything is copied.
	preview, err := migratePlatform(ctx, o, inst, charts, chart, out)
	if err != nil {
		return migrateInstallSteps{}, err
	}
	return migrateInstallSteps{
		versions: versions,
		survey: func(ctx context.Context) (apply.Summary, *platform.Plan, error) {
			summary, err := migrateSurvey(ctx, p, opened, raw, chart, cat, rehearsal, values)
			return summary, preview.plan, err
		},
		install: func(ctx context.Context) error {
			return migrateInstall(ctx, o, inst, charts, answers, switchesOn, sourcePort, preview.class, out)
		},
		lead: confirmLead(preview.cluster),
	}, nil
}

// pendingValues is the chart's defaults with the operator's file; no grant yet.
func pendingValues(o *migrateApplyFlags, chart release.Chart) (map[string]any, error) {
	return seedbox.Pending(seedbox.Chart{Values: chart.Values, Schema: chart.Schema}, *o.valuesPath)
}

// sourceTorrentPort is the port the source announced, read from the snapshot
// (another host has only the copy). Keeping it keeps trackers and forwards
// working.
func sourceTorrentPort(p *plan.Plan, opened *bundle.Opened) int {
	for _, instance := range sortedKeys(p.Apps) {
		dir, ok := opened.Configs[instance]
		if !ok {
			continue
		}
		if port, ok := qbit.TorrentPort(dir); ok {
			return port
		}
	}
	return 0
}

// migrateInstall installs a fresh host's box with the apps off, then the timer.
// class is the GatewayClass the preview chose: asking twice, or choosing twice,
// is how a box's routes move. switchesOn and sourcePort carry init's port
// decision.
func migrateInstall(ctx context.Context, o *migrateApplyFlags, inst *installFlags, charts *chartRelease, answers []string, switchesOn []string, sourcePort int, class string, out say.Reporter) error {
	out.Phase("cluster")
	if _, _, err := hostconf.Load(hostconf.Path(*inst.record)); err != nil {
		return err
	}
	charts.out = out

	d := migrationDecision(inst, charts, out)
	res, err := d.Bootstrap(ctx, bootstrap.Options{
		Kubeconfig:    *o.kubeconfig,
		KubeconfigEnv: os.Getenv("KUBECONFIG"),
		Platform:      bootstrap.DefaultPlatform(),
		GatewayClass:  class,
	})
	if err != nil {
		return err
	}
	bootstrap.ReportResult(res, out)
	if res.Action == bootstrap.ActionStop {
		return errors.New("bootstrap stopped, so there is no cluster to install into")
	}
	chart, err := charts.load(ctx)
	if err != nil {
		return err
	}
	// The platform was the confirm's first half: do not ask or reprint it.
	out.Phase("platform")
	if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, "", chart.Pins, res.Firewall, answered{}, true, out); err != nil {
		return err
	}
	if _, err := hostconf.Ensure(hostconf.Path(*inst.record)); err != nil {
		return err
	}

	// Parse the plan's answers as init's flags, so its re-run logic sees them.
	iflg, err := migrationInit(inst, charts, answers, switchesOn, sourcePort)
	if err != nil {
		return err
	}
	finish := installFinish{
		adopt:    func(ctx context.Context) error { return iflg.run(ctx, res.Kubeconfig, *o.yes, *o.plain, out) },
		schedule: scheduleUpdates,
	}
	return finish.run(ctx, inst, iflg, out)
}

// scheduler is bootstrap's scheduleUpdates, a field so a test sees its inputs.
type scheduler func(ctx context.Context, unitDir, record, grant, values string, out say.Reporter) []say.Row

// installFinish is init's flow and then the timer. A failed install schedules
// nothing, and the timer follows init so the units name files that exist.
type installFinish struct {
	// adopt is init's flow with the plan's answers.
	adopt func(context.Context) error
	// schedule writes the timer's units.
	schedule scheduler
}

// run is init's flow then the timer; a timer failure is a warning, not the
// install's.
func (f installFinish) run(ctx context.Context, inst *installFlags, iflg *initFlags, out say.Reporter) error {
	if err := f.adopt(ctx); err != nil {
		return err
	}
	// The run's final block, as bootstrap ends.
	out.ToFinish(append(iflg.followUps, f.schedule(ctx, inst.unitDir, *inst.record, *iflg.grantPath, *iflg.valuesPath, out)...))
	return nil
}

// migrationDecision is bootstrap's own decision, so migrate cannot pick another.
func migrationDecision(inst *installFlags, charts *chartRelease, out say.Reporter) *bootstrap.Decision {
	return &bootstrap.Decision{
		Source: bootstrap.FilesystemSource{},
		Pins:   charts.pins,
		NewProber: func(kc []byte) (bootstrap.Prober, error) {
			return bootstrap.NewHTTPProber(kc, false)
		},
		Host:    bootstrap.LinuxHost{},
		Records: bootstrap.FilesystemRecords{Path: *inst.record},
		Install: bootstrap.K0sInstaller{Say: out},
	}
}

// migratePlatform previews the platform without installing: bootstrap's decision
// in preview mode, turned into the plan the one confirm shows. The class returns
// so the install does not choose a different one.
func migratePlatform(ctx context.Context, o *migrateApplyFlags, inst *installFlags, charts *chartRelease, chart release.Chart, out say.Reporter) (platformPreview, error) {
	d := migrationDecision(inst, charts, out)
	res, err := d.Bootstrap(ctx, bootstrap.Options{
		Kubeconfig:    *o.kubeconfig,
		KubeconfigEnv: os.Getenv("KUBECONFIG"),
		Platform:      bootstrap.DefaultPlatform(),
		Preview:       true,
	})
	if err != nil {
		return platformPreview{}, err
	}
	if res.Action == bootstrap.ActionStop {
		return platformPreview{}, errors.New(res.Reason)
	}
	plan, err := platform.Preview(chart.Pins, res.Platform, res.Gateway.Class, "", res.Firewall)
	if err != nil {
		return platformPreview{}, err
	}
	return platformPreview{
		plan:    &plan,
		class:   res.Gateway.Class,
		cluster: res.Action == bootstrap.ActionInstall,
	}, nil
}

// platformPreview is the plan, class and whether this run installs the cluster.
type platformPreview struct {
	plan  *platform.Plan
	class string
	// cluster says box installs k0s here.
	cluster bool
}

// migrateSurvey is the pre-install pass: the same plan, snapshot and values the
// post-install run reads. It stages into a private directory beside the snapshot
// and removes it, never /tmp, which is a tmpfs on Fedora and Arch.
func migrateSurvey(ctx context.Context, p *plan.Plan, opened *bundle.Opened, raw []byte, chart release.Chart, cat *catalog.Catalog, rehearsal bool, values map[string]any) (apply.Summary, error) {
	dir, done, err := work.New(filepath.Dir(opened.Path))
	if err != nil {
		return apply.Summary{}, fmt.Errorf("a private directory for the pre-install pass: %w", err)
	}
	defer done()
	env, err := surveyEnv(p, opened, cat, chart, values, rehearsal, dir)
	if err != nil {
		return apply.Summary{}, err
	}
	return apply.Survey(ctx, p, raw, env)
}

// surveyEnv is the pre-install environment without the cluster; its config root
// is private, so nothing is owned as the seedbox user.
func surveyEnv(p *plan.Plan, opened *bundle.Opened, cat *catalog.Catalog, chart release.Chart, values map[string]any, rehearsal bool, configRoot string) (apply.Env, error) {
	var env apply.Env
	env.ConfigRoot = configRoot
	if p.Data != nil {
		env.DataRoot = p.Data.Root
	}
	env.Configs = filepath.Join(opened.Dir, archive.ConfigPrefix)
	// Count the tree extraction left; the post-install run stages it once.
	env.Bulk = snapshotBulk{opened: opened}
	env.Catalog = cat.Names()
	env.Image = chartImage(chart, values)
	targets, err := apply.Targets(values, env.Catalog, sortedKeys(p.Apps))
	if err != nil {
		return env, err
	}
	env.Targets = targets
	// Nothing is installed, so every planned instance is switched on later.
	env.Enabled = func(string) bool { return false }
	env.Rehearsal = rehearsal
	return env, nil
}

// migrationInit is init's flags with the plan's answers, the loaded release,
// Adopting, and the apps and port the migration switches on later.
func migrationInit(inst *installFlags, charts *chartRelease, answers []string, switchesOn []string, sourcePort int) (*initFlags, error) {
	fs := flag.NewFlagSet("migrate install", flag.ContinueOnError)
	iflg := addInitFlags(fs)
	iflg.recordPath = *inst.record
	iflg.charts = charts
	iflg.adopting = true
	iflg.switchesOn = switchesOn
	iflg.sourceTorrentPort = sourcePort
	iflg.takeDev(initDevFlags{pathsInNode: inst.pathsInNode})
	if err := fs.Parse(answers); err != nil {
		return nil, fmt.Errorf("the plan's answers do not read as init's flags: %w", err)
	}
	return iflg, nil
}

// installAnswers is the plan's answers as init's command line; an unattended run
// missing one of the four refuses naming all at once. A guided run leaves them
// off and lets init ask with its own validation; the plan's answers mean init
// asks none of those (setup.Options.Adopting).
func installAnswers(o *migrateApplyFlags, inst *installFlags, p *plan.Plan, guided bool) ([]string, error) {
	var missing []string
	if strings.TrimSpace(*inst.domain) == "" {
		missing = append(missing, "--domain")
	}
	if strings.TrimSpace(*inst.login) == "" {
		missing = append(missing, "--login")
	}
	if strings.TrimSpace(*inst.password) == "" {
		missing = append(missing, "--password-file")
	}
	if strings.TrimSpace(*inst.configPath) == "" {
		missing = append(missing, "--config-path")
	}
	if len(missing) > 0 && !guided {
		return nil, fmt.Errorf("this host has no box, so migrate installs one: it needs %s, the answers a plan cannot hold", strings.Join(missing, ", "))
	}
	// A given login decides the namespace, so check it before bootstrap: a name
	// that cannot be one refuses here. With --box-namespace it names only the
	// account.
	if login := strings.TrimSpace(*inst.login); login != "" {
		validate := setup.ValidateLogin
		if strings.TrimSpace(*inst.namespace) != "" {
			validate = authelia.ValidateLogin
		}
		if err := validate(login); err != nil {
			return nil, err
		}
	}
	if p.Data == nil || strings.TrimSpace(p.Data.Root) == "" {
		return nil, errors.New("the plan names no data root, and a host with no box has no storage to adopt into: `box migrate cutover` writes a plan with one")
	}
	// Apps go in off; the migration switches on what it moved later.
	answers := []string{
		"--apps", "none",
		// The files this run already has: install and adoption share one grant.
		"--grant", *o.grantPath,
		"--values", *o.valuesPath,
		"--data-path", p.Data.Root,
	}
	// Pass a missing one only when present: init asks, or the refusal above.
	for _, given := range []struct {
		flag  string
		value string
	}{
		{"--domain", *inst.domain},
		{"--login", *inst.login},
		{"--password-file", *inst.password},
		{"--config-path", *inst.configPath},
	} {
		if v := strings.TrimSpace(given.value); v != "" {
			answers = append(answers, given.flag, v)
		}
	}
	// Pass --box-namespace only when given; init derives it from the login.
	if ns := strings.TrimSpace(*inst.namespace); ns != "" {
		answers = append(answers, "--namespace", ns)
	}
	// Use the source's owner when recorded; otherwise init settles it.
	if p.Source != nil && p.Source.Owner != nil {
		answers = append(answers,
			"--puid", strconv.Itoa(p.Source.Owner.UID),
			"--pgid", strconv.Itoa(p.Source.Owner.GID))
	}
	return answers, nil
}
