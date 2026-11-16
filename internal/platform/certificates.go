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

// CertificateTimeout is how long a run's Certificates get to become Ready
// together before box leaves a host on HTTP and says so. The wait is one per
// run, whatever the number of hosts: a box whose names cannot be issued yet is
// slow once, not once per name. It is never fatal: a certificate that cannot be
// issued yet is DNS or an ACME problem, and docs/box.md is explicit that a
// failed issuance or renewal does not block apply.
const CertificateTimeout = 3 * time.Minute

// certNotReadyReason is what box says when cert-manager has no reason of its
// own for a Certificate that is not Ready.
const certNotReadyReason = "cert-manager has not marked it Ready"

// CertificateCluster is the cluster client the per-host TLS flow needs beyond
// the platform writes: a Certificate's status, whether cert-manager is there, a
// write under its own field manager, and the two reads the upgrade check needs
// (the root's Secret and the host's own).
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
	// Get reads one object, or reports that it is not there.
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
	// UpdateStatus writes one object's status. It is how box asks
	// cert-manager for a new certificate: the Issuing condition is status.
	UpdateStatus(ctx context.Context, group, version, kind, namespace string, obj map[string]any) error
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
	// Reissued says the certificate box found was not signed by the box root
	// and this run asked cert-manager for a new one.
	Reissued bool
}

func (d *CertificateDecision) report() say.Reporter { return say.Or(d.Say, nil) }

// Apply writes one host's port-80 listener and its Allow, then the
// Certificate, and -- once the Certificate is Ready -- the HTTPS listener and
// the redirect. It is Certificates over one name: both box commands now write
// every decided host through the same phases, so a host is never served
// differently because it happened to be the only one.
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

// Certificates is root's TLS half for every host of one run. It writes each
// host's port-80 listener and its Allow and then its Certificate, waits for
// all of them together under one CertificateTimeout, and then serves each host
// that came Ready and warns for each one that did not.
//
// The single wait is the point: a box whose names cannot issue yet (no DNS, an
// ACME outage) spends one timeout on the run, not one per name, and says so
// once. A Certificate that is already Ready answers on the first read and is
// served after the run's wait, never before: the wait is the run's, not the
// host's.
type Certificates struct {
	Hosts []CertificateRequest
	// Open builds the cluster client; it is only called when there is a host
	// to write.
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

// Apply runs the three phases in order: every host's Certificate is written
// first, so cert-manager is working on all of them while the wait runs; then
// the run waits once; then each Ready host is served. A failure in the write
// phase stops before the wait and leaves what this run wrote -- a port-80
// listener and a Certificate answer the challenge on the next run -- the way
// the per-host flow did. Nothing is rolled back.
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
	// The gate before the first write: the objects below are root's, and a
	// kubeconfig without them must be refused before any Certificate exists.
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
	return results, nil
}

// stage is one host's write half: port 80 without a login, the Allow that
// keeps the login off it, then the Certificate. Nothing is served here.
func (c *Certificates) stage(ctx context.Context, cl CertificateCluster, res *CertificateResult) error {
	// Port 80 first, without a login: the HTTP-01 challenge is a request to
	// this listener, and Authelia refuses any request whose forwarded scheme
	// is http before it reads a rule. Ordering the Certificate first (as box
	// once did) left the challenge on the shared, login-guarded listener and
	// no certificate could ever issue.
	d := &CertificateDecision{Host: res.Host, Issuer: res.Issuer, Say: c.Say, Timeout: c.Timeout}
	if err := d.attachHTTP(ctx, cl); err != nil {
		return err
	}
	res.Listener = true
	// The policy names the listener by sectionName, so the listener has to
	// exist first; and it has to exist before the challenge, or the login
	// holds the challenge's own request.
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
	step.Done(fmt.Sprintf("applied Certificate %s/%s (issuer %s)", GatewayNamespace, res.Name, res.Issuer))
	return nil
}

// await is the run's one wait: every Certificate it applied, polled together
// under one budget. One goroutine per name, joined before anything is
// reported, so a Ready certificate answers at once instead of queueing behind a
// slower one, the wait ends as soon as all of them are Ready, and the whole run
// spends at most one timeout. The reporter is only touched from this
// goroutine.
func (c *Certificates) await(ctx context.Context, cl CertificateCluster, results []CertificateResult) error {
	timeout := c.timeout()
	r := c.report()
	step := r.Start(waitOp(timeout, results))
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
			// cert-manager's own reason, read once for the operator; a name
			// that is not Ready at the deadline is the state box reports and
			// leaves for the next run, not an error.
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
	step.Done(waitDone(ready, results))
	return nil
}

// waitOp is the one wait step's screen line. A single host keeps the sentence
// box has always printed for it; several are named by their count, because the
// run has one budget for all of them.
func waitOp(timeout time.Duration, results []CertificateResult) say.Op {
	if len(results) == 1 {
		name := GatewayNamespace + "/" + results[0].Name
		return say.Op{
			Label:  "waiting for Certificate " + name + " to be Ready",
			Plain:  fmt.Sprintf("waiting up to %s for Certificate %s to be Ready", timeout, name),
			Budget: timeout,
		}
	}
	return say.Op{
		Label:  fmt.Sprintf("waiting for %d Certificates to be Ready", len(results)),
		Plain:  fmt.Sprintf("waiting up to %s for %d Certificates to be Ready", timeout, len(results)),
		Budget: timeout,
	}
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

// serve is one host's second half, after the run's wait: the success line, the
// box-root re-issue for a self-signed certificate, then its HTTPS listener and
// the redirect. A host that is not Ready gets the warning and keeps only port
// 80: the listener and its Allow are what let cert-manager try again.
func (c *Certificates) serve(ctx context.Context, cl CertificateCluster, res *CertificateResult) error {
	r := c.report()
	if !res.Ready {
		// Port 80 stays as it is, but the host is not served: there is no
		// HTTPS listener to serve it and no redirect to send anyone there.
		r.Warn(fmt.Sprintf("certificate %s/%s is not Ready (%s): %s is not served until it is; only the ACME challenge answers on port 80; box retries on the next run", GatewayNamespace, res.Name, res.Reason, res.Host))
		return nil
	}
	r.Success(fmt.Sprintf("certificate %s/%s is Ready", GatewayNamespace, res.Name))

	// A self-signed host's certificate has to chain to the box root, or the
	// anchor box writes into the seedbox's namespace is not the anchor its
	// applications verify against. cert-manager does not re-issue on its own
	// when the issuer behind an unchanged issuerRef changes what it signs with,
	// so box asks (followRoot).
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

// validateCertificate refuses a name or issuer a Certificate cannot be built
// from, before this run writes anything.
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

// followRoot keeps a self-signed host's certificate on the box root.
//
// 0.3.0 wrote every host certificate from a bare `selfSigned` ClusterIssuer, so
// its Secret's ca.crt is the leaf itself. A box upgraded from it keeps the same
// Certificate -- same name, same secretName, same issuerRef to
// s5n-selfsigned -- and cert-manager does not re-issue a Certificate because
// the issuer's spec changed: at v1.21.3 the trigger chain looks at the Secret,
// the current CertificateRequest and the expiry, never at what the issuer signs
// with (internal/controller/certificates/policies/policies.go:71-85). Without
// this, the certificate an upgraded box serves stays self-signed until it
// expires, and the anchor box writes is not what that certificate chains to.
//
// What starts an issuance is the `Issuing` status condition, which is exactly
// what `cmctl renew` sets (cert-manager/cmctl, pkg/renew/renew.go:217:
// `apiutil.SetCertificateCondition(crt, crt.Generation, cmapi.CertificateConditionIssuing,
// cmmeta.ConditionTrue, "ManuallyTriggered", "Certificate re-issuance manually triggered")`),
// and the certificates-issuing controller acts on it
// (pkg/controller/certificates/issuing/issuing_controller.go:197-204: with
// Issuing=True it fetches the next private key and creates the request instead
// of returning to ensureSecretData). Box sets the same condition, with the same
// reason, after comparing the two certificates itself (signedByRoot).
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
		// No Secret yet: there is nothing to compare, and the next run -- or
		// this one, a moment later -- writes one from the root anyway.
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
	step := r.Start(say.Op{Label: "re-issuing Certificate " + GatewayNamespace + "/" + name + " from the box root"})
	if err := triggerIssuing(ctx, cl, name); err != nil {
		step.Failed(err)
		return false, err
	}
	// Wait for the Secret to hold a different certificate: the Certificate's
	// Ready condition can still say True for the old one while the new request
	// is in flight, so the evidence has to be the bytes.
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
	step.Done(fmt.Sprintf("Certificate %s/%s: re-issued from the box root", GatewayNamespace, name))

	// The new certificate should be the root's child now; if cert-manager
	// answered with something else, say so rather than letting the anchor be
	// quietly wrong. It is a warning, not a stop: the host is served either
	// way, and the next run asks again.
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
			r.Warn(fmt.Sprintf("certificate %s/%s was re-issued but does not chain to the box root: check that %s is a CA issuer over %s", GatewayNamespace, name, SelfSignedIssuer, RootSecretName))
		}
	}
	return true, nil
}

// CertificateNeedsWork reports whether a host's Certificate is there but is not
// the one this run would issue: a different issuer than the run decided, or a
// self-signed one whose ca.crt is not the box root.
//
// It exists for a host that answers already. A Certificate's name and
// issuerRef do not change when the issuer behind it changes what it signs with
// (0.3.0's bare `selfSigned` became a CA over the box root), and cert-manager
// never re-issues for that: the host is served, by a certificate nothing else
// chains to. A run that only looked for missing hosts would leave it there.
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

// triggerIssuing sets the Issuing condition cert-manager's issuing controller
// acts on. The condition is status, not spec, so it is a status update: the
// same write cmctl renew makes, with the same reason, which is what makes box's
// re-issue visible in `kubectl describe` as a manual one rather than as
// cert-manager's own decision.
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

// awaitCertificate waits for one Certificate, after box asked cert-manager to
// re-issue it, with the Certificate's own shorter bound. A not-Ready answer at
// the deadline is not an error: it is the state box reports and leaves for the
// next run, and cert-manager's reason is read once for the operator. The run's
// ordinary wait is Certificates.await, which covers every host at once.
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

// HostServed reports whether a host already has the three objects an app host
// needs: its HTTPS listener on the Gateway, its Certificate, and the login's
// access_control rule for the name. It reads the cluster, never a record box
// kept, so a listener or a rule somebody deleted is missing again and the next
// run puts it back.
//
// The port-80 listener and the Allow are not part of the answer: they exist
// only to let a certificate issue, and a host whose certificate is Ready but
// whose challenge listener was removed still serves its app.
//
// The rule is: the Gateway asks the login about every listener, and a host with
// no access_control rule gets default_policy deny -- a 403 nobody can get past.
// A host with its listener and its certificate but no rule is an app that
// answers nothing, which is the state this read exists to catch.
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

// hostRuled reads the login's configuration for one host's rule. A box whose
// login is not installed yet has no configuration and therefore no rule, which
// is an answer (the host is not served), not an error.
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

// hasHostRule reads one host's rule out of the login's ConfigMap document. The
// configuration is the file the login starts with, so a name with no rule in it
// is a request the login answers with default_policy deny.
func hasHostRule(cm map[string]any, host string) bool {
	data, _, _ := unstructured.NestedStringMap(cm, "data")
	return authelia.HasRule(data[authelia.ConfigFile], host)
}

// hasHTTPSListener reads one listener out of a Gateway document: the host's own
// TLS terminator, by the name attachHTTPS writes it under.
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

// HostAccess answers whether this identity may write the per-host objects: the
// Gateway's listeners, the Certificate, its route and its Allow.
//
// It is the same list the per-host write preflights (certificateAccess), asked
// before the step so a user's run stops naming root's command instead of
// failing on its first write. A denial is an answer, not an error.
func HostAccess(ctx context.Context, cl Cluster) (bool, error) {
	denied, err := kube.Denied(ctx, cl, certificateAccess())
	if err != nil {
		return false, err
	}
	return len(denied) == 0, nil
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
