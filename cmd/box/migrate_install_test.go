package main

// migrate_install_test.go proves the answers a host with no box is installed
// from: the plan holds the apps, the data root and the owner, and the four a
// plan cannot know are refused by name, together, when they were not given.
// Nothing here touches a host: installAnswers is the whole decision.

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
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
	got, err := installAnswers(o, inst, planWithData())
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
	_, err := installAnswers(o, inst, planWithData())
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
	got, err := installAnswers(o, inst, planWithData())
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
			_, err := installAnswers(o, inst, planWithData())
			if err == nil {
				t.Fatalf("login %q was accepted", login)
			}
			if !strings.Contains(err.Error(), login) {
				t.Errorf("the refusal does not name %q: %v", login, err)
			}
		})
	}
}

// With --box-namespace given the login names nothing but the account, so any
// login Authelia accepts goes through: the namespace rule is the derived
// namespace's, not the account's.
func TestInstallAnswersTakeAnyLoginWithAnExplicitNamespace(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.namespace = "s5n-alice"
	*inst.login = "Alice.B"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	if _, err := installAnswers(o, inst, planWithData()); err != nil {
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
	_, err := installAnswers(o, inst, p)
	if err == nil || !strings.Contains(err.Error(), "data root") {
		t.Fatalf("a plan with no data root was accepted with %v", err)
	}
}

// TestInstallAnswersLeaveTheOwnerToHostFoundations: a plan with no recorded
// owner names no uid, so init settles it the way host foundations does rather
// than being told 0.
func TestInstallAnswersLeaveTheOwnerToHostFoundations(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	p := planWithData()
	p.Source = nil
	got, err := installAnswers(o, inst, p)
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	for _, arg := range got {
		if arg == "--puid" || arg == "--pgid" {
			t.Errorf("the answers name an owner the plan did not: %v", got)
		}
	}
}

// TestMigrationInitTellsInitTheAppsAreComing: init runs here with every app
// off, because the migration switches on what it moved, so the flags it is
// given say so -- otherwise a migrating install tells the operator that
// nothing runs yet, one step before it switches everything on. Everything else
// is init's own: the plan's answers, and the release this run already loaded.
func TestMigrationInitTellsInitTheAppsAreComing(t *testing.T) {
	o, inst := installFlagSet(t)
	*inst.domain = "box.example.com"
	*inst.login = "someone"
	*inst.password = "/run/password"
	*inst.configPath = "/srv/config"
	inst.record = new(string)
	answers, err := installAnswers(o, inst, planWithData())
	if err != nil {
		t.Fatalf("installAnswers: %v", err)
	}
	charts := &chartRelease{}
	flags, err := migrationInit(inst, charts, answers)
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
	if _, err := migrationInit(inst, charts, []string{"--no-such-flag"}); err == nil {
		t.Fatal("an answer init has no flag for was accepted")
	} else if !strings.Contains(err.Error(), "init's flags") {
		t.Errorf("the refusal = %v, want it to say the plan does not read as init's flags", err)
	}
}

// TestInstallAnswersTakeThePlansOwnerOrNone: the uid:gid the source's files
// belonged to is the plan's (`source.owner`, which discovery writes when every
// planned app agrees), and init is handed it as its own flags so the data root
// and the copied configs keep the owner they had. A plan with no owner names
// neither flag: init then settles the ids the way it does on any host -- the
// flag, the data path's owner, $SUDO_UID, the invoking user -- rather than
// being handed a zero the grant would keep.
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
			got, err := installAnswers(o, inst, p)
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
