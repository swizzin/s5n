// Package setup is `box init`: it asks the questions a seedbox has, splices the
// answers into the grant and the values file one key at a time, and hands over to
// `box apply`, whose diff and confirm are the review (docs/box.md).
//
// Re-running init edits: every question is prefilled from the merged values, only
// a changed answer is spliced, and nothing is written until every question has an
// answer, so a cancel leaves both files as they were. It is a pure decision over
// small interfaces (Prompter, PathHost, seedbox, catalog) tested from fixtures;
// LinuxPathHost is the only implementation that touches a machine.
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
	// Apply is the hand-over, a function so init does not link Helm.
	Apply ApplyFunc
	// Login installs root's login after apply's confirm and before its first write:
	// a refusal never reaches it, and a failure stops before the release exists.
	Login LoginFunc
	// UserLookup names the OS account behind a uid -- the login's default, rather
	// than whoever ran sudo. init never creates an account.
	UserLookup UserLookupFunc
	// HasLogin tells a first run (empty password: box makes one) from a re-run (it
	// keeps the one there).
	HasLogin HasLoginFunc
	// GatewayIP is the Gateway's proxy ClusterIP, how an OIDC app reaches the issuer
	// at the box's name. Root's run writes it into the grant.
	GatewayIP func(context.Context) (string, error)
	// PodCIDRs is the cluster's pod network: an app trusting the forwarded address
	// sees the proxy's pod, which changes on every restart.
	PodCIDRs func(context.Context) ([]string, error)
	// Schema stops init writing a grant key an older release would refuse.
	Schema []byte
	// Say is every line init writes; nil writes nothing.
	Say say.Reporter
	// Resolver decides Let's Encrypt against self-signed; nil answers "nothing", and
	// the safe issuer is chosen rather than a lookup invented.
	Resolver Resolver
	// NodePorts is the cluster-wide nodePort LIST only root may read, how the torrent
	// port is picked free; nil refuses rather than guessing.
	NodePorts func(ctx context.Context) ([]NodePort, error)
	// Rand answers a number in [0, n) for the torrent port pick; nil is crypto/rand.
	Rand func(n int) int

	// steps is how many steps this run shows: advice for a UI, never an input.
	steps int
}

// step tells a prompter with progress where the next answer belongs.
func (d *Decision) step(n int, question string) {
	if st, ok := d.Prompt.(Stepper); ok {
		st.Step(n, d.steps, question)
	}
}

// Options are one `box init`: a field per grant question, which is what makes an
// unattended run possible, under the flags' names.
type Options struct {
	// Chart is the release box installs: the prefill defaults and the answer schema.
	// The caller loads it; init never reads charts/ from its own tree.
	Chart      seedbox.Chart
	GrantPath  string
	ValuesPath string

	// Namespace is the release namespace, not a question: a re-run keeps the grant's.
	Namespace  string
	Domain     string
	DataPath   string
	ConfigPath string
	// Apps is the app list by catalog name; instances are root's, via `box install`.
	Apps []string
	// SwitchesOn are the apps a migration turns on after this install, so a box-wide
	// decision (the torrent port) is still made.
	SwitchesOn []string
	// TorrentPort is the source's peer port: kept when this cluster can use it, since
	// trackers and forwards already point at it.
	TorrentPort int
	// PUID and PGID own the data path's files; PUIDSet and PGIDSet win over the grant.
	PUID, PGID int
	PUIDSet    bool
	PGIDSet    bool
	// SetTransient is the unattended form of a question that lives in no file.
	SetTransient map[string]string

	// Login is the admitted account (--login); LoginSet wins over a re-run's grant.
	Login    string
	LoginSet bool
	// DefaultLogin is a fresh box's offer: the invoking user's account name.
	DefaultLogin string
	// PasswordFile is --password-file ("-" for stdin); empty asks or generates one.
	PasswordFile string

	// Unattended asks nothing: a missing answer names its flag.
	Unattended bool
	// Adopting says another flow runs init as one step of its own, so the empty app
	// list is deliberate (`box migrate apply` installs with every app off and
	// switches on what it moved), and an answer that flow holds is a decision, not a
	// default: it is used as given, and only a missing one is asked, so a guided
	// migration asks only for what it lacks. The empty-box line and the notice
	// address belong elsewhere, so neither appears.
	Adopting bool
	// PathsInNode: the paths are inside a test node, so init neither verifies nor
	// creates them.
	PathsInNode bool
	// NoticePath is root's config: with an empty address there, init asks for the one
	// answer that belongs to the host. Empty means this run does not ask.
	NoticePath string
	// BeforeWrite asks about the platform and the seedbox together: it runs after
	// init's last question and before its first write, with every answer settled, so
	// the plan it shows has all of them, and a refusal leaves both files and both
	// directories as they were. `box bootstrap` sets it on a fresh host.
	BeforeWrite func(ctx context.Context, a Answers) error
}

// Answers is what init's questions settled, carrying the partial Result so a run
// stopping in the hook still reports what it decided.
type Answers struct {
	Result Result
	// DataPath and ConfigPath are the host paths the box keeps its tree on.
	DataPath, ConfigPath string
	// PUID and PGID are the ids every file on those paths is owned by.
	PUID, PGID int
	// Apps are the catalog apps this box runs, sorted.
	Apps []string
	// Login is the account the login admits.
	Login string
	// Namespace is the seedbox's namespace, which the login or the flag named.
	Namespace string
	// Port is the torrent port the grant records, or nil when no app has one.
	Port any
	// Hosts are the certificate names this run decided, the box's own first.
	Hosts []Host
	// NoticeURL is where box posts notices, when this run was asked.
	NoticeURL string
}

// ApplyOptions is `box apply`'s arguments plus the hook for answers that outlive no
// file.
type ApplyOptions struct {
	GrantPath   string
	ValuesPath  string
	PathsInNode bool
	// AfterConfirm asks the transient questions, after apply's own confirm.
	AfterConfirm func(ctx context.Context) (map[string]any, error)
	// TransientKeys are the paths AfterConfirm answers, kept out of the diff.
	TransientKeys []string
	// Login installs root's login after the same confirm; separate from AfterConfirm
	// because a failure is a refusal, not an answer to merge.
	Login func(ctx context.Context) error
	// Issuer is for root's host-CA step, which runs before init's per-host step: on a
	// first init there is nothing to read it from.
	Issuer string
	// HostIssuers lets a name moved to Let's Encrypt be written even though
	// tls.issuer is empty.
	HostIssuers map[string]string
}

// ApplyFunc is one `box apply`.
type ApplyFunc func(ctx context.Context, opts ApplyOptions) error

// Result is what init did, for the caller to report and a test to assert.
type Result struct {
	ReRun bool
	// Domain is the host name the box answered with, for the closing card.
	Domain string
	// Issuer is from tls.issuer when set, else from what the domain resolved to.
	Issuer string
	// Hosts is every decided name, the domain first: tls.issuer forces them all, else
	// each name's own answer.
	Hosts []Host
	// GrantChanged and ValuesChanged are the keys spliced; identical answers leave both
	// empty.
	GrantChanged  []string
	ValuesChanged []string
	// Created are the directories init made, owned by the box's uid:gid.
	Created []string
	// NewPassword says this run supplied one; GeneratedPassword only when box made it,
	// printed once and nowhere on disk.
	Login             string
	NewPassword       bool
	GeneratedPassword string
	// Applied says the hand-over ran, not that the release changed.
	Applied bool
}

// LoginFunc installs root's login: internal/login's Apply, over what init decided.
type LoginFunc func(ctx context.Context, l Login) error

// UserLookupFunc names one OS account by its uid; false means there is none.
type UserLookupFunc func(uid int) (string, bool)

// HasLoginFunc answers whether the users database has a row; internal/login reads it.
type HasLoginFunc func(login string) bool

// Login is one login install. Password is this run's plaintext only: nothing here
// reaches a file.
type Login struct {
	Host        string
	Name        string
	Password    string
	NewPassword bool
	// Hosted are the apps served at a name of their own, each needing the rule of the
	// posture it had on its path.
	Hosted []string
	// OwnLogin are the apps that sign users in themselves; their host or path bypasses
	// the login.
	OwnLogin []string
	// Values is the document the release will render: its instance list gives the
	// access_control rules, so login and release cannot disagree about prefixes.
	Values map[string]any
	// Namespace is the seedbox's namespace, half of every OIDC client id.
	Namespace string
	// OIDC are the apps signing in through the login: the first makes box a provider.
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
	// Root's config is read strictly first: a file init cannot parse stops the run
	// before its first question.
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

	// The baseline every question is prefilled from: today's files or the chart's
	// defaults.
	base, err := d.baseline(sb, opts.Chart)
	if err != nil {
		return res, err
	}
	grantNow := sb.Grant.Map()

	// The questions in docs/box.md's order: four steps, a fifth when an app has its
	// own questions.
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
	// The issuer follows the name, not the node, and tls.issuer overrides both. It is
	// read from the values document first: a first run's base is the chart's defaults.
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
	res.Hosts = []Host{{Host: domain, Issuer: issuer, Why: why, NoDNS: noDNS, Record: record, Detail: domAnswer.detail(domain)}}
	// Every host's certificate decision is one row, printed once the apps are known.
	hostRows := []say.Row{tlsRow(Host{Host: domain, Issuer: issuer, Why: why})}
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

	// The owner is settled after the data path is known: files on it name their own.
	puid, pgid, err := d.owners(opts, grantNow, res.ReRun, dataPath)
	if err != nil {
		return res, d.stop(err)
	}
	if !opts.Unattended {
		d.report().Fact("owner", filesLine(d.UserLookup, puid, pgid))
	}

	// What is at those paths and whether a missing one may be created; the report is
	// read-only.
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
		// Said once, not asked again: an empty box is a state; a migration says
		// nothing.
		d.report().Muted("nothing runs yet: this box has no apps. add one with `box install <app>`, or run `box migrate` for an install that already exists")
	}
	// The app list decides whether there is a fifth step: correct the counter.
	d.steps = 4
	for _, name := range apps {
		if app, ok := d.Catalog.App(name); ok && len(app.Stored()) > 0 {
			d.steps = 5
			break
		}
	}
	// Root's notice address is the last step, asked only when bootstrap left it
	// empty.
	askNotice := false
	pending, err := d.noticePending(opts)
	if err != nil {
		return res, err
	}
	askNotice = pending && !opts.Unattended && !opts.Adopting
	if askNotice {
		d.steps++
	}

	// The document the release will render: the app list decides which entries are on,
	// and its instance list is then the workloads, each with the name it is served at.
	values := withApps(base, apps, d.Catalog)
	// The names the chosen workloads are served at: an app's host must answer where the
	// domain does, or its certificate can never issue. Both resolving to nothing is the
	// test-box case; a real mismatch stops naming the record (docs/box.md).
	appHosts, err := appHostsFor(ctx, d.Resolver, d.Catalog, domain, domAnswer, text(forced), "init", seedbox.Instances(values))
	if err != nil {
		return res, err
	}
	for _, h := range appHosts {
		res.Hosts = append(res.Hosts, h)
		hostRows = append(hostRows, tlsRow(h))
	}
	d.report().Rows(say.Fold(certificatesRow, "host", hostRows))
	for _, h := range res.Hosts {
		if h.Detail != "" {
			d.report().Detail(h.Detail)
		}
	}

	// The login: one account, asked once, because every path in front of the Gateway
	// admits only it.
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

	// The namespace follows the login (s5n-<login>); a re-run keeps the grant's, and
	// --namespace still wins on a first run.
	namespace, err := d.namespace(opts, grantNow, res.ReRun, loginName)
	if err != nil {
		return res, err
	}
	// The peer port is on the node: read before any answer is written, so no free port
	// is a refusal, not a half-finished dialog.
	port, err := d.torrentPort(ctx, namespace, apps, grantNow, opts)
	if err != nil {
		return res, err
	}

	// Each selected app's own questions; a transient one waits for apply's confirm.
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

	// Where box tells someone not watching what it did, written with the answers
	// below.
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

	// Every question asked, nothing written: a flow showing platform and seedbox in
	// one plan asks its question here.
	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(ctx, Answers{
			Result:     res,
			DataPath:   dataPath,
			ConfigPath: configPath,
			PUID:       puid,
			PGID:       pgid,
			Apps:       apps,
			Login:      loginName,
			Namespace:  namespace,
			Port:       port,
			Hosts:      res.Hosts,
			NoticeURL:  noticeURL,
		}); err != nil {
			return res, err
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
	// The peer port last, so the grant reads app by app.
	if port != nil {
		grantAnswers = append(grantAnswers, answer{key: torrentPortKey, value: port})
	}
	// The Gateway's proxy address, for an app that signs in through the login. The
	// question is whether this chart serves an app that way, not whether one this
	// run switched on does: a migrate install answers with every app off and
	// switches the plan's on afterwards, so "an OIDC app is on now" was false on a
	// run with two to install and their client Secrets went unused
	// (cmd/box/migrate_apply.go). An address that cannot be read yet is a warning:
	// the next root run fills it in.
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
				// Only an app that is on now waits for it; a warning about an
				// app nobody switched on is noise.
				if len(d.Catalog.OIDCApps(apps)) > 0 {
					d.report().Warn("the Gateway's proxy Service is not there yet, so the OIDC apps have no address for box's own name: run `box init` again once `box bootstrap` has installed the Gateway")
				}
			} else {
				grantAnswers = append(grantAnswers, answer{key: "auth.oidc.gatewayIP", value: ip})
			}
		}
	}
	// The cluster's pod network for an app trusting the forwarded address. The chart's
	// key decides whether this release carries it: an older chart would refuse it.
	// Empty is not fatal -- every request just looks local.
	if d.PodCIDRs != nil {
		declared, err := seedbox.Declares(d.Schema, "apps", "jellyfin", "knownProxies")
		if err != nil {
			return res, err
		}
		if declared && contains(apps, "jellyfin") {
			cidrs, err := d.PodCIDRs(ctx)
			if err != nil {
				return res, err
			}
			if len(cidrs) == 0 {
				d.report().Warn("this cluster's Nodes name no pod network yet, so Jellyfin would treat every request through the Gateway as local: run `box init` again once a Node has its pod CIDR")
			} else {
				list := make([]any, len(cidrs))
				for i, c := range cidrs {
					list[i] = c
				}
				grantAnswers = append(grantAnswers, answer{key: "apps.jellyfin.knownProxies", value: list})
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
		// The login's rules come from the rendered document: a 4K instance that is on
		// has them.
		l := Login{
			Host:        domain,
			Name:        loginName,
			Password:    password,
			NewPassword: newPassword,
			Values:      values,
			Hosted:      appHostNames(appHosts),
			OwnLogin:    d.Catalog.SelfAuthenticating(),
			Namespace:   namespace,
			OIDC:        d.Catalog.OIDCApps(apps),
		}
		req.Login = func(ctx context.Context) error { return d.Login(ctx, l) }
	}
	// The answers that outlive no file: the catalog's transient questions, and
	// nothing else.
	if len(transient) > 0 {
		asked := d.transientAsker(transient, opts)
		req.AfterConfirm = func(ctx context.Context) (map[string]any, error) {
			return asked(ctx)
		}
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
		// init edits what is there: a file that no longer checks out must be fixed
		// first, or the splice hides the mistake.
		return nil, fmt.Errorf("this seedbox's files do not check out, and init edits them rather than replacing them: %w", err)
	}
	return values, nil
}

// textQuestion is one answer: a flag when unattended, else a prompt, the file's
// current value as the default.
type textQuestion struct {
	key        string
	flag       string
	prompt     string
	help       string
	value      string
	prefill    any
	unattended bool
	// decided says the running flow already answered this: it is used as given, and an
	// empty one is a refusal naming the flag.
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
		// A flag in guided mode is the starting answer, which the operator can change.
		def = given
	}
	return d.Prompt.Input(Input{
		Key: q.key, Prompt: q.prompt, Help: q.help, Default: def, Validate: q.validate,
	})
}

// namespace is the one grant key that is not a question: a seedbox's identity, which
// a re-run never changes. A first run takes the flag or derives s5n-<login>.
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

// id is a puid or pgid being decided: the value and whether something answered.
type id struct {
	value int
	set   bool
}

// take records the answer for this id.
func (i *id) take(v int) { i.value, i.set = v, true }

// owners resolves puid and pgid in docs/box.md's order: the flag, the grant, the data
// path's owner, then the invoking user. The files come before sudo's ids, since
// re-chowning a library would leave the apps unable to write it; no symlink is
// followed.
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

// usableIDs: root is never the answer, since the apps run unprivileged.
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

// dataOwner is the account owning the data path when it holds something; a missing or
// empty path, a file and a symlink are all "no data".
func (d *Decision) dataOwner(path string) (int, int, bool, error) {
	info, err := d.Host.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, 0, false, nil
	case err != nil:
		return 0, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	if info.Symlink {
		d.report().Warn(fmt.Sprintf("%s is a symlink: its owner is not the owner of what it points at, so the files here keep the ids you gave", path))
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

// askID asks for one id the host gave no usable answer for.
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

// filesLine is the record line for the owner, "alice (uid 1000, gid 1000)".
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

// appsOn is the preselected app list; `--apps` stays a starting answer the picker
// shows.
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

// appsNone is the explicit empty list: `--apps ""` is not it, nor is the cursor.
const appsNone = "none"

// AskApps resolves the app list: the flag, else a multi-select. A migration hands over
// `--apps none` and never sees the picker. Nothing is a legitimate answer, so the
// picker's minimum is zero.
func (d *Decision) askApps(opts Options, grantNow map[string]any, reRun bool) ([]string, error) {
	names := d.Catalog.Names()
	options := make([]Option, 0, len(names))
	for _, name := range names {
		app, _ := d.Catalog.App(name)
		options = append(options, Option{Value: name, Label: name, Help: app.Description})
	}

	current := d.appsOn(grantNow, reRun)

	// "none" is the empty list spelled out, so it cannot sit beside an app: one of
	// the two is not what the operator meant.
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

// splitApps flattens --apps on commas; empty pieces disappear, so `--apps ""` is
// the same as not passing it.
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

// checkApps validates the list and keeps the catalog's order, for a stable grant.
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

// askQuestion asks one stored question, prefilled from the merged values.
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
		// A stored secret is never offered back as a default: enter keeps it
		// (writeFiles skips an empty answer over a current value).
		return d.Prompt.Secret(Secret{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Optional: true})
	}
	var validate func(string) error
	if !q.Optional {
		validate = nonEmpty
	}
	return d.Prompt.Input(Input{Key: q.Key, Prompt: q.Prompt, Help: q.Help, Default: def, Validate: validate})
}

// transientAsker builds the hook apply calls after its confirm: --set-transient
// answers when unattended, else it asks.
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

// writeFiles splices each changed answer: only a difference is written, so hand edits
// elsewhere survive byte for byte.
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
		// An empty answer over a set value is "keep it": init never clears a setting
		// by accident.
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

// reportWrites is the two files as rows; the paths are the log's (rule 4).
func (d *Decision) reportWrites(opts Options, res Result) {
	r := d.report()
	r.Rows([]say.Row{
		writeRow("grant", res.GrantChanged),
		writeRow("values", res.ValuesChanged),
	})
	r.Detail(opts.GrantPath + ": " + writeLine(res.GrantChanged))
	r.Detail(opts.ValuesPath + ": " + writeLine(res.ValuesChanged))
}

// writeRow is one file: ✓ with its count of keys, or a note that it already said so.
func writeRow(name string, changed []string) say.Row {
	if len(changed) == 0 {
		return say.Row{Mark: say.MarkNote, Name: name, Facts: []string{"already says this"}}
	}
	return say.Row{Mark: say.MarkDone, Name: name, Facts: []string{fmt.Sprintf("%d %s", len(changed), plural(len(changed), "key", "keys"))}}
}

func writeLine(changed []string) string {
	if len(changed) == 0 {
		return "already says this"
	}
	return fmt.Sprintf("edited %d %s", len(changed), plural(len(changed), "key", "keys"))
}

// pathTarget is one host path, what init found and whether it may create it.
type pathTarget struct {
	kind   string
	flag   string
	path   string
	report PathReport
	create bool
}

// pathCheckFooter is shared, not in a UI, because both prompters say the same thing.
const pathCheckFooter = "  downloads/<category> and media/<category> are made on the first start, owned as above"

// reportPaths shows both paths and asks about each missing one in data-then-config
// order; the answer is acted on after the last question.
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
	// A terminal UI draws the block; the line prompt gets the plain text.
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
			// Yes is the default: init cannot finish on a missing path.
			Default:     true,
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

// checkPathPair refuses one path inside the other: overlapping volumes are a
// nested mount neither the chart nor a hardlink survives.
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

// loadSeedbox reads the two files or starts empty: a missing grant is normal, a
// malformed one is not.
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
		// The values file may already say something (tuning, tls.issuer).
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

// checkTransientFlags refuses an answer nothing asked for: a typo would be a claim
// token the operator believes they supplied.
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

// prefill reads a key from the merged values, else the grant (namespace, paths).
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

// same compares an answer with a file, treating YAML's and JSON's integers as one.
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

// validDomain is shape only: doctor asks whether DNS points here.
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
		return errors.New("drop the port: box serves HTTP itself")
	}
	return nil
}

// validHostPath insists on a volume path's shape: absolute, clean, not the root.
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

// validNamespace is a DNS-1123 label, said without Kubernetes words.
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
