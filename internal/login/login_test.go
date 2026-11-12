package login

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"io/fs"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/brand"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/platform"
	"github.com/swizzin/s5n/internal/release"
	"github.com/swizzin/s5n/internal/seedbox"
)

// ---- fakes ------------------------------------------------------------------

// fakeCluster answers the reads the login does and records every write, in
// order, so a test can assert what was written and that nothing was.
type fakeCluster struct {
	servesPolicy bool
	namespace    bool
	denied       bool
	objects      map[string]map[string]any

	// applied is every document applied, and every Create, in call order.
	applied []map[string]any
	created []map[string]any

	// status is the sequence of policy status documents the API server
	// returns: the last one repeats, and each is read by platform.PolicyAccepted
	// so the tests exercise the shape Envoy Gateway really writes.
	status []map[string]any
	// deploys is how many Deployment waits happened; awaitErr fails them.
	deploys  int
	awaitErr error

	// failApply fails the nth ApplyWith call (1-based); 0 never.
	applyCalls int
	failApply  int
	// failCreate fails Create, the Secret's one write, which comes before
	// any of the login's other objects.
	failCreate error
}

// policyStatus is the document Envoy Gateway writes for a SecurityPolicy: the
// acceptance lives under status.ancestors, one entry per target and controller,
// and the policy itself has no status.conditions at all.
func policyStatus(accepted bool, reason, message string) map[string]any {
	cond := map[string]any{"type": "Accepted", "status": "False"}
	if accepted {
		cond["status"] = "True"
	}
	if reason != "" {
		cond["reason"] = reason
	}
	if message != "" {
		cond["message"] = message
	}
	return map[string]any{"ancestors": []any{map[string]any{
		"ancestorRef": map[string]any{
			"group":     "gateway.networking.k8s.io",
			"kind":      "Gateway",
			"name":      platform.GatewayName,
			"namespace": platform.GatewayNamespace,
		},
		"conditions":     []any{cond},
		"controllerName": bootstrap.EnvoyGatewayController,
	}}}
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		servesPolicy: true,
		namespace:    true,
		objects:      map[string]map[string]any{},
		status:       []map[string]any{policyStatus(true, "Accepted", "Policy has been accepted.")},
	}
}

func key(group, kind, ns, name string) string { return group + "/" + kind + "/" + ns + "/" + name }

func (f *fakeCluster) Allowed(context.Context, kube.Access) (bool, error) { return !f.denied, nil }

func (f *fakeCluster) Serves(context.Context, string, string, string) (bool, error) {
	return f.servesPolicy, nil
}

func (f *fakeCluster) ApplyWith(_ context.Context, _ string, docs []map[string]any) error {
	f.applyCalls++
	if f.failApply > 0 && f.applyCalls >= f.failApply {
		return errors.New("the API server refused the write")
	}
	for _, doc := range docs {
		f.applied = append(f.applied, doc)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		gv, _ := doc["apiVersion"].(string)
		group, _, _ := strings.Cut(gv, "/")
		if !strings.Contains(gv, "/") {
			group = ""
		}
		kind, _ := doc["kind"].(string)
		f.objects[key(group, kind, ns, name)] = doc
	}
	return nil
}

func (f *fakeCluster) Create(_ context.Context, doc map[string]any) error {
	if f.failCreate != nil {
		return f.failCreate
	}
	f.created = append(f.created, doc)
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	f.objects[key("", "Secret", ns, name)] = doc
	return nil
}

func (f *fakeCluster) Get(_ context.Context, group, _, kind, namespace, name string) (map[string]any, bool, error) {
	doc, ok := f.objects[key(group, kind, namespace, name)]
	return doc, ok, nil
}

func (f *fakeCluster) NamespaceExists(context.Context, string) (bool, error) {
	return f.namespace, nil
}

func (f *fakeCluster) Await(context.Context, platform.Wait) error {
	f.deploys++
	return f.awaitErr
}

func (f *fakeCluster) Accepted(_ context.Context, _, _, _, _, _ string, target platform.PolicyTarget) (bool, string, error) {
	status := f.status[0]
	if len(f.status) > 1 {
		f.status = f.status[1:]
	}
	ok, why := platform.PolicyAccepted(status, target)
	return ok, why, nil
}

// fakeStorage plans one claim and one volume, and records what was applied and
// deleted.
type fakeStorage struct {
	objects []release.StorageObject
	applied int
	deleted []string
}

func (f *fakeStorage) Plan(context.Context, string, []seedbox.Volume) ([]release.StorageObject, error) {
	return f.objects, nil
}
func (f *fakeStorage) PlanNamespace(context.Context) (release.NamespacePlan, error) {
	return release.NamespacePlan{Name: Namespace, Exists: true}, nil
}
func (f *fakeStorage) CreateNamespace(context.Context) error { return nil }
func (f *fakeStorage) Apply(context.Context, release.StorageObject) error {
	f.applied++
	return nil
}
func (f *fakeStorage) Delete(_ context.Context, obj release.StorageObject) error {
	f.deleted = append(f.deleted, obj.Name)
	return nil
}

// fakeHost records the directory and file writes, and shares one write log
// with the cluster so their order is assertable.
type fakeHost struct {
	log      *[]string
	files    map[string][]byte
	dirMade  bool
	readErr  error
	writeErr error
}

func newFakeHost(log *[]string) *fakeHost {
	return &fakeHost{log: log, files: map[string][]byte{}, dirMade: true}
}

func (f *fakeHost) EnsureDir(path string, _ fs.FileMode, uid, gid int) (bool, error) {
	*f.log = append(*f.log, "mkdir "+path)
	return f.dirMade, nil
}

func (f *fakeHost) ReadFile(path string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if data, ok := f.files[path]; ok {
		return data, nil
	}
	return nil, fs.ErrNotExist
}

func (f *fakeHost) WriteFile(path string, data []byte, _ fs.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	*f.log = append(*f.log, "write "+path)
	f.files[path] = data
	return nil
}

// ---- helpers ----------------------------------------------------------------

var testValues = map[string]any{
	"apps": map[string]any{
		"sonarr": map[string]any{
			"enabled":   true,
			"envPrefix": "SONARR__",
			"instances": []any{
				map[string]any{"suffix": "", "enabled": true},
				map[string]any{"suffix": "4k", "enabled": true},
			},
		},
		"plex": map[string]any{"enabled": true},
	},
}

type fixture struct {
	decision *Decision
	cluster  *fakeCluster
	storage  *fakeStorage
	host     *fakeHost
	log      []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{cluster: newFakeCluster(), storage: &fakeStorage{
		objects: []release.StorageObject{
			{Kind: release.KindPersistentVolume, Name: authelia.ClaimName},
			{Kind: release.KindPersistentVolumeClaim, Name: authelia.ClaimName},
		},
	}}
	f.host = newFakeHost(&f.log)
	f.decision = &Decision{
		Open:    func() (Cluster, error) { return f.cluster, nil },
		Storage: func() (release.StorageClient, error) { return f.storage, nil },
		Host:    f.host,
		// A millisecond budget and poll: the policy test is about what box
		// does with the answers, not about how long it waits.
		PolicyTimeout: 5 * time.Millisecond,
		Poll:          time.Millisecond,
	}
	return f
}

func testOptions() Options {
	return Options{
		Host:        "box.example.com",
		Login:       "alice",
		Password:    "a long enough passphrase",
		NewPassword: true,
		Image:       "authelia/authelia:4.39.28@sha256:" + strings.Repeat("a", 64),
		Values:      testValues,
	}
}

// find returns the one applied document of a kind and name.
func find(t *testing.T, docs []map[string]any, kind, ns, name string) map[string]any {
	t.Helper()
	for _, doc := range docs {
		k, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		n, _ := meta["name"].(string)
		s, _ := meta["namespace"].(string)
		if k == kind && n == name && s == ns {
			return doc
		}
	}
	t.Fatalf("no %s %s/%s among the applied documents", kind, ns, name)
	return nil
}

// ---- the objects ------------------------------------------------------------

// TestApplyWritesTheLogin is the object contract: one ConfigMap, the Service,
// the Deployment, the ReferenceGrant and the SecurityPolicy, with the wiring
// that makes the login actually enforce anything.
func TestApplyWritesTheLogin(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true // the file's write is its own test
	res, err := f.decision.Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.PolicyAccepted || !res.Wrote {
		t.Fatalf("result = %+v", res)
	}

	// The ConfigMap holds the configuration, and it is the configuration the
	// Deployment's hash names: that pair is what rolls the pod.
	cm := find(t, f.cluster.applied, "ConfigMap", Namespace, authelia.ConfigMapName)
	data, _, _ := unstructured.NestedStringMap(cm, "data")
	configuration := data[authelia.ConfigFile]
	for _, want := range []string{"box.example.com", "user:alice", `^/sonarr-4k/api`, `^/plex`} {
		if !strings.Contains(configuration, want) {
			t.Errorf("the configuration does not mention %q:\n%s", want, configuration)
		}
	}
	deploy := find(t, f.cluster.applied, "Deployment", Namespace, authelia.ServiceName)
	annotations, _, _ := unstructured.NestedStringMap(deploy, "spec", "template", "metadata", "annotations")
	if got := annotations["s5n.io/config-hash"]; got != authelia.ConfigHash([]byte(configuration)) {
		t.Errorf("pod annotation %q is not the configuration's hash: a config change would not roll the pod", got)
	}

	// One replica, Recreate: two pods would answer one cookie from two
	// in-memory session stores.
	if n, _, _ := unstructured.NestedFloat64(deploy, "spec", "replicas"); n != 1 {
		t.Errorf("replicas = %v", n)
	}
	if strategy, _, _ := unstructured.NestedString(deploy, "spec", "strategy", "type"); strategy != "Recreate" {
		t.Errorf("strategy = %q", strategy)
	}

	// The policy: the Gateway itself, the ext-authz endpoint, the cookie
	// header (without it every request looks anonymous) and fail-closed.
	policy := find(t, f.cluster.applied, "SecurityPolicy", platform.GatewayNamespace, authelia.PolicyName)
	if on, _, _ := unstructured.NestedBool(policy, "spec", "extAuth", "failOpen"); on {
		t.Error("failOpen is true: an unreachable login would serve the apps")
	}
	refs, _, _ := unstructured.NestedSlice(policy, "spec", "targetRefs")
	if len(refs) != 1 {
		t.Fatalf("targetRefs = %v", refs)
	}
	ref, _ := refs[0].(map[string]any)
	if ref["kind"] != "Gateway" || ref["name"] != platform.GatewayName {
		t.Errorf("the policy targets %v, want the Gateway itself", ref)
	}
	if _, ok := ref["sectionName"]; ok {
		t.Error("the policy pins a sectionName: it must cover every listener")
	}
	// headersToExtAuth is a field of extAuth, beside http -- not of http. The
	// API server is strict about it (an unknown field is refused), and a test
	// that read it from the wrong place would pass while every install failed.
	headers, _, _ := unstructured.NestedStringSlice(policy, "spec", "extAuth", "headersToExtAuth")
	if !contains(headers, "cookie") {
		t.Errorf("spec.extAuth.headersToExtAuth = %v: without cookie every request is anonymous", headers)
	}
	if !contains(headers, "x-forwarded-proto") {
		t.Errorf("spec.extAuth.headersToExtAuth = %v: Authelia would see every object as http and refuse it", headers)
	}
	// Accept decides 302-to-the-portal versus a bare 401 (Authelia's
	// getAuthzRedirectStatusCode), and it is not among Envoy's ext_authz
	// defaults: without it a browser never reaches the login page.
	if !contains(headers, "accept") {
		t.Errorf("spec.extAuth.headersToExtAuth = %v: Authelia answers 401 instead of redirecting to the portal", headers)
	}
	if wrong, _, _ := unstructured.NestedStringSlice(policy, "spec", "extAuth", "http", "headersToExtAuth"); len(wrong) != 0 {
		t.Errorf("headersToExtAuth is under extAuth.http (%v): Envoy Gateway's ExtAuth struct declares it on extAuth", wrong)
	}
	if backend, _, _ := unstructured.NestedStringSlice(policy, "spec", "extAuth", "http", "headersToBackend"); len(backend) != 4 {
		t.Errorf("headersToBackend = %v, want the four Remote-* headers", backend)
	}
	if path, _, _ := unstructured.NestedString(policy, "spec", "extAuth", "http", "path"); path != "/api/authz/ext-authz/" {
		t.Errorf("extAuth path = %q: Envoy appends the request path and Authelia's route captures a leading slash", path)
	}
	svc, _, _ := unstructured.NestedSlice(policy, "spec", "extAuth", "http", "backendRefs")
	backend, _ := svc[0].(map[string]any)
	if backend["name"] != authelia.ServiceName || backend["namespace"] != Namespace {
		t.Errorf("backendRef = %v", backend)
	}

	// The ReferenceGrant: the cross-namespace reference Gateway API requires,
	// written for correctness even though EG 1.9.3 does not ask for it.
	grant := find(t, f.cluster.applied, "ReferenceGrant", Namespace, authelia.ReferenceGrantName)
	from, _, _ := unstructured.NestedSlice(grant, "spec", "from")
	entry, _ := from[0].(map[string]any)
	if entry["kind"] != "SecurityPolicy" || entry["namespace"] != platform.GatewayNamespace {
		t.Errorf("ReferenceGrant from = %v", entry)
	}

	// The portal route: /auth on the host's own name, forwarding to the Service
	// the policy calls. Without it Authelia's 302 lands on a 404 and the login
	// is unreachable -- the one object the flow cannot do without.
	route := find(t, f.cluster.applied, "HTTPRoute", Namespace, authelia.RouteName)
	if hosts, _, _ := unstructured.NestedStringSlice(route, "spec", "hostnames"); len(hosts) != 1 || hosts[0] != "box.example.com" {
		t.Errorf("portal route hostnames = %v, want the host's own name", hosts)
	}
	rules, _, _ := unstructured.NestedSlice(route, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("portal route rules = %v, want one", rules)
	}
	rule, _ := rules[0].(map[string]any)
	matches, _ := rule["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("portal route matches = %v", matches)
	}
	match, _ := matches[0].(map[string]any)
	pathSpec, _ := match["path"].(map[string]any)
	if pathSpec["type"] != "PathPrefix" || pathSpec["value"] != "/auth" {
		t.Errorf("portal route path = %v, want PathPrefix /auth", pathSpec)
	}
	backends, _ := rule["backendRefs"].([]any)
	portalBackend, _ := backends[0].(map[string]any)
	if portalBackend["name"] != authelia.ServiceName {
		t.Errorf("portal route backend = %v, want the login's Service", portalBackend["name"])
	}

	// One Service in the platform namespace, on the port the policy names.
	service := find(t, f.cluster.applied, "Service", Namespace, authelia.ServiceName)
	ports, _, _ := unstructured.NestedSlice(service, "spec", "ports")
	port, _ := ports[0].(map[string]any)
	// The decoder gives numbers as float64: the manifests go through
	// apimachinery's YAML-to-JSON reader, which is what the API server sees.
	if port["port"] != float64(authelia.ServicePort) {
		t.Errorf("Service port = %v", port["port"])
	}

	// The container is what Pod Security restricted admits, and it can read
	// its own claim because box owns that directory.
	container, _, _ := unstructured.NestedSlice(deploy, "spec", "template", "spec", "containers")
	c, _ := container[0].(map[string]any)
	sc, _ := c["securityContext"].(map[string]any)
	if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("container securityContext = %v", sc)
	}
	if uid, _, _ := unstructured.NestedFloat64(deploy, "spec", "template", "spec", "securityContext", "runAsUser"); uid != float64(authelia.UID) {
		t.Errorf("runAsUser = %v", uid)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestApplyWritesTheFileBeforeTheDeployment is the ordering Authelia requires:
// watch:true fails at startup when the file is missing, so the file has to be
// on the claim before the pod exists.
func TestApplyWritesTheFileBeforeTheDeployment(t *testing.T) {
	f := newFixture(t)
	res, err := f.decision.Apply(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.UsersChanged || !res.DirCreated {
		t.Fatalf("result = %+v", res)
	}
	path := authelia.HostPath + "/" + authelia.UsersDBFile
	data, ok := f.host.files[path]
	if !ok {
		t.Fatalf("the users database was not written; host log %v", f.log)
	}
	if !strings.Contains(string(data), "alice:") || !strings.Contains(string(data), "$argon2id$") {
		t.Fatalf("users database:\n%s", data)
	}
	// The write is in the log before anything was applied to the cluster.
	writeAt := -1
	for i, entry := range f.log {
		if strings.HasPrefix(entry, "write ") {
			writeAt = i
		}
	}
	if writeAt != len(f.log)-1 {
		t.Fatalf("the write is not the last host action: %v", f.log)
	}
	if len(f.cluster.applied) == 0 {
		t.Fatal("nothing was applied")
	}
}

// TestApplyKeepsTheEncryptionKey: the Secret is created once and never
// updated, because rotating it orphans every encrypted row.
func TestApplyKeepsTheEncryptionKey(t *testing.T) {
	f := newFixture(t)
	encKey := strings.Repeat("k", 32)
	f.cluster.objects[key("", "Secret", Namespace, authelia.SecretName)] = map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": authelia.SecretName, "namespace": Namespace},
		"data":       map[string]any{authelia.EncryptionKeySecretKey: base64.StdEncoding.EncodeToString([]byte(encKey))},
	}
	res, err := f.decision.Apply(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.SecretKept {
		t.Fatal("an existing Secret was not kept")
	}
	if len(f.cluster.created) != 0 {
		t.Fatalf("box created %d objects, want none (the Secret already existed)", len(f.cluster.created))
	}
	cm := find(t, f.cluster.applied, "ConfigMap", Namespace, authelia.ConfigMapName)
	data, _, _ := unstructured.NestedStringMap(cm, "data")
	if !strings.Contains(data[authelia.ConfigFile], "encryption_key: '"+encKey+"'") {
		t.Error("the configuration does not carry the key that is in the Secret")
	}
}

// TestApplyCreatesTheSecretOnce proves the other half: a first run makes one
// 32-byte key, hex-encoded.
func TestApplyCreatesTheSecretOnce(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), testOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.cluster.created) != 1 {
		t.Fatalf("created %d objects, want just the Secret", len(f.cluster.created))
	}
	secret := f.cluster.created[0]
	data, _, _ := unstructured.NestedStringMap(secret, "data")
	raw, err := base64.StdEncoding.DecodeString(data[authelia.EncryptionKeySecretKey])
	if err != nil {
		t.Fatalf("the key is not base64: %v", err)
	}
	if len(raw) != 64 {
		t.Fatalf("the key is %d hex characters, want 64", len(raw))
	}
	if secret["type"] != "Opaque" {
		t.Fatalf("Secret type = %v", secret["type"])
	}
}

// TestApplyKeepsARowItWasNotGivenAPasswordFor: a re-run with no new password
// leaves the file alone, digest and all.
func TestApplyKeepsARowItWasNotGivenAPasswordFor(t *testing.T) {
	f := newFixture(t)
	digest := "$argon2id$v=19$m=32768,t=1,p=8$eUhVT1dQa082YVk2VUhDMQ$E8QI4jHbUBt3EdsU1NFDu4Bq5jObKNx7nBKSn1EYQxk"
	path := authelia.HostPath + "/" + authelia.UsersDBFile
	f.host.files[path] = []byte("users:\n  alice:\n    displayname: 'alice'\n    password: '" + digest + "'\n")
	opts := testOptions()
	opts.NewPassword = false
	opts.Password = ""
	res, err := f.decision.Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.UsersChanged {
		t.Fatal("the users database was rewritten for an unchanged row")
	}
	if got := string(f.host.files[path]); !strings.Contains(got, digest) {
		t.Fatalf("the digest is gone:\n%s", got)
	}
}

// TestApplyRefusesWithoutAPassword: there is nothing to write, and inventing a
// digest for a login nobody knows is worse than stopping.
func TestApplyRefusesWithoutAPassword(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.NewPassword = false
	opts.Password = ""
	if _, err := f.decision.Apply(context.Background(), opts); !errors.Is(err, ErrNoRow) {
		t.Fatalf("Apply = %v, want ErrNoRow", err)
	}
	if len(f.cluster.applied) != 0 || len(f.cluster.created) != 0 || len(f.log) != 0 {
		t.Fatal("a refused run wrote something")
	}
}

// TestApplyRefusesAFileItCannotRead: rewriting an unreadable file would
// discard the only copy of a digest.
func TestApplyRefusesAFileItCannotRead(t *testing.T) {
	f := newFixture(t)
	path := authelia.HostPath + "/" + authelia.UsersDBFile
	f.host.files[path] = []byte("users:\n  alice:\n    password: 'hunter2'\n")
	if _, err := f.decision.Apply(context.Background(), testOptions()); err == nil {
		t.Fatal("Apply accepted a users database Authelia would refuse")
	}
	if len(f.cluster.applied) != 0 {
		t.Fatal("a refused run wrote to the cluster")
	}
}

// TestApplyRefusesWithoutThePlatform: a cluster with no SecurityPolicy CRD or
// no s5n-system is one box bootstrap has not finished on.
func TestApplyRefusesWithoutThePlatform(t *testing.T) {
	for name, mutate := range map[string]func(*fakeCluster){
		"no SecurityPolicy": func(c *fakeCluster) { c.servesPolicy = false },
		"no s5n-system":     func(c *fakeCluster) { c.namespace = false },
		"no permission":     func(c *fakeCluster) { c.denied = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f.cluster)
			if _, err := f.decision.Apply(context.Background(), testOptions()); err == nil {
				t.Fatal("Apply went ahead on a cluster it cannot install the login on")
			}
			if len(f.cluster.applied) != 0 || f.storage.applied != 0 || len(f.log) != 0 {
				t.Fatal("a refused run wrote something")
			}
		})
	}
}

// TestApplyRollsBackTheVolumeItCreated: a PV is cluster-scoped and Retain, so
// one left behind by a run that stopped before any of the login's own objects
// existed is a volume nothing uses.
func TestApplyRollsBackTheVolumeItCreated(t *testing.T) {
	f := newFixture(t)
	f.cluster.failCreate = errors.New("the API server refused the Secret")
	if _, err := f.decision.Apply(context.Background(), testOptions()); err == nil {
		t.Fatal("Apply succeeded although the API server refused the Secret")
	}
	if len(f.storage.deleted) != 1 || f.storage.deleted[0] != authelia.ClaimName {
		t.Fatalf("deleted = %v, want the volume this run created", f.storage.deleted)
	}
	if f.cluster.applyCalls != 0 {
		t.Fatalf("ApplyWith ran %d times after the Secret failed", f.cluster.applyCalls)
	}
}

// TestApplyKeepsEverythingOnceTheLoginIsWritten: from the first write of the
// login's objects on, a failure removes nothing. Undoing only the volume would
// leave the Deployment waiting on a claim that is gone, which hides the cause.
func TestApplyKeepsEverythingOnceTheLoginIsWritten(t *testing.T) {
	for name, set := range map[string]func(*fixture){
		"apply refused":   func(f *fixture) { f.cluster.failApply = 1 },
		"login not ready": func(f *fixture) { f.cluster.awaitErr = errors.New("not ready") },
		"policy not accepted": func(f *fixture) {
			f.decision.PolicyTimeout = time.Millisecond
			f.cluster.status = []map[string]any{policyStatus(false, "the Gateway is not Programmed", "")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			set(f)
			res, err := f.decision.Apply(context.Background(), testOptions())
			if err == nil {
				t.Fatal("Apply succeeded")
			}
			if len(f.storage.deleted) != 0 {
				t.Fatalf("deleted = %v, want nothing removed", f.storage.deleted)
			}
			if !res.Wrote {
				t.Fatal("the result says nothing was written")
			}
			if want := "kubectl -n s5n-system describe deployment authelia"; !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to say where to look (%q)", err, want)
			}
		})
	}
}

// TestApplyStopsWhenTheLoginIsNotReady: the release must not be written behind
// a login that is not answering.
func TestApplyStopsWhenTheLoginIsNotReady(t *testing.T) {
	f := newFixture(t)
	f.cluster.awaitErr = errors.New("not ready")
	if _, err := f.decision.Apply(context.Background(), testOptions()); err == nil {
		t.Fatal("Apply succeeded with a login that never became ready")
	}
}

// TestApplyPollsATransientPolicyRefusal: Envoy Gateway reports a policy it has
// not translated yet with a reason; box keeps asking to the deadline.
func TestApplyPollsATransientPolicyRefusal(t *testing.T) {
	f := newFixture(t)
	f.decision.PolicyTimeout = time.Second
	f.decision.Poll = time.Millisecond
	f.cluster.status = []map[string]any{
		policyStatus(false, "Waiting for controller", "The SecurityPolicy has not been picked up yet"),
		policyStatus(false, "Waiting for controller", "The SecurityPolicy has not been picked up yet"),
		policyStatus(false, "Waiting for controller", "The SecurityPolicy has not been picked up yet"),
		policyStatus(true, "Accepted", "Policy has been accepted."),
	}
	res, err := f.decision.Apply(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.PolicyAccepted {
		t.Fatal("the policy was accepted but the result says otherwise")
	}
}

// TestApplyReportsThePolicyRefusal: a policy that is never accepted stops the
// run with Envoy Gateway's own reason in the sentence.
func TestApplyReportsThePolicyRefusal(t *testing.T) {
	f := newFixture(t)
	f.decision.PolicyTimeout = time.Millisecond
	f.cluster.status = []map[string]any{policyStatus(false, "the Gateway is not Programmed", "")}
	_, err := f.decision.Apply(context.Background(), testOptions())
	if err == nil || !strings.Contains(err.Error(), "the Gateway is not Programmed") {
		t.Fatalf("Apply = %v, want a refusal naming Envoy Gateway's reason", err)
	}
}

// TestApplyIsIdempotent: a second run with the same answers writes the same
// documents and reports no change to the configuration.
func TestApplyIsIdempotent(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	res, err := f.decision.Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if res.ConfigurationChanged {
		t.Fatal("the second run reports a configuration change for the same values")
	}
	if len(f.cluster.created) != 1 {
		t.Fatalf("the second run created %d objects: the Secret must be created once", len(f.cluster.created)-0)
	}
}

// TestApplyRefusesAnUnpinnedImage: box writes the Deployment, so the image is
// box's pin and a tag alone is not one.
func TestApplyRefusesAnUnpinnedImage(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.Image = "authelia/authelia:4.39.28"
	if _, err := f.decision.Apply(context.Background(), opts); err == nil {
		t.Fatal("Apply accepted an image that is not pinned by digest")
	}
	if len(f.cluster.applied) != 0 {
		t.Fatal("a refused run wrote to the cluster")
	}
}

// TestApplyValidatesTheLoginName: the name is a row key and a box option, and
// a name Authelia's file could not express must be refused before anything is
// written.
func TestApplyValidatesTheLoginName(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.Login = "al ice"
	if _, err := f.decision.Apply(context.Background(), opts); err == nil {
		t.Fatal("Apply accepted a login name with a space in it")
	}
}

// ---- the rules one host needs ------------------------------------------------

// hostedValues is testValues with Seerr on: the app served at a name of its own
// that an apply adds to a box installed without it. Built fresh each call so a
// test cannot change what the next one sees.
func hostedValues() map[string]any {
	return map[string]any{
		"apps": map[string]any{
			"sonarr": map[string]any{
				"enabled":   true,
				"envPrefix": "SONARR__",
				"instances": []any{
					map[string]any{"suffix": "", "enabled": true},
					map[string]any{"suffix": "4k", "enabled": true},
				},
			},
			"plex":  map[string]any{"enabled": true},
			"seerr": map[string]any{"enabled": true},
		},
	}
}

// ruleOptions is one rule write for a box served at box.example.com.
func ruleOptions() RulesOptions {
	return RulesOptions{
		Host:   "box.example.com",
		Login:  "alice",
		Hosts:  []string{"seerr.box.example.com"},
		Image:  "authelia/authelia:4.39.28@sha256:" + strings.Repeat("a", 64),
		Values: hostedValues(),
		Hosted: []string{"plex", "seerr"},
	}
}

// configOf is the configuration.yml in the ConfigMap last applied.
func configOf(t *testing.T, docs []map[string]any) string {
	t.Helper()
	cm := find(t, docs, "ConfigMap", Namespace, authelia.ConfigMapName)
	data, _, _ := unstructured.NestedStringMap(cm, "data")
	return data[authelia.ConfigFile]
}

// TestEnsureRulesWritesTheRuleForANewAppHost: a host with its listener and
// Certificate but no access_control rule answers the login's default deny, so
// the rule is written -- and the pod rolls, because the running login has read
// the old file.
func TestEnsureRulesWritesTheRuleForANewAppHost(t *testing.T) {
	f := newFixture(t)
	// The box as init installed it, without Seerr.
	opts := testOptions()
	opts.PathsInNode = true
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := configOf(t, f.cluster.applied)
	if strings.Contains(before, "seerr.box.example.com") {
		t.Fatalf("the fixture's login already has a rule for the app host:\n%s", before)
	}
	mark, deploys := len(f.cluster.applied), f.cluster.deploys

	changed, err := f.decision.EnsureRules(context.Background(), ruleOptions())
	if err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	if !changed {
		t.Fatal("the rule write reported no change")
	}
	if len(f.cluster.applied) == mark {
		t.Fatal("nothing was applied")
	}
	after := configOf(t, f.cluster.applied[mark:])
	if !strings.Contains(after, "- domain: ['seerr.box.example.com']") {
		t.Fatalf("the configuration has no rule for the app host:\n%s", after)
	}
	// The posture the app had on its path: Seerr signs its own users in, so the
	// whole name bypasses the login exactly as ^/seerr did.
	if !strings.Contains(after, "- domain: ['seerr.box.example.com']\n      policy: 'bypass'") {
		t.Fatalf("the app host's rule is not a bypass:\n%s", after)
	}
	// The rule the box's own name already had is still there: the configuration
	// is rewritten whole, and a name left out would lose its rule.
	if !strings.Contains(after, "user:alice") || !strings.Contains(after, "^/sonarr-4k/api") {
		t.Fatalf("the rewrite lost the box's own rules:\n%s", after)
	}
	deploy := find(t, f.cluster.applied[mark:], "Deployment", Namespace, authelia.ServiceName)
	annotations, _, _ := unstructured.NestedStringMap(deploy, "spec", "template", "metadata", "annotations")
	if got := annotations["s5n.io/config-hash"]; got != authelia.ConfigHash([]byte(after)) {
		t.Fatalf("pod annotation %q is not the new configuration's hash: the running login would keep the old rules", got)
	}
	if f.cluster.deploys == deploys {
		t.Fatal("the login was not waited for after the rule write")
	}
}

// keyed is the fixture with a known storage encryption key, so two fixtures in
// one test render the same configuration: the key is part of it.
func keyed(f *fixture, enc string) {
	f.cluster.objects[key("", "Secret", Namespace, authelia.SecretName)] = map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": authelia.SecretName, "namespace": Namespace},
		"data":       map[string]any{authelia.EncryptionKeySecretKey: base64.StdEncoding.EncodeToString([]byte(enc))},
	}
}

// TestEnsureRulesWritesNothingWhenTheRuleIsThere: the configuration is
// deterministic, so a run whose rules are already in the cluster is not a write
// and cannot churn the login's pod.
func TestEnsureRulesWritesNothingWhenTheRuleIsThere(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Values = hostedValues()
	opts.Hosted = []string{"plex", "seerr"}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	applied, deploys := len(f.cluster.applied), f.cluster.deploys
	changed, err := f.decision.EnsureRules(context.Background(), ruleOptions())
	if err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	if changed {
		t.Fatal("the run changed a configuration whose rules were already there")
	}
	if len(f.cluster.applied) != applied || f.cluster.deploys != deploys {
		t.Fatalf("the run wrote: %d documents, %d waits", len(f.cluster.applied)-applied, f.cluster.deploys-deploys)
	}
}

// TestEnsureRulesRendersWhatInitRenders: init installs the login with its rules
// and apply adds one host's rule; for the same box the two must write the same
// file, because both render it from authelia.Rules. Anything else is two
// spellings of one rule, and the next init would rewrite what apply wrote.
func TestEnsureRulesRendersWhatInitRenders(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"

	init := newFixture(t)
	keyed(init, key)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Values = hostedValues()
	opts.Hosted = []string{"plex", "seerr"}
	if _, err := init.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fromInit := configOf(t, init.cluster.applied)

	apply := newFixture(t)
	keyed(apply, key)
	if _, err := apply.decision.EnsureRules(context.Background(), ruleOptions()); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	fromApply := configOf(t, apply.cluster.applied)
	if fromApply != fromInit {
		t.Fatalf("apply wrote a different configuration from init:\n--- init ---\n%s\n--- apply ---\n%s", fromInit, fromApply)
	}
}

// TestEnsureRulesWithoutALoginNamesInit: a rule write is not an install, so a
// box with no login is refused rather than given a second, half-configured one.
func TestEnsureRulesWithoutALoginNamesInit(t *testing.T) {
	f := newFixture(t)
	_, err := f.decision.EnsureRules(context.Background(), ruleOptions())
	if err == nil {
		t.Fatal("a rule was written into a box with no login")
	}
	if !strings.Contains(err.Error(), "box init") {
		t.Fatalf("error = %v, want the command that installs the login", err)
	}
	if len(f.cluster.applied) != 0 {
		t.Fatalf("a refused rule write applied %d documents", len(f.cluster.applied))
	}
}

// TestEnsureRulesRefusesAnUnpinnedImage: the rule write re-applies the login's
// objects, so the image it names is pinned by digest like every other write.
func TestEnsureRulesRefusesAnUnpinnedImage(t *testing.T) {
	f := newFixture(t)
	opts := ruleOptions()
	opts.Image = "authelia/authelia:4.39.28"
	if _, err := f.decision.EnsureRules(context.Background(), opts); err == nil {
		t.Fatal("EnsureRules accepted an image that is not pinned by digest")
	}
	if len(f.cluster.applied) != 0 {
		t.Fatal("a refused rule write applied something")
	}
}

// ---- the login's face -------------------------------------------------------

// lastOf is the newest applied document of a kind and name in the login's
// namespace: every apply writes the login's ConfigMap again, and a test that
// applies twice wants the second one rather than the first.
func lastOf(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for i := len(docs) - 1; i >= 0; i-- {
		doc := docs[i]
		k, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		n, _ := meta["name"].(string)
		s, _ := meta["namespace"].(string)
		if k == kind && n == name && s == Namespace {
			return doc
		}
	}
	t.Fatalf("no %s %s/%s among the applied documents", kind, Namespace, name)
	return nil
}

// TestApplyWritesTheMarkAndFavicon: the login page carries box's mark, so the
// ConfigMap that holds the configuration holds the two files server.asset_path
// points at -- and the configuration names that path and the mapped theme.
func TestApplyWritesTheMarkAndFavicon(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cm := find(t, f.cluster.applied, "ConfigMap", Namespace, authelia.ConfigMapName)

	binary, _, _ := unstructured.NestedStringMap(cm, "binaryData")
	if len(binary) != 2 {
		t.Fatalf("binaryData carries %d files: %v", len(binary), binary)
	}
	logo, err := base64.StdEncoding.DecodeString(binary[authelia.AssetLogo])
	if err != nil {
		t.Fatalf("%s: %v", authelia.AssetLogo, err)
	}
	img, err := png.Decode(bytes.NewReader(logo))
	if err != nil {
		t.Fatalf("%s is not a PNG: %v", authelia.AssetLogo, err)
	}
	if got, want := img.Bounds().Dx(), brand.Cols*authelia.LogoCell; got != want {
		t.Errorf("the logo is %d px wide, want the mark's %d", got, want)
	}
	if _, err := base64.StdEncoding.DecodeString(binary[authelia.AssetFavicon]); err != nil {
		t.Errorf("%s is not base64: %v", authelia.AssetFavicon, err)
	}

	configuration := configOf(t, f.cluster.applied)
	for _, want := range []string{"theme: 'dark'", "asset_path: '" + authelia.AssetPath + "'"} {
		if !strings.Contains(configuration, want) {
			t.Errorf("the configuration does not contain %q:\n%s", want, configuration)
		}
	}
}

// TestAThemeChangeRewritesTheMark: the theme is a values key, so changing it
// changes the configuration and the pictures in one write. A second apply that
// differs only in the theme has to say so, or the login keeps the old face and
// the pod keeps the old page.
func TestAThemeChangeRewritesTheMark(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Values = map[string]any{"theme": "s5n", "apps": testValues["apps"]}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	first := find(t, f.cluster.applied, "ConfigMap", Namespace, authelia.ConfigMapName)
	firstBinary, _, _ := unstructured.NestedStringMap(first, "binaryData")

	opts.Values = map[string]any{"theme": "s5n-light", "apps": testValues["apps"]}
	res, err := f.decision.Apply(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if !res.ConfigurationChanged {
		t.Fatal("a theme change was not reported as a change: the pod would not roll and the page would stay s5n's dark")
	}
	second := lastOf(t, f.cluster.applied, "ConfigMap", authelia.ConfigMapName)
	secondBinary, _, _ := unstructured.NestedStringMap(second, "binaryData")
	if firstBinary[authelia.AssetLogo] == secondBinary[authelia.AssetLogo] {
		t.Fatal("the light theme's logo is the dark theme's: the mark did not follow the palette")
	}
	data, _, _ := unstructured.NestedStringMap(second, "data")
	if got := data[authelia.ConfigFile]; !strings.Contains(got, "theme: 'light'") {
		t.Errorf("the configuration does not pin the light page:\n%s", got)
	}
}

// TestEnsureRulesCarriesTheMark: a rule write re-applies the login's objects
// from the same template, so it carries the pictures too -- a box whose theme
// changed and whose only new work is a host's rule still ends up with the
// right face.
func TestEnsureRulesCarriesTheMark(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Values = map[string]any{"theme": "s5n", "apps": testValues["apps"]}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rules := ruleOptions()
	rules.Values = map[string]any{"theme": "s5n-light", "apps": hostedValues()["apps"]}
	changed, err := f.decision.EnsureRules(context.Background(), rules)
	if err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	if !changed {
		t.Fatal("the rule write reported no change for a configuration with a new theme")
	}
	cm := lastOf(t, f.cluster.applied, "ConfigMap", authelia.ConfigMapName)
	binary, _, _ := unstructured.NestedStringMap(cm, "binaryData")
	if len(binary) != 2 {
		t.Fatalf("the rule write applied a ConfigMap with %d pictures", len(binary))
	}
	data, _, _ := unstructured.NestedStringMap(cm, "data")
	if got := data[authelia.ConfigFile]; !strings.Contains(got, "theme: 'light'") {
		t.Errorf("the rule write did not carry the theme:\n%s", got)
	}
}

// ---- the login's own step ---------------------------------------------------

// initRules is what `box init` installed the login with in these tests, as the
// rule options the read and the write take: the same host, account, document
// and image, so "nothing differs" means nothing about the login.
func initRules(opts Options) RulesOptions {
	return RulesOptions{
		Host:   opts.Host,
		Login:  opts.Login,
		Hosts:  []string{opts.Host},
		Image:  opts.Image,
		Values: opts.Values,
	}
}

// TestPendingWritesNothing: the read an apply makes before its confirm renders
// the login the values name and compares it with the cluster, and writes
// neither the ConfigMap nor a roll. A read that wrote would be work done before
// the operator said yes.
func TestPendingWritesNothing(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Values = map[string]any{"theme": "s5n", "apps": testValues["apps"]}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	applied, deploys := len(f.cluster.applied), f.cluster.deploys

	pending, err := f.decision.Pending(context.Background(), initRules(opts))
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending {
		t.Error("the login the box already has was reported as a change")
	}
	if len(f.cluster.applied) != applied || f.cluster.deploys != deploys {
		t.Fatalf("Pending wrote: %d documents, %d waits", len(f.cluster.applied)-applied, f.cluster.deploys-deploys)
	}

	// The same box with a different theme: the page is a change, and still no
	// write.
	rules := initRules(opts)
	rules.Values = map[string]any{"theme": "s5n-light", "apps": testValues["apps"]}
	pending, err = f.decision.Pending(context.Background(), rules)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if !pending {
		t.Fatal("a theme change was not reported: apply would not re-render the page")
	}
	if len(f.cluster.applied) != applied || f.cluster.deploys != deploys {
		t.Fatalf("Pending wrote: %d documents, %d waits", len(f.cluster.applied)-applied, f.cluster.deploys-deploys)
	}
}

// TestReconcileWritesThePageOnce: the write half of the same read. A theme
// change re-renders the configuration and the mark in one apply, rolls the pod
// through the configuration's hash, and the second run is empty -- which is what
// keeps an apply with no change from churning the login.
func TestReconcileWritesThePageOnce(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Values = map[string]any{"theme": "s5n", "apps": testValues["apps"]}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := lastOf(t, f.cluster.applied, "ConfigMap", authelia.ConfigMapName)
	beforeBinary, _, _ := unstructured.NestedStringMap(before, "binaryData")
	applied, deploys := len(f.cluster.applied), f.cluster.deploys

	rules := initRules(opts)
	rules.Values = map[string]any{"theme": "s5n-light", "apps": testValues["apps"]}
	changed, err := f.decision.Reconcile(context.Background(), rules)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !changed {
		t.Fatal("Reconcile reported no change for a login with a new theme")
	}
	if len(f.cluster.applied) == applied {
		t.Fatal("Reconcile applied nothing")
	}
	if f.cluster.deploys == deploys {
		t.Fatal("the login was not waited for after the page changed")
	}
	after := lastOf(t, f.cluster.applied, "ConfigMap", authelia.ConfigMapName)
	data, _, _ := unstructured.NestedStringMap(after, "data")
	if got := data[authelia.ConfigFile]; !strings.Contains(got, "theme: 'light'") {
		t.Errorf("the reconciled configuration does not pin the light page:\n%s", got)
	}
	afterBinary, _, _ := unstructured.NestedStringMap(after, "binaryData")
	if beforeBinary[authelia.AssetLogo] == afterBinary[authelia.AssetLogo] {
		t.Error("the light page's mark is the dark page's: the pictures did not follow the theme")
	}
	// The Deployment the write applied carries the new configuration's hash, or
	// the running login keeps the old page.
	deploy := lastOf(t, f.cluster.applied, "Deployment", authelia.ServiceName)
	annotations, _, _ := unstructured.NestedStringMap(deploy, "spec", "template", "metadata", "annotations")
	if got := annotations["s5n.io/config-hash"]; got != authelia.ConfigHash([]byte(data[authelia.ConfigFile])) {
		t.Errorf("pod annotation %q is not the new configuration's hash", got)
	}

	// Nothing differs now, so the next run is not a write at all.
	applied, deploys = len(f.cluster.applied), f.cluster.deploys
	changed, err = f.decision.Reconcile(context.Background(), rules)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if changed {
		t.Error("Reconcile wrote a login that was already up to date")
	}
	if len(f.cluster.applied) != applied || f.cluster.deploys != deploys {
		t.Fatalf("the second Reconcile wrote: %d documents, %d waits", len(f.cluster.applied)-applied, f.cluster.deploys-deploys)
	}
}

// TestReconcileWithoutALoginNamesInit: the login's step is not an install
// either, so a box with no login is refused by the command that makes one.
func TestReconcileWithoutALoginNamesInit(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Reconcile(context.Background(), ruleOptions()); err == nil {
		t.Fatal("a login page was written into a box with no login")
	} else if !strings.Contains(err.Error(), "box init") {
		t.Fatalf("error = %v, want the command that installs the login", err)
	}
	if len(f.cluster.applied) != 0 {
		t.Fatalf("a refused reconcile applied %d documents", len(f.cluster.applied))
	}
}

// TestMayWriteIsRoots: the login is one page for the whole host, and only an
// identity that may apply a ConfigMap in the platform's namespace writes it. A
// seedbox user's apply is somebody else's configmap, which is why a `theme:` in
// their values file never reaches the sign-in page.
func TestMayWriteIsRoots(t *testing.T) {
	f := newFixture(t)
	ok, err := MayWrite(context.Background(), f.cluster)
	if err != nil {
		t.Fatalf("MayWrite: %v", err)
	}
	if !ok {
		t.Fatal("the identity that installed the login cannot write it")
	}
	f.cluster.denied = true
	ok, err = MayWrite(context.Background(), f.cluster)
	if err != nil {
		t.Fatalf("MayWrite: %v", err)
	}
	if ok {
		t.Fatal("an identity the API server denies was told it may write the login")
	}
}
