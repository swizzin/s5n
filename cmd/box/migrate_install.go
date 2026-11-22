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
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
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

// migrateInstall takes a host with no box to one with a seedbox whose apps are
// off, from the plan. It returns when init has installed the release, which is
// the state the adoption then runs against.
func migrateInstall(ctx context.Context, o *migrateApplyFlags, inst *installFlags, p *plan.Plan, charts *chartRelease, out say.Reporter) error {
	// A guided run is one with a terminal to ask on and no --yes: the same rule
	// init uses to decide whether it asks. Its four own answers that this run
	// does not have are init's questions, so they are not a refusal here.
	guided := !*o.yes && isTerminal(os.Stdin)
	answers, err := installAnswers(o, inst, p, guided)
	if err != nil {
		return err
	}
	out.Phase("cluster")
	if _, _, err := hostconf.Load(hostconf.Path(*inst.record)); err != nil {
		return err
	}
	charts.out = out

	d := &bootstrap.Decision{
		Source: bootstrap.FilesystemSource{},
		Pins:   charts.pins,
		NewProber: func(kc []byte) (bootstrap.Prober, error) {
			return bootstrap.NewHTTPProber(kc, false)
		},
		Host:    bootstrap.LinuxHost{},
		Records: bootstrap.FilesystemRecords{Path: *inst.record},
		Install: bootstrap.K0sInstaller{Say: out},
	}
	res, err := d.Bootstrap(ctx, bootstrap.Options{
		Kubeconfig:    *o.kubeconfig,
		KubeconfigEnv: os.Getenv("KUBECONFIG"),
		Platform:      bootstrap.DefaultPlatform(),
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
	if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, "", chart.Pins, res.Firewall, *o.yes, out); err != nil {
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
