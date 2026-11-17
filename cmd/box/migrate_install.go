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

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/hostconf"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
)

// installFlags are the answers a host with no box still has to be given: the
// four a plan cannot know. Everything else comes from the plan.
type installFlags struct {
	domain     *string
	login      *string
	password   *string
	configPath *string
	namespace  *string
	// pathsInNode is the development-only storage switch (--paths-in-node),
	// taken out of argv before the flags are parsed: the e2e's paths are inside
	// its node, so init neither reports nor creates them.
	pathsInNode bool
}

func addInstallFlags(fs *flag.FlagSet) *installFlags {
	return &installFlags{
		domain:     fs.String("domain", "", "the domain this box is reached at, e.g. box.example.com (required on a host with no box)"),
		login:      fs.String("login", "", "the account the login page admits (required on a host with no box)"),
		password:   fs.String("password-file", "", "read the login's password from this file, or from stdin with - (required on a host with no box)"),
		configPath: fs.String("config-path", "", "the host path the apps keep their settings on (required on a host with no box)"),
		namespace:  fs.String("box-namespace", "seedbox", "the namespace the seedbox is installed in"),
	}
}

// boxInstalled reports whether this host has a box: a cluster record bootstrap
// wrote. A record that cannot be read is an error, not "no box": installing over
// what box cannot see is how a running seedbox gets a second one beside it.
func boxInstalled() (bool, error) {
	rec, err := bootstrap.FilesystemRecords{Path: bootstrap.RecordPath}.Load()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", bootstrap.RecordPath, err)
	}
	return rec != nil, nil
}

// migrateInstall takes a host with no box to one with a seedbox whose apps are
// off, from the plan. It returns when init has installed the release, which is
// the state the adoption then runs against.
func migrateInstall(ctx context.Context, o *migrateApplyFlags, inst *installFlags, p *plan.Plan, charts *chartRelease, out say.Reporter) error {
	answers, err := installAnswers(inst, p)
	if err != nil {
		return err
	}
	out.Phase("cluster")
	if _, _, err := hostconf.Load(hostconf.Path(bootstrap.RecordPath)); err != nil {
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
		Records: bootstrap.FilesystemRecords{Path: bootstrap.RecordPath},
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
	if err := applyPlatform(ctx, res.Kubeconfig, res.Platform, res.Gateway.Class, "", chart.Pins, *o.yes, out); err != nil {
		return err
	}
	if _, err := hostconf.Ensure(hostconf.Path(bootstrap.RecordPath)); err != nil {
		return err
	}

	// init's own flow, over init's own flags: the answers the plan holds are
	// parsed as if they had been given on the command line, so init's
	// first-run-or-re-run logic sees them exactly as it sees an operator's.
	ifs := flag.NewFlagSet("migrate install", flag.ContinueOnError)
	iflg := addInitFlags(ifs)
	iflg.recordPath = bootstrap.RecordPath
	iflg.charts = charts
	iflg.takeDev(initDevFlags{pathsInNode: inst.pathsInNode})
	if err := ifs.Parse(answers); err != nil {
		return fmt.Errorf("the plan's answers do not read as init's flags: %w", err)
	}
	return iflg.run(ctx, res.Kubeconfig, *o.yes, *o.plain, out)
}

// installAnswers is the plan's answers as init's command line, and the refusal
// that names the four a plan cannot know when they were not given.
//
// Unattended is the only way this runs: a guided install is `box bootstrap`
// followed by `box init`, which ask their own questions. So each of the four is
// required, and the refusal names all of the missing ones at once rather than
// one per run.
func installAnswers(inst *installFlags, p *plan.Plan) ([]string, error) {
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
	if len(missing) > 0 {
		return nil, fmt.Errorf("this host has no box, so migrate installs one: it needs %s, the answers a plan cannot hold", strings.Join(missing, ", "))
	}
	if p.Data == nil || strings.TrimSpace(p.Data.Root) == "" {
		return nil, errors.New("the plan names no data root, and a host with no box has no storage to adopt into: `box migrate cutover` writes a plan with one")
	}
	// The apps go in off; the migration switches on what it moved.
	answers := []string{
		"--apps", "none",
		"--namespace", *inst.namespace,
		"--data-path", p.Data.Root,
		"--config-path", *inst.configPath,
		"--domain", *inst.domain,
		"--login", *inst.login,
		"--password-file", *inst.password,
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
