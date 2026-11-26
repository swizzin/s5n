package main

// migrate_install_test.go proves the answers a host with no box is installed
// from: the plan holds the apps, the data root and the owner, and the four a
// plan cannot know are refused by name, together, when they were not given.
// Nothing here touches a host: installAnswers is the whole decision.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/archive"
	"github.com/swizzin/s5n/internal/migrate/bundle"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
)

// planWithData is the plan a cutover writes: one app, a data root, and the
// owner the source's files belonged to.
func planWithData() *plan.Plan {
	return &plan.Plan{
		Version: 2,
		Source:  &plan.Source{Host: "media-host", Taken: "2026-11-17T09:00:00Z", Owner: &plan.Owner{UID: 1000, GID: 1000}, Kinds: []string{"kubernetes"}},
		Data:    &plan.Data{Root: "/srv/library", Downloads: "Downloads", Media: "media"},
		Apps: map[string]plan.App{
			"sonarr": {Kind: "kubernetes", Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media"},
		},
	}
}

// installFlagSet is the four answers a plan cannot hold, all empty: each test
// fills what it is about. The two files are the ones a run already has.
func installFlagSet(t *testing.T) (*migrateApplyFlags, *installFlags) {
	t.Helper()
	ns := "seedbox"
	grant, values := "/etc/s5n/seedboxes/root.yaml", "/root/.config/box/values.yaml"
	o := &migrateApplyFlags{applyFlags: &applyFlags{grantPath: &grant, valuesPath: &values}}
	return o, &installFlags{domain: new(string), login: new(string), password: new(string), configPath: new(string), namespace: &ns}
}

func TestInstallAnswersComeFromThePlan(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	got, err := installAnswers(o, inst, planWithData(), false)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	joined := strings.Join(got, " ")
	// The apps go in off, and the migration switches on what it moved: an app
	// started before its config is moved would run on an empty one.
	for _, want := range []string{
		"--apps none",
		"--namespace seedbox",
		"--data-path /srv/library",
		"--config-path /srv/config",
		"--domain box.example.com",
		"--login someone",
		"--password-file /run/password",
		"--puid 1000",
		"--pgid 1000",
		"--grant /etc/s5n/seedboxes/root.yaml",
		"--values /root/.config/box/values.yaml",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the answers do not carry %q: %v", want, got)
		}
	}
}

func TestInstallAnswersRefuseEveryMissingOneByName(t *testing.T) {
	o, inst := installFlagSet(t)
	_, err := installAnswers(o, inst, planWithData(), false)
	if err == nil {
		t.Fatal("an install with no domain, login, password or config path was accepted")
	}
	// All of them at once: a refusal one per run is four runs to find out.
	for _, want := range []string{"--domain", "--login", "--password-file", "--config-path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "no box") {
		t.Errorf("the refusal does not say why it is asking: %v", err)
	}
}

func TestInstallAnswersLeaveTheNamespaceToTheLogin(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "alice"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	empty := ""
	inst.namespace = &empty
	got, err := installAnswers(o, inst, planWithData(), false)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	for _, arg := range got {
		if arg == "--namespace" {
			t.Fatalf("the answers name a namespace box was not given: %v", got)
		}
	}
}

func TestInstallAnswersRefuseALoginThatCannotBeASeedbox(t *testing.T) {
	for _, login := range []string{"system", "gateway", "alice@example.com"} {
		t.Run(login, func(t *testing.T) {
			o, inst := installFlagSet(t)
			*inst.domain = "box.example.com"
			*inst.namespace = "" // no --box-namespace: the login names it
			*inst.login = login
			*inst.password = "/run/password"
			*inst.configPath = "/srv/config"
			_, err := installAnswers(o, inst, planWithData(), false)
			if err == nil {
				t.Fatalf("login %q was accepted", login)
			}
			if !strings.Contains(err.Error(), login) {
				t.Errorf("the refusal does not name %q: %v", login, err)
			}
		})
	}
}

func TestInstallAnswersTakeAnyLoginWithAnExplicitNamespace(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.namespace = "s5n-alice"
	*inst.login = "Alice.B"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	if _, err := installAnswers(o, inst, planWithData(), false); err != nil {
		t.Fatalf("login Alice.B with --box-namespace was refused: %v", err)
	}
}

func TestInstallAnswersNeedADataRoot(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	p := planWithData()
	p.Data = nil
	_, err := installAnswers(o, inst, p, false)
	if err == nil || !strings.Contains(err.Error(), "data root") {
		t.Fatalf("a plan with no data root was accepted with %v", err)
	}
}

func TestInstallAnswersLeaveTheOwnerToHostFoundations(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	p := planWithData()
	p.Source = nil
	got, err := installAnswers(o, inst, p, false)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	for _, arg := range got {
		if arg == "--puid" || arg == "--pgid" {
			t.Errorf("the answers name an owner the plan did not: %v", got)
		}
	}
}

func TestMigrationInitTellsInitTheAppsAreComing(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	inst.record = new(string)
	answers, err := installAnswers(o, inst, planWithData(), false)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	charts := &chartRelease{}
	flags, err := migrationInit(inst, charts, answers, nil, 0)
	if err != nil {
		t.Fatalf("migrationInit: %v", err)
	}
	if !flags.adopting {
		t.Error("the migrating init was not told the apps are coming, so it would say nothing runs yet")
	}
	if *flags.apps != "none" {
		t.Errorf("apps = %q, want none: the migration switches on what it moved", *flags.apps)
	}
	if *flags.domain != "box.example.com" || *flags.configPath != "/srv/config" {
		t.Errorf("the plan's answers did not reach init: domain %q, config %q", *flags.domain, *flags.configPath)
	}
	if flags.charts != charts {
		t.Error("the migrating init would pull its own chart, and this run already loaded one")
	}
	// A bare init is not adopting into anything: `box init` with every app off
	// keeps its line.
	var bare initFlags
	if bare.adopting {
		t.Error("a bare init would say nothing about an empty box")
	}
	// An answer init's own flags cannot read is still the installer's refusal,
	// and it names what it was trying to do.
	if _, err := migrationInit(inst, charts, []string{"--no-such-flag"}, nil, 0); err == nil {
		t.Fatal("an answer init has no flag for was accepted")
	} else if !strings.Contains(err.Error(), "init's flags") {
		t.Errorf("the refusal = %v, want it to say the plan does not read as init's flags", err)
	}
}

func TestInstallAnswersTakeThePlansOwnerOrNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner *plan.Owner
		want  []string
		not   []string
	}{
		{name: "an owner in the plan", owner: &plan.Owner{UID: 1000, GID: 1000}, want: []string{"--puid 1000", "--pgid 1000"}},
		{name: "no owner in the plan", owner: nil, not: []string{"--puid", "--pgid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, inst := installFlagSet(t)
			*inst.domain = "box.example.com"
			*inst.login = "someone"
			*inst.password = "/run/password"
			*inst.configPath = "/srv/config"
			p := planWithData()
			p.Source.Owner = tc.owner
			got, err := installAnswers(o, inst, p, false)
			if err != nil {
				t.Fatalf("installAnswers: %v", err)
			}
			joined := strings.Join(got, " ")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("the answers do not carry %q: %v", want, got)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(joined, not) {
					t.Errorf("the answers carry %q, and the plan names no owner: %v", not, got)
				}
			}
		})
	}
}

func TestInstallAnswersAskOnlyWhatTheRunLacks(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	got, err := installAnswers(o, inst, planWithData(), true)
	if err != nil {
		t.Fatalf("a guided run with two answers left for init was refused: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"--login someone", "--password-file /run/password", "--config-path /srv/config"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the answers do not carry %q: %v", want, got)
		}
	}
	if strings.Contains(joined, "--domain") {
		t.Errorf("the answers carry a --domain this run was not given, so init would not ask for it: %v", got)
	}
}

func TestInstallAnswersRefuseAPlanWithNoDataRootEvenGuided(t *testing.T) {
	o, inst := installFlagSet(t)
	p := planWithData()
	p.Data = nil
	_, err := installAnswers(o, inst, p, true)
	if err == nil || !strings.Contains(err.Error(), "data root") {
		t.Fatalf("a guided run accepted a plan with no data root: %v", err)
	}
}

func TestInstallTargetNamesTheBoxTheFlagsDecide(t *testing.T) {
	_, inst := installFlagSet(t)
	if got := installTarget(inst); got != "seedbox" {
		t.Errorf("with --box-namespace, the target is %q, want seedbox", got)
	}
	inst.namespace = new(string)
	*inst.login = "alice"
	if got := installTarget(inst); got != "s5n-alice" {
		t.Errorf("with --login alice, the target is %q, want the namespace it names", got)
	}
	if got := confirmTarget(installTarget(inst)); got != "s5n-alice" {
		t.Errorf("the question names %q, want s5n-alice", got)
	}
	*inst.login = ""
	if got := installTarget(inst); got != "" {
		t.Errorf("with no login, the target is %q, and init has not chosen a namespace", got)
	}
	if got := confirmTarget(installTarget(inst)); got != "this box" {
		t.Errorf("the question with no namespace reads %q, want it to name the box it is about", got)
	}
}

func TestTheInstallerSchedulesTheTimerLast(t *testing.T) {
	var order, got []string
	fin := installFinish{
		adopt: func(context.Context) error {
			order = append(order, "install")
			return nil
		},
		schedule: func(_ context.Context, unitDir, record, grant, values string, _ say.Reporter) []say.Row {
			order = append(order, "timer")
			got = []string{unitDir, record, grant, values}
			return nil
		},
	}
	o, inst := installFlagSet(t)
	inst.unitDir = "/run/units"
	inst.record = new(string)
	*inst.record = "/etc/s5n/cluster.yaml"
	iflg := &initFlags{grantPath: o.grantPath, valuesPath: o.valuesPath}
	if err := fin.run(context.Background(), inst, iflg, &say.Plain{W: &bytes.Buffer{}}); err != nil {
		t.Fatalf("the install's last step: %v", err)
	}
	if want := "install timer"; strings.Join(order, " ") != want {
		t.Errorf("the install went %q, want %q: the timer is last, as bootstrap's is", strings.Join(order, " "), want)
	}
	// The three files are the ones this run installed from, and the directory is
	// the one --unit-dir named: the timer has to update the box it was given, not
	// a default one.
	want := []string{"/run/units", "/etc/s5n/cluster.yaml", *o.grantPath, *o.valuesPath}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the timer was scheduled for %v, want the run's own %v", got, want)
	}

	// The same step with an install that stopped: an error comes back as it is,
	// and nothing is scheduled.
	order = nil
	stopped := errors.New("the release never became healthy")
	failed := installFinish{
		adopt: func(context.Context) error {
			order = append(order, "install")
			return stopped
		},
		schedule: func(context.Context, string, string, string, string, say.Reporter) []say.Row {
			order = append(order, "timer")
			return nil
		},
	}
	err := failed.run(context.Background(), inst, iflg, &say.Plain{W: &bytes.Buffer{}})
	if !errors.Is(err, stopped) {
		t.Fatalf("a failed install = %v, want the install's own error", err)
	}
	if want := "install"; strings.Join(order, " ") != want {
		t.Errorf("a box that never finished went %q, want %q: it must not be on a schedule", strings.Join(order, " "), want)
	}
}

// planWithQbittorrent is planWithData plus the app whose peer port the install
// has to decide: the migration switches it on after the configs are in place.
func planWithQbittorrent() *plan.Plan {
	p := planWithData()
	p.Apps["qbittorrent"] = plan.App{Kind: "kubernetes", Source: "qbittorrent", Config: "/srv/apps/qbittorrent", Namespace: "media"}
	return p
}

func TestMigrationInitTellsInitWhatThePlanWillSwitchOn(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	inst.record = new(string)
	answers, err := installAnswers(o, inst, planWithQbittorrent(), false)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	flags, err := migrationInit(inst, &chartRelease{}, answers, []string{"qbittorrent", "sonarr"}, 30047)
	if err != nil {
		t.Fatalf("migrationInit: %v", err)
	}
	if *flags.apps != "none" {
		t.Errorf("apps = %q, want none", *flags.apps)
	}
	if got := strings.Join(flags.switchesOn, ","); got != "qbittorrent,sonarr" {
		t.Errorf("the apps the plan switches on = %q, want qbittorrent and sonarr", got)
	}
	if flags.sourceTorrentPort != 30047 {
		t.Errorf("the source's torrent port = %d, want 30047", flags.sourceTorrentPort)
	}
}

func TestSourceTorrentPortReadsTheSnapshotsQbittorrent(t *testing.T) {
	dir := t.TempDir()
	opened := &bundle.Opened{Dir: dir, Configs: map[string]string{}}
	for _, instance := range []string{"sonarr", "qbittorrent"} {
		at := filepath.Join(dir, archive.ConfigPrefix, instance)
		opened.Configs[instance] = at
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(opened.Configs["qbittorrent"], "qBittorrent", "qBittorrent.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("[BitTorrent]\nSession\\Port=30047\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := sourceTorrentPort(planWithQbittorrent(), opened); got != 30047 {
		t.Errorf("sourceTorrentPort = %d, want the source's 30047", got)
	}
	// A plan without qBittorrent has no peer port to keep.
	if got := sourceTorrentPort(planWithData(), opened); got != 0 {
		t.Errorf("sourceTorrentPort without qBittorrent in the plan = %d, want 0", got)
	}
	// A snapshot with no config for it says nothing rather than guessing.
	empty := &bundle.Opened{Dir: dir, Configs: map[string]string{}}
	if got := sourceTorrentPort(planWithQbittorrent(), empty); got != 0 {
		t.Errorf("sourceTorrentPort with no config in the snapshot = %d, want 0", got)
	}
}
