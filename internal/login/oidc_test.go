package login

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/authelia"
)

// oidcOptions is init's install for a box whose catalog says qui signs in
// through the login.
func oidcOptions() Options {
	o := testOptions()
	o.PathsInNode = true
	o.Namespace = "seedbox"
	o.OIDC = []string{"qui"}
	return o
}

// lastConfig is the configuration.yml in the ConfigMap's most recent write: a
// second write appends to the fake's list, and the question is what the cluster
// is left with.
func lastConfig(t *testing.T, docs []map[string]any) string {
	t.Helper()
	for i := len(docs) - 1; i >= 0; i-- {
		doc := docs[i]
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if kind != "ConfigMap" || name != authelia.ConfigMapName {
			continue
		}
		data, _, _ := unstructured.NestedStringMap(doc, "data")
		return data[authelia.ConfigFile]
	}
	t.Fatalf("no ConfigMap %s/%s among the written documents", Namespace, authelia.ConfigMapName)
	return ""
}

// lastLoginSecretData is the login's Secret as the most recent write left it.
func lastLoginSecretData(t *testing.T, docs []map[string]any) map[string]string {
	t.Helper()
	for i := len(docs) - 1; i >= 0; i-- {
		doc := docs[i]
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if kind != "Secret" || name != authelia.SecretName || ns != Namespace {
			continue
		}
		data, _, _ := unstructured.NestedStringMap(doc, "data")
		return data
	}
	t.Fatalf("no Secret %s/%s among the written documents", Namespace, authelia.SecretName)
	return nil
}

// allWrites is every document this run wrote, applied or created: the login's
// Secret is the one object written with Create on a fresh box.
func allWrites(f *fixture) []map[string]any {
	return append(append([]map[string]any{}, f.cluster.applied...), f.cluster.created...)
}

// loginSecretData is the login's Secret as the last write left it.
func loginSecretData(t *testing.T, docs []map[string]any) map[string]string {
	t.Helper()
	doc := find(t, docs, "Secret", Namespace, authelia.SecretName)
	data, _, _ := unstructured.NestedStringMap(doc, "data")
	return data
}

// clientSecretOf is the plaintext in one app's Secret.
func clientSecretOf(t *testing.T, docs []map[string]any, namespace, app string) string {
	t.Helper()
	doc := find(t, docs, "Secret", namespace, authelia.OIDCClientSecretName(app))
	data, _, _ := unstructured.NestedStringMap(doc, "data")
	raw := data[authelia.OIDCClientSecretKey]
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("the client secret is not base64: %v", err)
	}
	return string(decoded)
}

func digestOf(t *testing.T, data map[string]string, clientID string) string {
	t.Helper()
	raw, ok := data[authelia.OIDCClientDigestKey(clientID)]
	if !ok {
		t.Fatalf("the login's Secret has no digest for %s: %v", clientID, keysOf(data))
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("the digest is not base64: %v", err)
	}
	return string(decoded)
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// deploymentOf is the login's Deployment as the last write left it.
func deploymentOf(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	doc := find(t, docs, "Deployment", Namespace, authelia.ServiceName)
	spec, _ := doc["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	return podSpec
}

// lastDeploymentOf is the most recent Deployment among the writes: the first
// install writes one without the provider, and the write that adds a client
// writes it again with one.
func lastDeploymentOf(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for i := len(docs) - 1; i >= 0; i-- {
		doc := docs[i]
		k, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		n, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if k == "Deployment" && n == authelia.ServiceName && ns == Namespace {
			spec, _ := doc["spec"].(map[string]any)
			template, _ := spec["template"].(map[string]any)
			podSpec, _ := template["spec"].(map[string]any)
			return podSpec
		}
	}
	t.Fatalf("no Deployment %s/%s among the written documents", Namespace, authelia.ServiceName)
	return nil
}

// podContainer is the login's one container.
func podContainer(t *testing.T, podSpec map[string]any) map[string]any {
	t.Helper()
	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("the Deployment carries %d containers, want one", len(containers))
	}
	c, _ := containers[0].(map[string]any)
	return c
}

// podArgs is a container's args as strings, in order.
func podArgs(t *testing.T, c map[string]any) []string {
	t.Helper()
	raw, _ := c["args"].([]any)
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		s, _ := a.(string)
		out = append(out, s)
	}
	return out
}

// podMounts is a container's volume mounts as their documents.
func podMounts(t *testing.T, c map[string]any) []map[string]any {
	t.Helper()
	raw, _ := c["volumeMounts"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		out = append(out, mm)
	}
	return out
}

// providerMounted reports whether the container mounts the provider's Secret
// and passes its file as a second --config.
func providerMounted(t *testing.T, c map[string]any) (args, mount bool) {
	t.Helper()
	joined := podArgs(t, c)
	args = len(joined) == 4 && joined[2] == "--config" &&
		joined[3] == authelia.OIDCMountPath+"/"+authelia.OIDCFile
	for _, m := range podMounts(t, c) {
		if m["mountPath"] == authelia.OIDCMountPath {
			if ro, _ := m["readOnly"].(bool); !ro {
				t.Fatal("the provider's Secret is mounted writable")
			}
			mount = true
		}
	}
	return args, mount
}

// TestApplyWritesTheSigningMaterialOnly: init's login step runs before apply
// has created the seedbox's namespace, so it writes the provider's signing
// material into the login's Secret and nothing into the namespace. A first
// install has no namespace to write a client Secret into, and the step that
// does have one is the host-CA step (EnsureClients).
func TestApplyWritesTheSigningMaterialOnly(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data := loginSecretData(t, allWrites(f))
	provider, ok := data[authelia.OIDCFile]
	if !ok {
		t.Fatalf("the login's Secret carries no %s: every token request would fail: %v", authelia.OIDCFile, keysOf(data))
	}
	decoded, err := base64.StdEncoding.DecodeString(provider)
	if err != nil {
		t.Fatalf("the provider's file is not base64: %v", err)
	}
	if !strings.Contains(string(decoded), "hmac_secret:") || !strings.Contains(string(decoded), "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("the provider's file carries no signing material:\n%s", decoded)
	}
	for k := range data {
		if strings.HasPrefix(k, "oidc-client-") {
			t.Fatalf("init wrote a client digest before the client exists: %s", k)
		}
	}
	// Nothing in the seedbox's namespace: that is the bug this shape cannot
	// have. The login's own Secret is in s5n-system.
	for _, doc := range allWrites(f) {
		meta, _ := doc["metadata"].(map[string]any)
		if meta["namespace"] == "seedbox" {
			t.Fatalf("init wrote %v/%v into the seedbox's namespace, which does not exist yet",
				doc["kind"], meta["name"])
		}
	}
	// And it is not mounted yet: Authelia refuses an identity_providers.oidc
	// block with no clients ("option 'clients' must have one or more clients
	// configured"), and a first install has none -- the client Secrets are the
	// anchor step's write. The signing material waits in the Secret until the
	// render that names a client turns the provider on.
	c := podContainer(t, deploymentOf(t, f.cluster.applied))
	if args, mount := providerMounted(t, c); args || mount {
		t.Fatalf("the Deployment reads the provider's file before there is a client (args %v, mount %v): Authelia exits on a provider with no clients", podArgs(t, c), mount)
	}
}

// TestEnsureClientsWritesTheClientIntoTheNamespace is the step that runs after
// apply created the namespace: one Secret holding the plaintext the app reads,
// and the digest in the login's Secret. The two verify against each other.
func TestEnsureClientsWritesTheClientIntoTheNamespace(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	planned, err := f.decision.PlanClients(context.Background(), "seedbox", []string{"qui"})
	if err != nil {
		t.Fatalf("PlanClients: %v", err)
	}
	if len(planned) != 1 || planned[0] != "qui" {
		t.Fatalf("PlanClients = %v, want qui before the write", planned)
	}
	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", []string{"qui"}); err != nil {
		t.Fatalf("EnsureClients: %v", err)
	}
	secret := clientSecretOf(t, allWrites(f), "seedbox", "qui")
	if len(secret) != authelia.OIDCClientSecretLength {
		t.Fatalf("the client secret is %d characters, want %d", len(secret), authelia.OIDCClientSecretLength)
	}
	digest := digestOf(t, loginSecretData(t, allWrites(f)), authelia.ClientID("seedbox", "qui"))
	if !authelia.VerifyClientSecret(digest, secret) {
		t.Fatal("the digest in the login's Secret does not verify against the plaintext in the seedbox's Secret")
	}
	planned, err = f.decision.PlanClients(context.Background(), "seedbox", []string{"qui"})
	if err != nil {
		t.Fatalf("PlanClients: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("PlanClients still wants %v after the write", planned)
	}
	// And the configuration the next render produces names it.
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = []string{"qui"}
	if _, err := f.decision.EnsureRules(context.Background(), rules); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	config := lastConfig(t, allWrites(f))
	for _, want := range []string{
		"      - client_id: 'seedbox-qui'",
		"          - 'https://box.example.com/qui/api/auth/oidc/callback'",
		"        client_secret: '" + digest + "'",
		"        authorization_policy: 'seedbox'",
		"              - 'user:alice'",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("the configuration does not contain %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, secret) {
		t.Fatal("the configuration carries the client's plaintext secret: only the digest belongs there")
	}
	if strings.Contains(config, "BEGIN RSA PRIVATE KEY") {
		t.Fatal("the configuration carries the signing key: it belongs in the Secret's file")
	}
	// And now that a client exists, the Deployment reads the provider's file:
	// the two writes are one install, and the second one turns the provider on.
	c := podContainer(t, lastDeploymentOf(t, allWrites(f)))
	if args, mount := providerMounted(t, c); !args || !mount {
		t.Fatalf("the Deployment does not read the provider's file once a client exists (args %v, mount %v)", podArgs(t, c), mount)
	}
}

// TestEnsureClientsDoesNothingWithoutTheNamespace: the first-install state. The
// write is skipped rather than attempted, because a Secret cannot be written
// into a namespace that is not there and the host-CA step will do it later.
func TestEnsureClientsDoesNothingWithoutTheNamespace(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	f.cluster.namespace = false
	before := len(f.cluster.applied)
	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", []string{"qui"}); err != nil {
		t.Fatalf("EnsureClients with no namespace: %v", err)
	}
	if len(f.cluster.applied) != before {
		t.Fatalf("EnsureClients wrote %d documents into a namespace that does not exist", len(f.cluster.applied)-before)
	}
}

// TestApplyWithNoOIDCAppIsNotAProvider: a box that installed no OIDC client has
// no identity_providers block, no client Secret, and a Deployment that reads one
// configuration file. That is what every box did before this existed, and it is
// still what a box with qui off must be.
func TestApplyWithNoOIDCAppIsNotAProvider(t *testing.T) {
	f := newFixture(t)
	opts := testOptions()
	opts.PathsInNode = true
	opts.Namespace = "seedbox"
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	config := configOf(t, f.cluster.applied)
	if strings.Contains(config, "identity_providers") {
		t.Fatalf("a box with no OIDC app rendered a provider:\n%s", config)
	}
	data := loginSecretData(t, allWrites(f))
	if _, ok := data[authelia.OIDCFile]; ok {
		t.Fatalf("a box with no OIDC app generated signing material: %v", keysOf(data))
	}
	for k := range f.cluster.objects {
		if strings.Contains(k, authelia.OIDCClientSecretName("qui")) {
			t.Fatalf("a client Secret was written for an app that is not a client: %s", k)
		}
	}
	c := podContainer(t, deploymentOf(t, f.cluster.applied))
	args, _ := c["args"].([]any)
	if len(args) != 2 {
		t.Fatalf("the Deployment's args = %v, want the one configuration file", args)
	}
}

// TestEnsureRulesRendersTheSameProviderTwice: init writes the signing material
// and the rule write writes the clients, so the first rule write after an
// install is real work -- and the second is not. A configuration that changed on
// every render would roll the login, and drop every session, for nothing.
func TestEnsureRulesRendersTheSameProviderTwice(t *testing.T) {
	f := newFixture(t)
	opts := oidcOptions()
	opts.Values = hostedValues()
	opts.Hosted = []string{"plex", "seerr"}
	opts.OwnLogin = []string{"plex", "seerr"}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = []string{"qui"}

	changed, err := f.decision.EnsureRules(context.Background(), rules)
	if err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	if !changed {
		t.Fatal("the first rule write after an install did not add the client: qui would start with an issuer and no registration")
	}
	first := lastConfig(t, allWrites(f))
	if !strings.Contains(first, "client_id: 'seedbox-qui'") {
		t.Fatalf("the rule write added no client:\n%s", first)
	}

	applied := len(f.cluster.applied)
	changed, err = f.decision.EnsureRules(context.Background(), rules)
	if err != nil {
		t.Fatalf("second EnsureRules: %v", err)
	}
	if changed || len(f.cluster.applied) != applied {
		t.Fatalf("a second rule write rewrote an unchanged login:\n%s", lastConfig(t, allWrites(f)))
	}
	if got := lastConfig(t, allWrites(f)); got != first {
		t.Fatalf("the rendering is not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, got)
	}
}

// TestUpgradeAddsTheProviderAndKeepsTheKey: a box installed before the provider
// existed has a Secret with the encryption key alone. The next init adds the
// signing material and the client, and never rewrites the key -- rotating it
// would orphan every row in the SQLite database.
func TestUpgradeAddsTheProviderAndKeepsTheKey(t *testing.T) {
	f := newFixture(t)
	const existing = "0123456789abcdef0123456789abcdef"
	keyed(f, existing)

	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data := loginSecretData(t, allWrites(f))
	got, err := base64.StdEncoding.DecodeString(data[authelia.EncryptionKeySecretKey])
	if err != nil {
		t.Fatalf("the encryption key is not base64: %v", err)
	}
	if string(got) != existing {
		t.Fatalf("the encryption key changed from %q to %q: it is never rotated", existing, got)
	}
	if _, ok := data[authelia.OIDCFile]; !ok {
		t.Fatalf("the upgrade added no signing material: %v", keysOf(data))
	}
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = []string{"qui"}
	if _, err := f.decision.EnsureRules(context.Background(), rules); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	if !strings.Contains(lastConfig(t, allWrites(f)), "client_id: 'seedbox-qui'") {
		t.Fatal("the upgrade added no client")
	}
}

// TestClientSecretIsGeneratedOnce: the app is configured with the plaintext in
// its Secret, so a second run must read it rather than make a new one.
func TestClientSecretIsGeneratedOnce(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", []string{"qui"}); err != nil {
		t.Fatalf("EnsureClients: %v", err)
	}
	first := clientSecretOf(t, allWrites(f), "seedbox", "qui")
	digest := digestOf(t, loginSecretData(t, allWrites(f)), authelia.ClientID("seedbox", "qui"))

	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", []string{"qui"}); err != nil {
		t.Fatalf("second EnsureClients: %v", err)
	}
	if second := clientSecretOf(t, allWrites(f), "seedbox", "qui"); second != first {
		t.Fatalf("the second run replaced the client secret: %q -> %q", first, second)
	}
	if got := digestOf(t, loginSecretData(t, allWrites(f)), authelia.ClientID("seedbox", "qui")); got != digest {
		t.Fatal("the second run replaced the digest: a digest is stored, not recomputed, because its salt is random")
	}
}

// TestTurningTheAppOffRemovesTheClient: the configuration is rendered whole, so
// a digest left behind would be a client Authelia still answers for.
func TestTurningTheAppOffRemovesTheClient(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	on := ruleOptions()
	on.Namespace = "seedbox"
	on.OIDC = []string{"qui"}
	if _, err := f.decision.EnsureRules(context.Background(), on); err != nil {
		t.Fatalf("EnsureRules with the app on: %v", err)
	}
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = nil
	if _, err := f.decision.EnsureRules(context.Background(), rules); err != nil {
		t.Fatalf("EnsureRules with the app off: %v", err)
	}
	config := lastConfig(t, allWrites(f))
	if strings.Contains(config, "identity_providers") {
		t.Fatalf("the provider is still configured after the app was turned off:\n%s", config)
	}
	for k := range lastLoginSecretData(t, allWrites(f)) {
		if strings.HasPrefix(k, "oidc-client-") {
			t.Fatalf("the login's Secret still holds %s", k)
		}
	}
	// The signing material stays: it is generate-once, and a box that turns
	// its app back on must not reissue tokens clients have cached.
	if _, ok := lastLoginSecretData(t, allWrites(f))[authelia.OIDCFile]; !ok {
		t.Fatal("turning the app off removed the signing material: it is generated once and never rotated")
	}
}

// TestPendingCreatesNothing: a run that may not write the login must not create
// the provider's material either.
func TestPendingCreatesNothing(t *testing.T) {
	f := newFixture(t)
	const existing = "0123456789abcdef0123456789abcdef"
	keyed(f, existing)
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = []string{"qui"}
	if _, err := f.decision.Pending(context.Background(), rules); err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(f.cluster.applied) != 0 || len(f.cluster.created) != 0 {
		t.Fatalf("Pending wrote %d applied and %d created documents", len(f.cluster.applied), len(f.cluster.created))
	}
	for k := range f.cluster.objects {
		if strings.Contains(k, authelia.OIDCClientSecretName("qui")) {
			t.Fatalf("Pending created %s", k)
		}
	}
}

// TestOIDCClientNeedsTheNamespace: a client id is <namespace>-<app>, so a run
// with apps and no namespace is refused rather than writing a client called
// "-qui".
func TestOIDCClientNeedsTheNamespace(t *testing.T) {
	f := newFixture(t)
	opts := oidcOptions()
	opts.Namespace = ""
	if _, err := f.decision.Apply(context.Background(), opts); err == nil {
		t.Fatal("Apply rendered a client with no namespace")
	}
}

// TestPendingAgreesWithReconcile: the plan is what a run promises, and the
// write is what it does. A rendering that differs between the two makes every
// apply promise a login it then decides not to write -- the idempotent second
// run stops saying "nothing to change", which is the one thing it must say.
func TestPendingAgreesWithReconcile(t *testing.T) {
	f := newFixture(t)
	if _, err := f.decision.Apply(context.Background(), oidcOptions()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", []string{"qui"}); err != nil {
		t.Fatalf("EnsureClients: %v", err)
	}
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = []string{"qui"}
	if _, err := f.decision.Reconcile(context.Background(), rules); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	pending, err := f.decision.Pending(context.Background(), rules)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending {
		t.Fatalf("Pending after a Reconcile says the login differs: the plan promises a write the write does not make:\n%s", lastConfig(t, allWrites(f)))
	}
	before := len(f.cluster.applied)
	wrote, err := f.decision.Reconcile(context.Background(), rules)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if wrote || len(f.cluster.applied) != before {
		t.Fatalf("a second Reconcile wrote %d documents with nothing to change", len(f.cluster.applied)-before)
	}
}

// TestAnInstallPlansTheLoginItWrites: init's apply reads the cluster before the
// login is installed (it is installed after the confirm), so "no login yet" is
// work that run is about to do. Every other run keeps the refusal: nothing in
// it would install one, and a plan that promised it would be a lie.
func TestAnInstallPlansTheLoginItWrites(t *testing.T) {
	f := newFixture(t) // no login in this cluster yet
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = []string{"qui"}
	rules.Installs = true

	pending, err := f.decision.Pending(context.Background(), rules)
	if err != nil {
		t.Fatalf("Pending on the run that installs the login: %v", err)
	}
	if !pending {
		t.Fatal("the install planned no login: the page it writes after the confirm is work this run does")
	}
	if len(f.cluster.applied) != 0 {
		t.Fatalf("Pending wrote %d documents", len(f.cluster.applied))
	}

	rules.Installs = false
	if _, err := f.decision.Pending(context.Background(), rules); err == nil {
		t.Fatal("a run that does not install the login planned one that is not there")
	} else if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("the refusal is not ErrNotInstalled: %v", err)
	}
}

// TestTwoClientsGetTheirOwnSecretsAndOnePolicy is the second client: the same
// machinery, one more Secret in the seedbox's namespace and one more digest in
// the login's, with ids that name their app. The two plaintexts differ because
// a copy would let either app present the other's credential at the token
// endpoint, and both clients name the one policy this seedbox has -- a client
// pointing anywhere else is a token issued to someone the box did not name.
func TestTwoClientsGetTheirOwnSecretsAndOnePolicy(t *testing.T) {
	f := newFixture(t)
	opts := oidcOptions()
	opts.OIDC = []string{"qui", "autobrr"}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	planned, err := f.decision.PlanClients(context.Background(), "seedbox", opts.OIDC)
	if err != nil {
		t.Fatalf("PlanClients: %v", err)
	}
	if strings.Join(planned, ",") != "autobrr,qui" {
		t.Fatalf("PlanClients = %v, want both clients in catalog order", planned)
	}
	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", opts.OIDC); err != nil {
		t.Fatalf("EnsureClients: %v", err)
	}

	writes := allWrites(f)
	qui := clientSecretOf(t, writes, "seedbox", "qui")
	autobrr := clientSecretOf(t, writes, "seedbox", "autobrr")
	if len(qui) != authelia.OIDCClientSecretLength || len(autobrr) != authelia.OIDCClientSecretLength {
		t.Fatalf("the client secrets are %d and %d characters, want %d each", len(qui), len(autobrr), authelia.OIDCClientSecretLength)
	}
	if qui == autobrr {
		t.Fatal("both clients were given the same secret: either app could then sign in as the other")
	}
	login := loginSecretData(t, writes)
	quiDigest := digestOf(t, login, authelia.ClientID("seedbox", "qui"))
	autobrrDigest := digestOf(t, login, authelia.ClientID("seedbox", "autobrr"))
	if !authelia.VerifyClientSecret(quiDigest, qui) {
		t.Fatal("qui's digest does not verify against qui's plaintext")
	}
	if !authelia.VerifyClientSecret(autobrrDigest, autobrr) {
		t.Fatal("autobrr's digest does not verify against autobrr's plaintext")
	}
	if authelia.VerifyClientSecret(quiDigest, autobrr) || authelia.VerifyClientSecret(autobrrDigest, qui) {
		t.Fatal("a client's digest accepts the other client's secret")
	}
	if quiDigest == autobrrDigest {
		t.Fatal("the two digests are identical, so one client's secret is the other's")
	}

	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = opts.OIDC
	if _, err := f.decision.EnsureRules(context.Background(), rules); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	config := lastConfig(t, allWrites(f))
	for _, want := range []string{
		"      - client_id: 'seedbox-autobrr'",
		"      - client_id: 'seedbox-qui'",
		"          - 'https://box.example.com/autobrr/api/auth/oidc/callback'",
		"          - 'https://box.example.com/qui/api/auth/oidc/callback'",
		"        client_secret: '" + autobrrDigest + "'",
		"        client_secret: '" + quiDigest + "'",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("the configuration does not contain %q:\n%s", want, config)
		}
	}
	if n := strings.Count(config, "authorization_policy: 'seedbox'"); n != 2 {
		t.Errorf("%d clients name the seedbox's policy, want both of them:\n%s", n, config)
	}
	// One admitted account in the provider, not one in the file: the
	// access_control rules name the same person for the host they cover.
	provider := config[strings.Index(config, "identity_providers:"):]
	if strings.Count(provider, "user:alice") != 1 {
		t.Errorf("the provider admits more than one account:\n%s", provider)
	}
	if strings.Contains(config, qui) || strings.Contains(config, autobrr) {
		t.Error("the configuration carries a client's plaintext secret: only digests belong there")
	}
}

// TestBothClientsArePlannedOnceAndWrittenOnce is the read and write halves
// agreeing across two runs of the same inputs -- an apply after a command that
// changed nothing. Nothing is planned, nothing is written, and neither a
// secret nor a digest is regenerated: a new digest would change the login's
// ConfigMap on every apply and roll the login, which drops every session.
func TestBothClientsArePlannedOnceAndWrittenOnce(t *testing.T) {
	f := newFixture(t)
	opts := oidcOptions()
	opts.OIDC = []string{"qui", "autobrr"}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	wrote, err := f.decision.EnsureClients(context.Background(), "seedbox", opts.OIDC)
	if err != nil {
		t.Fatalf("EnsureClients: %v", err)
	}
	if !wrote {
		t.Fatal("the first run wrote nothing, so there was nothing to plan either")
	}
	planned, err := f.decision.PlanClients(context.Background(), "seedbox", opts.OIDC)
	if err != nil {
		t.Fatalf("PlanClients after the write: %v", err)
	}
	if len(planned) != 0 {
		t.Fatalf("PlanClients still wants %v after the write", planned)
	}
	login := loginSecretData(t, allWrites(f))
	loginBefore := map[string]string{}
	for k, v := range login {
		loginBefore[k] = v
	}
	quiSecret := clientSecretOf(t, allWrites(f), "seedbox", "qui")
	autobrrSecret := clientSecretOf(t, allWrites(f), "seedbox", "autobrr")

	before := len(f.cluster.applied)
	wrote, err = f.decision.EnsureClients(context.Background(), "seedbox", opts.OIDC)
	if err != nil {
		t.Fatalf("second EnsureClients: %v", err)
	}
	if wrote || len(f.cluster.applied) != before {
		t.Fatalf("a second run wrote %d documents with both clients already there", len(f.cluster.applied)-before)
	}
	if got := clientSecretOf(t, allWrites(f), "seedbox", "qui"); got != quiSecret {
		t.Error("the second run regenerated qui's secret")
	}
	if got := clientSecretOf(t, allWrites(f), "seedbox", "autobrr"); got != autobrrSecret {
		t.Error("the second run regenerated autobrr's secret")
	}
	loginAfter := loginSecretData(t, allWrites(f))
	for clientID, secret := range map[string]string{
		authelia.ClientID("seedbox", "qui"):     quiSecret,
		authelia.ClientID("seedbox", "autobrr"): autobrrSecret,
	} {
		key := authelia.OIDCClientDigestKey(clientID)
		if loginAfter[key] != loginBefore[key] {
			t.Errorf("%s's digest changed on the second run", clientID)
		}
		if !authelia.VerifyClientSecret(digestOf(t, loginAfter, clientID), secret) {
			t.Errorf("%s's digest no longer verifies against its secret", clientID)
		}
	}
}

// TestTheClientThatIsOffIsNotRegistered: an app the catalog marks but the box
// did not install is not a client -- no Secret in its namespace, no digest in
// the login's, and nothing in the provider that would issue a token for it.
func TestTheClientThatIsOffIsNotRegistered(t *testing.T) {
	f := newFixture(t)
	opts := oidcOptions()
	opts.OIDC = []string{"qui"}
	if _, err := f.decision.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := f.decision.EnsureClients(context.Background(), "seedbox", opts.OIDC); err != nil {
		t.Fatalf("EnsureClients: %v", err)
	}
	writes := allWrites(f)
	for _, doc := range writes {
		meta, _ := doc["metadata"].(map[string]any)
		if name, _ := meta["name"].(string); name == authelia.OIDCClientSecretName("autobrr") {
			t.Fatal("autobrr has a client Secret although this box did not install it")
		}
	}
	if _, ok := loginSecretData(t, writes)[authelia.OIDCClientDigestKey(authelia.ClientID("seedbox", "autobrr"))]; ok {
		t.Fatal("the login's Secret holds a digest for autobrr although this box did not install it")
	}
	rules := ruleOptions()
	rules.Namespace = "seedbox"
	rules.OIDC = opts.OIDC
	if _, err := f.decision.EnsureRules(context.Background(), rules); err != nil {
		t.Fatalf("EnsureRules: %v", err)
	}
	config := lastConfig(t, allWrites(f))
	if strings.Contains(config, "seedbox-autobrr") {
		t.Errorf("the provider registers autobrr although this box did not install it:\n%s", config)
	}
}
