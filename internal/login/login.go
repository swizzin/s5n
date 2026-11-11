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
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	Namespace = "s5n-system"
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

	// The Secret: created once, never updated. Reading it is also how the
	// configuration gets the key it must encrypt with.
	key, kept, err := d.encryptionKey(ctx, cl)
	if err != nil {
		return res, undo(err)
	}
	res.SecretKept = kept
	if !kept {
		res.Wrote = true
	}

	config, err := authelia.RenderConfiguration(authelia.Configuration{
		Host:          opts.Host,
		Login:         opts.Login,
		EncryptionKey: key,
		Rules:         authelia.Rules(opts.Host, opts.Login, authelia.InstancesFromValues(opts.Values), opts.Hosted),
	})
	if err != nil {
		return res, undo(err)
	}
	res.ConfigurationChanged, err = d.configurationChanged(ctx, cl, config)
	if err != nil {
		return res, undo(err)
	}

	manifests, err := authelia.RenderManifests(authelia.Manifests{
		Namespace:        Namespace,
		GatewayNamespace: platform.GatewayNamespace,
		Gateway:          platform.GatewayName,
		Host:             opts.Host,
		Claim:            authelia.ClaimName,
		ConfigMap:        authelia.ConfigMapName,
		Image:            opts.Image,
		UID:              authelia.UID,
		GID:              authelia.GID,
		ConfigHash:       authelia.ConfigHash(config),
	})
	if err != nil {
		return res, undo(err)
	}
	docs, err := platform.ParseManifests(manifests)
	if err != nil {
		return res, undo(err)
	}
	docs = append([]map[string]any{configMapDoc(config)}, docs...)
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

// encryptionKey reads s5n-authelia, creating it once if it is absent.
func (d *Decision) encryptionKey(ctx context.Context, cl Cluster) (key string, kept bool, err error) {
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", Namespace, authelia.SecretName)
	if err != nil {
		return "", false, err
	}
	if found {
		raw, _, _ := unstructured.NestedStringMap(doc, "data")
		encoded, ok := raw[authelia.EncryptionKeySecretKey]
		key, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if !ok || decodeErr != nil || len(key) < 20 {
			return "", false, fmt.Errorf("secret %s/%s exists but has no usable %s: box never rotates it, so restore the key from your backup, or delete the Secret and the claim together and start the login again",
				Namespace, authelia.SecretName, authelia.EncryptionKeySecretKey)
		}
		return string(key), true, nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("generate the storage encryption key: %w", err)
	}
	key = hex.EncodeToString(buf)
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      authelia.SecretName,
			"namespace": Namespace,
			"labels": map[string]any{
				kube.PartOfLabel:    kube.PartOf,
				kube.ManagedByLabel: kube.ManagedBy,
			},
		},
		"type": "Opaque",
		"data": map[string]any{
			authelia.EncryptionKeySecretKey: base64.StdEncoding.EncodeToString([]byte(key)),
		},
	}
	if err := cl.Create(ctx, secret); err != nil {
		return "", false, fmt.Errorf("create Secret %s/%s: %w", Namespace, authelia.SecretName, err)
	}
	d.report().Line(fmt.Sprintf("created Secret %s/%s: its key must be backed up with the claim, and box never changes it", Namespace, authelia.SecretName))
	return key, false, nil
}

// configMapDoc is the configuration's ConfigMap. It is built here rather than
// templated with the rest because its content is one rendered file, and a
// template would have to re-indent YAML inside YAML.
func configMapDoc(configuration []byte) map[string]any {
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
		"data": map[string]any{authelia.ConfigFile: string(configuration)},
	}
}

// configurationChanged compares the rendered configuration with the ConfigMap
// that is there. It is a report, not a decision: the apply is idempotent
// either way, and the pod template's hash is what makes a change roll the pod.
func (d *Decision) configurationChanged(ctx context.Context, cl Cluster, configuration []byte) (bool, error) {
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "ConfigMap", Namespace, authelia.ConfigMapName)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	data, _, _ := unstructured.NestedStringMap(doc, "data")
	return data[authelia.ConfigFile] != string(configuration), nil
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
