// Package login is root's login in the cluster: Authelia's state, its
// configuration, and the SecurityPolicy that puts it in front of the Gateway.
//
// It is the write half of internal/authelia, and it exists as its own package
// so the cluster work is separable from the decisions: what the configuration
// says is decided and tested there, and what has to be written, in what order,
// and rolled back when a step fails is here. Everything it writes is root's:
// the login serves every seedbox on the host, so a user's kubeconfig must not
// be able to reach it, and the grant's admission policy sees to that.
//
// The order is the contract. A users database box cannot read is refused before
// any object exists; every namespace and permission is checked before the first
// write; the claim and the file come before the Deployment (Authelia will not
// start without the file, and it can only read it from the claim); and the
// volume this run created is removed again if the run cannot finish.
package login

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
	"github.com/swizzin/s5n/internal/wait"
)

// The namespaces and constants this package writes with.
const (
	// Namespace is s5n-system: the platform's namespace, so the login is
	// root's and lives beside the admission policy that protects it.
	Namespace = authelia.SystemNamespace
	// PolicyGroup and PolicyVersion are Envoy Gateway's SecurityPolicy, which
	// is what enforces the login. The version is what EG 1.9.3 serves.
	PolicyGroup   = "gateway.envoyproxy.io"
	PolicyVersion = "v1alpha1"
	// GatewayGroup and GatewayVersion are the Gateway the policy targets.
	GatewayGroup   = "gateway.networking.k8s.io"
	GatewayVersion = "v1"
	// CoreGroup is the API group of a Service, a Secret and a ConfigMap.
	CoreGroup = ""
)

// Defaults for the two waits. The Deployment wait is bounded like every other
// workload wait box does; the policy's is shorter because Envoy Gateway
// translates a policy in seconds, and a longer wait only delays the sentence
// that says it refused it.
const (
	DeployTimeout = 3 * time.Minute
	PolicyTimeout = 60 * time.Second
	Poll          = 2 * time.Second
)

// ErrNoRow says the users database has no row for the login and this run was
// given no password: creating one would need a password nobody supplied, and
// reusing another row's would be a login nobody knows.
var ErrNoRow = errors.New("no password")

// Cluster is the cluster the login needs. It is the platform client plus three
// reads and one create the platform flow never needed, so a fake can answer
// them without a cluster.
type Cluster interface {
	Allowed(ctx context.Context, a kube.Access) (bool, error)
	Serves(ctx context.Context, group, version, kind string) (bool, error)
	ApplyWith(ctx context.Context, manager string, docs []map[string]any) error
	Create(ctx context.Context, doc map[string]any) error
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
	NamespaceExists(ctx context.Context, name string) (bool, error)
	Await(ctx context.Context, w platform.Wait) error
	Accepted(ctx context.Context, group, version, kind, namespace, name string, target platform.PolicyTarget) (bool, string, error)
}

// Host is the one host directory the login needs: /var/lib/s5n/authelia. It is
// an interface so a test can prove the ordering -- nothing is written before
// the confirm, and the file exists before the Deployment does -- without a
// root-owned directory.
type Host interface {
	// EnsureDir creates path, owned by uid:gid and mode 0700, and reports
	// whether this call created it.
	EnsureDir(path string, mode fs.FileMode, uid, gid int) (bool, error)
	// ReadFile reads the users database, or reports fs.ErrNotExist.
	ReadFile(path string) ([]byte, error)
	// WriteFile replaces path atomically.
	WriteFile(path string, data []byte, mode fs.FileMode) error
}

// LinuxHost is the real host: os, and internal/atomicfile for the file.
type LinuxHost struct{}

func (LinuxHost) EnsureDir(path string, mode fs.FileMode, uid, gid int) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return false, fmt.Errorf("%s exists and is not a directory", path)
		}
		// An existing directory is chowned every run: the login's uid is
		// box's decision, and a directory left over from a different one is
		// a login that cannot read its own database.
		if err := os.Chown(path, uid, gid); err != nil {
			return false, fmt.Errorf("chown %s: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return false, fmt.Errorf("chmod %s: %w", path, err)
		}
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return false, fmt.Errorf("create %s: %w", path, err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return false, fmt.Errorf("chown %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return false, fmt.Errorf("chmod %s: %w", path, err)
	}
	return true, nil
}

func (LinuxHost) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (LinuxHost) WriteFile(path string, data []byte, mode fs.FileMode) error {
	if err := atomicfile.Write(path, data, mode); err != nil {
		return err
	}
	// The file's owner is the login's uid, not root: the container runs as
	// that uid and would otherwise be unable to read a 0600 file. The rename
	// above is what makes this the file's final inode, so the chown lands on
	// the file Authelia opens.
	if err := os.Chown(path, authelia.UID, authelia.GID); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

// Options is one login install.
type Options struct {
	// Host is the seedbox's domain: the cookie domain, the portal's URL and
	// the only domain access_control rules name.
	Host string
	// Login is the one account, and Password its new password. NewPassword
	// false keeps whatever digest the file already has.
	Login       string
	Password    string
	NewPassword bool
	// Path is the host directory the claim backs. It is /var/lib/s5n/authelia
	// unless a caller says otherwise, which only a test does.
	Path string
	// PathsInNode says that directory is inside a test node rather than on
	// this host, so box cannot write the users file: the e2e writes it from
	// the same code (hack/authelia-users) and this flag is what keeps box from
	// claiming it did.
	PathsInNode bool
	// Image is the pinned Authelia image.
	Image string
	// Values is the seedbox's merged values: the chart's instance list is
	// what the access_control rules are derived from, and it is the same
	// document the release renders, so a 4K instance that exists has rules.
	Values map[string]any
	// Hosted are the apps the chart serves at <app>.<domain> instead of a
	// path. The edge asks Authelia about every listener, so each of those
	// names needs its rule, and the app's path rule must not exist: the chart
	// no longer serves the app there.
	Hosted []string
	// Namespace is the seedbox's namespace. It is the per-app client Secrets'
	// home and half of every client id, so an empty one is a refusal: a client
	// called "-qui" belongs to nobody.
	Namespace string
	// OIDC are the apps that sign in through this login, from the chart's
	// catalog. Each gets a client in this box's own namespace, its own Secret
	// and its own authorization policy; a box with none has no provider at
	// all.
	OIDC []string
}

// Decision is one login install. Open and Storage are called only once the
// options are valid and the files are read, so a refusal writes nothing.
type Decision struct {
	Open func() (Cluster, error)
	// Storage creates the claim. It is the release package's storage client
	// for s5n-system: a PV and a claim are the same two objects whatever
	// namespace they are for, and box already has one implementation of
	// "never mutate a volume in place".
	Storage func() (release.StorageClient, error)
	Host    Host
	Say     say.Reporter

	// DeployTimeout, PolicyTimeout and Poll override the defaults.
	DeployTimeout time.Duration
	PolicyTimeout time.Duration
	Poll          time.Duration
}

// Result is what happened, for the caller's report and for a test.
type Result struct {
	// Wrote is whether this run changed the cluster or the host at all.
	Wrote bool
	// UsersChanged is whether the users database was written.
	UsersChanged bool
	// SecretKept is true when s5n-authelia already existed and was left
	// alone: its key is never rotated.
	SecretKept bool
	// ConfigurationChanged is whether the rendered configuration differs from
	// the ConfigMap that was there.
	ConfigurationChanged bool
	// PolicyAccepted is whether Envoy Gateway accepted the SecurityPolicy
	// within the wait. A policy that is not accepted stops the run.
	PolicyAccepted bool
	// DirCreated and ClaimCreated say what this run made on the host and in
	// the cluster.
	DirCreated   bool
	ClaimCreated int
}

func (d *Decision) report() say.Reporter { return say.Or(d.Say, nil) }

func (d *Decision) poll() time.Duration {
	if d.Poll > 0 {
		return d.Poll
	}
	return Poll
}

// Apply installs the login.
func (d *Decision) Apply(ctx context.Context, opts Options) (Result, error) {
	var res Result
	if err := d.check(opts); err != nil {
		return res, err
	}
	if opts.Path == "" {
		opts.Path = authelia.HostPath
	}
	r := d.report()

	cl, err := d.Open()
	if err != nil {
		return res, err
	}
	store, err := d.Storage()
	if err != nil {
		return res, err
	}

	// Every read that can refuse the run happens before the first write.
	serves, err := cl.Serves(ctx, PolicyGroup, PolicyVersion, "SecurityPolicy")
	if err != nil {
		return res, fmt.Errorf("login: %w", err)
	}
	if !serves {
		return res, fmt.Errorf("this cluster does not serve %s SecurityPolicy: the login is Enforced by Envoy Gateway, so run `box bootstrap` first", PolicyGroup)
	}
	exists, err := cl.NamespaceExists(ctx, Namespace)
	if err != nil {
		return res, err
	}
	if !exists {
		return res, fmt.Errorf("namespace %s does not exist: it is the platform layer's, so run `box bootstrap` first", Namespace)
	}

	// The users database as it is on the host. A file box cannot read is a
	// refusal: rewriting it would discard the only copy of a digest, and
	// Authelia refuses to start on a row that does not parse.
	usersPath := filepath.Join(opts.Path, authelia.UsersDBFile)
	db := authelia.NewUsers()
	haveFile := false
	if !opts.PathsInNode {
		raw, err := d.host().ReadFile(usersPath)
		switch {
		case err == nil:
			haveFile = true
			db, err = authelia.ParseUsers(raw)
			if err != nil {
				return res, fmt.Errorf("%s: %w; fix or remove it, then run init again", usersPath, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return res, fmt.Errorf("read %s: %w", usersPath, err)
		}
	}
	row, haveRow := db.Get(opts.Login)
	switch {
	case opts.NewPassword:
		digest, err := authelia.HashPassword(opts.Password)
		if err != nil {
			return res, err
		}
		row = authelia.User{Login: opts.Login, DisplayName: opts.Login, Password: digest, Groups: row.Groups}
		db.Put(row)
	case !haveRow:
		return res, fmt.Errorf("%w: %s is not in %s, and this run supplied none; pass --password-file, or let init generate one", ErrNoRow, opts.Login, authelia.UsersDBFile)
	}
	users := db.Render()
	writeUsers := opts.NewPassword || !haveFile

	// The permissions every object below needs, asked as this identity, before
	// the first write. This is root's own seedbox: a kubeconfig that cannot do
	// these is not the one bootstrap left behind.
	if err := kube.Preflight(ctx, cl, accessList()); err != nil {
		return res, fmt.Errorf("login: %w", err)
	}

	// The claim, then the file, then the rest: Authelia can only read the
	// database from the claim, and it will not start without it.
	step := r.Start(say.Op{Label: "installing root's login"})
	vols := []seedbox.Volume{{Kind: "login", Path: opts.Path, Claim: authelia.ClaimName}}
	plan, err := store.Plan(ctx, Namespace, vols)
	if err != nil {
		step.Failed(err)
		return res, fmt.Errorf("login storage: %w", err)
	}
	var created []release.StorageObject
	for _, obj := range plan {
		if err := store.Apply(ctx, obj); err != nil {
			step.Failed(err)
			return res, d.undo(ctx, store, created, fmt.Errorf("login storage: %w", err))
		}
		if obj.Kind == release.KindPersistentVolume {
			created = append(created, obj)
		}
		res.Wrote = true
	}
	res.ClaimCreated = len(plan)
	undo := func(cause error) error { return d.undo(ctx, store, created, cause) }

	if !opts.PathsInNode {
		made, err := d.host().EnsureDir(opts.Path, 0o700, authelia.UID, authelia.GID)
		if err != nil {
			return res, undo(fmt.Errorf("login: %w", err))
		}
		res.DirCreated = made
		if writeUsers {
			if err := d.host().WriteFile(usersPath, users, 0o600); err != nil {
				return res, undo(fmt.Errorf("login: %w", err))
			}
			res.UsersChanged = true
			res.Wrote = true
			r.Line(fmt.Sprintf("users database: wrote %s (0600, login %s)", usersPath, opts.Login))
		} else {
			r.Line(fmt.Sprintf("users database: %s already has %s, and this run supplied no new password", usersPath, opts.Login))
		}
	} else {
		r.Line(fmt.Sprintf("users database: %s is inside the test node, so box cannot write it (--paths-in-node)", usersPath))
	}

	// The Secret: its encryption key created once, never changed, and the
	// provider's signing material added to it the first time this box has an
	// app that signs in through the login. Reading it is also how the
	// configuration gets the key it must encrypt with.
	//
	// The clients are not made here. An install that writes this Secret runs
	// before apply has created the seedbox's namespace, and a client is a
	// Secret in that namespace; login.EnsureClients is the step that runs where
	// the namespace exists, and the configuration picks its clients up on the
	// next render.
	apps := oidcApps(opts.OIDC)
	sec, kept, err := d.ensureKey(ctx, cl, true, len(apps) > 0)
	if err != nil {
		return res, undo(err)
	}
	res.SecretKept = kept
	if !kept {
		res.Wrote = true
	}
	key := sec.key

	// One rendering for the whole face of the login: the configuration file
	// that decides the page's theme, and the mark and favicon that go in it.
	// The per-host step calls the same function, so init and apply cannot
	// disagree about either.
	config, assets, err := renderLogin(opts.Values, opts.Host, opts.Login, key, opts.Hosted,
		oidcClients(opts.Host, opts.Namespace, opts.Login, apps, sec.digests))
	if err != nil {
		return res, undo(err)
	}
	res.ConfigurationChanged, err = d.configurationChanged(ctx, cl, config, assets)
	if err != nil {
		return res, undo(err)
	}

	// The provider is mounted only once it has a client. Authelia refuses the
	// block with no clients ("identity_providers: oidc: option 'clients' must
	// have one or more clients configured"), and a first install has none yet:
	// the client Secrets are the anchor step's write, in the run that creates
	// the namespace. Until then the login serves the edge with the signing
	// material sitting unused in its Secret.
	manifests, err := authelia.RenderManifests(manifestsFor(opts.Host, opts.Image, authelia.ConfigHash(config), len(sec.digests) > 0))
	if err != nil {
		return res, undo(err)
	}
	docs, err := platform.ParseManifests(manifests)
	if err != nil {
		return res, undo(err)
	}
	docs = append([]map[string]any{configMapDoc(config, assets)}, docs...)
	// From the first write of the login's own objects on, a failure keeps
	// everything, the volumes included. Undoing only the volumes would leave a
	// Deployment waiting on a claim that no longer exists, and the operator
	// would read that instead of the real cause. Every object here is applied
	// idempotently, so the next run continues from what is left.
	res.Wrote = true
	if err := cl.ApplyWith(ctx, kube.FieldManager, docs); err != nil {
		step.Failed(err)
		return res, keepAll(fmt.Errorf("login: %w", err))
	}

	// Ready, then accepted: the login has to answer before the Gateway is told
	// to ask it, and a policy Envoy Gateway refuses must stop the run before
	// the release is written.
	if err := d.awaitDeployment(ctx, cl); err != nil {
		step.Failed(err)
		return res, keepAll(err)
	}
	if err := d.awaitPolicy(ctx, cl, &res); err != nil {
		step.Failed(err)
		return res, keepAll(err)
	}
	step.Done(fmt.Sprintf("login ready: %s/%s, SecurityPolicy %s/%s accepted", Namespace, authelia.ServiceName, platform.GatewayNamespace, authelia.PolicyName))
	return res, nil
}

// RulesOptions is one access-control write: the inputs init installs the login
// with, so that an apply writes the rule for a host it is about to serve
// without inventing a second rendering of the login's configuration.
type RulesOptions struct {
	// Host is the box's domain: the cookie's domain, and the name every app
	// host is a label under.
	Host string
	// Login is the one account the rules admit.
	Login string
	// Hosts are the names this run is writing, for the one line it prints. They
	// are not what the configuration is rendered from.
	Hosts []string
	// Image is the login Deployment's image, pinned by digest: the rule write
	// applies the login's objects from the same template init does, and the
	// image is a part of that template.
	Image string
	// Values is the seedbox's merged values: the chart's instance list is what
	// the rules are derived from, so a 4K instance that exists has its rule and
	// one that does not is never mentioned.
	Values map[string]any
	// Hosted are the apps the chart serves at <app>.<domain> -- the whole list,
	// not only the names this run writes, because the configuration is
	// rewritten whole and a name left out would lose the rule it already had.
	Hosted []string
	// Namespace and OIDC are the provider's half of the same configuration: the
	// seedbox's namespace (half of every client id) and the apps that sign in
	// through this login. The configuration is one file, so a rule write
	// renders the clients too and a client left out would be a client Authelia
	// stopped answering for.
	Namespace string
	OIDC      []string
	// Installs says this run installs the login after it has read the cluster:
	// `box init` does, in the step that runs once the confirm is answered. A
	// cluster without a login is then work Pending shows rather than the
	// refusal a later rule write makes, and the render it would compare is
	// simply not there yet.
	Installs bool
}

// EnsureRules writes the login's configuration for the hosts a run is about to
// serve, and reports whether it changed anything.
//
// The login keeps one configuration for the whole box and Authelia reads it
// once at startup, so this is not a per-host edit: the file is rendered from
// the same inputs init renders it from (internal/authelia renders both, so the
// two cannot spell a rule differently), compared with the ConfigMap that is
// there, and applied only when it differs -- together with the Deployment,
// whose pod-template hash is what makes the running login read it. A run with
// nothing to change writes nothing and returns false.
//
// It is root's, like the rest of the login: the configuration names the one
// account the box logs in with. A box whose login is not installed has nothing
// to write a rule into, and the refusal names the command that installs it.
func (d *Decision) EnsureRules(ctx context.Context, opts RulesOptions) (bool, error) {
	cl, files, changed, err := d.prepare(ctx, opts, true)
	if err != nil {
		return false, err
	}
	names := strings.Join(opts.Hosts, ", ")
	if !changed {
		d.report().Line(fmt.Sprintf("login: the rule for %s is already there", names))
		return false, nil
	}
	return d.write(ctx, cl, opts, files,
		"applying the login's rule for "+names,
		fmt.Sprintf("login: the rule for %s is in place", names))
}

// Pending reports whether the login's configuration or pictures differ from
// what is in the cluster, and writes nothing. It is EnsureRules' read half, for
// a caller that has to ask before it does the write: `box apply` shows a
// re-rendered login in its plan like any other piece of work.
//
// It is root's: reading the login's Secret and ConfigMap is the first thing it
// does, so an identity that cannot write the login is told so here rather than
// half way through a write.
func (d *Decision) Pending(ctx context.Context, opts RulesOptions) (bool, error) {
	_, _, changed, err := d.prepare(ctx, opts, false)
	return changed, err
}

// Reconcile writes the login's configuration and pictures when they differ, and
// reports whether it wrote. It is the login's own step, separate from the
// per-host one: a theme change is a new face for the page, not a new host, so
// it re-renders the whole face and touches no listener, no Certificate and no
// DNS record.
//
// What it writes is exactly what EnsureRules writes -- one ConfigMap and the
// objects that mount it -- so the two cannot disagree, and a write that changes
// the configuration rolls the pod through the same annotation.
func (d *Decision) Reconcile(ctx context.Context, opts RulesOptions) (bool, error) {
	cl, files, changed, err := d.prepare(ctx, opts, true)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	return d.write(ctx, cl, opts, files,
		"reconciling the login's page",
		"login: the configuration, mark and favicon are up to date")
}

// prepare is the read half of both writers: the login's rendering for these
// options, the cluster it was compared against, and whether it differs.
//
// ensure says this run is going to write, so the provider's material an app
// needs may be created here -- a box upgraded from before the provider has
// none, and a rule write is where it gains one. Pending passes false and only
// reads: a run that may not write the login must not create anything in it.
func (d *Decision) prepare(ctx context.Context, opts RulesOptions, ensure bool) (Cluster, loginFiles, bool, error) {
	var none loginFiles
	if err := checkRules(opts); err != nil {
		return nil, none, false, err
	}
	cl, err := d.Open()
	if err != nil {
		return nil, none, false, err
	}
	apps := oidcApps(opts.OIDC)
	var sec loginSecret
	if ensure {
		sec, _, err = d.ensureKey(ctx, cl, false, len(apps) > 0)
		if err != nil {
			return nil, none, false, err
		}
		// The clients, when this run is allowed to write them: the seedbox's
		// namespace exists by the time a rule write happens, and the digest
		// this writes is what the configuration below is rendered from.
		if _, err := d.EnsureClients(ctx, opts.Namespace, apps); err != nil {
			return nil, none, false, err
		}
		sec, err = d.readSecret(ctx, cl)
	} else {
		sec, err = d.readSecret(ctx, cl)
		if err != nil {
			// The run that installs the login installs it after the confirm
			// (`box init`), so reading a cluster without one is work to show,
			// not the refusal a rule write makes. Installs says this run is
			// that one.
			if opts.Installs && errors.Is(err, ErrNotInstalled) {
				return cl, none, true, nil
			}
			return nil, none, false, err
		}
	}
	if err != nil {
		return nil, none, false, err
	}
	key := sec.key
	config, assets, err := renderLogin(opts.Values, opts.Host, opts.Login, key, opts.Hosted,
		oidcClients(opts.Host, opts.Namespace, opts.Login, apps, sec.digests))
	if err != nil {
		return nil, none, false, err
	}
	changed, err := d.configurationChanged(ctx, cl, config, assets)
	if err != nil {
		return nil, none, false, err
	}
	return cl, loginFiles{config: config, assets: assets, provider: len(sec.digests) > 0}, changed, nil
}

// manifestsFor is the login's objects for one host, from the constants this
// package and the platform own. It is one function because init's install and
// every later rule write apply the same set: a second rendering would let an
// apply downgrade the Deployment (or drop the NetworkPolicy that keeps every
// pod but the Gateway off the login).
func manifestsFor(host, image, configHash string, oidc bool) authelia.Manifests {
	return authelia.Manifests{
		Namespace:        Namespace,
		GatewayNamespace: platform.GatewayNamespace,
		Gateway:          platform.GatewayName,
		// Envoy Gateway installs its proxy fleet in the controller's
		// namespace in standard mode, which is the only mode box installs and
		// the only implementation it supports.
		ProxyNamespace: platform.EnvoyGatewayNamespace,
		Host:           host,
		Claim:          authelia.ClaimName,
		ConfigMap:      authelia.ConfigMapName,
		Secret:         authelia.SecretName,
		Image:          image,
		OIDC:           oidc,
		OIDCFile:       authelia.OIDCFile,
		OIDCMount:      authelia.OIDCMountPath,
		UID:            authelia.UID,
		GID:            authelia.GID,
		ConfigHash:     configHash,
	}
}

// loginFiles is the login's rendering: the configuration Authelia starts with,
// and the mark and favicon mounted beside it.
type loginFiles struct {
	config []byte
	assets authelia.Assets
	// provider says the login's Secret holds at least one client, which is
	// what makes Authelia accept an identity_providers.oidc block at all: it
	// refuses one with no clients, and a first install has none until apply's
	// anchor step writes them.
	provider bool
}

// write applies a rendering that differs and waits for the pod that has to read
// it. The permissions are asked here, before the first write, so a kubeconfig
// that may write a Gateway's listeners but not the login's configuration is
// told so rather than half way through.
func (d *Decision) write(ctx context.Context, cl Cluster, opts RulesOptions, files loginFiles, label, done string) (bool, error) {
	if err := kube.Preflight(ctx, cl, rulesAccess()); err != nil {
		return false, fmt.Errorf("login: %w", err)
	}
	manifests, err := authelia.RenderManifests(manifestsFor(opts.Host, opts.Image, authelia.ConfigHash(files.config), files.provider))
	if err != nil {
		return false, err
	}
	docs, err := platform.ParseManifests(manifests)
	if err != nil {
		return false, err
	}
	// The ConfigMap is not part of the manifests template: its content is one
	// rendered file, and it is written beside the objects that name it.
	docs = append([]map[string]any{configMapDoc(files.config, files.assets)}, docs...)

	step := d.report().Start(say.Op{Label: label})
	if err := cl.ApplyWith(ctx, kube.FieldManager, docs); err != nil {
		step.Failed(err)
		return false, fmt.Errorf("login: %w", err)
	}
	// The pod that is running still has the old file: the hash is what rolls
	// it, and a rule or a page the running login has not read is not the login
	// that is answering.
	if err := d.awaitDeployment(ctx, cl); err != nil {
		step.Failed(err)
		return true, err
	}
	step.Done(done)
	return true, nil
}

// checkRules refuses a rule write that cannot be rendered, before a client
// exists.
func checkRules(opts RulesOptions) error {
	if strings.TrimSpace(opts.Host) == "" {
		return errors.New("login: no host name: the rules are per host")
	}
	if err := authelia.ValidateLogin(opts.Login); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if len(opts.Values) == 0 {
		return errors.New("login: no values: the rules come from the chart's instance list")
	}
	if !strings.Contains(opts.Image, "@sha256:") {
		return fmt.Errorf("login: image %q is not pinned by digest", opts.Image)
	}
	if len(opts.OIDC) > 0 && strings.TrimSpace(opts.Namespace) == "" {
		return errors.New("login: an OIDC client's id is <namespace>-<app>, so a box with one needs its namespace")
	}
	return nil
}

// check refuses what cannot be installed, before a client exists.
func (d *Decision) check(opts Options) error {
	switch {
	case d.Open == nil:
		return errors.New("login: no cluster client")
	case d.Storage == nil:
		return errors.New("login: no storage client")
	case d.Host == nil:
		return errors.New("login: no host")
	}
	if len(opts.OIDC) > 0 && strings.TrimSpace(opts.Namespace) == "" {
		return errors.New("login: an OIDC client's id is <namespace>-<app>, so a box with one needs its namespace")
	}
	if strings.TrimSpace(opts.Host) == "" {
		return errors.New("login: no host name")
	}
	if err := authelia.ValidateLogin(opts.Login); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if !strings.Contains(opts.Image, "@sha256:") {
		return fmt.Errorf("login: image %q is not pinned by digest", opts.Image)
	}
	if opts.Path != "" && !filepath.IsAbs(opts.Path) {
		return fmt.Errorf("login: host path %q is not absolute", opts.Path)
	}
	return nil
}

func (d *Decision) host() Host {
	if d.Host != nil {
		return d.Host
	}
	return LinuxHost{}
}

// accessList is every write the login makes, as the permissions they need.
func accessList() []kube.Access {
	return []kube.Access{
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "persistentvolumes", What: "the login's PersistentVolume"},
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "persistentvolumeclaims", Namespace: Namespace, What: "the login's claim in " + Namespace},
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "configmaps", Namespace: Namespace, What: "ConfigMaps in " + Namespace},
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "secrets", Namespace: Namespace, What: "Secrets in " + Namespace},
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "services", Namespace: Namespace, What: "Services in " + Namespace},
		{Verb: kube.VerbApply, Group: "apps", Resource: "deployments", Namespace: Namespace, What: "Deployments in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "referencegrants", Namespace: Namespace, What: "ReferenceGrants in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "httproutes", Namespace: Namespace, What: "the portal's HTTPRoute in " + Namespace},
		{Verb: kube.VerbApply, Group: PolicyGroup, Resource: "securitypolicies", Namespace: platform.GatewayNamespace, What: "SecurityPolicies in " + platform.GatewayNamespace},
	}
}

// MayWrite reports whether this identity may write the login at all.
//
// It is the question that decides whose the login is. The login serves every
// seedbox on the host, so it is root's: root's kubeconfig applies a ConfigMap
// in the platform's namespace, and a seedbox user's does not. A caller that is
// not root therefore skips the login rather than failing half way through its
// own apply, and a user's `theme:` never reaches the page everyone signs in on
// -- it is the palette that user's own box draws its dialogs in.
func MayWrite(ctx context.Context, cl Cluster) (bool, error) {
	denied, err := kube.Denied(ctx, cl, []kube.Access{{
		Verb:      kube.VerbApply,
		Group:     CoreGroup,
		Resource:  "configmaps",
		Namespace: Namespace,
		What:      "the login's configuration in " + Namespace,
	}})
	if err != nil {
		return false, fmt.Errorf("login: %w", err)
	}
	return len(denied) == 0, nil
}

// rulesAccess is what a rule write needs: the login's own objects -- the
// configuration, the Deployment whose pod-template hash rolls it, and the
// objects the same template renders -- plus the SecurityPolicy in the Gateway's
// namespace. It is smaller than accessList on purpose: a rule write is not an
// install, and it creates no volume.
func rulesAccess() []kube.Access {
	return []kube.Access{
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "configmaps", Namespace: Namespace, What: "ConfigMaps in " + Namespace},
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "services", Namespace: Namespace, What: "Services in " + Namespace},
		{Verb: kube.VerbApply, Group: "apps", Resource: "deployments", Namespace: Namespace, What: "Deployments in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "referencegrants", Namespace: Namespace, What: "ReferenceGrants in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "httproutes", Namespace: Namespace, What: "the portal's HTTPRoute in " + Namespace},
		{Verb: kube.VerbApply, Group: PolicyGroup, Resource: "securitypolicies", Namespace: platform.GatewayNamespace, What: "SecurityPolicies in " + platform.GatewayNamespace},
	}
}

// configMapDoc is the login's one ConfigMap: the configuration and, in
// binaryData, the mark and the favicon. It is built here rather than templated
// with the rest because its content is a rendered file, and a template would
// have to re-indent YAML inside YAML.
//
// The pictures ride with the configuration because they are mounted at the same
// path -- server.asset_path is /etc/authelia, where this ConfigMap is already
// read-only -- so an apply writes the login's whole face as one object, and a
// pod only ever waits on one ConfigMap to appear.
func configMapDoc(configuration []byte, assets authelia.Assets) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      authelia.ConfigMapName,
			"namespace": Namespace,
			"labels": map[string]any{
				kube.PartOfLabel:    kube.PartOf,
				kube.ManagedByLabel: kube.ManagedBy,
			},
		},
		"data":       map[string]any{authelia.ConfigFile: string(configuration)},
		"binaryData": anyMap(assets.BinaryData()),
	}
}

// configurationChanged compares the rendered file and pictures with the
// ConfigMap that is there. It is a report, not a decision: the apply is
// idempotent either way, and the pod template's hash is what makes a
// configuration change roll the pod.
//
// The pictures are compared too, and they are the one half that does not need
// the roll: Authelia stats and serves them from the filesystem on every request
// (internal/server/template.go:47, internal/middlewares/asset_override.go:24), so
// a re-rendered logo reaches the browser once the kubelet has updated the
// mounted ConfigMap -- a minute at most, and no restart.
func (d *Decision) configurationChanged(ctx context.Context, cl Cluster, configuration []byte, assets authelia.Assets) (bool, error) {
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "ConfigMap", Namespace, authelia.ConfigMapName)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	data, _, _ := unstructured.NestedStringMap(doc, "data")
	if data[authelia.ConfigFile] != string(configuration) {
		return true, nil
	}
	binary, _, _ := unstructured.NestedStringMap(doc, "binaryData")
	for name, want := range assets.BinaryData() {
		if binary[name] != want {
			return true, nil
		}
	}
	return false, nil
}

// anyMap is a string map as the untyped document a server-side apply takes:
// the API server's JSON form wants the base64 strings, and unstructured's
// accessors want them under any.
func anyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// renderLogin is the login's one rendering, from the inputs init and the
// per-host step both hold: the configuration file Authelia starts with, and the
// mark and favicon mounted beside it. It is one function because the same
// inputs must produce the same files -- a rule or a theme that depended on
// which command wrote it would be a second login.
func renderLogin(values map[string]any, host, name, key string, hosted []string, oidc *authelia.OIDC) ([]byte, authelia.Assets, error) {
	lt, err := authelia.ResolveTheme(themeName(values))
	if err != nil {
		return nil, authelia.Assets{}, err
	}
	config, err := authelia.RenderConfiguration(authelia.Configuration{
		Host:          host,
		Login:         name,
		EncryptionKey: key,
		Theme:         lt.Base,
		AssetPath:     authelia.AssetPath,
		Rules:         authelia.Rules(host, name, authelia.InstancesFromValues(values), hosted),
		OIDC:          oidc,
	})
	if err != nil {
		return nil, authelia.Assets{}, err
	}
	assets, err := authelia.RenderAssets(lt)
	if err != nil {
		return nil, authelia.Assets{}, err
	}
	return config, assets, nil
}

// themeName is the values file's top-level `theme:`. The chart defaults it to
// s5n, so a file that leaves it out is the default rather than a refusal, and an
// unknown name is refused by ResolveTheme with the list of names that work.
func themeName(values map[string]any) string {
	name, _ := values["theme"].(string)
	return name
}

// awaitDeployment waits for the pod to answer. A login that is not Ready is a
// failure, not a warning: the release behind it would be served unauthenticated
// by a Gateway whose policy points at nothing.
func (d *Decision) awaitDeployment(ctx context.Context, cl Cluster) error {
	timeout := d.DeployTimeout
	if timeout <= 0 {
		timeout = DeployTimeout
	}
	r := d.report()
	task := r.Start(say.Op{
		Label:  "waiting for the login to be ready",
		Plain:  fmt.Sprintf("waiting up to %s for Deployment %s/%s", timeout, Namespace, authelia.ServiceName),
		Budget: timeout,
	})
	// platform.Await is the one poll loop (internal/wait), with the workload
	// readiness rule every other box wait uses.
	err := cl.Await(ctx, platform.Wait{Kind: platform.WaitDeployment, Namespace: Namespace, Name: authelia.ServiceName, Timeout: timeout})
	if err != nil {
		task.Failed(err)
		return fmt.Errorf("deployment %s/%s did not become ready within %s: the login is not answering, so the release behind it is not installed: %w", Namespace, authelia.ServiceName, timeout, err)
	}
	task.Done(fmt.Sprintf("Deployment %s/%s is ready", Namespace, authelia.ServiceName))
	return nil
}

// policyTarget is what the Gateway-wide SecurityPolicy attaches to: the whole
// Gateway, so no sectionName. Envoy Gateway reports its acceptance under that
// ancestorRef, and the wait is for that entry, not for a condition on the
// policy.
func policyTarget() platform.PolicyTarget {
	return platform.PolicyTarget{
		Group:     "gateway.networking.k8s.io",
		Kind:      "Gateway",
		Name:      platform.GatewayName,
		Namespace: platform.GatewayNamespace,
	}
}

// awaitPolicy waits for Envoy Gateway to accept the SecurityPolicy. The
// not-yet-accepted reasons are transient (the controller has not reconciled it
// yet) and are polled to the deadline; the last reason is what the operator is
// told, because "Envoy Gateway refused it" and "it never answered" are
// different mornings.
func (d *Decision) awaitPolicy(ctx context.Context, cl Cluster, res *Result) error {
	timeout := d.PolicyTimeout
	if timeout <= 0 {
		timeout = PolicyTimeout
	}
	r := d.report()
	task := r.Start(say.Op{
		Label:  "waiting for the SecurityPolicy to be accepted",
		Plain:  fmt.Sprintf("waiting up to %s for SecurityPolicy %s/%s to be Accepted", timeout, platform.GatewayNamespace, authelia.PolicyName),
		Budget: timeout,
	})
	var reason string
	waited, err := wait.Until(ctx, timeout, d.poll(), func(ctx context.Context) (bool, error) {
		ok, why, err := cl.Accepted(ctx, PolicyGroup, PolicyVersion, "SecurityPolicy", platform.GatewayNamespace, authelia.PolicyName, policyTarget())
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		reason = why
		return false, nil
	})
	switch {
	case err != nil:
		task.Failed(err)
		return err
	case waited.Done:
		res.PolicyAccepted = true
		task.Done(fmt.Sprintf("SecurityPolicy %s/%s is Accepted", platform.GatewayNamespace, authelia.PolicyName))
		return nil
	}
	task.Failed(nil)
	if reason == "" {
		reason = "Envoy Gateway has not marked it Accepted"
	}
	return fmt.Errorf("SecurityPolicy %s/%s was not Accepted within %s (%s): the login is not in front of the Gateway, so the release is not installed",
		platform.GatewayNamespace, authelia.PolicyName, timeout, reason)
}

// keepAll is the failure once the login's objects exist: nothing is removed, and
// the sentence says where to look and that the next run carries on.
func keepAll(cause error) error {
	return fmt.Errorf("%w; the login's objects stay in %s so the cause can be read there (kubectl -n %s describe deployment %s), and the next run picks them up",
		cause, Namespace, Namespace, authelia.ServiceName)
}

// undo removes the volumes this run created, after a failure that stopped it
// before it wrote any of the login's own objects. Only what this run wrote: a
// volume that was already there is what a later run binds the claim to.
func (d *Decision) undo(ctx context.Context, store release.StorageClient, created []release.StorageObject, cause error) error {
	if len(created) == 0 {
		return cause
	}
	errs := []error{cause}
	for _, obj := range created {
		if err := store.Delete(ctx, obj); err != nil {
			errs = append(errs, fmt.Errorf("%s %s was created by this run and is still there; delete it with kubectl: %w", obj.Kind, obj.Name, err))
			continue
		}
		d.report().Line(fmt.Sprintf("removed %s %s: this run created it and did not finish", obj.Kind, obj.Name))
	}
	return errors.Join(errs...)
}
