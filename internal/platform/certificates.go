package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/say"
)

// CertificateTimeout is how long a host's Certificate gets to become Ready
// before box leaves HTTP alone and says so. The wait is never fatal: a
// certificate that cannot be issued yet is DNS or an ACME problem, and
// docs/box.md is explicit that a failed issuance or renewal does not block
// apply.
const CertificateTimeout = 3 * time.Minute

// CertificateCluster is the cluster client the per-host TLS flow needs beyond
// the platform writes: a Certificate's status, whether cert-manager is there,
// and a write under its own field manager.
type CertificateCluster interface {
	Cluster
	// CertificateReady answers the Certificate's Ready condition, with
	// cert-manager's reason when it is not.
	CertificateReady(ctx context.Context, namespace, name string) (bool, string, error)
	// Serves reports whether the cluster has the Certificate CRD. A box whose
	// platform never installed cert-manager can still have its files written;
	// it just has no certificate yet.
	Serves(ctx context.Context, group, version, kind string) (bool, error)
	// ApplyWith server-side-applies the documents under manager.
	ApplyWith(ctx context.Context, manager string, docs []map[string]any) error
}

// CertificateDecision is root's TLS half for one host name: a cert-manager
// Certificate in s5n-gateway, and -- once the Certificate is Ready -- that
// host's HTTPS listener and the port-80 redirect that leaves the ACME
// challenge route alone. The host's own port-80 listener and the Allow that
// keeps the login off it are written before the Certificate, because the
// HTTP-01 challenge cannot answer through a listener that asks for a login.
// Nothing is served for the host until the Certificate is Ready: a not-Ready
// run leaves the port-80 listener answering the challenge and no HTTPS
// listener at all.
type CertificateDecision struct {
	Host string
	// Issuer is init's tls.issuer value ("letsencrypt" or "selfsigned"), or
	// the ClusterIssuer name itself; box maps the first to the second.
	Issuer string
	// Open builds the cluster client; it is only called when there is a host
	// to write. The return type includes the reads and the manager-scoped
	// write the flow needs, so a client that cannot do them does not compile.
	Open func() (CertificateCluster, error)
	Say  say.Reporter
	// Timeout bounds the wait for the Certificate. Zero means
	// CertificateTimeout.
	Timeout time.Duration
}

// CertificateResult is what happened, for the caller and for tests.
type CertificateResult struct {
	Host   string
	Issuer string
	Name   string
	Ready  bool
	Reason string
	// Listener and Policy are the writes that precede the Certificate: this
	// host's port-80 listener and the Allow that keeps the login off it.
	// They stay when the Certificate is not Ready, because the challenge
	// still has to answer.
	Listener bool
	Policy   bool
	// HTTPS is this host's TLS terminator, and Redirect the port-80 route
	// to it. Both are written only after the Certificate is Ready; a host
	// whose certificate never issues is never served over HTTPS.
	HTTPS    bool
	Redirect bool
}

func (d *CertificateDecision) report() say.Reporter { return say.Or(d.Say, nil) }

// Apply writes one host's port-80 listener and its Allow, then the
// Certificate, and -- once the Certificate is Ready -- the HTTPS listener and
// the redirect.
func (d *CertificateDecision) Apply(ctx context.Context) (CertificateResult, error) {
	res := CertificateResult{Host: d.Host}
	if err := d.validate(); err != nil {
		return res, err
	}
	issuer, err := clusterIssuerName(d.Issuer)
	if err != nil {
		return res, err
	}
	res.Issuer = issuer
	res.Name = CertificateName(d.Host)

	r := d.report()
	cl, err := d.Open()
	if err != nil {
		return res, err
	}
	serves, err := cl.Serves(ctx, "cert-manager.io", "v1", "Certificate")
	if err != nil {
		return res, fmt.Errorf("TLS: %w", err)
	}
	if !serves {
		r.Warn(fmt.Sprintf("cert-manager is not installed in this cluster: %s has no certificate yet; run `box bootstrap`", d.Host))
		return res, nil
	}
	// The gate before the first write: the objects below are root's, and a
	// kubeconfig without them must be refused before the Certificate exists.
	if err := preflight(ctx, cl, certificateAccess()); err != nil {
		return res, err
	}

	// Port 80 first, without a login: the HTTP-01 challenge is a request to
	// this listener, and Authelia refuses any request whose forwarded scheme
	// is http before it reads a rule. Ordering the Certificate first (as box
	// once did) left the challenge on the shared, login-guarded listener and
	// no certificate could ever issue.
	if err := d.attachHTTP(ctx, cl); err != nil {
		return res, err
	}
	res.Listener = true
	// The policy names the listener by sectionName, so the listener has to
	// exist first; and it has to exist before the challenge, or the login
	// holds the challenge's own request.
	if err := d.listenerPolicy(ctx, cl); err != nil {
		return res, err
	}
	res.Policy = true

	step := r.Start(say.Op{Label: "applying Certificate " + GatewayNamespace + "/" + res.Name})
	docs, err := ParseManifests(certificateManifest(d.Host, issuer, res.Name))
	if err != nil {
		step.Failed(err)
		return res, fmt.Errorf("certificate for %s: %w", d.Host, err)
	}
	if err := cl.Apply(ctx, docs); err != nil {
		step.Failed(err)
		return res, fmt.Errorf("certificate for %s: %w", d.Host, err)
	}
	step.Done(fmt.Sprintf("applied Certificate %s/%s (issuer %s)", GatewayNamespace, res.Name, issuer))

	ready, reason, err := d.awaitCertificate(ctx, cl, res.Name)
	res.Ready, res.Reason = ready, reason
	if err != nil {
		return res, err
	}
	if !ready {
		// Port 80 stays as it is -- the listener and its Allow are what let
		// cert-manager retry -- but the host is not served: there is no
		// HTTPS listener to serve it and no redirect to send anyone there.
		r.Warn(fmt.Sprintf("certificate %s/%s is not Ready (%s): %s is not served until it is; only the ACME challenge answers on port 80; box retries on the next run", GatewayNamespace, res.Name, reason, d.Host))
		return res, nil
	}
	r.Success(fmt.Sprintf("certificate %s/%s is Ready", GatewayNamespace, res.Name))

	if err := d.attachHTTPS(ctx, cl, res.Name); err != nil {
		return res, err
	}
	res.HTTPS = true
	if err := d.redirect(ctx, cl, res.Name); err != nil {
		return res, err
	}
	res.Redirect = true
	return res, nil
}

func (d *CertificateDecision) validate() error {
	host := strings.TrimSpace(d.Host)
	if host == "" {
		return errors.New("no host name: a certificate is per host name")
	}
	if host != d.Host || strings.ContainsAny(host, " /\\:") {
		return fmt.Errorf("host name %q is not a name box can put on a certificate", d.Host)
	}
	if _, err := clusterIssuerName(d.Issuer); err != nil {
		return err
	}
	return nil
}

// clusterIssuerName maps init's tls.issuer value to the ClusterIssuer root
// installs. The two vocabularies are the file's ("letsencrypt", "selfsigned",
// pinned by the chart schema) and the cluster's (the object names); each
// constant is one name of the same thing.
func clusterIssuerName(issuer string) (string, error) {
	switch issuer {
	case LetsEncryptIssuer, "letsencrypt":
		return LetsEncryptIssuer, nil
	case SelfSignedIssuer, "selfsigned":
		return SelfSignedIssuer, nil
	case "":
		return "", errors.New("no issuer: init decides between Let's Encrypt and self-signed from the domain")
	default:
		return "", fmt.Errorf("unknown issuer %q: box installs %s and %s only", issuer, LetsEncryptIssuer, SelfSignedIssuer)
	}
}

// awaitCertificate is the platform's existing wait, with the Certificate's own
// shorter bound. A not-Ready answer at the deadline is not an error: it is the
// state box reports and leaves for the next run, and cert-manager's reason is
// read once for the operator.
func (d *CertificateDecision) awaitCertificate(ctx context.Context, cl CertificateCluster, name string) (bool, string, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = CertificateTimeout
	}
	r := d.report()
	wait := r.Start(say.Op{
		Label:  "waiting for Certificate " + GatewayNamespace + "/" + name + " to be Ready",
		Plain:  fmt.Sprintf("waiting up to %s for Certificate %s/%s to be Ready", timeout, GatewayNamespace, name),
		Budget: timeout,
	})
	err := cl.Await(ctx, Wait{Kind: WaitCertificate, Namespace: GatewayNamespace, Name: name, Timeout: timeout})
	if err == nil {
		wait.Done(fmt.Sprintf("Certificate %s/%s: Ready", GatewayNamespace, name))
		return true, "", nil
	}
	if errors.Is(err, ErrNotReady) {
		wait.Done("")
		_, reason, readErr := cl.CertificateReady(ctx, GatewayNamespace, name)
		if readErr != nil || reason == "" {
			reason = "cert-manager has not marked it Ready"
		}
		return false, reason, nil
	}
	wait.Failed(err)
	return false, "", fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, name, err)
}

// attachHTTP adds this host's port-80 listener, and only that listener.
//
// It runs before the Certificate because the HTTP-01 challenge is served by a
// route on this listener: the solver's HTTPRoute names the Gateway with no
// sectionName, so it attaches to every listener that admits s5n-gateway -- and
// this one is the only one a challenge request for this Host reaches, because
// Gateway API listener isolation sends a request whose Host matches the
// hostname to the most specific listener. The listener admits only the
// Gateway's own namespace (root's redirect and the solver's route live there),
// so no app route can attach to it; every other host stays on the platform's
// generic http listener, which box never touches.
//
// The listener is written under the host's own field manager, so the
// platform's full Gateway apply cannot remove it and a second host's TLS apply
// cannot either.
func (d *CertificateDecision) attachHTTP(ctx context.Context, cl CertificateCluster) error {
	r := d.report()
	host := d.Host
	step := r.Start(say.Op{Label: "applying the HTTP listener for " + host})
	if err := cl.ApplyWith(ctx, httpListenerManager(host), []map[string]any{
		gatewayPartial(map[string]any{"listeners": []any{httpHostListener(host)}}),
	}); err != nil {
		step.Failed(err)
		return fmt.Errorf("HTTP listener for %s: %w", host, err)
	}
	step.Done(fmt.Sprintf("applied listener %s on %s", HTTPListenerName(host), host))
	return nil
}

// attachHTTPS adds this host's TLS terminator, once its certificate is Ready.
//
// The two listeners are written under two managers, one per listener
// (httpListenerManager, httpsListenerManager), rather than one manager per
// host carrying both. Server-side apply removes any field a manager owned and
// left out of its next apply, so a single manager would force every write to
// name both listeners: the port-80 apply (which has to happen while the
// certificate is still being issued, when there is no HTTPS listener to name)
// would delete the HTTPS listener on every re-run of an established box, and
// the HTTPS apply would have to keep re-stating a listener it never changes.
// One manager per listener keeps each apply minimal and complete for the
// manager that makes it, and no apply can remove a listener it does not own --
// including another host's.
func (d *CertificateDecision) attachHTTPS(ctx context.Context, cl CertificateCluster, name string) error {
	r := d.report()
	host := d.Host
	step := r.Start(say.Op{Label: "applying the HTTPS listener for " + host})
	if err := cl.ApplyWith(ctx, httpsListenerManager(host), []map[string]any{
		gatewayPartial(map[string]any{"listeners": []any{httpsListener(host, name)}}),
	}); err != nil {
		step.Failed(err)
		return fmt.Errorf("HTTPS listener for %s: %w", host, err)
	}
	step.Done(fmt.Sprintf("applied listener %s on %s", ListenerName(host), host))
	return nil
}

// listenerPolicy takes the login off this host's port-80 listener.
//
// The SecurityPolicy root puts on the Gateway covers every listener unless
// that listener has one of its own, and Envoy Gateway applies a listener-scoped
// policy instead of the Gateway-scoped one (internal/gatewayapi/securitypolicy.go,
// v1.9.3: listener policies are translated first, and the Gateway's is skipped
// where a specific level policy is already set). Without this one, a plain-HTTP
// app URL on this listener never reaches the router: ext_authz runs first, and
// Authelia refuses an object URL whose scheme is http with a 400. So the 301
// never happens, and an ACME renewal is a request to a login that is not there
// yet.
//
// An explicit Allow and nothing else is the right answer because no app route
// can attach to this listener: allowedRoutes admits only the Gateway's own
// namespace, where the redirect and the solver's route live. There is nothing
// to protect on port 80 -- every app is on its https-<host> listener -- and
// the shared generic http listener keeps the login, so a host without a
// certificate is still closed.
//
// It is written before the Certificate, not after it: the HTTP-01 challenge
// arrives on this listener while the certificate is still being issued, and a
// login there would refuse the challenge's own request.
func (d *CertificateDecision) listenerPolicy(ctx context.Context, cl CertificateCluster) error {
	docs, err := ParseManifests(listenerPolicyManifest(d.Host))
	if err != nil {
		return fmt.Errorf("login-free listener policy for %s: %w", d.Host, err)
	}
	r := d.report()
	step := r.Start(say.Op{Label: "applying the port-80 Allow policy for " + d.Host})
	if err := cl.ApplyWith(ctx, httpListenerManager(d.Host), docs); err != nil {
		step.Failed(err)
		return fmt.Errorf("login-free listener policy for %s: %w", d.Host, err)
	}
	step.Done(fmt.Sprintf("applied SecurityPolicy %s/%s: no login on %s", GatewayNamespace, ListenerPolicyName(d.Host), HTTPListenerName(d.Host)))
	return nil
}

// redirect writes the port-80 route. The solver's own route is an exact path
// match, which wins over this route's prefix, so a renewal still works with
// the redirect in place.
func (d *CertificateDecision) redirect(ctx context.Context, cl Cluster, name string) error {
	docs, err := ParseManifests(redirectManifest(d.Host, name))
	if err != nil {
		return fmt.Errorf("redirect for %s: %w", d.Host, err)
	}
	r := d.report()
	step := r.Start(say.Op{Label: "applying the HTTP->HTTPS redirect for " + d.Host})
	if err := cl.Apply(ctx, docs); err != nil {
		step.Failed(err)
		return fmt.Errorf("redirect for %s: %w", d.Host, err)
	}
	step.Done(fmt.Sprintf("applied HTTP->HTTPS redirect for %s (/.well-known/acme-challenge/ stays with the solver)", d.Host))
	return nil
}

// certificateAccess is the RBAC the per-host write needs, checked before the
// first object is written. All three are applies: on every run after the first,
// this host's Certificate and HTTPRoute exist, so the write the API server
// authorizes is a patch, and the Gateway's listeners are patched by a partial
// apply under the host's own field manager.
func certificateAccess() []Access {
	return []Access{
		{Verb: kube.VerbApply, Group: "cert-manager.io", Resource: "certificates", Namespace: GatewayNamespace, What: "cert-manager Certificates in " + GatewayNamespace},
		{Verb: kube.VerbApply, Group: "gateway.networking.k8s.io", Resource: "httproutes", Namespace: GatewayNamespace, What: "HTTPRoutes in " + GatewayNamespace},
		{Verb: kube.VerbApply, Group: "gateway.networking.k8s.io", Resource: "gateways", Namespace: GatewayNamespace, What: "gateways"},
		{Verb: kube.VerbApply, Group: "gateway.envoyproxy.io", Resource: "securitypolicies", Namespace: GatewayNamespace, What: "SecurityPolicies in " + GatewayNamespace},
	}
}

// CertificateName is the Certificate and Secret name for a host: a DNS name
// flattened into a DNS label. Stable across runs, which is what keeps
// re-applying it a no-op.
func CertificateName(host string) string { return "s5n-tls-" + dnsLabel(host) }

// ListenerName is the Gateway listener name for a host's HTTPS terminator.
func ListenerName(host string) string { return "https-" + dnsLabel(host) }

// HTTPListenerName is the Gateway listener name for a host's port-80 traffic.
// It is the sectionName the redirect attaches to.
func HTTPListenerName(host string) string { return "http-" + dnsLabel(host) }

// ListenerPolicyName is the SecurityPolicy that keeps the login off one host's
// port-80 listener. It is named the way the host's Certificate and listeners
// are, so `kubectl -n s5n-gateway get securitypolicy` reads as the one login
// (s5n-authelia) plus one absence per host that has a certificate.
func ListenerPolicyName(host string) string { return "s5n-no-login-" + dnsLabel(host) }

// httpListenerManager is the field manager that owns this host's port-80
// listener (and the Allow that goes with it). One manager per listener, so an
// apply can never remove the other listener by leaving it out: see attachHTTPS.
func httpListenerManager(host string) string {
	return kube.TLSManagerPrefix + dnsLabel(host) + "-http"
}

// httpsListenerManager is the field manager that owns this host's TLS
// terminator.
func httpsListenerManager(host string) string {
	return kube.TLSManagerPrefix + dnsLabel(host) + "-https"
}

// maxDNSLabel is the longest host label box puts into a name. The objects are
// DNS subdomains (253) and listener names section names (253); the tightest
// fit is a field manager (128), which adds `box-tls-` and `-https` around it.
const maxDNSLabel = 100

// hostHashLen is how many hex characters of a host's digest a lossy label
// carries. Four bytes make two hosts colliding on the readable prefix
// vanishingly unlikely, and the object names stay readable.
const hostHashLen = 8

// plainHost matches the hosts whose flattening is reversible: lowercase
// alphanumeric labels joined by single dots. A hyphen is excluded on purpose,
// because a hyphen and a dot flatten to the same character.
var plainHost = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*$`)

// dnsLabel flattens a host name into characters Kubernetes object names and
// field managers accept, short enough for both.
//
// Flattening alone is lossy: "a-b.example.com" and "a.b.example.com" both
// become "a-b-example-com", "*" and "_" collapse into hyphens, and a host
// longer than the limit loses its tail. Two hosts that flatten to one label
// would share a Certificate, a listener, a SecurityPolicy and a field
// manager, so whichever applied last would silently own the other's objects.
//
// A host whose flattening is unambiguous -- only alphanumeric labels joined by
// dots, and short enough -- keeps that readable name, so box.example.test is
// still box-example-test. Every other host gets the same readable prefix, cut
// to leave room, plus "-" and eight hex characters of the sha256 of the
// lowercased host. The hash is over the lowercased form so case variants of
// one host keep one name.
func dnsLabel(host string) string {
	lower := strings.ToLower(host)
	out := flattenHost(lower)
	if plainHost.MatchString(lower) && len(out) <= maxDNSLabel {
		return out
	}
	sum := sha256.Sum256([]byte(lower))
	suffix := "-" + hex.EncodeToString(sum[:])[:hostHashLen]
	if room := maxDNSLabel - len(suffix); len(out) > room {
		out = strings.TrimRight(out[:room], "-")
	}
	// A host with nothing readable in it ("*") leaves only the suffix, and a
	// name must not start with a hyphen.
	return strings.TrimLeft(out+suffix, "-")
}

// flattenHost is the old, lossy naming: every byte outside a DNS label
// becomes a hyphen, runs collapse and dashes trim. It does not cut to the
// limit: dnsLabel needs to know whether the readable form fits before it
// decides, and only the branch that keeps the name may truncate it.
func flattenHost(lower string) string {
	var b strings.Builder
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

// certificateManifest is one Certificate in the Gateway's namespace, where its
// Secret has to live for a listener in that namespace to reference it without
// a ReferenceGrant.
func certificateManifest(host, issuer, name string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    %s: %s
spec:
  secretName: %s
  dnsNames:
    - %s
  issuerRef:
    name: %s
    kind: ClusterIssuer
`, name, GatewayNamespace, kube.PartOfLabel, kube.PartOf, kube.ManagedByLabel, kube.ManagedBy, name, host, issuer))
}

// redirectManifest is the port-80 route for one host. It attaches to that
// host's own http listener (listener isolation keeps it off every other
// host's port 80), and only answers requests the ACME solver's exact-path
// route did not claim.
func redirectManifest(host, name string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: %s-redirect
  namespace: %s
  labels:
    %s: %s
    %s: %s
spec:
  parentRefs:
    - name: %s
      namespace: %s
      sectionName: %s
  hostnames:
    - %s
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      filters:
        - type: RequestRedirect
          requestRedirect:
            scheme: https
            statusCode: 301
`, name, GatewayNamespace, kube.PartOfLabel, kube.PartOf, kube.ManagedByLabel, kube.ManagedBy, GatewayName, GatewayNamespace, HTTPListenerName(host), host))
}

// listenerPolicyManifest is the Allow one host's port-80 listener needs. It
// targets the listener by sectionName, so it is scoped to that one listener
// and cannot be read as an exemption for the host's HTTPS listener or for the
// shared one; authorization carries the default action only, because an Allow
// with no extAuth is the whole point.
func listenerPolicyManifest(host string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    %s: %s
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: %s
      sectionName: %s
  authorization:
    defaultAction: Allow
`, ListenerPolicyName(host), GatewayNamespace, kube.PartOfLabel, kube.PartOf, kube.ManagedByLabel, kube.ManagedBy, GatewayName, HTTPListenerName(host)))
}

// gatewayPartial is the smallest Gateway object a server-side apply can carry
// one listener in: identity plus the list item. The listeners list is a map
// keyed by name, so everything this object does not set is left to whichever
// manager owns it.
func gatewayPartial(spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata": map[string]any{
			"name":      GatewayName,
			"namespace": GatewayNamespace,
		},
		"spec": spec,
	}
}

// httpsListener is the host's TLS terminator. Its allowedRoutes admits every
// namespace: the chart's app routes live in the seedbox's namespace, and
// adding TLS narrows nothing.
func httpsListener(host, secret string) map[string]any {
	return map[string]any{
		"name":     ListenerName(host),
		"hostname": host,
		"port":     int64(443),
		"protocol": "HTTPS",
		"tls": map[string]any{
			"mode": "Terminate",
			"certificateRefs": []any{
				map[string]any{"group": "", "kind": "Secret", "name": secret},
			},
		},
		"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "All"}},
	}
}

// httpHostListener is this host's port-80 listener: the same port as the
// platform's generic http listener, but matched by hostname, so listener
// isolation gives this host's requests the routes attached here and leaves
// every other host on the generic listener. It admits only the Gateway's own
// namespace, which is where root's redirect and cert-manager's solver route
// live, so an app route can never attach to it.
func httpHostListener(host string) map[string]any {
	return map[string]any{
		"name":     HTTPListenerName(host),
		"hostname": host,
		"port":     int64(80),
		"protocol": "HTTP",
		"allowedRoutes": map[string]any{
			"namespaces": map[string]any{
				"from": "Selector",
				"selector": map[string]any{
					"matchLabels": map[string]any{"kubernetes.io/metadata.name": GatewayNamespace},
				},
			},
		},
	}
}

// interface checks: the only client cmd/box builds must satisfy the per-host
// flow statically, so a missing read or write is a compile error and not a
// failed bootstrap after a four-minute cluster.
var _ CertificateCluster = (*Kubernetes)(nil)
