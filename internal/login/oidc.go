package login

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/say"
)

// The provider's material is split in two.
//
// The signing material (an HMAC key and one RSA key) is one file in the login's
// Secret, mounted and read as a second `--config`: its plaintext exists nowhere else
// and never touches the ConfigMap.
//
// Each client's secret is the opposite: the plaintext belongs in the seedbox's own
// namespace, and the login keeps only the digest the token endpoint checks. The digest
// is stored, not recomputed, because its random salt would change the ConfigMap on
// every apply and roll the login, dropping every session.

// loginSecret is the login's Secret as this run needs it.
type loginSecret struct {
	// key is the storage encryption key, which is never rotated.
	key string
	// provider is the rendered oidc.yml file (the signing material), or nil on
	// a box whose provider was never set up.
	provider []byte
	// digests maps a client id to the digest of its secret.
	digests map[string]string
}

// ErrNotInstalled says the login is not in this cluster yet: the installing run
// treats it as work it is about to do, every other run as a refusal.
var ErrNotInstalled = errors.New("the login is not installed")

// notInstalled is the one refusal, in the one wording.
func notInstalled() error {
	return fmt.Errorf("%w (%s/%s is missing): run `box init` as root, which installs it and writes its rules", ErrNotInstalled, Namespace, authelia.SecretName)
}

// readSecret reads the login's Secret: no generation, so a run that may not write
// the login can still render the configuration it would have.
func (d *Decision) readSecret(ctx context.Context, cl Cluster) (loginSecret, error) {
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", Namespace, authelia.SecretName)
	if err != nil {
		return loginSecret{}, err
	}
	if !found {
		return loginSecret{}, notInstalled()
	}
	return parseLoginSecret(doc)
}

// parseLoginSecret is the Secret's document as the fields this package needs.
func parseLoginSecret(doc map[string]any) (loginSecret, error) {
	raw, _, _ := unstructured.NestedStringMap(doc, "data")
	enc := loginSecret{digests: map[string]string{}}
	encoded, ok := raw[authelia.EncryptionKeySecretKey]
	key, decodeErr := base64.StdEncoding.DecodeString(encoded)
	if !ok || decodeErr != nil || len(key) < 20 {
		return enc, fmt.Errorf("secret %s/%s exists but has no usable %s: box never rotates it, so restore the key from your backup, or delete the Secret and the claim together and start the login again",
			Namespace, authelia.SecretName, authelia.EncryptionKeySecretKey)
	}
	enc.key = string(key)
	if provider, ok := raw[authelia.OIDCFile]; ok {
		decoded, err := base64.StdEncoding.DecodeString(provider)
		if err != nil {
			return enc, fmt.Errorf("secret %s/%s: %s is not base64: %w", Namespace, authelia.SecretName, authelia.OIDCFile, err)
		}
		enc.provider = decoded
	}
	for name, value := range raw {
		if !strings.HasPrefix(name, "oidc-client-") {
			continue
		}
		digest, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return enc, fmt.Errorf("secret %s/%s: %s is not base64: %w", Namespace, authelia.SecretName, name, err)
		}
		enc.digests[strings.TrimPrefix(name, "oidc-client-")] = string(digest)
	}
	return enc, nil
}

// ensureKey is readSecret plus the two things only root's init may create: the
// storage encryption key (once, ever) and the provider's signing material. It writes
// nothing into the seedbox's namespace, so the clients are a separate step
// (EnsureClients).
//
// "Once" is per key: a box from before the provider has a Secret with the encryption
// key alone, and an upgrade adds keys without rewriting what is there.
func (d *Decision) ensureKey(ctx context.Context, cl Cluster, create bool, wantProvider bool) (loginSecret, bool, error) {
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", Namespace, authelia.SecretName)
	if err != nil {
		return loginSecret{}, false, err
	}
	if !found && !create {
		// A rule write is not an install: a box with no login is refused, not given
		// a Secret nobody asked for.
		return loginSecret{}, false, notInstalled()
	}
	kept := found
	var sec loginSecret
	data := map[string]string{}
	if found {
		existing, _, _ := unstructured.NestedStringMap(doc, "data")
		for k, v := range existing {
			data[k] = v
		}
		if sec, err = parseLoginSecret(doc); err != nil {
			return sec, kept, err
		}
	} else {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return sec, kept, fmt.Errorf("generate the storage encryption key: %w", err)
		}
		sec.key = hex.EncodeToString(buf)
		sec.digests = map[string]string{}
		data[authelia.EncryptionKeySecretKey] = base64.StdEncoding.EncodeToString([]byte(sec.key))
	}
	changed := !found
	if wantProvider && len(sec.provider) == 0 {
		provider, err := authelia.GenerateOIDCSecrets()
		if err != nil {
			return sec, kept, err
		}
		sec.provider = provider
		data[authelia.OIDCFile] = base64.StdEncoding.EncodeToString(provider)
		changed = true
	}
	if !changed {
		return sec, kept, nil
	}
	if err := d.writeLoginSecret(ctx, cl, found, data); err != nil {
		return sec, kept, err
	}
	return sec, kept, nil
}

// EnsureClients is the per-app half of the provider, at the one point where the
// seedbox's namespace exists: after apply created it and before Helm runs. It writes
// one Secret per client into that namespace, keeps only the digest in the login's, and
// is idempotent.
//
// A failure stops the run here: an app whose client secret is missing cannot start,
// and failing before the release says so once instead of crash-looping through Helm.
func (d *Decision) EnsureClients(ctx context.Context, namespace string, apps []string) (bool, error) {
	apps = oidcApps(apps)
	// An empty list is not "nothing to do": a box whose app was turned off still has
	// that client's digest, and the configuration is rendered whole. The prune below
	// removes it.
	if len(apps) == 0 {
		return d.pruneClients(ctx)
	}
	if strings.TrimSpace(namespace) == "" {
		return false, errors.New("login: an OIDC client's id is <namespace>-<app>, so a box with one needs its namespace")
	}
	cl, err := d.Open()
	if err != nil {
		return false, err
	}
	if err := kube.Preflight(ctx, cl, oidcAccess(namespace)); err != nil {
		return false, fmt.Errorf("login: %w", err)
	}
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", Namespace, authelia.SecretName)
	if err != nil {
		return false, err
	}
	if !found {
		return false, notInstalled()
	}
	sec, err := parseLoginSecret(doc)
	if err != nil {
		return false, err
	}
	data := map[string]string{}
	existing, _, _ := unstructured.NestedStringMap(doc, "data")
	for k, v := range existing {
		data[k] = v
	}
	if _, err := d.ensureClientSecrets(ctx, cl, namespace, apps, sec, data); err != nil {
		return false, err
	}
	// A client whose app is off loses its registration: a digest left behind would
	// be a client Authelia still answers for.
	want := map[string]bool{}
	for _, app := range apps {
		want[authelia.OIDCClientDigestKey(authelia.ClientID(namespace, app))] = true
	}
	for name := range data {
		if strings.HasPrefix(name, "oidc-client-") && !want[name] {
			delete(data, name)
			delete(sec.digests, strings.TrimPrefix(name, "oidc-client-"))
		}
	}
	if sameData(data, existing) {
		return false, nil
	}
	if err := d.writeLoginSecret(ctx, cl, true, data); err != nil {
		return false, err
	}
	return true, nil
}

// pruneClients drops every client digest from the login's Secret: the app list
// is empty, so there is no client this box still answers for.
func (d *Decision) pruneClients(ctx context.Context) (bool, error) {
	cl, err := d.Open()
	if err != nil {
		return false, err
	}
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", Namespace, authelia.SecretName)
	if err != nil || !found {
		return false, err
	}
	existing, _, _ := unstructured.NestedStringMap(doc, "data")
	data := map[string]string{}
	found_client := false
	for k, v := range existing {
		if strings.HasPrefix(k, "oidc-client-") {
			found_client = true
			continue
		}
		data[k] = v
	}
	if !found_client {
		return false, nil
	}
	if err := d.writeLoginSecret(ctx, cl, true, data); err != nil {
		return false, err
	}
	return true, nil
}

// PlanClients is EnsureClients' read half: the apps whose Secret or digest is
// missing. A namespace that is not there yet counts as every app, because the write
// creates it after the namespace does.
func (d *Decision) PlanClients(ctx context.Context, namespace string, apps []string) ([]string, error) {
	apps = oidcApps(apps)
	if len(apps) == 0 || strings.TrimSpace(namespace) == "" {
		return nil, nil
	}
	cl, err := d.Open()
	if err != nil {
		return nil, err
	}
	there, err := cl.NamespaceExists(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if !there {
		return apps, nil
	}
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", Namespace, authelia.SecretName)
	if err != nil {
		return nil, err
	}
	if !found {
		return apps, nil
	}
	sec, err := parseLoginSecret(doc)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, app := range apps {
		if sec.digests[authelia.ClientID(namespace, app)] == "" {
			out = append(out, app)
			continue
		}
		secret, err := d.clientSecret(ctx, cl, namespace, authelia.OIDCClientSecretName(app))
		if err != nil {
			return nil, err
		}
		if secret == "" {
			out = append(out, app)
		}
	}
	return out, nil
}

// writeLoginSecret applies the Secret whole: server-side apply would drop the keys
// this run did not name.
func (d *Decision) writeLoginSecret(ctx context.Context, cl Cluster, found bool, data map[string]string) error {
	secret := secretDocument(data)
	if found {
		if err := cl.ApplyWith(ctx, kube.FieldManager, []map[string]any{secret}); err != nil {
			return fmt.Errorf("update Secret %s/%s: %w", Namespace, authelia.SecretName, err)
		}
		d.report().Detail(fmt.Sprintf("updated Secret %s/%s: the provider's key and one digest per client, added and never changed", Namespace, authelia.SecretName))
		return nil
	}
	if err := cl.Create(ctx, secret); err != nil {
		return fmt.Errorf("create Secret %s/%s: %w", Namespace, authelia.SecretName, err)
	}
	// The one fact a person has to act on: the key exists nowhere else and never
	// changes.
	d.report().Rows([]say.Row{{
		Mark:  say.MarkNeeds,
		Name:  "login key",
		Facts: []string{"back it up with the claim, in " + Namespace + "/" + authelia.SecretName},
	}})
	return nil
}

// sameData reports whether two Secret data maps say the same thing.
func sameData(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// ensureClientSecrets makes one Secret per OIDC app and records its digest in the
// login's Secret. A Secret that is there is never regenerated: the app is configured
// with that value, so replacing it would break the login it is for.
func (d *Decision) ensureClientSecrets(ctx context.Context, cl Cluster, namespace string, apps []string, sec loginSecret, data map[string]string) (bool, error) {
	// On a first install this runs before apply created the namespace, and a Secret
	// cannot be written into one that is not there; this guard keeps an earlier
	// caller from trying.
	there, err := cl.NamespaceExists(ctx, namespace)
	if err != nil {
		return false, err
	}
	if !there {
		return false, nil
	}
	changed := false
	for _, app := range apps {
		clientID := authelia.ClientID(namespace, app)
		digestKey := authelia.OIDCClientDigestKey(clientID)
		secretName := authelia.OIDCClientSecretName(app)

		secret, err := d.clientSecret(ctx, cl, namespace, secretName)
		if err != nil {
			return changed, err
		}
		if secret == "" {
			secret, err = authelia.GenerateOIDCClientSecret()
			if err != nil {
				return changed, err
			}
			if err := cl.ApplyWith(ctx, kube.FieldManager, []map[string]any{clientSecretDocument(namespace, secretName, secret)}); err != nil {
				return changed, fmt.Errorf("write Secret %s/%s: %w", namespace, secretName, err)
			}
			d.report().Detail(fmt.Sprintf("created Secret %s/%s: %s reads it as its client secret", namespace, secretName, app))
			changed = true
		}
		if sec.digests[clientID] == "" {
			// The plaintext may predate the provider (an upgrade) or postdate it (a
			// hand rotation): the digest follows what the app is configured with.
			digest, err := authelia.HashClientSecret(secret)
			if err != nil {
				return changed, err
			}
			data[digestKey] = base64.StdEncoding.EncodeToString([]byte(digest))
			sec.digests[clientID] = digest
			changed = true
		} else if !authelia.VerifyClientSecret(sec.digests[clientID], secret) {
			// The Secret and the digest disagree, so the plaintext was replaced by
			// hand. Re-hash rather than refuse: a login that will not start helps
			// nobody.
			digest, err := authelia.HashClientSecret(secret)
			if err != nil {
				return changed, err
			}
			data[digestKey] = base64.StdEncoding.EncodeToString([]byte(digest))
			sec.digests[clientID] = digest
			changed = true
			d.report().Line(fmt.Sprintf("Secret %s/%s no longer matches the login's digest for %s: re-hashed it", namespace, secretName, clientID))
		}
	}
	return changed, nil
}

// clientSecret reads one app's client secret; an absent Secret is "".
func (d *Decision) clientSecret(ctx context.Context, cl Cluster, namespace, name string) (string, error) {
	doc, found, err := cl.Get(ctx, CoreGroup, "v1", "Secret", namespace, name)
	if err != nil || !found {
		return "", err
	}
	raw, _, _ := unstructured.NestedStringMap(doc, "data")
	encoded, ok := raw[authelia.OIDCClientSecretKey]
	if !ok {
		return "", nil
	}
	secret, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("secret %s/%s: %s is not base64: %w", namespace, name, authelia.OIDCClientSecretKey, err)
	}
	return string(secret), nil
}

// providerEnabled is the one predicate for "this login serves an identity provider":
// the rendering names at least one client, so the signing material is mounted and
// passed as a second --config. The block and the mount both read it, because Authelia
// refuses a block with no clients: either half alone is a login that does not start.
//
// It is deliberately not "the Secret holds a digest": a box whose last OIDC app was
// turned off keeps its signing material, so a client that comes back does not reissue
// cached tokens, and has no provider until an app signs in again.
func providerEnabled(clients *authelia.OIDC) bool {
	return clients != nil && clients.Enabled()
}

// oidcClients is the provider's client list for one seedbox: one client per enabled
// app with a digest, pointing at that seedbox's policy. An app with no digest is left
// out, so the configuration never names a client Authelia cannot check; the missing
// client is planned and written where that is known.
//
// The return is the whole provider decision: nil means no identity_providers block and
// nothing mounted. Callers render both from it so they cannot disagree.
func oidcClients(host, namespace, loginName string, apps []string, digests map[string]string) *authelia.OIDC {
	if len(apps) == 0 {
		return nil
	}
	o := &authelia.OIDC{
		Policies: []authelia.Policy{{Name: authelia.OIDCPolicyName(namespace), Login: loginName}},
	}
	for _, app := range apps {
		clientID := authelia.ClientID(namespace, app)
		digest := digests[clientID]
		if digest == "" {
			continue
		}
		// An OIDC app is served under its own path (qui's baseUrl), so its
		// callback is under that prefix.
		base := "/" + app
		o.Clients = append(o.Clients, authelia.Client{
			ID:           clientID,
			Name:         app,
			Secret:       digest,
			RedirectURIs: []string{authelia.RedirectURI(host, base)},
			Policy:       authelia.OIDCPolicyName(namespace),
		})
	}
	if len(o.Clients) == 0 {
		return nil
	}
	return o
}

// The fields box's object documents are built from: the login's Secret and the
// per-app ones are the same shape, and a misspelt key drops it silently.
const (
	fieldAPIVersion = "apiVersion"
	fieldKind       = "kind"
	fieldMetadata   = "metadata"
	fieldName       = "name"
	fieldNamespace  = "namespace"
	fieldLabels     = "labels"
	fieldData       = "data"
)

// secretDocument is the login's Secret as box writes it: the whole data map, because
// server-side apply would otherwise drop the keys this run did not name.
func secretDocument(data map[string]string) map[string]any {
	encoded := make(map[string]any, len(data))
	for k, v := range data {
		encoded[k] = v
	}
	return map[string]any{
		fieldAPIVersion: "v1",
		fieldKind:       "Secret",
		fieldMetadata: map[string]any{
			fieldName:      authelia.SecretName,
			fieldNamespace: Namespace,
			fieldLabels: map[string]any{
				kube.PartOfLabel:    kube.PartOf,
				kube.ManagedByLabel: kube.ManagedBy,
			},
		},
		"type":    "Opaque",
		fieldData: encoded,
	}
}

// clientSecretDocument is one app's Secret in its own namespace.
func clientSecretDocument(namespace, name, secret string) map[string]any {
	return map[string]any{
		fieldAPIVersion: "v1",
		fieldKind:       "Secret",
		fieldMetadata: map[string]any{
			fieldName:      name,
			fieldNamespace: namespace,
			fieldLabels: map[string]any{
				kube.PartOfLabel:    kube.PartOf,
				kube.ManagedByLabel: kube.ManagedBy,
			},
		},
		"type": "Opaque",
		fieldData: map[string]any{
			authelia.OIDCClientSecretKey: base64.StdEncoding.EncodeToString([]byte(secret)),
		},
	}
}

// oidcAccess is what creating the per-app Secrets needs, checked before the
// first write like every other permission this package asks for.
func oidcAccess(namespace string) []kube.Access {
	if strings.TrimSpace(namespace) == "" {
		return nil
	}
	return []kube.Access{
		{Verb: kube.VerbApply, Group: CoreGroup, Resource: "secrets", Namespace: namespace, What: "the OIDC client Secret in " + namespace},
	}
}

// oidcApps is the app list as sorted, de-duplicated names: the rendering has to be
// identical for the same inputs whatever order the catalog was read in.
func oidcApps(apps []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(apps))
	for _, app := range apps {
		app = strings.TrimSpace(app)
		if app == "" || seen[app] {
			continue
		}
		seen[app] = true
		out = append(out, app)
	}
	sort.Strings(out)
	return out
}

// ErrOIDCNotInstalled says a run was asked to render a provider but the login has no
// material for it yet.
var ErrOIDCNotInstalled = errors.New("the login has no OIDC client yet")
