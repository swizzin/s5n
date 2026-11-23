// Package setup is `box init`: it asks the questions a seedbox has, writes the
// answers into the grant and the values file one key at a time, and hands over
// to `box apply`, whose diff and confirm are the review. There is no separate
// "here is your file" step (docs/box.md, "bootstrap and init").
//
// Two properties shape the code. Re-running init edits: every question is
// prefilled from the current merged values and only a changed answer is spliced,
// so hand edits elsewhere in either file survive byte for byte. And nothing is
// written until every question has an answer, so a cancel leaves both files
// exactly as they were.
//
// It is a pure decision over small interfaces, like internal/bootstrap: the UI
// is a Prompter, the host filesystem a PathHost, the files internal/seedbox, the
// questions internal/catalog, and the hand-over a function. Everything is tested
// from fixtures; the only implementation that touches a machine is
// LinuxPathHost, and only cmd/box builds it.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// Decision is `box init`'s whole flow.
type Decision struct {
	Host    PathHost
	Prompt  Prompter
	Catalog *catalog.Catalog
	// Apply is the hand-over: the same `box apply` a user runs. It is a
	// function rather than the release package so init does not link Helm.
	Apply ApplyFunc
	// Login installs root's login. init calls it from the hook apply runs
	// after its confirm and before its first write, so a refused confirm never
	// reaches it and a login that does not come up stops the run before the
	// release exists.
	Login LoginFunc
	// UserLookup names the OS account behind a uid, when the host has one.
	// init never creates an account; this only reads the host's own database,
	// and it is what makes the login's default the account that owns the box's
	// files rather than whoever ran sudo.
	UserLookup UserLookupFunc
	// HasLogin answers whether the users database already holds this login.
	// It is how init tells a first run (an empty password means box makes one)
	// from a re-run (an empty password keeps the one that is there).
	HasLogin HasLoginFunc
	// GatewayIP reads the ClusterIP of the Gateway's proxy Service, which is
	// how an OIDC app's pod reaches the issuer at the box's own name. It is
	// read by root's run and written into the grant; a user's apply only
	// renders what the grant says, because the Gateway's namespace is root's.
	GatewayIP func(context.Context) (string, error)
	// Schema is the chart's values.schema.json, so a box installing a release
	// from before the provider does not write a grant key that release would
	// refuse.
	Schema []byte
	// Say is every line init writes. A nil reporter writes nothing; cmd/box
	// always gives the plain or the live one.
	Say say.Reporter
	// Resolver answers what the box's domain resolves to, which decides
	// between Let's Encrypt and a self-signed certificate. Nil answers
	// "nothing", so the safe issuer is chosen rather than a DNS lookup
	// invented.
	Resolver Resolver
	// NodePorts lists every nodePort the cluster's Services already hold: one
	// LIST of Services in every namespace, which is a cluster-wide read only
	// root's kubeconfig may do (k0s's admin.conf is cluster-admin). It is how
	// the torrent port is picked free on a box that has none yet. Nil means
	// init cannot read a cluster, and a first install then refuses rather than
	// guessing a port the API server may already have given away.
	NodePorts func(ctx context.Context) ([]NodePort, error)
	// Rand answers a number in [0, n) for the torrent port pick. A field so a
	// test can pin the answer; nil means crypto/rand.
	Rand func(n int) int

	// steps is how many steps this run shows, for a prompter that draws a step
	// counter. It is advice for a UI, never an input: nothing reads it back.
	steps int
}

// step tells a prompter that draws progress where the next answer belongs.
// Prompters without a Stepper (LinePrompt) hear nothing.
func (d *Decision) step(n int, question string) {
	if st, ok := d.Prompt.(Stepper); ok {
		st.Step(n, d.steps, question)
	}
}

// Options are one `box init`. Every grant question has a field here: that is
// what makes an unattended run possible, and the flags are the same names.
type Options struct {
	// Chart is the release box is installing: the defaults every question is
	// prefilled from and the schema every answer is checked against. init does
	// not read charts/ from its own tree, so the caller loads the published
	// release and hands it over.
	Chart      seedbox.Chart
	GrantPath  string
	ValuesPath string

	// Namespace is the release namespace. It is not one of the questions init
	// asks: a seedbox's namespace is its identity, so a re-run keeps whatever
	// the grant already says. The flag exists for the first run.
	Namespace  string
	Domain     string
	DataPath   string
	ConfigPath string
	// Apps is the grant's app list, by catalog name ("sonarr", not
	// "sonarr-4k": instances are root's, through `box install`).
	Apps []string
	// SwitchesOn are the apps another flow turns on after this install, by
	// instance name: `box migrate apply` installs the box with every app off
	// and switches the plan's on once their configs are in place, so a
	// decision that belongs to the box and not to this run's app list -- the
	// torrent port -- is still decided for it. It never becomes the app list
	// init writes: Apps is.
	SwitchesOn []string
	// TorrentPort is the peer port the source install's qBittorrent announced,
	// when a migration read one from its config. init keeps it if this cluster
	// can use it (it is in the NodePort range and no Service holds it), because
	// trackers and any forward the operator set already point at it, and picks
	// one from the band otherwise. Zero means the source named none.
	TorrentPort int
	// PUID and PGID are the ids every file on the data path is owned by.
	// PUIDSet and PGIDSet say the operator named them, so they win over a
	// re-run's grant; without them the grant's own values do.
	PUID, PGID int
	PUIDSet    bool
	PGIDSet    bool
	// SetTransient answers a transient question by its values path, which is
	// the unattended form of a question that cannot live in a file.
	SetTransient map[string]string

	// Login is the account root's login admits (--login). LoginSet says the
	// flag was given, which is what makes it win over a re-run's grant.
	Login    string
	LoginSet bool
	// DefaultLogin is what a fresh box offers when no flag was given: the
	// invoking user's account name, which cmd/box reads from the environment.
	DefaultLogin string
	// PasswordFile is where the login's password is read from (--password-file,
	// "-" for stdin). Empty means box asks, or generates one.
	PasswordFile string

	// Unattended asks nothing: flags and file keys only, and a missing required
	// answer is an error naming the flag or key.
	Unattended bool
	// Adopting says another flow is running init as one step of its own, and
	// that the empty app list is deliberate: `box migrate apply` installs a box
	// with every app off and switches on what it moved once the configs are in
	// place. The line init prints for an empty box ("nothing runs yet ... add
	// one with box install") would be wrong there, so this suppresses it.
	//
	// It also makes every answer that flow already holds a decision rather than
	// a default: the data root and the app list come from the plan, so neither
	// is asked, and a flag that was given is used as it is instead of being
	// offered back as a starting answer. A run that was given all four of its
	// own answers asks nothing at all; one missing is asked with init's own
	// question, which is what makes a guided migration on a host with no box
	// ask only for what it lacks. The notice address belongs to bootstrap's
	// host, not to this install, so it is not asked here either.
	Adopting bool
	// PathsInNode says the storage paths live inside a test node (kind), not on
	// this host, so init neither verifies nor creates them. `box apply` has the
	// same flag for the same reason.
	PathsInNode bool
	// NoticePath is root's config, beside the cluster record. When the file is
	// there and its notice address is empty, init asks for one -- it is the one
	// answer that belongs to the host rather than to the seedbox. Empty means
	// this run does not ask (a box whose config bootstrap did not write).
	NoticePath string
}

// ApplyOptions is what init hands the applier: the same arguments `box apply`
// takes, plus the hook for answers that outlive no file.
type ApplyOptions struct {
	GrantPath   string
	ValuesPath  string
	PathsInNode bool
	// AfterConfirm asks the transient questions, after apply's own confirm.
	AfterConfirm func(ctx context.Context) (map[string]any, error)
	// TransientKeys are the values paths AfterConfirm answers, so the apply
	// keeps them out of its diff.
	TransientKeys []string
	// Login installs root's login, after the same confirm and before the
	// first write. It is separate from AfterConfirm because its failure is a
	// refusal (the release must not exist behind a login nobody can pass),
	// not an answer to merge.
	Login func(ctx context.Context) error
	// Issuer is the ClusterIssuer this run writes the box's own host
	// Certificate with, for root's host-CA step, which runs before init's
	// per-host step writes that Certificate: on a first init there is nothing
	// in the cluster to read it from.
	Issuer string
	// HostIssuers is the issuer init decided for each name it serves, by
	// host. The apply's plan compares each Certificate that already answers
	// with its own host's decision, so a name moved to Let's Encrypt by this
	// run is written even though tls.issuer is empty, and its per-host step
	// writes that same issuer instead of resolving the name again.
	HostIssuers map[string]string
}

// ApplyFunc is one `box apply`.
type ApplyFunc func(ctx context.Context, opts ApplyOptions) error

// Result is what init did, for the caller to report and a test to assert.
type Result struct {
	ReRun bool
	// Domain is the host name the box answered with, so the caller can close
	// the run with the one fact a person wants repeated back.
	Domain string
	// Issuer is the ClusterIssuer the host's certificate should name:
	// IssuerLetsEncrypt or IssuerSelfSigned. It is the values file's
	// tls.issuer when that is set, and otherwise what the domain resolved to.
	Issuer string
	// Hosts is every certificate name this run decided, the box's domain
	// first and then one per app served at a name of its own. Each carries its
	// own issuer: tls.issuer forces them all, and otherwise each name's own
	// answer decides.
	Hosts []Host
	// GrantChanged and ValuesChanged are the keys spliced, in the order they
	// were written. A re-run with identical answers leaves both empty.
	GrantChanged  []string
	ValuesChanged []string
	// Created are the directories init made, owned by the box's uid:gid.
	Created []string
	// Login is the account the login admits, and NewPassword says this run
	// supplied one. GeneratedPassword is set only when box made it, and the
	// caller prints it once: it is nowhere on disk.
	Login             string
	NewPassword       bool
	GeneratedPassword string
	// Applied says the hand-over ran. It does not say the release changed:
	// apply decides that, and a re-run is a no-op there too.
	Applied bool
}

// LoginFunc installs root's login: it is internal/login's Apply, and its
// options are the four values init decided.
type LoginFunc func(ctx context.Context, l Login) error

// UserLookupFunc names one OS account by its uid. The second return says the
// host has an account for it: a uid with no account is not a login name.
type UserLookupFunc func(uid int) (string, bool)

// HasLoginFunc answers whether the users database already has a row. The file
// lives on the login's claim, so the login package reads it; init only needs
// the yes or no.
type HasLoginFunc func(login string) bool

// Login is one login install as init decided it. Password is the plaintext for
// this run only: the login hashes it and writes the digest, and nothing here
// is ever written to a file.
type Login struct {
	Host        string
	Name        string
	Password    string
	NewPassword bool
	// Hosted are the apps the chart serves at a name of their own
	// (plex.example.com, seerr.example.com). The login's rules need them: the
	// edge checks every listener, so each app host needs the rule that matches
	// the login posture the app had on its path.
	Hosted []string
	// OwnLogin are the apps the chart's catalog says sign their users in
	// themselves. Their host or path prefix bypasses the login: an app with
	// its own sign-in behind the login would ask for two passwords.
	OwnLogin []string
	// Values is the document the release will render: chart defaults, then
	// the user's file, then the grant, read back after init wrote them. The
	// access_control rules come from its instance list, so the login and the
	// release cannot disagree about which URL prefixes exist.
	Values map[string]any
	// Namespace is the seedbox's namespace, half of every OIDC client id.
	Namespace string
	// OIDC are the apps that sign in through the login, from the chart's
	// catalog: a box's first one is what makes it a provider at all.
	OIDC []string
}

// report is the one place init writes progress.
func (d *Decision) report() say.Reporter { return say.Or(d.Say, nil) }

// stop prints the voice for a cancelled question and passes the error on.
func (d *Decision) stop(err error) error {
	if errors.Is(err, ErrCancelled) {
		d.report().Line("oh heck, nvm")
	}
	return err
}

// Run asks the questions, edits the two files and hands over to apply.
func (d *Decision) Run(ctx context.Context, opts Options) (Result, error) {
	var res Result
	if err := d.check(); err != nil {
		return res, err
	}
	// Root's config is read first, and strictly: a file init cannot parse stops
	// the run before its first question, naming the line, so nothing is asked
	// and nothing is written. init writes one line into that file, and it is
	// the same read `box update` and `box status` make.
	if _, _, err := readNotice(opts.NoticePath); err != nil {
		return res, err
	}
	if len(opts.Chart.Values) == 0 || len(opts.Chart.Schema) == 0 {
		return res, errors.New("no chart: box init asks its questions from the chart release it installs")
	}
	sb, err := loadSeedbox(opts.GrantPath, opts.ValuesPath)
	if err != nil {
		return res, err
	}
	res.ReRun = sb.Grant.Exists

	// The baseline every question is prefilled from: what the two files say
	// today (merged, checked against the chart) or, on a fresh host, the chart's
	// own defaults with every app off.
	base, err := d.baseline(sb, opts.Chart)
	if err != nil {
		return res, err
	}
	grantNow := sb.Grant.Map()

	// The grant's questions, in the order docs/box.md lists them.
	//
	// The flow has four steps of its own, and a fifth when an app has
	// questions of its own (the catalog's qBittorrent does). A prompter that
	// draws a counter is told, and a prompter without one never notices.
	d.steps = 4
	for _, name := range d.appsOn(grantNow, res.ReRun) {
		if app, ok := d.Catalog.App(name); ok && len(app.Stored()) > 0 {
			d.steps = 5
			break
		}
	}
	d.step(1, "what domain will this box answer on?")
	domain, err := d.askText(textQuestion{
		key: "gateway.host", flag: "--domain",
		prompt: "domain", help: "the name this box is reached at, e.g. box.example.com",
		value: opts.Domain, prefill: prefill(base, grantNow, "gateway.host"),
		unattended: opts.Unattended, decided: opts.Adopting && strings.TrimSpace(opts.Domain) != "", validate: validDomain,
	})
	if err != nil {
		return res, d.stop(err)
	}
	res.Domain = domain
	// The certificate's issuer follows the name, not the node: a public
	// answer gets Let's Encrypt, and a private one, no answer or a forced
	// tls.issuer says otherwise. init says which and why before it asks
	// anything else, so the operator can correct the domain.
	// tls.issuer is the operator's own file, not a chart default: on a first
	// run base is the chart's values only, so the answer is read from the
	// values document first.
	forced, _ := lookupPath(base, "tls.issuer")
	if v, ok := lookupPath(sb.Values.Map(), "tls.issuer"); ok && text(v) != "" {
		forced = v
	}
	domAnswer := resolveHost(ctx, d.Resolver, domain)
	issuer, why := issuerFor(domain, domAnswer, text(forced))
	res.Issuer = issuer
	noDNS := !domAnswer.resolves() && issuer == IssuerSelfSigned
	record := ""
	if noDNS {
		record = "add one"
	}
	res.Hosts = []Host{{Host: domain, Issuer: issuer, Why: why, NoDNS: noDNS, Record: record}}
	d.report().Line("tls: " + why)
	d.step(2, "where should your data and settings live?")
	dataPath, err := d.askText(textQuestion{
		key: "storage.data.path", flag: "--data-path",
		prompt: "data path", help: "where this user's downloads and media live",
		value: opts.DataPath, prefill: prefill(base, grantNow, "storage.data.path"),
		unattended: opts.Unattended, decided: opts.Adopting, validate: validHostPath,
	})
	if err != nil {
		return res, d.stop(err)
	}
	configPath, err := d.askText(textQuestion{
		key: "storage.config.path", flag: "--config-path",
		prompt: "config path", help: "where this user's apps keep their settings",
		value: opts.ConfigPath, prefill: prefill(base, grantNow, "storage.config.path"),
		unattended: opts.Unattended, decided: opts.Adopting && strings.TrimSpace(opts.ConfigPath) != "", validate: validHostPath,
	})
	if err != nil {
		return res, d.stop(err)
	}
	if err := checkPathPair(dataPath, configPath); err != nil {
		return res, err
	}

	// Who the files belong to is settled here, after the data path is known and
	// before anything is reported about it: a data path that already holds
	// files names its own owner, and the report below says which ids init is
	// working with.
	puid, pgid, err := d.owners(opts, grantNow, res.ReRun, dataPath)
	if err != nil {
		return res, d.stop(err)
	}
	if !opts.Unattended {
		d.report().Fact("files", filesLine(d.UserLookup, puid, pgid))
	}

	// What is at those paths, and whether a missing one may be created. The
	// report is read-only; the directories are made after the last question, so
	// a cancel here still leaves the host alone.
	paths := []pathTarget{
		{kind: "data", flag: "--data-path", path: dataPath},
		{kind: "config", flag: "--config-path", path: configPath},
	}
	if !opts.PathsInNode {
		d.step(2, "where should your data and settings live?")
		if err := d.reportPaths(paths, puid, pgid, opts.Unattended); err != nil {
			return res, d.stop(err)
		}
	}

	d.step(3, "which apps should run here?")
	apps, err := d.askApps(opts, grantNow, res.ReRun)
	if err != nil {
		return res, d.stop(err)
	}
	if len(apps) == 0 && !opts.Adopting {
		// Said once, and not asked again: an empty box is a state, and the way
		// out of it is a command rather than another form. A migration says
		// nothing here: it is about to switch the apps it moved on.
		d.report().Muted("nothing runs yet: this box has no apps. add one with `box install <app>`, or run `box migrate` for an install that already exists")
	}
	// The app list decides whether there is a fifth step, so correct the
	// counter before the questions that belong to it.
	d.steps = 4
	for _, name := range apps {
		if app, ok := d.Catalog.App(name); ok && len(app.Stored()) > 0 {
			d.steps = 5
			break
		}
	}
	// Root's notice address is its own step, and the last one: it is the
	// host's, not the seedbox's. It is only asked when bootstrap wrote the file
	// and left the address empty, so a re-run with one set asks nothing.
	askNotice := false
	pending, err := d.noticePending(opts)
	if err != nil {
		return res, err
	}
	askNotice = pending && !opts.Unattended && !opts.Adopting
	if askNotice {
		d.steps++
	}

	// The names the chosen apps are served at, before anything is written for
	// one: an app's own host has to answer where the domain does, or the
	// certificate for it can never issue. Both names resolving to nothing is
	// the test-box case and proceeds self-signed; a real mismatch stops here,
	// naming the record to add (docs/box.md, "Certificates").
	appHosts, err := appHostsFor(ctx, d.Resolver, d.Catalog, domain, domAnswer, text(forced), "init", apps)
	if err != nil {
		return res, err
	}
	for _, h := range appHosts {
		res.Hosts = append(res.Hosts, h)
		d.report().Line("tls: " + h.Why)
	}

	// The login: one account, asked once. It is its own step because it is the
	// one answer that is not the seedbox's shape but the identity on it, and
	// every path in front of the Gateway is admitted only to this user.
	d.step(4, "who logs in to this box?")
	loginName, err := d.askLogin(opts, base, grantNow, puid, derivesNamespace(opts, grantNow, res.ReRun))
	if err != nil {
		return res, d.stop(err)
	}
	password, newPassword, generated, err := d.askPassword(opts, loginName)
	if err != nil {
		return res, d.stop(err)
	}
	res.Login, res.NewPassword, res.GeneratedPassword = loginName, newPassword, generated

	// The namespace follows the login: a seedbox's is s5n-<login>, beside box's
	// own s5n-system and s5n-gateway. A re-run keeps the grant's, so a box that
	// already has `seedbox` keeps it whatever this run's login is, and an
	// explicit --namespace still wins on a first run.
	namespace, err := d.namespace(opts, grantNow, res.ReRun, loginName)
	if err != nil {
		return res, err
	}
	// The peer port: a port on the node, so root settles it once and the grant
	// keeps it from then on. It is read before any answer is written, because a
	// cluster with no free port in the band is a refusal and not a
	// half-finished dialog.
	port, err := d.torrentPort(ctx, namespace, apps, grantNow, opts)
	if err != nil {
		return res, err
	}

	// Each selected app's own questions, from the catalog. A transient one is
	// not asked here: it goes to the apply, after its confirm.
	var stored []answer
	var transient []catalog.Question
	for _, name := range apps {
		app, ok := d.Catalog.App(name)
		if !ok {
			continue
		}
		transient = append(transient, app.Transient()...)
		if opts.Unattended {
			continue
		}
		if len(app.Stored()) > 0 {
			d.step(5, "how should "+name+" be set up?")
		}
		for _, q := range app.Stored() {
			value, err := d.askQuestion(q, base)
			if err != nil {
				return res, d.stop(err)
			}
			stored = append(stored, answer{key: q.Key, value: value})
		}
	}

	// Where box tells someone who is not watching what it did. It is asked here,
	// after the seedbox's own questions, and written with the other answers
	// below: a cancel before this point writes nothing at all.
	noticeURL, err := d.askNoticeURL(askNotice)
	if err != nil {
		return res, d.stop(err)
	}

	transientKeys := make([]string, 0, len(transient))
	for _, q := range transient {
		transientKeys = append(transientKeys, q.Key)
	}
	if err := checkTransientFlags(opts.SetTransient, transientKeys); err != nil {
		return res, err
	}
	if opts.Unattended {
		for _, q := range transient {
			if q.Optional {
				continue
			}
			if strings.TrimSpace(opts.SetTransient[q.Key]) == "" {
				return res, fmt.Errorf("%s is needed for this install: pass --set-transient %s=<value>", q.Prompt, q.Key)
			}
		}
	}

	grantAnswers := []answer{
		{key: "namespace", value: namespace},
		{key: "login", value: loginName},
		{key: "global.puid", value: puid},
		{key: "global.pgid", value: pgid},
		{key: "storage.data.path", value: dataPath},
		{key: "storage.config.path", value: configPath},
		{key: "gateway.host", value: domain},
	}
	for _, name := range d.Catalog.Names() {
		grantAnswers = append(grantAnswers, answer{key: "apps." + name + ".enabled", value: contains(apps, name)})
	}
	// The peer port last, so the grant reads app by app: what is on, then the
	// one number that app cannot share with its own settings file.
	if port != nil {
		grantAnswers = append(grantAnswers, answer{key: torrentPortKey, value: port})
	}
	// The Gateway's proxy address, for an app that signs in through the login.
	// It is written here because init is root's and the read is root's: a
	// user's apply renders the value from the grant and never touches the
	// Gateway's namespace. An address that cannot be read yet is not a
	// failure -- the chart renders the app without it, and the next root run
	// fills it in -- but it is said out loud, because the app will not start
	// until it is there.
	//
	// The question is whether this chart serves an app that way, not whether
	// one of the apps this run switched on does. A migrate install answers init
	// with every app off and switches the plan's on afterwards, from the grant
	// (cmd/box/migrate_apply.go), so "an OIDC app is on now" was false on the
	// one run that had two to install: autobrr and qui rendered without the
	// alias, their client Secrets went unused, and autobrr showed its own
	// password form. The address is the box's, not the app's; a box that never
	// switches one on carries an unused key, and a re-run of init refreshes it.
	if d.GatewayIP != nil {
		declared, err := seedbox.DeclaresOIDCGatewayIP(d.Schema)
		if err != nil {
			return res, err
		}
		if declared {
			ip, err := d.GatewayIP(ctx)
			if err != nil {
				return res, err
			}
			if strings.TrimSpace(ip) == "" {
				// Only an app that is on now waits for it: an empty box is not
				// waiting on anything, and a warning about an app nobody
				// switched on is noise.
				if len(d.Catalog.OIDCApps(apps)) > 0 {
					d.report().Warn("the Gateway's proxy Service is not there yet, so the OIDC apps have no address for the box's own name: run `box init` again once `box bootstrap` has installed the Gateway")
				}
			} else {
				grantAnswers = append(grantAnswers, answer{key: "auth.oidc.gatewayIP", value: ip})
			}
		}
	}

	res.GrantChanged, res.ValuesChanged, err = d.writeFiles(opts, sb, grantNow, base, grantAnswers, stored)
	if err != nil {
		return res, err
	}
	d.reportWrites(opts, res)
	if noticeURL != "" {
		if err := d.writeNoticeURL(opts.NoticePath, noticeURL); err != nil {
			return res, err
		}
	}

	if !opts.PathsInNode {
		var dirs []string
		for _, t := range paths {
			if t.create {
				dirs = append(dirs, t.path)
			}
		}
		dirs = append(dirs, SettingsDirs(configPath, apps, base)...)
		res.Created, err = d.makeDirs(dirs, puid, pgid)
		if err != nil {
			return res, err
		}
		if len(res.Created) > 0 {
			d.report().Created(res.Created)
		}
	}

	if d.Apply == nil {
		return res, errors.New("no apply: init edits the two files and hands over to `box apply`")
	}
	req := ApplyOptions{
		GrantPath:     opts.GrantPath,
		ValuesPath:    opts.ValuesPath,
		PathsInNode:   opts.PathsInNode,
		TransientKeys: transientKeys,
		Issuer:        res.Issuer,
		HostIssuers:   hostIssuers(res.Hosts),
	}
	if d.Login != nil {
		// The rules the login writes are derived from the document the release
		// will render. Its apps section is the one part init just decided, and
		// the instances under them are grant keys, so this is the same list the
		// chart derives -- a 4K instance the operator enabled has its own
		// rules, and one that is off has none.
		l := Login{
			Host:        domain,
			Name:        loginName,
			Password:    password,
			NewPassword: newPassword,
			Values:      withApps(base, apps, d.Catalog),
			Hosted:      appHostApps(appHosts),
			OwnLogin:    d.Catalog.SelfAuthenticating(),
			Namespace:   namespace,
			OIDC:        d.Catalog.OIDCApps(apps),
		}
		req.Login = func(ctx context.Context) error { return d.Login(ctx, l) }
	}
	if len(transient) > 0 {
		req.AfterConfirm = d.transientAsker(transient, opts)
	}
	if err := d.Apply(ctx, req); err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}

// check refuses a Decision that cannot run, before anything reads a file.
func (d *Decision) check() error {
	switch {
	case d.Host == nil:
		return errors.New("no host: box init needs the path probes")
	case d.Prompt == nil:
		return errors.New("no prompter: box init needs a way to ask")
	case d.Catalog == nil:
		return errors.New("no catalog: box init's questions come from the chart's catalog.yaml")
	}
	return nil
}

// baseline is the document every question is prefilled from.
func (d *Decision) baseline(sb *seedbox.Seedbox, chart seedbox.Chart) (map[string]any, error) {
	if !sb.Grant.Exists {
		return seedbox.Base(chart)
	}
	values, err := sb.Effective(chart)
	if err != nil {
		// init edits what is there; it does not replace it. A file that no
		// longer checks out has to be fixed first, or the prefill would be a
		// guess and the splice would hide the mistake.
		return nil, fmt.Errorf("this seedbox's files do not check out, and init edits them rather than replacing them: %w", err)
	}
	return values, nil
}

// textQuestion is one grant answer: a flag when unattended, a prompt otherwise,
// with the current file value as the default in both.
type textQuestion struct {
	key        string
	flag       string
	prompt     string
	help       string
	value      string
	prefill    any
	unattended bool
	// decided says this answer is already known: the flow that runs init (a
	// migration, whose plan holds the data root) answers it, so init uses it as
	// it is and never offers it back as a prompt's starting answer. An empty
	// one is a refusal naming the flag rather than a question, because the
	// flow that decided it is the one that has to supply it.
	decided  bool
	validate func(string) error
}

func (d *Decision) askText(q textQuestion) (string, error) {
	given := strings.TrimSpace(q.value)
	if q.decided {
		if given == "" {
			return "", fmt.Errorf("%s is needed: pass %s", q.key, q.flag)
		}
		if q.validate != nil {
			if err := q.validate(given); err != nil {
				return "", fmt.Errorf("%s %q: %w", q.flag, given, err)
			}
		}
		return given, nil
	}
	if q.unattended {
		if given == "" {
			return "", fmt.Errorf("%s is needed: pass %s", q.key, q.flag)
		}
		if q.validate != nil {
			if err := q.validate(given); err != nil {
				return "", fmt.Errorf("%s %q: %w", q.flag, given, err)
			}
		}
		return given, nil
	}
	def := strings.TrimSpace(text(q.prefill))
	if given != "" {
		// A flag given in guided mode is the starting answer, not a decision:
		// the prompt still shows it and the operator can change it.
		def = given
	}
	return d.Prompt.Input(Input{
		Key: q.key, Prompt: q.prompt, Help: q.help, Default: def, Validate: q.validate,
	})
}

// namespace resolves the one grant key that is not a question. A seedbox's
// namespace is its identity -- claim names and storage are derived from it -- so
// a re-run never changes it, whatever the flag says. A first run takes the flag
// when it was given, and otherwise derives s5n-<login>: every seedbox gets one,
// root's included, beside box's own s5n-system and s5n-gateway.
func (d *Decision) namespace(opts Options, grantNow map[string]any, reRun bool, login string) (string, error) {
	if ns, ok := grantNamespace(grantNow, reRun); ok {
		return ns, nil
	}
	ns := strings.TrimSpace(opts.Namespace)
	if ns != "" {
		if err := validNamespace(ns); err != nil {
			return "", fmt.Errorf("--namespace %q: %w", ns, err)
		}
		return ns, nil
	}
	return NamespaceFor(login)
}

// grantNamespace is the namespace a re-run keeps: the grant's, when it names one.
func grantNamespace(grantNow map[string]any, reRun bool) (string, bool) {
	if !reRun {
		return "", false
	}
	if ns, ok := lookupPath(grantNow, "namespace"); ok {
		if s, ok := ns.(string); ok && strings.TrimSpace(s) != "" {
			return s, true
		}
	}
	return "", false
}

// derivesNamespace says whether this run names the namespace from the login:
// not when a re-run keeps the grant's, and not when --namespace names it.
func derivesNamespace(opts Options, grantNow map[string]any, reRun bool) bool {
	if _, ok := grantNamespace(grantNow, reRun); ok {
		return false
	}
	return strings.TrimSpace(opts.Namespace) == ""
}

// id is one of puid and pgid while it is still being decided: the value, and
// whether something has already answered for it.
type id struct {
	value int
	set   bool
}

// take records the answer for this id.
func (i *id) take(v int) { i.value, i.set = v, true }

// owners resolves puid and pgid, in the order docs/box.md sets out: the flag
// when it was given, the grant's value on a re-run, the owner of a data path
// that already holds files, and only then what cmd/box derived from the invoking
// user (sudo's ids, else the caller's).
//
// The data path comes before sudo's ids because the files are the answer: an
// install that changed the owner of an existing library would leave the apps
// unable to write it. It is never read through a symlink.
func (d *Decision) owners(opts Options, grantNow map[string]any, reRun bool, dataPath string) (int, int, error) {
	var puid, pgid id
	if opts.PUIDSet {
		puid.take(opts.PUID)
	}
	if opts.PGIDSet {
		pgid.take(opts.PGID)
	}
	// An installed box keeps its owner: a stray file does not re-chown a release.
	if reRun && !puid.set {
		if v, ok := intAt(grantNow, "global.puid"); ok {
			puid.take(v)
		}
	}
	if reRun && !pgid.set {
		if v, ok := intAt(grantNow, "global.pgid"); ok {
			pgid.take(v)
		}
	}
	if !puid.set || !pgid.set {
		uid, gid, holds, err := d.dataOwner(dataPath)
		if err != nil {
			return 0, 0, err
		}
		if holds {
			if !puid.set {
				puid.take(uid)
			}
			if !pgid.set {
				pgid.take(gid)
			}
		}
	}
	if !puid.set {
		puid.take(opts.PUID)
	}
	if !pgid.set {
		pgid.take(opts.PGID)
	}
	return d.usableIDs(opts, dataPath, puid.value, pgid.value)
}

// usableIDs is the last word on the two ids: root is never the answer, because
// the apps run unprivileged and a root-owned data path is one they could not
// write. Unattended names the flags; a terminal asks, because the operator knows
// whose files those are.
func (d *Decision) usableIDs(opts Options, dataPath string, puid, pgid int) (int, int, error) {
	if puid > 0 && pgid > 0 {
		return puid, pgid, nil
	}
	if opts.Unattended {
		return 0, 0, fmt.Errorf("--puid and --pgid are needed: %s is owned by root (0:0), and the apps do not run as root", dataPath)
	}
	d.step(2, "who owns the files in "+dataPath+"?")
	var err error
	if puid <= 0 {
		if puid, err = d.askID("global.puid", "file owner uid", "the uid of the account that owns the data path's files"); err != nil {
			return 0, 0, err
		}
	}
	if pgid <= 0 {
		if pgid, err = d.askID("global.pgid", "file owner gid", "the gid of that account's group"); err != nil {
			return 0, 0, err
		}
	}
	return puid, pgid, nil
}

// dataOwner is the account that owns the data path, when the path already holds
// something. A missing path, an empty one, a file and a symlink are all "no
// data": the first two because nobody owns an empty directory yet, the last
// because box never reads through a link, and the owner on the other side of one
// is not the box's answer.
func (d *Decision) dataOwner(path string) (int, int, bool, error) {
	info, err := d.Host.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, 0, false, nil
	case err != nil:
		return 0, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	if info.Symlink {
		d.report().Warn(fmt.Sprintf("%s is a symlink: box does not read through it, so the owner of what it points at is not used for the box's files", path))
		return 0, 0, false, nil
	}
	if !info.IsDir {
		return 0, 0, false, nil
	}
	entries, _, err := d.Host.ReadDir(path, 1)
	if err != nil {
		return 0, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(entries) == 0 {
		return 0, 0, false, nil
	}
	return info.UID, info.GID, true, nil
}

// askID asks for one id the operator has to name, because the host gave no
// answer box may use.
func (d *Decision) askID(key, prompt, help string) (int, error) {
	answer, err := d.Prompt.Input(Input{Key: key, Prompt: prompt, Help: help, Validate: validID})
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s %q: %w", key, answer, validID(answer))
	}
	return n, nil
}

// validID is one uid or gid: a number, and never 0.
func validID(v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return errors.New("a number, the account's id")
	}
	if n <= 0 {
		return errors.New("0 is root, and the apps do not run as root")
	}
	return nil
}

// filesLine is the record line for the account the box's files belong to:
// "core (uid 1000, gid 1000)", with the name when the host has one.
func filesLine(lookup UserLookupFunc, puid, pgid int) string {
	ids := fmt.Sprintf("uid %d, gid %d", puid, pgid)
	if lookup == nil {
		return ids
	}
	name, ok := lookup(puid)
	if !ok || strings.TrimSpace(name) == "" {
		return ids
	}
	return name + " (" + ids + ")"
}

// appsOn is the app list init offers preselected: the grant's on a re-run, the
// catalog's defaults on a fresh box. `--apps` is not here: in guided mode it is
// a starting answer the multi-select still shows, so the only use for this list
// is the step count before that question is asked.
func (d *Decision) appsOn(grantNow map[string]any, reRun bool) []string {
	names := d.Catalog.Names()
	var current []string
	if reRun {
		for _, name := range names {
			if on, ok := lookupPath(grantNow, "apps."+name+".enabled"); ok {
				if b, _ := on.(bool); b {
					current = append(current, name)
				}
			}
		}
		return current
	}
	return d.Catalog.Defaults()
}

// appsNone is the word that answers the app question with nothing: an explicit
// empty list. `--apps ""` is not it -- an empty flag is a flag nobody filled in
// -- and neither is leaving the picker's cursor where it started.
const appsNone = "none"

// AskApps resolves the app list: the flag, a multi-select otherwise,
// preselected from the grant on a re-run and from the catalog on a fresh box.
// There is no multi-select when another flow runs init: a migration's plan
// decides every app, and it hands init the list (`--apps none`) because the
// migration switches on what it moved at its end. The picker never appears
// there, which is also why `--apps` is required rather than optional on that
// path.
//
// Nothing is a legitimate answer. A box with no apps is a state of its own:
// apps arrive later with `box install`, or all at once with the install
// `box migrate apply` adopts into, and that is why the picker's minimum is zero.
func (d *Decision) askApps(opts Options, grantNow map[string]any, reRun bool) ([]string, error) {
	names := d.Catalog.Names()
	options := make([]Option, 0, len(names))
	for _, name := range names {
		app, _ := d.Catalog.App(name)
		options = append(options, Option{Value: name, Label: name, Help: app.Description})
	}

	current := d.appsOn(grantNow, reRun)

	// "none" is the empty list spelled out, so it can never be one of the
	// answers beside an app: one of the two is not what the operator meant, and
	// guessing which would install the wrong box.
	var given []string
	none := false
	for _, part := range splitApps(opts.Apps) {
		if strings.EqualFold(part, appsNone) {
			none = true
			continue
		}
		given = append(given, part)
	}
	if none && len(given) > 0 {
		return nil, fmt.Errorf("--apps %s: %s is the empty list, so it cannot be one of the apps", strings.Join(splitApps(opts.Apps), ", "), appsNone)
	}

	if opts.Unattended || opts.Adopting {
		switch {
		case none:
			return nil, nil
		case len(given) == 0:
			if reRun {
				return current, nil
			}
			return nil, fmt.Errorf("--apps is needed: comma separated app names from %s, or %s for a box with nothing on yet", strings.Join(names, ", "), appsNone)
		}
		return checkApps(given, names)
	}

	selected := current
	switch {
	case none:
		selected = nil
	case len(given) > 0:
		chosen, err := checkApps(given, names)
		if err != nil {
			return nil, err
		}
		selected = chosen
	}
	return d.Prompt.MultiSelect(MultiSelect{
		Key:      "apps",
		Prompt:   "which apps should this box have",
		Options:  options,
		Selected: selected,
		Min:      0,
	})
}

// splitApps flattens --apps: one flag value that may carry commas, or several
// if it is ever made repeatable. Empty pieces disappear, so `--apps ""` is the
// same as not passing it.
func splitApps(given []string) []string {
	var out []string
	for _, raw := range given {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// checkApps validates an app list against the catalog and keeps the catalog's
// order, so the grant and the report are stable.
func checkApps(given []string, names []string) ([]string, error) {
	want := map[string]bool{}
	for _, part := range given {
		if !contains(names, part) {
			return nil, fmt.Errorf("--apps %s: the catalog has no such app (it has %s)", part, strings.Join(names, ", "))
		}
		want[part] = true
	}
	var out []string
	for _, name := range names {
		if want[name] {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--apps names no app; choose from %s", strings.Join(names, ", "))
	}
	return out, nil
}

// askQuestion asks one stored catalog question, prefilled from the current
// merged values so a re-run shows the answer that is in force.
func (d *Decision) askQuestion(q catalog.Question, base map[string]any) (any, error) {
	current, hasCurrent := lookupPath(base, q.Key)
	if q.Kind == catalog.KindBool {
		def := false
		if b, ok := current.(bool); ok && hasCurrent {
			def = b
		} else if b, ok := q.Default.(bool); ok {
			def = b
		}
		return d.Prompt.Confirm(Confirm{Prompt: q.Prompt, Default: def})
	}

	def := ""
	if hasCurrent {
		def = text(current)
	}
	if def == "" {
		if s, ok := q.Default.(string); ok {
			def = s
		}
	}
	if q.Kind == catalog.KindSecret {
		// A stored secret is never offered back as a default: pressing enter
		// keeps it (writeFiles skips an empty answer over a current value).
		return d.Prompt.Secret(Secret{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Optional: true})
	}
	var validate func(string) error
	if !q.Optional {
		validate = nonEmpty
	}
	return d.Prompt.Input(Input{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Default: def, Validate: validate})
}

// transientAsker builds the hook apply calls after its confirm. Unattended, it
// is the --set-transient answers; guided, it asks, and an empty answer is not an
// answer.
func (d *Decision) transientAsker(questions []catalog.Question, opts Options) func(context.Context) (map[string]any, error) {
	return func(ctx context.Context) (map[string]any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out := make(map[string]any, len(questions))
		for _, q := range questions {
			if v := strings.TrimSpace(opts.SetTransient[q.Key]); v != "" {
				out[q.Key] = v
				continue
			}
			if opts.Unattended {
				continue
			}
			v, err := d.Prompt.Secret(Secret{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Optional: true})
			if err != nil {
				if errors.Is(err, ErrCancelled) {
					d.report().Line("oh heck, nvm")
				}
				return nil, err
			}
			if strings.TrimSpace(v) != "" {
				out[q.Key] = v
			}
		}
		return out, nil
	}
}

// answer is one key and the value it should hold.
type answer struct {
	key   string
	value any
}

// writeFiles splices every changed answer into its file. Both files are loaded,
// each key is compared with what is there, and only a difference is written, so
// a re-run with the same answers touches nothing and hand edits elsewhere
// survive byte for byte.
func (d *Decision) writeFiles(opts Options, sb *seedbox.Seedbox, grantNow, base map[string]any, grantAnswers, storedAnswers []answer) ([]string, []string, error) {
	for _, dir := range []string{filepath.Dir(opts.GrantPath), filepath.Dir(opts.ValuesPath)} {
		if err := d.Host.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	var grantChanged, valuesChanged []string
	for _, a := range grantAnswers {
		if same(lookup(grantNow, a.key), a.value) {
			continue
		}
		if err := sb.SetGrant(a.key, a.value); err != nil {
			return grantChanged, valuesChanged, err
		}
		grantChanged = append(grantChanged, a.key)
	}
	for _, a := range storedAnswers {
		current, ok := lookupPath(base, a.key)
		if same(current, a.value) {
			continue
		}
		// An empty answer over a value that is already set is "keep it": a
		// secret question cannot offer its current value back, and init never
		// clears a setting by accident.
		if s, isString := a.value.(string); isString && strings.TrimSpace(s) == "" && ok {
			continue
		}
		if err := sb.SetValue(a.key, a.value); err != nil {
			return grantChanged, valuesChanged, err
		}
		valuesChanged = append(valuesChanged, a.key)
	}
	return grantChanged, valuesChanged, nil
}

func (d *Decision) reportWrites(opts Options, res Result) {
	r := d.report()
	if len(res.GrantChanged) == 0 {
		r.Line(fmt.Sprintf("%s: already says this", opts.GrantPath))
	} else {
		r.Line(fmt.Sprintf("%s: edited %d %s", opts.GrantPath, len(res.GrantChanged), plural(len(res.GrantChanged), "key", "keys")))
	}
	if len(res.ValuesChanged) == 0 {
		r.Line(fmt.Sprintf("%s: nothing to add", opts.ValuesPath))
	} else {
		r.Line(fmt.Sprintf("%s: edited %d %s", opts.ValuesPath, len(res.ValuesChanged), plural(len(res.ValuesChanged), "key", "keys")))
	}
}

// pathTarget is one of the two host paths, with what init found and whether the
// operator let it create the directory.
type pathTarget struct {
	kind   string
	flag   string
	path   string
	report PathReport
	create bool
}

// pathCheckFooter says what init then does without asking: the directories
// under the two paths that belong to the apps. It lives here, not in a UI,
// because both prompters must say the same thing.
const pathCheckFooter = "  downloads/<category> and media/<category> are made on the first start, owned as above"

// reportPaths shows what is at both paths and asks about each missing one, in
// data-then-config order: they are two volumes, and accepting one is no answer
// about the other. The answer is remembered, not acted on: the directories are
// created after the last question, so a cancel leaves the host as it was.
func (d *Decision) reportPaths(paths []pathTarget, uid, gid int, unattended bool) error {
	check := PathCheck{Footer: pathCheckFooter}
	for i := range paths {
		rep, err := d.inspect(paths[i].kind, paths[i].path, uid, gid)
		if err != nil {
			return err
		}
		paths[i].report = rep
		check.Reports = append(check.Reports, rep)
	}
	// A terminal UI draws the block; the line prompt gets the plain text, which
	// is exactly what it printed before this seam existed.
	var err error
	if pc, ok := d.Prompt.(PathChecker); ok {
		err = pc.ShowPathCheck(check)
	} else {
		err = d.Prompt.Note(check.Text())
	}
	if err != nil {
		return err
	}
	for i := range paths {
		if paths[i].report.Exists {
			continue
		}
		if unattended {
			return fmt.Errorf("%s %s does not exist, and --yes does not create it: create the directory (or mount its disk) and run init again", paths[i].flag, paths[i].path)
		}
		ok, err := d.Prompt.Confirm(Confirm{
			Prompt: "create " + paths[i].path + " on that filesystem?",
			// The safe answer stays the focused one: init never creates a
			// directory because somebody pressed enter.
			Default:     false,
			Affirmative: "create it",
			Negative:    "stop here",
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("not creating %s: nothing was written", paths[i].path)
		}
		paths[i].create = true
	}
	return nil
}

// checkPathPair refuses a config path inside the data path (or the reverse).
// Two volumes that overlap are a nested mount neither the chart nor a hardlink
// survives, and it is always a typo.
func checkPathPair(data, config string) error {
	data = filepath.Clean(data)
	config = filepath.Clean(config)
	if data == config {
		return fmt.Errorf("--data-path and --config-path are both %s: a box keeps its downloads and its settings apart", data)
	}
	if under(data, config) {
		return fmt.Errorf("--data-path %s is inside --config-path %s: the two volumes must not overlap", data, config)
	}
	if under(config, data) {
		return fmt.Errorf("--config-path %s is inside --data-path %s: the two volumes must not overlap", config, data)
	}
	return nil
}

// loadSeedbox reads the two files, or starts from empty ones. A missing grant is
// the normal case on a host that has never run init; a grant that is there but
// does not parse is not.
func loadSeedbox(grantPath, valuesPath string) (*seedbox.Seedbox, error) {
	if grantPath == "" {
		return nil, errors.New("no grant file: pass --grant")
	}
	if valuesPath == "" {
		return nil, errors.New("no values file: pass --values")
	}
	if _, err := os.Stat(grantPath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", grantPath, err)
		}
		// The grant is root's and does not exist yet; the values file is the
		// user's and may already say something (tuning, tls.issuer). Read it
		// rather than starting from empty, so init's questions and its one
		// forced answer see what is there.
		values, err := seedbox.LoadValues(valuesPath)
		if err != nil {
			return nil, err
		}
		sb := seedbox.New(grantPath, valuesPath)
		sb.Values = values
		return sb, nil
	}
	return seedbox.Load(grantPath, valuesPath)
}

// checkTransientFlags refuses an answer nothing asked for: a typo in
// --set-transient that was silently ignored would be a claim token the operator
// believes they supplied.
func checkTransientFlags(given map[string]string, keys []string) error {
	known := make(map[string]bool, len(keys))
	for _, k := range keys {
		known[k] = true
	}
	bad := make([]string, 0, len(given))
	for k := range given {
		if !known[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	if len(keys) == 0 {
		return fmt.Errorf("--set-transient %s: no app this box has asks a question that lives in no file", bad[0])
	}
	return fmt.Errorf("--set-transient %s: no transient question in this box's catalog names it (%s)",
		bad[0], strings.Join(keys, ", "))
}

// prefill reads a key from the merged values, falling back to the grant for the
// keys that are box's own record and never reach the chart (the namespace, the
// host paths).
func prefill(base, grantNow map[string]any, key string) any {
	if v, ok := lookupPath(base, key); ok {
		return v
	}
	v, _ := lookupPath(grantNow, key)
	return v
}

// lookupPath reads a dotted key from a decoded values document.
func lookupPath(m map[string]any, key string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(key, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// lookup reads a dotted key, treating a miss as nil.
func lookup(m map[string]any, key string) any {
	v, _ := lookupPath(m, key)
	return v
}

// intAt reads an integer key, which YAML decodes as int.
func intAt(m map[string]any, key string) (int, bool) {
	switch v := lookup(m, key).(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case uint64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// same compares an answer with what a file already says, treating the integer
// widths YAML and JSON produce as one number.
func same(current, want any) bool {
	if current == nil && want == nil {
		return true
	}
	if a, ok := asInt(current); ok {
		if b, ok := asInt(want); ok {
			return a == b
		}
	}
	switch c := current.(type) {
	case string:
		w, ok := want.(string)
		return ok && c == w
	case bool:
		w, ok := want.(bool)
		return ok && c == w
	}
	return false
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint64:
		return int64(x), true
	case float64:
		return int64(x), x == float64(int64(x))
	}
	return 0, false
}

// text renders a prefilled value as the default the prompt shows.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// nonEmpty is the validator for a required text answer.
func nonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("a value is needed")
	}
	return nil
}

// validDomain is deliberately about shape, not reachability: init cannot know
// whether DNS points here, and doctor is the command that asks.
func validDomain(v string) error {
	if err := nonEmpty(v); err != nil {
		return err
	}
	if strings.Contains(v, "://") || strings.ContainsAny(v, " /\\") {
		return errors.New("write the name only, e.g. box.example.com")
	}
	if strings.HasPrefix(v, ".") || strings.HasSuffix(v, ".") || strings.Contains(v, "..") {
		return errors.New("that is not a host name")
	}
	if strings.Contains(v, ":") {
		return errors.New("drop the port: the box serves HTTP itself")
	}
	return nil
}

// validHostPath insists on the shape of a path the box will turn into a volume:
// absolute, clean, and not the root of the filesystem.
func validHostPath(v string) error {
	if err := nonEmpty(v); err != nil {
		return err
	}
	if !filepath.IsAbs(v) {
		return errors.New("give an absolute path, e.g. /srv/s5n/data")
	}
	if filepath.Clean(v) != v {
		return fmt.Errorf("write it without a trailing slash or a %q, as %s", "..", filepath.Clean(v))
	}
	if v == string(filepath.Separator) {
		return errors.New("that is the root of the filesystem; give the directory this box owns")
	}
	return nil
}

// validNamespace is the shape a Kubernetes namespace name has to have, said
// without Kubernetes words: a DNS-1123 label, which is also what a node port's
// Kubernetes rules and a Service's DNS name need it to be.
func validNamespace(v string) error {
	if v == "" {
		return errors.New("a seedbox name cannot be empty")
	}
	if len(v) > 63 {
		return fmt.Errorf("a seedbox name is at most 63 characters, and this one is %d", len(v))
	}
	for i, r := range v {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		dash := r == '-'
		if !lower && !digit && !dash {
			return errors.New("a seedbox name is lowercase letters, digits and dashes")
		}
		if dash && (i == 0 || i == len(v)-1) {
			return errors.New("a seedbox name cannot start or end with a dash")
		}
	}
	return nil
}
