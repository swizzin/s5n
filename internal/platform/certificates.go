package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/say"
)

// CertificateTimeout is how long a host's Certificate gets to become Ready
// before box leaves HTTP alone and says so. The wait is never fatal: a
// certificate that cannot be issued yet is DNS or an ACME problem, and
// docs/box.md is explicit that a failed issuance or renewal does not block
// apply.
const CertificateTimeout = 3 * time.Minute

// tlsManagerPrefix is the field manager prefix the per-host TLS flow applies
// under, besides box's own. A Gateway's listeners are a map keyed by name, so a
// partial server-side apply of one host's listeners merges them and leaves the
// platform's full Gateway apply (manager box) unable to remove them -- and one
// host's manager cannot remove another host's listeners.
const tlsManagerPrefix = "box-tls-"

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
// host's HTTPS listener, its own port-80 listener, and the port-80 redirect
// that leaves the ACME challenge route alone. It writes nothing else when the
// Certificate is not Ready, so a box whose name does not resolve yet keeps
// serving HTTP through the platform's shared listener.
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
	Host     string
	Issuer   string
	Name     string
	Ready    bool
	Reason   string
	Listener bool
	Redirect bool
}

func (d *CertificateDecision) report() say.Reporter { return say.Or(d.Say, nil) }

// Apply writes one host's certificate, listener and redirect.
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
		// HTTP is the only way to reach the box, and the ACME solver may
		// still need it. The redirect waits for a certificate that exists;
		// leaving HTTP open is the safe half.
		r.Warn(fmt.Sprintf("certificate %s/%s is not Ready (%s): %s stays on HTTP for now; box retries on the next run", GatewayNamespace, res.Name, reason, d.Host))
		return res, nil
	}
	r.Success(fmt.Sprintf("certificate %s/%s is Ready", GatewayNamespace, res.Name))

	if err := d.attach(ctx, cl, res.Name); err != nil {
		return res, err
	}
	res.Listener = true
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
	return false, "", fmt.Errorf("Certificate %s/%s: %w", GatewayNamespace, name, err)
}

// attach adds this host's two listeners in one partial Gateway apply: the
// HTTPS terminator on 443, and a port-80 listener that matches this hostname
// only and admits the Gateway's own namespace. Gateway API listener isolation
// sends a request whose Host matches the hostname to the most specific
// listener, so that host's port-80 traffic reaches the redirect and the ACME
// solver route -- while every other host keeps the platform's generic http
// listener. The shared listener is never touched: box owns it, and narrowing
// it would re-open (or close) port 80 for every host at once.
//
// Both listeners are written under the host's own field manager, so the
// platform's full Gateway apply cannot remove them and a second host's TLS
// apply cannot either.
func (d *CertificateDecision) attach(ctx context.Context, cl CertificateCluster, name string) error {
	r := d.report()
	host := d.Host
	step := r.Start(say.Op{Label: "applying the HTTPS and HTTP listeners for " + host})
	if err := cl.ApplyWith(ctx, tlsManager(host), []map[string]any{
		gatewayPartial(map[string]any{"listeners": []any{
			httpsListener(host, name),
			httpHostListener(host),
		}}),
	}); err != nil {
		step.Failed(err)
		return fmt.Errorf("listeners for %s: %w", host, err)
	}
	step.Done(fmt.Sprintf("applied listeners %s and %s on %s", ListenerName(host), HTTPListenerName(host), host))
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
// first object is written.
func certificateAccess() []Access {
	return []Access{
		{Verb: "create", Group: "cert-manager.io", Resource: "certificates", Namespace: GatewayNamespace, What: "cert-manager Certificates in " + GatewayNamespace},
		{Verb: "create", Group: "gateway.networking.k8s.io", Resource: "httproutes", Namespace: GatewayNamespace, What: "HTTPRoutes in " + GatewayNamespace},
		{Verb: "create", Group: "gateway.networking.k8s.io", Resource: "gateways", Namespace: GatewayNamespace, What: "gateways"},
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

// tlsManager is the field manager that owns this host's listeners.
func tlsManager(host string) string { return tlsManagerPrefix + dnsLabel(host) }

// dnsLabel flattens a host name into characters Kubernetes object names and
// field managers accept, short enough for both.
func dnsLabel(host string) string {
	host = strings.ToLower(host)
	var b strings.Builder
	for _, r := range host {
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
	if len(out) > 100 {
		out = strings.Trim(out[:100], "-")
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
    app.kubernetes.io/part-of: s5n
    app.kubernetes.io/managed-by: box
spec:
  secretName: %s
  dnsNames:
    - %s
  issuerRef:
    name: %s
    kind: ClusterIssuer
`, name, GatewayNamespace, name, host, issuer))
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
    app.kubernetes.io/part-of: s5n
    app.kubernetes.io/managed-by: box
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
`, name, GatewayNamespace, GatewayName, GatewayNamespace, HTTPListenerName(host), host))
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
