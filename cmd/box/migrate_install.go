package main

// migrate_install.go is `box migrate apply` on a host that has no box: the
// installer (docs/box.md, "apply is the installer on a host with no box").
//
// It is not a second installer. The cluster is bootstrap's own decision and the
// platform layer is bootstrap's own apply; the seedbox is init's own flow, with
// the answers the plan already holds (the apps, the data root, the owner) and
// only the four a plan cannot know asked or taken from flags: the domain, the
// login, the password and the config path. On a host that already has a record,
// none of this runs: apply adopts into the box that is there.
//
// The apps go in off, and the migration switches them on at its end. That is
// the order apply's preflight requires -- it adopts into a seedbox whose apps
// are off -- and it is also the only order that is correct: an app started
// before its config is moved into place would run on an empty config and never
// read the migrated one.

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
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/setup"
)

// installFlags are the answers a host with no box still has to be given: the
// four a plan cannot know. Everything else comes from the plan.
type installFlags struct {
	domain     *string
	login      *string
	password   *string
	configPath *string
	namespace  *string
	// record is where this host records its cluster decision, as
	// `box bootstrap --record` names it. On a host with no box, migrate writes
	// it here, and it is the file whose presence means "this host has a box".
	record *string
	// pathsInNode is the development-only storage switch (--paths-in-node),
	// taken out of argv before the flags are parsed: the e2e's paths are inside
	// its node, so init neither reports nor creates them.
	pathsInNode bool
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

// boxInstalled reports whether this host has a box: a cluster record bootstrap
// wrote. A record that cannot be read is an error, not "no box": installing over
// what box cannot see is how a running seedbox gets a second one beside it.
func boxInstalled(recordPath string) (bool, error) {
	rec, err := bootstrap.FilesystemRecords{Path: recordPath}.Load()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", recordPath, err)
	}
	return rec != nil, nil
}

// installGuided says this run can ask: a terminal to ask on and no --yes. It is
// the rule init uses to decide whether it asks, and the pre-install pass has to
// ask it the same way, because the answers the plan cannot hold are init's own
// questions.
func installGuided(o *migrateApplyFlags) bool { return !*o.yes && isTerminal(os.Stdin) }

// installTarget is the box this run installs, as the question names it: the
// namespace the flags decide, or the empty string when init will ask for the
// login that names it. An empty answer is a question about "this box" rather
// than a guess at a namespace the install has not chosen yet.
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
		// A login that cannot name a namespace is init's own refusal, asked
		// where it belongs; this run's question does not need it.
		return ""
	}
	return ns
}

// migrateInstallSteps is the half of `box migrate apply` a host with no box runs
// before it adopts anything: the pre-install pass that decides what the run
// would do, and the install that does it once the question is answered. They are
// fields so a test can watch the order -- the one question comes before either
// of them writes anything -- without a host, a cluster or a registry.
type migrateInstallSteps struct {
	// survey is the pre-install pass: the migration's summary from the snapshot,
	// and the platform plan this run would install.
	survey func(ctx context.Context) (apply.Summary, *platform.Plan, error)
	// install is bootstrap, the platform and init: a host with no box becomes
	// one, with the release's apps off.
	install func(ctx context.Context) error
	// lead is the confirm's first line: what a yes installs, named only as far
	// as this host has it to install.
	lead string
}

// beforeInstall is the order the whole run exists for: decide, show, ask -- and
// only after the answer install anything. It returns the summary the operator
// agreed to, which the adoption is then held to.
func (s migrateInstallSteps) beforeInstall(ctx context.Context, ask asker, details bool, target string, rehearsal bool, out say.Reporter) (*apply.Summary, error) {
	summary, plat, err := s.survey(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := migrateConfirm(out, ask, target, rehearsal, details, plat, s.lead)(summary)
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

// migrationSteps builds the pre-install half for one run: the summary from the
// snapshot's plan and the release's files, and the install from the plan's
// answers.
func migrationStepsWith(ctx context.Context, o *migrateApplyFlags, inst *installFlags, p *plan.Plan, opened *bundle.Opened, raw []byte, charts *chartRelease, chart release.Chart, cat *catalog.Catalog, rehearsal bool, out say.Reporter) (migrateInstallSteps, error) {
	// The answers a plan cannot hold come first: they are the cheapest refusal
	// (a flag this run was not given) and nothing else here can run without them.
	answers, err := installAnswers(o, inst, p, installGuided(o))
	if err != nil {
		return migrateInstallSteps{}, err
	}
	// The platform this run would install, decided without installing it: a
	// cluster box would create, or the one it found, probed read-only. It comes
	// before the snapshot is staged, so a platform this host cannot have is a
	// refusal before anything is copied.
	preview, err := migratePlatform(ctx, o, inst, charts, chart, out)
	if err != nil {
		return migrateInstallSteps{}, err
	}
	return migrateInstallSteps{
		survey: func(ctx context.Context) (apply.Summary, *platform.Plan, error) {
			summary, err := migrateSurvey(ctx, o, p, opened, raw, chart, cat, rehearsal)
			return summary, preview.plan, err
		},
		install: func(ctx context.Context) error {
			return migrateInstall(ctx, o, inst, charts, answers, preview.class, out)
		},
		lead: confirmLead(preview.cluster),
	}, nil
}

// migrateInstall takes a host with no box to one with a seedbox whose apps are
// off, from the plan. It returns when init has installed the release, which is
// the state the adoption then runs against.
//
// class is the GatewayClass the pre-install pass chose. It is handed back in, so
// the second decision does not ask a question the first one already answered: the
// two runs have to settle on one class, and choosing it twice is how a box's
// routes move.
func migrateInstall(ctx context.Context, o *migrateApplyFlags, inst *installFlags, charts *chartRelease, answers []string, class string, out say.Reporter) error {
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
	// The platform was the first half of the one confirm: it is not asked about
	// here, and its plan is not printed here either, because the question came
	// before anything was installed and showed it then.
	if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, "", chart.Pins, res.Firewall, answered{}, true, out); err != nil {
		return err
	}
	if _, err := hostconf.Ensure(hostconf.Path(*inst.record)); err != nil {
		return err
	}

	// init's own flow, over init's own flags: the answers the plan holds are
	// parsed as if they had been given on the command line, so init's
	// first-run-or-re-run logic sees them exactly as it sees an operator's.
	iflg, err := migrationInit(inst, charts, answers)
	if err != nil {
		return err
	}
	return iflg.run(ctx, res.Kubeconfig, *o.yes, *o.plain, out)
}

// migrationDecision is box bootstrap's own decision, built from this run's flags:
// the same one `box bootstrap` makes, so a migrate install cannot pick a cluster
// or a platform the other command would not.
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

// migratePlatform is the platform plan this run would install, decided without
// installing anything: bootstrap's own cluster decision in preview mode (it
// writes no k0s, no cluster record and no firewall change), turned into the plan
// the migration's one confirm shows first.
//
// The class comes back with it because the install settles on it again: asking
// the operator twice for one decision, or letting the two runs choose different
// classes, is how a box's routes move.
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

// platformPreview is what the read-only decision settles for the run that
// follows it: the plan it would apply, the class the second decision has to
// arrive at again, and whether the cluster is this run's to install.
type platformPreview struct {
	plan  *platform.Plan
	class string
	// cluster says box installs k0s on this host: it has no cluster of its own.
	cluster bool
}

// migrateSurvey is the pre-install pass: the migration's summary, from the same
// plan and the same snapshot a run after the install reads, and from the values
// the chart and the user's file already hold.
//
// It stages into a private directory it removes, so nothing of this pass stays on
// the host: the run's own state directory under the config root is not created
// until the answer is yes.
func migrateSurvey(ctx context.Context, o *migrateApplyFlags, p *plan.Plan, opened *bundle.Opened, raw []byte, chart release.Chart, cat *catalog.Catalog, rehearsal bool) (apply.Summary, error) {
	dir, err := os.MkdirTemp("", "box-migrate-survey-")
	if err != nil {
		return apply.Summary{}, fmt.Errorf("a private directory for the pre-install pass: %w", err)
	}
	defer os.RemoveAll(dir)
	values, err := seedbox.Pending(seedbox.Chart{Values: chart.Values, Schema: chart.Schema}, *o.valuesPath)
	if err != nil {
		return apply.Summary{}, err
	}
	env, err := surveyEnv(p, opened, cat, values, rehearsal, carriedNotes(p, opened), dir)
	if err != nil {
		return apply.Summary{}, err
	}
	return apply.Survey(ctx, p, raw, env)
}

// surveyEnv is the environment the pre-install pass runs with: the plan, the
// snapshot's configs, the values the chart and the user's file hold, and none of
// the cluster. The config root is the caller's private directory, which is why
// nothing here is owned as the seedbox's user: what this pass stages is measured
// and thrown away.
func surveyEnv(p *plan.Plan, opened *bundle.Opened, cat *catalog.Catalog, values map[string]any, rehearsal bool, carried []apply.Carried, configRoot string) (apply.Env, error) {
	var env apply.Env
	env.ConfigRoot = configRoot
	if p.Data != nil {
		env.DataRoot = p.Data.Root
	}
	env.Configs = filepath.Join(opened.Dir, archive.ConfigPrefix)
	env.Catalog = cat.Names()
	targets, err := apply.Targets(values, env.Catalog, sortedKeys(p.Apps))
	if err != nil {
		return env, err
	}
	env.Targets = targets
	// Nothing is installed, so nothing is on: every planned instance is one this
	// run switches on.
	env.Enabled = func(string) bool { return false }
	env.Rehearsal = rehearsal
	// The notes about what a snapshot does not hold of an app's config are the
	// same two functions the run after the install uses: they come from the
	// snapshot, not from the cluster, and a summary without them would not be the
	// one that run shows.
	env.Carried = carried
	return env, nil
}

// migrationInit is init's flags as a migration runs them: the plan's answers as
// if they had been typed on the command line, the release this run has already
// loaded (so bootstrap and init cannot ask the registry for two versions), and
// the one thing that is this flow's and no one else's -- Adopting. The apps go
// in off and the migration switches on what it moved, so an empty app list here
// is a box one step from running, and init says nothing about it.
func migrationInit(inst *installFlags, charts *chartRelease, answers []string) (*initFlags, error) {
	fs := flag.NewFlagSet("migrate install", flag.ContinueOnError)
	iflg := addInitFlags(fs)
	iflg.recordPath = *inst.record
	iflg.charts = charts
	iflg.adopting = true
	iflg.takeDev(initDevFlags{pathsInNode: inst.pathsInNode})
	if err := fs.Parse(answers); err != nil {
		return nil, fmt.Errorf("the plan's answers do not read as init's flags: %w", err)
	}
	return iflg, nil
}

// installAnswers is the plan's answers as init's command line, and, on an
// unattended run, the refusal that names the four a plan cannot know when they
// were not given.
//
// A guided run (a terminal, no --yes) asks only what it lacks: each of the four
// that was not given is simply left off, and init asks for it with its own
// question, its own help text and its own validation. Everything the plan does
// hold -- the data root, the app list, the owner -- is handed over as an answer,
// so init asks none of those (setup.Options.Adopting). An unattended run has
// nobody to ask, so a missing one is a refusal naming every one at once rather
// than one per run.
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
	// The login decides the seedbox's namespace (s5n-<login>), so a given one is
	// checked here, before bootstrap installs anything: a name that could never
	// be a namespace is a refusal, not a seedbox box cannot create. A login this
	// run has to ask for is checked by init's own question, which is the same
	// rule.
	// With --box-namespace given, the login names nothing but the account.
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
	// The apps go in off; the migration switches on what it moved.
	answers := []string{
		"--apps", "none",
		// The two files are the ones this run already has: init writes them,
		// and the adoption reads them back, so the install and the adoption
		// cannot end up on different grants.
		"--grant", *o.grantPath,
		"--values", *o.valuesPath,
		"--data-path", p.Data.Root,
	}
	// The four a plan cannot know are passed only when this run has them: one
	// that is missing is init's own question on a terminal, and the refusal
	// above on an unattended run.
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
	// --box-namespace is passed only when it was given: with none, init derives
	// the seedbox's namespace from the login, the same way `box init` does.
	if ns := strings.TrimSpace(*inst.namespace); ns != "" {
		answers = append(answers, "--namespace", ns)
	}
	// The owner is the plan's when the source recorded one: the copied configs
	// are owned by the uid that owned them, and so is the data. Otherwise init
	// settles it the way host foundations does -- the flag, the data path's
	// owner, $SUDO_UID, the invoking user.
	if p.Source != nil && p.Source.Owner != nil {
		answers = append(answers,
			"--puid", strconv.Itoa(p.Source.Owner.UID),
			"--pgid", strconv.Itoa(p.Source.Owner.GID))
	}
	return answers, nil
}
