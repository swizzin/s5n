package platform

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/authelia"
	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/wait"
)

// CertificateTimeout is how long a run's Certificates get to become Ready together
// before box leaves a host on HTTP and says so. One wait per run whatever the number
// of hosts, and never fatal: a failed issuance or renewal does not block apply
// (docs/box.md).
const CertificateTimeout = 3 * time.Minute

// certNotReadyReason is box's words when cert-manager gives no reason.
const certNotReadyReason = "cert-manager has not marked it Ready"

// CertificateCluster is what the per-host TLS flow needs beyond the platform writes:
// status, whether cert-manager is there, a manager-scoped write, and the two Secret
// reads the upgrade check makes.
type CertificateCluster interface {
	Cluster
	// CertificateReady answers the Ready condition with cert-manager's reason.
	CertificateReady(ctx context.Context, namespace, name string) (bool, string, error)
	// Serves reports whether the Certificate CRD exists: a box without cert-manager
	// still has its files written, it just has no certificate yet.
	Serves(ctx context.Context, group, version, kind string) (bool, error)
	// ApplyWith server-side-applies the documents under manager.
	ApplyWith(ctx context.Context, manager string, docs []map[string]any) error
	// Get reads one object, or reports that it is not there.
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
	// UpdateStatus writes one object's status: how box asks cert-manager for a new
	// certificate, since the Issuing condition is status.
	UpdateStatus(ctx context.Context, group, version, kind, namespace string, obj map[string]any) error
}

// CertificateDecision is root's TLS half for one host: a Certificate in
// s5n-gateway, and once it is Ready the host's HTTPS listener and the port-80
// redirect that leaves the ACME challenge route alone. The port-80 listener and its
// login-free Allow come first, because the HTTP-01 challenge cannot answer through a
// login; a not-Ready run leaves only those.
type CertificateDecision struct {
	Host string
	// Issuer is tls.issuer's value or the ClusterIssuer name itself; box maps one to
	// the other.
	Issuer string
	// Open builds the cluster client, only called when there is a host to write; the
	// return type carries the reads and manager-scoped write, so a client that cannot
	// do them does not compile.
	Open func() (CertificateCluster, error)
	Say  say.Reporter
	// Timeout bounds the wait; zero means CertificateTimeout.
	Timeout time.Duration
}

// CertificateResult is what happened, for the caller and for tests.
type CertificateResult struct {
	Host   string
	Issuer string
	Name   string
	Ready  bool
	Reason string
	// Listener and Policy precede the Certificate and stay when it is not Ready: the
	// challenge still has to answer.
	Listener bool
	Policy   bool
	// HTTPS and Redirect are written only after the Certificate is Ready: a host whose
	// certificate never issues is never served over HTTPS.
	HTTPS    bool
	Redirect bool
	// Reissued says the certificate was not signed by box's root and this run asked for
	// a new one.
	Reissued bool
}

func (d *CertificateDecision) report() say.Reporter { return say.Or(d.Say, nil) }

// Apply writes one host's port-80 listener and Allow, the Certificate, then the HTTPS
// listener and redirect once Ready. It is Certificates over one name, so a host is
// never served differently for being the only one.
func (d *CertificateDecision) Apply(ctx context.Context) (CertificateResult, error) {
	set := &Certificates{
		Hosts:   []CertificateRequest{{Host: d.Host, Issuer: d.Issuer}},
		Open:    d.Open,
		Say:     d.Say,
		Timeout: d.Timeout,
	}
	results, err := set.Apply(ctx)
	return results[0], err
}

// CertificateRequest is one host's half of a run: the name and the issuer this
// run decided for it.
type CertificateRequest struct {
	Host   string
	Issuer string
}

// Certificates is root's TLS half for every host of one run: each host's port-80
// listener, Allow and Certificate, one wait over all of them under one
// CertificateTimeout, then every Ready host served and every other warned about.
//
// The single wait is the point: names that cannot issue yet (no DNS, an ACME outage)
// cost one timeout, not one per name. A Certificate already Ready answers on the first
// read but is still served after the run's wait.
type Certificates struct {
	Hosts []CertificateRequest
	// Open builds the cluster client, only called when there is a host to write.
	Open    func() (CertificateCluster, error)
	Say     say.Reporter
	Timeout time.Duration
}

func (c *Certificates) report() say.Reporter { return say.Or(c.Say, nil) }

// timeout is the budget the run's one wait gets.
func (c *Certificates) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return CertificateTimeout
}

// Apply runs three phases: every host's listener, Allow and Certificate written first,
// so cert-manager works on all of them during the wait; one wait for the run; then
// every Ready host served. A write-phase failure leaves what this run wrote -- a
// listener and a Certificate let the next run answer the challenge. Nothing is rolled
// back.
func (c *Certificates) Apply(ctx context.Context) ([]CertificateResult, error) {
	results := make([]CertificateResult, len(c.Hosts))
	for i, req := range c.Hosts {
		results[i] = CertificateResult{Host: req.Host}
		if err := validateCertificate(req.Host, req.Issuer); err != nil {
			return results, err
		}
		issuer, err := clusterIssuerName(req.Issuer)
		if err != nil {
			return results, err
		}
		results[i].Issuer = issuer
		results[i].Name = CertificateName(req.Host)
	}
	if len(results) == 0 {
		return results, nil
	}

	r := c.report()
	cl, err := c.Open()
	if err != nil {
		return results, err
	}
	serves, err := cl.Serves(ctx, "cert-manager.io", "v1", "Certificate")
	if err != nil {
		return results, fmt.Errorf("TLS: %w", err)
	}
	if !serves {
		for _, res := range results {
			r.Warn(fmt.Sprintf("cert-manager is not installed in this cluster: %s has no certificate yet; run `box bootstrap`", res.Host))
		}
		return results, nil
	}
	// The gate before the first write: the objects below are root's.
	if err := preflight(ctx, cl, certificateAccess()); err != nil {
		return results, err
	}

	for i := range results {
		if err := c.stage(ctx, cl, &results[i]); err != nil {
			return results, err
		}
	}
	if err := c.await(ctx, cl, results); err != nil {
		return results, err
	}
	for i := range results {
		if err := c.serve(ctx, cl, &results[i]); err != nil {
			return results, err
		}
	}
	c.reportHosts(results)
	return results, nil
}

// reportHosts closes the run with one row per name: what that host is served as. The
// per-object lines are progress the live renderer already showed.
func (c *Certificates) reportHosts(results []CertificateResult) {
	rows := make([]say.TableRow, 0, len(results))
	for _, res := range results {
		switch {
		case res.Ready:
			rows = append(rows, say.TableRow{Glyph: "✓", Name: res.Host, Facts: []string{"https (" + issuerLabel(res.Issuer) + ")"}})
		case res.Reason != "":
			rows = append(rows, say.TableRow{Glyph: "!", Name: res.Host, Facts: []string{"http only: the certificate is not Ready (" + res.Reason + ")", "the next run retries it"}})
		default:
			rows = append(rows, say.TableRow{Glyph: "·", Name: res.Host, Facts: []string{"not served yet"}})
		}
	}
	c.report().Table("front door", rows)
}

// issuerLabel is the ClusterIssuer as a person reads it.
func issuerLabel(issuer string) string {
	switch issuer {
	case LetsEncryptIssuer, "letsencrypt":
		return "Let's Encrypt"
	case SelfSignedIssuer, "selfsigned":
		return "self-signed"
	}
	return issuer
}

// stage is one host's write half: port 80 without a login, its Allow, the
// Certificate. Nothing is served here.
func (c *Certificates) stage(ctx context.Context, cl CertificateCluster, res *CertificateResult) error {
	// Port 80 first, without a login: the HTTP-01 challenge is a request to this
	// listener, and Authelia refuses any request whose forwarded scheme is http before
	// it reads a rule.
	d := &CertificateDecision{Host: res.Host, Issuer: res.Issuer, Say: c.Say, Timeout: c.Timeout}
	if err := d.attachHTTP(ctx, cl); err != nil {
		return err
	}
	res.Listener = true
	// The policy names the listener by sectionName, so the listener first; and both
	// before the challenge, or the login holds the challenge's own request.
	if err := d.listenerPolicy(ctx, cl); err != nil {
		return err
	}
	res.Policy = true

	r := c.report()
	step := r.Start(say.Op{Label: "applying Certificate " + GatewayNamespace + "/" + res.Name})
	docs, err := ParseManifests(certificateManifest(res.Host, res.Issuer, res.Name))
	if err != nil {
		step.Failed(err)
		return fmt.Errorf("certificate for %s: %w", res.Host, err)
	}
	if err := cl.Apply(ctx, docs); err != nil {
		step.Failed(err)
		return fmt.Errorf("certificate for %s: %w", res.Host, err)
	}
	step.Done("")
	return nil
}

// await is the run's one wait: every Certificate polled together under one budget. A
// goroutine per name, joined before anything is reported, so a Ready certificate is
// not queued behind a slower one and the run spends at most one timeout. The
// reporter is touched only from this goroutine.
func (c *Certificates) await(ctx context.Context, cl CertificateCluster, results []CertificateResult) error {
	timeout := c.timeout()
	r := c.report()
	step := r.Start(waitOp(timeout, results))
	r.Detail(waitDetail(timeout, results))
	errs := make([]error, len(results))
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = cl.Await(ctx, Wait{Kind: WaitCertificate, Namespace: GatewayNamespace, Name: results[i].Name, Timeout: timeout})
		}(i)
	}
	wg.Wait()

	ready := 0
	for i := range results {
		if err := errs[i]; err != nil {
			if !errors.Is(err, ErrNotReady) {
				step.Failed(err)
				return fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, results[i].Name, err)
			}
			// cert-manager's own reason, read once; a name not Ready at the deadline
			// is a state box reports and leaves, not an error.
			_, reason, readErr := cl.CertificateReady(ctx, GatewayNamespace, results[i].Name)
			if readErr != nil {
				step.Failed(readErr)
				return fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, results[i].Name, readErr)
			}
			if reason == "" {
				reason = certNotReadyReason
			}
			results[i].Reason = reason
			continue
		}
		results[i].Ready = true
		ready++
	}
	r.Detail(waitDone(ready, results))
	step.Done(waitFact(ready, results))
	return nil
}

// waitOp is the run's one wait step: a row named `certificates`, the names and
// budget in the log.
func waitOp(timeout time.Duration, results []CertificateResult) say.Op {
	return say.Op{Name: certificatesRow, Budget: timeout}
}

// certificatesRow is the one row the run's certificate wait is: how many of the
// names came Ready.
const certificatesRow = "certificates"

// waitDetail is that step's log line: what it waits for, and how long it has.
func waitDetail(timeout time.Duration, results []CertificateResult) string {
	if len(results) == 1 {
		return fmt.Sprintf("waiting up to %s for Certificate %s/%s to be Ready", timeout, GatewayNamespace, results[0].Name)
	}
	return fmt.Sprintf("waiting up to %s for %d Certificates to be Ready", timeout, len(results))
}

// waitDone names how many of the run's Certificates came Ready.
func waitDone(ready int, results []CertificateResult) string {
	if len(results) == 1 {
		if ready == 1 {
			return fmt.Sprintf("Certificate %s/%s: Ready", GatewayNamespace, results[0].Name)
		}
		return ""
	}
	return fmt.Sprintf("%d of %d Certificates are Ready", ready, len(results))
}

// waitFact is the row's fact: how many came Ready, and from whom when every
// name was issued by the same issuer -- which is the case on every box whose
// names resolve the same way.
func waitFact(ready int, results []CertificateResult) string {
	fact := fmt.Sprintf("%d of %d", ready, len(results))
	issuer := ""
	for _, res := range results {
		if res.Issuer == "" {
			return fact
		}
		if issuer == "" {
			issuer = res.Issuer
			continue
		}
		if issuer != res.Issuer {
			return fact
		}
	}
	if issuer != "" {
		fact += " from " + issuerLabel(issuer)
	}
	return fact
}

// serve is one host's second half, after the run's wait: the box-root re-issue
// for a self-signed certificate, then its HTTPS listener and the redirect. A
// host that is not Ready keeps only port 80 -- the listener and its Allow are
// what let cert-manager try again -- and the run's host row says so.
func (c *Certificates) serve(ctx context.Context, cl CertificateCluster, res *CertificateResult) error {
	if !res.Ready {
		// Port 80 stays, the host is not served: no HTTPS listener, no redirect.
		return nil
	}

	// A self-signed host's certificate has to chain to box's root, or the anchor
	// written into the seedbox is not the one its apps verify against. cert-manager
	// does not re-issue when the issuer behind an unchanged issuerRef changes what it
	// signs with, so box asks (followRoot).
	d := &CertificateDecision{Host: res.Host, Issuer: res.Issuer, Say: c.Say, Timeout: c.Timeout}
	if res.Issuer == SelfSignedIssuer {
		reissued, err := d.followRoot(ctx, cl, res.Name)
		if err != nil {
			return err
		}
		res.Reissued = reissued
	}

	if err := d.attachHTTPS(ctx, cl, res.Name); err != nil {
		return err
	}
	res.HTTPS = true
	if err := d.redirect(ctx, cl, res.Name); err != nil {
		return err
	}
	res.Redirect = true
	return nil
}

// validateCertificate refuses a name or issuer no Certificate can be built from.
func validateCertificate(host, issuer string) error {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return errors.New("no host name: a certificate is per host name")
	}
	if trimmed != host || strings.ContainsAny(trimmed, " /\\:") {
		return fmt.Errorf("host name %q is not a name box can put on a certificate", host)
	}
	if _, err := clusterIssuerName(issuer); err != nil {
		return err
	}
	return nil
}

// clusterIssuerName maps tls.issuer's vocabulary ("letsencrypt", "selfsigned") to the
// ClusterIssuer object names root installs.
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

// followRoot keeps a self-signed host's certificate on box's root.
//
// A certificate written from a bare `selfSigned` issuer has ca.crt == the leaf, and
// cert-manager does not re-issue when the issuer behind an unchanged issuerRef changes
// what it signs with: at v1.21.3 the trigger chain looks at the Secret, the current
// CertificateRequest and the expiry, never at the issuer's spec
// (internal/controller/certificates/policies/policies.go:71-85). The served
// certificate would stay self-signed until it expired, chaining to nothing box wrote.
//
// What starts an issuance is the `Issuing` status condition, which is exactly what
// `cmctl renew` sets (cert-manager/cmctl, pkg/renew/renew.go:217), acted on by the
// issuing controller (pkg/controller/certificates/issuing/issuing_controller.go:197-204).
// Box sets the same condition, with the same reason, after comparing the certificates
// itself (signedByRoot).
func (d *CertificateDecision) followRoot(ctx context.Context, cl CertificateCluster, name string) (bool, error) {
	root, err := rootCertificate(ctx, cl)
	if err != nil {
		return false, err
	}
	secret, ok, err := cl.Get(ctx, "", "v1", "Secret", GatewayNamespace, name)
	if err != nil {
		return false, fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, name, err)
	}
	if !ok {
		// No Secret yet: nothing to compare, and one is written from the root anyway.
		return false, nil
	}
	data, _, _ := unstructured.NestedStringMap(secret, "data")
	caCert, _ := base64.StdEncoding.DecodeString(data["ca.crt"])
	ours, err := signedByRoot(caCert, root)
	if err != nil {
		return false, err
	}
	if ours {
		return false, nil
	}

	r := d.report()
	step := r.Start(say.Op{Label: "re-issuing Certificate " + GatewayNamespace + "/" + name + " from box's root"})
	if err := triggerIssuing(ctx, cl, name); err != nil {
		step.Failed(err)
		return false, err
	}
	// Wait for the Secret to hold a different certificate: Ready can still be True for
	// the old one while the new request is in flight, so the evidence is the bytes.
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = CertificateTimeout
	}
	before := data["tls.crt"]
	waited, err := wait.Until(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) {
		obj, ok, err := cl.Get(ctx, "", "v1", "Secret", GatewayNamespace, name)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		now, _, _ := unstructured.NestedStringMap(obj, "data")
		return now["tls.crt"] != before && now["tls.crt"] != "", nil
	})
	if err != nil {
		step.Failed(err)
		return false, fmt.Errorf("certificate %s/%s: waiting for the re-issue: %w", GatewayNamespace, name, err)
	}
	if !waited.Done {
		step.Failed(ErrNotReady)
		return false, fmt.Errorf("certificate %s/%s: cert-manager did not replace it within %s after box asked (check `kubectl -n %s describe certificate %s`)",
			GatewayNamespace, name, timeout, GatewayNamespace, name)
	}
	step.Done(fmt.Sprintf("Certificate %s/%s: re-issued from box's root", GatewayNamespace, name))

	// The new certificate should be the root's child; if not, say so rather than
	// letting the anchor be quietly wrong. A warning, not a stop: the next run asks
	// again.
	ready, reason, err := d.awaitCertificate(ctx, cl, name)
	if err != nil {
		return true, err
	}
	if !ready {
		r.Warn(fmt.Sprintf("certificate %s/%s was re-issued but is not Ready (%s): %s is still served by the old one until it is", GatewayNamespace, name, reason, d.Host))
		return true, nil
	}
	if obj, ok, err := cl.Get(ctx, "", "v1", "Secret", GatewayNamespace, name); err == nil && ok {
		now, _, _ := unstructured.NestedStringMap(obj, "data")
		caCert, _ := base64.StdEncoding.DecodeString(now["ca.crt"])
		if ours, err := signedByRoot(caCert, root); err == nil && !ours {
			r.Warn(fmt.Sprintf("certificate %s/%s was re-issued but does not chain to box's root: check that %s is a CA issuer over %s", GatewayNamespace, name, SelfSignedIssuer, RootSecretName))
		}
	}
	return true, nil
}

// CertificateNeedsWork reports a Certificate that exists but is not the one this run
// would issue: another issuer, or a self-signed one whose ca.crt is not box's root.
//
// It exists for a host that already answers: name and issuerRef do not change when
// the issuer behind them changes what it signs with, and cert-manager never re-issues
// for that, so a run looking only for missing hosts would leave it there.
func CertificateNeedsWork(ctx context.Context, k objectReader, host, want string) (bool, error) {
	name := CertificateName(host)
	obj, ok, err := k.Get(ctx, "cert-manager.io", "v1", "Certificate", GatewayNamespace, name)
	if err != nil || !ok {
		return false, err
	}
	current, _, _ := unstructured.NestedString(obj, "spec", "issuerRef", "name")
	if want != "" {
		issuer, err := clusterIssuerName(want)
		if err != nil {
			return false, err
		}
		if current != issuer {
			return true, nil
		}
	}
	if current != SelfSignedIssuer {
		return false, nil
	}
	secretName, _, _ := unstructured.NestedString(obj, "spec", "secretName")
	if secretName == "" {
		secretName = name
	}
	secret, ok, err := k.Get(ctx, "", "v1", "Secret", GatewayNamespace, secretName)
	if err != nil || !ok {
		// No Secret yet: nothing to compare, and the step writes one.
		return false, err
	}
	data, _, _ := unstructured.NestedStringMap(secret, "data")
	caCert, _ := base64.StdEncoding.DecodeString(data["ca.crt"])
	root, err := rootCertificate(ctx, k)
	if err != nil {
		return false, err
	}
	ours, err := signedByRoot(caCert, root)
	if err != nil {
		return false, err
	}
	return !ours, nil
}

// triggerIssuing sets the Issuing condition the issuing controller acts on: status,
// not spec, the same write and reason `cmctl renew` makes, so `kubectl describe` shows
// a manual re-issue.
func triggerIssuing(ctx context.Context, cl CertificateCluster, name string) error {
	crt, ok, err := cl.Get(ctx, "cert-manager.io", "v1", "Certificate", GatewayNamespace, name)
	if err != nil {
		return fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, name, err)
	}
	if !ok {
		return fmt.Errorf("certificate %s/%s is not there: nothing to re-issue", GatewayNamespace, name)
	}
	generation, _, _ := unstructured.NestedInt64(crt, "metadata", "generation")
	cond := map[string]any{
		"type":               "Issuing",
		"status":             conditionTrue,
		"reason":             "ManuallyTriggered",
		"message":            "Certificate re-issuance manually triggered",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
		"observedGeneration": generation,
	}
	conditions, _, _ := unstructured.NestedSlice(crt, "status", "conditions")
	replaced := false
	for i, raw := range conditions {
		existing, _ := raw.(map[string]any)
		if existing["type"] != "Issuing" {
			continue
		}
		conditions[i] = cond
		replaced = true
	}
	if !replaced {
		conditions = append(conditions, cond)
	}
	if err := unstructured.SetNestedSlice(crt, conditions, "status", "conditions"); err != nil {
		return fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, name, err)
	}
	if err := cl.UpdateStatus(ctx, "cert-manager.io", "v1", "Certificate", GatewayNamespace, crt); err != nil {
		return fmt.Errorf("certificate %s/%s: asking cert-manager to re-issue it: %w", GatewayNamespace, name, err)
	}
	return nil
}

// awaitCertificate waits for one Certificate after box asked for a re-issue. Not Ready
// at the deadline is not an error but a state box reports with cert-manager's reason;
// the ordinary wait is Certificates.await.
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
			reason = certNotReadyReason
		}
		return false, reason, nil
	}
	wait.Failed(err)
	return false, "", fmt.Errorf("certificate %s/%s: %w", GatewayNamespace, name, err)
}

// attachHTTP adds this host's port-80 listener, and only that listener.
//
// It runs before the Certificate because the HTTP-01 challenge is served by a route on
// this listener: the solver's route names the Gateway with no sectionName, and Gateway
// API listener isolation sends a request whose Host matches to the most specific
// listener. The listener admits only the Gateway's own namespace, where the redirect
// and the solver's route live, so no app route attaches and every other host stays on
// the generic http listener.
//
// The host's own field manager writes it, so the platform's full Gateway apply and
// another host's TLS apply cannot remove it.
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
	step.Done("")
	return nil
}

// attachHTTPS adds this host's TLS terminator, once its certificate is Ready.
//
// The two listeners have two managers, one each, not one manager per host carrying
// both: an apply removes any field its manager owned and left out, so a single manager
// would make the port-80 apply (which runs while there is no HTTPS listener to name)
// delete the HTTPS listener on every re-run. One manager per listener keeps each apply
// minimal, and no apply can remove a listener it does not own.
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
	step.Done("")
	return nil
}

// listenerPolicy takes the login off this host's port-80 listener.
//
// The Gateway's SecurityPolicy covers every listener unless that listener has its own,
// and Envoy applies the listener-scoped one instead
// (internal/gatewayapi/securitypolicy.go, v1.9.3). Without this, a plain-HTTP request
// here never reaches the router: ext_authz runs first and Authelia answers 400 at an
// http scheme, so the 301 never happens and an ACME renewal is answered by a login.
//
// An explicit Allow is right because no app route can attach to this listener, which
// admits only the Gateway's namespace; the shared generic listener keeps the login, so
// a host without a certificate stays closed. It is written before the Certificate: the
// challenge arrives while the certificate is being issued.
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
	step.Done("")
	return nil
}

// redirect writes the port-80 route. The solver's exact-path route wins over this
// prefix, so a renewal works with the redirect in place.
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
	step.Done("")
	return nil
}

// HostServed reports whether a host already has the three objects an app host needs:
// its HTTPS listener, its Certificate and the login's access_control rule. It reads the
// cluster, never a record, so a deleted listener or rule is missing again and the next
// run puts it back.
//
// The port-80 listener and its Allow are not part of the answer. The rule is: the
// Gateway asks the login about every listener, and a host with no rule gets
// default_policy deny -- an app that answers nothing, which this read exists to catch.
func (k *Kubernetes) HostServed(ctx context.Context, host string) (bool, error) {
	gw, ok, err := k.Get(ctx, "gateway.networking.k8s.io", "v1", "Gateway", GatewayNamespace, GatewayName)
	if err != nil {
		return false, err
	}
	if !ok || !hasHTTPSListener(gw, ListenerName(host)) {
		return false, nil
	}
	serves, err := k.Serves(ctx, "cert-manager.io", "v1", "Certificate")
	if err != nil {
		return false, err
	}
	if !serves {
		return false, nil
	}
	_, ok, err = k.Get(ctx, "cert-manager.io", "v1", "Certificate", GatewayNamespace, CertificateName(host))
	if err != nil || !ok {
		return false, err
	}
	return k.hostRuled(ctx, host)
}

// hostRuled reads the login's configuration for one host's rule; no configuration
// means no rule, which is an answer (not served), not an error.
func (k *Kubernetes) hostRuled(ctx context.Context, host string) (bool, error) {
	cm, ok, err := k.Get(ctx, "", "v1", "ConfigMap", authelia.SystemNamespace, authelia.ConfigMapName)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return hasHostRule(cm, host), nil
}

// hasHostRule reads one host's rule out of the login's document: no rule means
// default_policy deny.
func hasHostRule(cm map[string]any, host string) bool {
	data, _, _ := unstructured.NestedStringMap(cm, "data")
	return authelia.HasRule(data[authelia.ConfigFile], host)
}

// hasHTTPSListener reads the host's TLS terminator out of a Gateway document.
func hasHTTPSListener(gateway map[string]any, name string) bool {
	listeners, _, _ := unstructured.NestedSlice(gateway, "spec", "listeners")
	for _, raw := range listeners {
		listener, _ := raw.(map[string]any)
		if listener["name"] != name {
			continue
		}
		return listener["protocol"] == "HTTPS"
	}
	return false
}

// HostAccess answers whether this identity may write the per-host objects: the list
// certificateAccess preflights, asked before the step so a user's run stops naming
// root's command. A denial is an answer, not an error.
func HostAccess(ctx context.Context, cl Cluster) (bool, error) {
	denied, err := kube.Denied(ctx, cl, certificateAccess())
	if err != nil {
		return false, err
	}
	return len(denied) == 0, nil
}

// certificateAccess is the RBAC the per-host write needs. All are applies: after the
// first run the objects exist, so the API server authorizes a patch.
func certificateAccess() []Access {
	return []Access{
		{Verb: kube.VerbApply, Group: "cert-manager.io", Resource: "certificates", Namespace: GatewayNamespace, What: "cert-manager Certificates in " + GatewayNamespace},
		{Verb: kube.VerbApply, Group: "gateway.networking.k8s.io", Resource: "httproutes", Namespace: GatewayNamespace, What: "HTTPRoutes in " + GatewayNamespace},
		{Verb: kube.VerbApply, Group: "gateway.networking.k8s.io", Resource: "gateways", Namespace: GatewayNamespace, What: "gateways"},
		{Verb: kube.VerbApply, Group: "gateway.envoyproxy.io", Resource: "securitypolicies", Namespace: GatewayNamespace, What: "SecurityPolicies in " + GatewayNamespace},
	}
}

// CertificateName is the Certificate and Secret name for a host: stable across runs,
// so re-applying is a no-op.
func CertificateName(host string) string { return "s5n-tls-" + dnsLabel(host) }

// ListenerName is the Gateway listener name for a host's HTTPS terminator.
func ListenerName(host string) string { return "https-" + dnsLabel(host) }

// HTTPListenerName is the Gateway listener name for a host's port-80 traffic.
// It is the sectionName the redirect attaches to.
func HTTPListenerName(host string) string { return "http-" + dnsLabel(host) }

// ListenerPolicyName is the SecurityPolicy that keeps the login off one host's port-80
// listener, named like the host's other objects.
func ListenerPolicyName(host string) string { return "s5n-no-login-" + dnsLabel(host) }

// httpListenerManager owns this host's port-80 listener and its Allow: one manager per
// listener, so an apply cannot remove the other (see attachHTTPS).
func httpListenerManager(host string) string {
	return kube.TLSManagerPrefix + dnsLabel(host) + "-http"
}

// httpsListenerManager is the field manager that owns this host's TLS
// terminator.
func httpsListenerManager(host string) string {
	return kube.TLSManagerPrefix + dnsLabel(host) + "-https"
}

// maxDNSLabel is the longest host label: the tightest fit is a field manager (128),
// which adds `box-tls-` and `-https` around it.
const maxDNSLabel = 100

// hostHashLen is how many hex characters a lossy label carries: four bytes make a
// readable-prefix collision vanishingly unlikely.
const hostHashLen = 8

// plainHost matches hosts whose flattening is reversible: lowercase labels joined by
// dots, never hyphens, since a hyphen and a dot flatten alike.
var plainHost = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9]+)*$`)

// dnsLabel flattens a host into characters object names and field managers accept.
//
// Flattening alone is lossy: "a-b.example.com" and "a.b.example.com" both become
// "a-b-example-com", and two hosts sharing a label would share a Certificate, a
// listener and a manager, so whichever applied last would own the other's objects.
// An unambiguous, short enough host keeps its readable name; every other gets that
// prefix plus eight hex characters of the lowercased host's sha256.
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
	// A host with nothing readable ("*") leaves only the suffix; a name must not start
	// with a hyphen.
	return strings.TrimLeft(out+suffix, "-")
}

// flattenHost turns every byte outside a DNS label into a hyphen, collapsing runs. It
// does not cut to the limit: dnsLabel must know whether the readable form fits first.
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

// certificateManifest is one Certificate in the Gateway's namespace, where its Secret
// must live for a listener there to reference it without a ReferenceGrant.
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

// redirectManifest is one host's port-80 route, on that host's own http listener, and
// only answers requests the solver's exact-path route did not claim.
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

// listenerPolicyManifest is the Allow one host's port-80 listener needs. It targets the
// listener by sectionName, so it is no exemption for the host's HTTPS or shared
// listener, and carries the default action only.
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

// gatewayPartial is the smallest Gateway a server-side apply can carry one listener
// in: the listeners list is a map keyed by name, so what this object omits stays with
// its own manager.
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

// httpsListener is the host's TLS terminator, admitting every namespace: the chart's
// app routes live in the seedbox's.
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

// httpHostListener is this host's port-80 listener, matched by hostname so listener
// isolation gives its requests the routes attached here. It admits only the Gateway's
// own namespace, where the redirect and the solver's route live, so no app route can
// attach.
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

// interface checks: the client cmd/box builds satisfies the per-host flow statically,
// so a missing read or write is a compile error, not a failed bootstrap.
var _ CertificateCluster = (*Kubernetes)(nil)
