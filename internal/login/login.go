// Package login is root's login in the cluster: Authelia's state, its
// configuration, and the SecurityPolicy that puts it in front of the Gateway. It is
// the write half of internal/authelia; everything it writes is root's, and the
// grant's admission policy keeps a user's kubeconfig out.
//
// The order is the contract: a users database box cannot read is refused before any
// object exists, every permission is checked before the first write, and the claim
// and the file come before the Deployment.
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
	// Namespace is s5n-system: the login is root's and lives beside the admission
	// policy that protects it.
	Namespace = authelia.SystemNamespace
	// PolicyGroup and PolicyVersion are Envoy Gateway's SecurityPolicy, at the
	// version EG 1.9.3 serves.
	PolicyGroup   = "gateway.envoyproxy.io"
	PolicyVersion = "v1alpha1"
	// GatewayGroup and GatewayVersion are the Gateway the policy targets.
	GatewayGroup   = "gateway.networking.k8s.io"
	GatewayVersion = "v1"
	// CoreGroup is the API group of a Service, a Secret and a ConfigMap.
	CoreGroup = ""
	// AppsGroup is the API group of a Deployment.
	AppsGroup = "apps"
)

// Defaults for the two waits: the policy's is shorter because Envoy Gateway
// translates it in seconds.
const (
	DeployTimeout = 3 * time.Minute
	PolicyTimeout = 60 * time.Second
	Poll          = 2 * time.Second
)

// ErrNoRow says the users database has no row for the login and this run supplied
// no password: creating one needs a password nobody supplied.
var ErrNoRow = errors.New("no password")

// Cluster is the cluster the login needs: the platform client plus three reads and
// one create, so a fake can answer them.
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

// Host is the one host directory the login needs: /var/lib/s5n/authelia. It is an
// interface so a test can prove the ordering without a root-owned directory.
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
		// Chowned every run: a directory left over from another uid is a login
		// that cannot read its own database.
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
	// The file's owner is the login's uid, not root: the container runs as that uid
	// and could not read a 0600 file. The rename above is the file's final inode, so
	// the chown lands on the file Authelia opens.
	if err := os.Chown(path, authelia.UID, authelia.GID); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

// Options is one login install.
type Options struct {
	// Host is the seedbox's domain: the cookie domain, the portal's URL and the
	// only domain the rules name.
	Host string
	// Login is the one account, and Password its new password; NewPassword false
	// keeps whatever digest the file has.
	Login       string
	Password    string
	NewPassword bool
	// Path is the host directory the claim backs; only a test changes it.
	Path string
	// PathsInNode says that directory is inside a test node, so box cannot write
	// the users file and does not claim it did.
	PathsInNode bool
	// Image is the pinned Authelia image.
	Image string
	// Values is the seedbox's merged values: the chart's instance list is what the
	// rules derive from, and it is the document the release renders.
	Values map[string]any
	// Hosted are the apps the chart serves at <app>.<domain>, whose rule is the
	// whole name and whose path rule must not exist.
	Hosted []string
	// OwnLogin are the apps whose own sign-in bypasses the login.
	OwnLogin []string
	// Namespace is the seedbox's namespace: the per-app Secrets' home and half of
	// every client id, so an empty one is a refusal.
	Namespace string
	// OIDC are the apps that sign in through this login; a box with none has no
	// provider at all.
	OIDC []string
}

// Decision is one login install. Open and Storage are called only once the options
// are valid and the files are read, so a refusal writes nothing.
type Decision struct {
	Open func() (Cluster, error)
	// Storage creates the claim, with the release package's storage client: a PV
	// and a claim are the same objects whatever the namespace.
	Storage func() (release.StorageClient, error)
	Host    Host
	Say     say.Reporter

	// DeployTimeout, PolicyTimeout and Poll override the defaults.
	DeployTimeout time.Duration
	PolicyTimeout time.Duration
	Poll          time.Duration
}

// Result is what happened.
type Result struct {
	// Wrote is whether this run changed the cluster or the host at all.
	Wrote bool
	// UsersChanged is whether the users database was written.
	UsersChanged bool
	// SecretKept is true when s5n-authelia already existed and was left alone.
	SecretKept bool
	// ConfigurationChanged is whether the rendered configuration differs.
	ConfigurationChanged bool
	// PolicyAccepted is whether Envoy Gateway accepted the SecurityPolicy within
	// the wait; one that is not accepted stops the run.
	PolicyAccepted bool
	// DirCreated and ClaimCreated say what this run made.
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

	// The users database as it is on the host. A file box cannot read is a refusal:
	// rewriting it would discard the only copy of a digest.
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

	// Every permission the objects below need, asked as this identity before the
	// first write: a kubeconfig that cannot do these is not the one bootstrap left.
	if err := kube.Preflight(ctx, cl, accessList()); err != nil {
		return res, fmt.Errorf("login: %w", err)
	}

	// The claim, then the file, then the rest: Authelia can only read the database
	// from the claim and will not start without it.
	step := r.Start(say.Op{Name: "login"})
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
			r.Detail(fmt.Sprintf("users database: wrote %s (0600, login %s)", usersPath, opts.Login))
		} else {
			r.Detail(fmt.Sprintf("users database: %s already has %s, and this run supplied no new password", usersPath, opts.Login))
		}
	} else {
		r.Detail(fmt.Sprintf("users database: %s is inside the test node, so box cannot write it (--paths-in-node)", usersPath))
	}

	// The Secret: its encryption key created once and never changed, plus the
	// provider's signing material the first time this box has an OIDC app.
	//
	// The clients are not made here: an install runs before apply has created the
	// seedbox's namespace, and a client is a Secret there. EnsureClients runs where
	// the namespace exists.
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

	// One rendering for the whole face of the login, from the same function the
	// per-host step calls, so init and apply cannot disagree.
	//
	// clients is the provider's one decision: the configuration's
	// identity_providers block and the Deployment's mount read it. A stored digest
	// is not that decision -- a box whose last OIDC app was turned off keeps the
	// signing material and has no provider.
	clients := oidcClients(opts.Host, opts.Namespace, opts.Login, apps, sec.digests)
	config, assets, err := renderLogin(opts.Values, opts.Host, opts.Login, key, opts.Hosted, opts.OwnLogin, clients)
	if err != nil {
		return res, undo(err)
	}
	res.ConfigurationChanged, err = d.configurationChanged(ctx, cl, config, assets)
	if err != nil {
		return res, undo(err)
	}

	// The provider is mounted exactly when the configuration renders it: Authelia
	// refuses the block with no clients, and a first install has none yet, so the
	// signing material sits unused in its Secret until the anchor step writes them.
	manifests, err := authelia.RenderManifests(manifestsFor(opts.Host, opts.Image, authelia.ConfigHash(config), providerEnabled(clients)))
	if err != nil {
		return res, undo(err)
	}
	docs, err := platform.ParseManifests(manifests)
	if err != nil {
		return res, undo(err)
	}
	docs = append([]map[string]any{configMapDoc(config, assets)}, docs...)
	// From the first write of the login's own objects on, a failure keeps everything,
	// volumes included: a Deployment waiting on a claim that no longer exists hides
	// the real cause, and every object here is applied idempotently.
	res.Wrote = true
	if err := cl.ApplyWith(ctx, kube.FieldManager, docs); err != nil {
		step.Failed(err)
		return res, keepAll(fmt.Errorf("login: %w", err))
	}

	// Ready, then accepted: the login has to answer before the Gateway asks it, and
	// a policy Envoy Gateway refuses must stop the run before the release.
	if err := d.awaitDeployment(ctx, cl); err != nil {
		step.Failed(err)
		return res, keepAll(err)
	}
	if err := d.awaitPolicy(ctx, cl, &res); err != nil {
		step.Failed(err)
		return res, keepAll(err)
	}
	step.Done("Authelia")
	r.Detail(fmt.Sprintf("login ready: %s/%s, SecurityPolicy %s/%s accepted", Namespace, authelia.ServiceName, platform.GatewayNamespace, authelia.PolicyName))
	return res, nil
}

// RulesOptions is one access-control write: the inputs init installs the login
// with, so an apply writes a rule without inventing a second rendering.
type RulesOptions struct {
	// Host is the box's domain: the cookie's domain, and the name every app host is
	// a label under.
	Host string
	// Login is the one account the rules admit.
	Login string
	// Hosts are the names this run writes, for the one line it prints.
	Hosts []string
	// Image is the login Deployment's image, pinned by digest.
	Image string
	// Values is the seedbox's merged values, which the rules derive from.
	Values map[string]any
	// Hosted is the whole list, not only this run's names: the configuration is
	// rewritten whole and a name left out would lose its rule.
	Hosted []string
	// OwnLogin is the whole list for the same reason.
	OwnLogin []string
	// Namespace and OIDC are the provider's half of the same configuration; a client
	// left out would be one Authelia stopped answering for.
	Namespace string
	OIDC      []string
	// Installs says this run installs the login after reading the cluster (`box
	// init` does): a cluster without one is then work to show, not a refusal.
	Installs bool
}

// EnsureRules writes the login's configuration for the hosts a run is about to
// serve, and reports whether it changed anything.
//
// The login keeps one configuration for the whole box and Authelia reads it once at
// startup, so this is not a per-host edit: it renders from the same inputs init does,
// compares with the ConfigMap, and applies only when it differs -- with the
// Deployment, whose pod-template hash makes the login read it.
func (d *Decision) EnsureRules(ctx context.Context, opts RulesOptions) (bool, error) {
	cl, files, changed, err := d.prepare(ctx, opts, true)
	if err != nil {
		return false, err
	}
	names := strings.Join(opts.Hosts, ", ")
	if !changed {
		d.report().Rows([]say.Row{{Mark: say.MarkNote, Name: rulesRow, State: "kept", Facts: []string{hostsWord(opts.Hosts)}}})
		d.report().Detail("login rules kept: " + names)
		return false, nil
	}
	d.report().Detail("login rules written: " + names)
	return d.write(ctx, cl, opts, files,
		say.Op{Label: "applying the login's rule for " + names},
		say.Row{Mark: say.MarkDone, Name: rulesRow, State: "written", Facts: []string{hostsWord(opts.Hosts)}})
}

// Pending reports whether the login's configuration, pictures or a missing client
// differ from the cluster, and writes nothing. It is EnsureRules' read half, for a
// caller that asks before it writes.
//
// It is root's: reading the Secret and ConfigMap is the first thing it does, so an
// identity that cannot write the login is told so here.
func (d *Decision) Pending(ctx context.Context, opts RulesOptions) (bool, error) {
	_, _, changed, err := d.prepare(ctx, opts, false)
	return changed, err
}

// Reconcile writes the login's configuration and pictures when they differ. It is
// the login's own step, separate from the per-host one: a theme change is a new face
// for the page, not a new host, so it touches no listener, Certificate or DNS.
func (d *Decision) Reconcile(ctx context.Context, opts RulesOptions) (bool, error) {
	cl, files, changed, err := d.prepare(ctx, opts, true)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	// The page is plumbing: one row says it moved, and the log names the three
	// things that moved with it (rule 5).
	d.report().Detail("login page: configuration, mark and favicon")
	return d.write(ctx, cl, opts, files,
		say.Op{Label: "reconciling the login's page"},
		say.Row{Mark: say.MarkDone, Name: "login", State: "updated"})
}

// prepare is the read half of both writers: the rendering, the cluster it was
// compared against, and whether it differs.
//
// ensure says this run will write, so the provider's material may be created here:
// a box upgraded from before the provider has none, and a rule write is where it
// gains one. Pending passes false and only reads.
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
		// The clients, when this run may write them: the seedbox's namespace exists
		// by the time a rule write happens, and the digest written here is what the
		// configuration below renders from.
		if _, err := d.EnsureClients(ctx, opts.Namespace, apps); err != nil {
			return nil, none, false, err
		}
		sec, err = d.readSecret(ctx, cl)
	} else {
		sec, err = d.readSecret(ctx, cl)
		if err != nil {
			// The installing run installs the login after the confirm (`box init`),
			// so a cluster without one is work to show, not the refusal a rule
			// write makes.
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
	// The same one decision Apply makes: the clients both the configuration's block
	// and the Deployment's mount come from.
	clients := oidcClients(opts.Host, opts.Namespace, opts.Login, apps, sec.digests)
	provider := providerEnabled(clients)
	config, assets, err := renderLogin(opts.Values, opts.Host, opts.Login, key, opts.Hosted, opts.OwnLogin, clients)
	if err != nil {
		return nil, none, false, err
	}
	changed, err := d.configurationChanged(ctx, cl, config, assets)
	if err != nil {
		return nil, none, false, err
	}
	if !changed {
		// The mount is the other half of the same face: a correct configuration can
		// sit beside a Deployment that reads signing material no client names, and
		// only comparing the mount heals that.
		changed, err = d.providerMountDiffers(ctx, cl, provider)
		if err != nil {
			return nil, none, false, err
		}
	}
	if !changed && !ensure {
		// The third half is the client the cluster has not got yet: the rendering
		// leaves an app with no digest out, so a run that only enabled one renders
		// what the cluster holds while the app's pod waits on a Secret nobody
		// writes. PlanClients is EnsureClients' read half, and naming it makes the
		// Reconcile that follows write it before the release moves.
		planned, err := d.PlanClients(ctx, opts.Namespace, apps)
		if err != nil {
			return nil, none, false, err
		}
		changed = len(planned) > 0
	}
	return cl, loginFiles{config: config, assets: assets, provider: provider}, changed, nil
}

// manifestsFor is the login's objects for one host, from the constants this package
// and the platform own: init's install and every later rule write apply the same set,
// because a second rendering could downgrade the Deployment or drop the
// NetworkPolicy that keeps every pod but the Gateway off the login.
func manifestsFor(host, image, configHash string, oidc bool) authelia.Manifests {
	return authelia.Manifests{
		Namespace:        Namespace,
		GatewayNamespace: platform.GatewayNamespace,
		Gateway:          platform.GatewayName,
		// Envoy Gateway installs its proxy fleet in the controller's namespace in
		// standard mode, the only mode box installs.
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

// loginFiles is the login's rendering: the configuration Authelia starts with, and
// the mark and favicon mounted beside it.
type loginFiles struct {
	config []byte
	assets authelia.Assets
	// provider is the rendering's own answer to "is there an identity provider",
	// never read from the digests the Secret holds.
	provider bool
}

// write applies a rendering that differs and waits for the pod that has to read it;
// the permissions are asked before the first write.
func (d *Decision) write(ctx context.Context, cl Cluster, opts RulesOptions, files loginFiles, op say.Op, row say.Row) (bool, error) {
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
	// The ConfigMap is not in the template: its content is one rendered file.
	docs = append([]map[string]any{configMapDoc(files.config, files.assets)}, docs...)

	step := d.report().Start(op)
	if err := cl.ApplyWith(ctx, kube.FieldManager, docs); err != nil {
		step.Failed(err)
		return false, fmt.Errorf("login: %w", err)
	}
	// The pod still has the old file: the hash is what rolls it, and a rule or page
	// the running login has not read is not the login that is answering.
	if err := d.awaitDeployment(ctx, cl); err != nil {
		step.Failed(err)
		return true, err
	}
	step.Done("")
	d.report().Rows([]say.Row{row})
	return true, nil
}

// rulesRow is the login's access rules as a row: kept or written, for how many
// names. The names themselves are the log's, where a list of host names belongs.
const rulesRow = "login rules"

// hostsWord is how many names a rule write is for: "1 host", "4 hosts".
func hostsWord(hosts []string) string {
	if len(hosts) == 1 {
		return "1 host"
	}
	return fmt.Sprintf("%d hosts", len(hosts))
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
		{Verb: kube.VerbApply, Group: AppsGroup, Resource: "deployments", Namespace: Namespace, What: "Deployments in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "referencegrants", Namespace: Namespace, What: "ReferenceGrants in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "httproutes", Namespace: Namespace, What: "the portal's HTTPRoute in " + Namespace},
		{Verb: kube.VerbApply, Group: PolicyGroup, Resource: "securitypolicies", Namespace: platform.GatewayNamespace, What: "SecurityPolicies in " + platform.GatewayNamespace},
	}
}

// MayWrite reports whether this identity may write the login at all, which is what
// decides whose it is. A non-root caller skips the login rather than failing half way
// through its apply, so a user's `theme:` never reaches the page everyone signs in on.
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

// rulesAccess is what a rule write needs: the login's own objects and the
// SecurityPolicy, but no volume -- a rule write is not an install.
func rulesAccess() []kube.Access {
	return []kube.Access{
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "configmaps", Namespace: Namespace, What: "ConfigMaps in " + Namespace},
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "services", Namespace: Namespace, What: "Services in " + Namespace},
		{Verb: kube.VerbApply, Group: AppsGroup, Resource: "deployments", Namespace: Namespace, What: "Deployments in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "referencegrants", Namespace: Namespace, What: "ReferenceGrants in " + Namespace},
		{Verb: kube.VerbApply, Group: GatewayGroup, Resource: "httproutes", Namespace: Namespace, What: "the portal's HTTPRoute in " + Namespace},
		{Verb: kube.VerbApply, Group: PolicyGroup, Resource: "securitypolicies", Namespace: platform.GatewayNamespace, What: "SecurityPolicies in " + platform.GatewayNamespace},
	}
}

// configMapDoc is the login's one ConfigMap: the configuration and, in binaryData,
// the mark and favicon. It is built here because its content is a rendered file, and
// a template would re-indent YAML inside YAML. The pictures ride with it because they
// are mounted at the same path, so a pod waits on one ConfigMap.
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

// configurationChanged compares the rendered file and pictures with the ConfigMap
// that is there. It is a report, not a decision: the apply is idempotent, and the pod
// template's hash rolls the pod.
//
// The pictures need no roll: Authelia serves them from the filesystem on every
// request (internal/server/template.go:47,
// internal/middlewares/asset_override.go:24), so a new logo reaches the browser once
// the kubelet updates the mount.
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

// providerMountDiffers reports whether the Deployment's provider mount disagrees
// with the rendering. A box installed by a run whose halves disagreed keeps a mount
// its configuration names no client for, and Authelia exits on it; comparing only the
// ConfigMap called that box "nothing to change" forever.
//
// Only the second --config argument and the volumeMount at OIDCMountPath are read: a
// whole-object comparison would see every default and status and never settle.
func (d *Decision) providerMountDiffers(ctx context.Context, cl Cluster, want bool) (bool, error) {
	doc, found, err := cl.Get(ctx, AppsGroup, "v1", "Deployment", Namespace, authelia.ServiceName)
	if err != nil {
		return false, err
	}
	if !found {
		// A configuration without its Deployment is not an installed login:
		// writing it is this run's work.
		return true, nil
	}
	args, mount := providerInDeployment(doc)
	return args != want || mount != want, nil
}

// providerInDeployment is whether the pods read the provider's file: the argument
// after a `--config`, and the volumeMount at that path. Both are written under one
// condition in the template, so one without the other is hand-edited.
func providerInDeployment(doc map[string]any) (args, mount bool) {
	containers, _, _ := unstructured.NestedSlice(doc, "spec", "template", "spec", "containers")
	for _, raw := range containers {
		c, _ := raw.(map[string]any)
		if c == nil {
			continue
		}
		a, m := providerInContainer(c)
		args, mount = args || a, mount || m
	}
	return args, mount
}

// providerInContainer is one container's half of it.
func providerInContainer(c map[string]any) (args, mount bool) {
	file := authelia.OIDCMountPath + "/" + authelia.OIDCFile
	raw, _ := c["args"].([]any)
	prev := ""
	for _, a := range raw {
		s, _ := a.(string)
		if prev == "--config" && s == file {
			args = true
		}
		prev = s
	}
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mm["mountPath"] == authelia.OIDCMountPath {
			mount = true
		}
	}
	return args, mount
}

// anyMap is a string map as the untyped document a server-side apply takes.
func anyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// renderLogin is the login's one rendering, from the inputs init and the per-host
// step both hold: the same inputs must produce the same files, or the login has two
// faces.
func renderLogin(values map[string]any, host, name, key string, hosted, ownLogin []string, oidc *authelia.OIDC) ([]byte, authelia.Assets, error) {
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
		Rules:         authelia.Rules(host, name, authelia.InstancesFromValues(values), hosted, ownLogin),
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

// themeName is the values file's top-level `theme:`; the chart defaults it to s5n,
// so a file that leaves it out is the default.
func themeName(values map[string]any) string {
	name, _ := values["theme"].(string)
	return name
}

// awaitDeployment waits for the pod to answer. A login that is not Ready is a
// failure: the release behind it would be served unauthenticated by a Gateway whose
// policy points at nothing.
func (d *Decision) awaitDeployment(ctx context.Context, cl Cluster) error {
	timeout := d.DeployTimeout
	if timeout <= 0 {
		timeout = DeployTimeout
	}
	r := d.report()
	r.Detail(fmt.Sprintf("waiting up to %s for Deployment %s/%s", timeout, Namespace, authelia.ServiceName))
	// platform.Await is the one poll loop (internal/wait), with the workload
	// readiness rule every other box wait uses.
	err := cl.Await(ctx, platform.Wait{Kind: platform.WaitDeployment, Namespace: Namespace, Name: authelia.ServiceName, Timeout: timeout})
	if err != nil {
		return fmt.Errorf("deployment %s/%s did not become ready within %s: the login is not answering, so the release behind it is not installed: %w", Namespace, authelia.ServiceName, timeout, err)
	}
	r.Detail(fmt.Sprintf("Deployment %s/%s is ready", Namespace, authelia.ServiceName))
	return nil
}

// policyTarget is what the Gateway-wide SecurityPolicy attaches to: the whole
// Gateway, so no sectionName.
func policyTarget() platform.PolicyTarget {
	return platform.PolicyTarget{
		Group:     "gateway.networking.k8s.io",
		Kind:      "Gateway",
		Name:      platform.GatewayName,
		Namespace: platform.GatewayNamespace,
	}
}

// awaitPolicy waits for Envoy Gateway to accept the SecurityPolicy. The last reason
// is what the operator is told, because "refused it" and "never answered" are
// different mornings.
func (d *Decision) awaitPolicy(ctx context.Context, cl Cluster, res *Result) error {
	timeout := d.PolicyTimeout
	if timeout <= 0 {
		timeout = PolicyTimeout
	}
	r := d.report()
	r.Detail(fmt.Sprintf("waiting up to %s for SecurityPolicy %s/%s to be Accepted", timeout, platform.GatewayNamespace, authelia.PolicyName))
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
		return err
	case waited.Done:
		res.PolicyAccepted = true
		r.Detail(fmt.Sprintf("SecurityPolicy %s/%s is Accepted", platform.GatewayNamespace, authelia.PolicyName))
		return nil
	}
	if reason == "" {
		reason = "Envoy Gateway has not marked it Accepted"
	}
	return fmt.Errorf("SecurityPolicy %s/%s was not Accepted within %s (%s): the login is not in front of the Gateway, so the release is not installed",
		platform.GatewayNamespace, authelia.PolicyName, timeout, reason)
}

// keepAll is the failure once the login's objects exist: nothing is removed, and the
// sentence says where to look and that the next run carries on.
func keepAll(cause error) error {
	return fmt.Errorf("%w; the login's objects stay in %s so the cause can be read there (kubectl -n %s describe deployment %s), and the next run picks them up",
		cause, Namespace, Namespace, authelia.ServiceName)
}

// undo removes the volumes this run created, after a failure before any of the
// login's own objects: a volume that was already there is what a later run binds to.
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
