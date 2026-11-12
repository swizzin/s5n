package platform

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/swizzin/s5n/internal/kube"
	"github.com/swizzin/s5n/internal/say"
)

// The host CA is the box's answer to one question: how does something inside
// the cluster trust a certificate box made for itself? For Let's Encrypt there
// is nothing to do -- the root is public and every image carries it. For a
// self-signed box the certificate chains to one root box owns, so the root
// travels into the seedbox's namespace as a ConfigMap, and an app that signs in
// through the login's OIDC provider mounts it (docs/box.md, "Certificates").
//
// The ConfigMap holds the certificate and only the certificate: never the key,
// never the Secret it was copied out of.

// pemCertificate is the PEM block type of an X.509 certificate: the only kind
// of block the anchor ever carries.
const pemCertificate = "CERTIFICATE"

// HostCACluster is what root's host-CA step reads and writes. It is the
// platform Cluster plus the two calls the step needs beyond it, so a client
// that cannot make them does not compile.
type HostCACluster interface {
	Cluster
	// Get reads one object, or reports that it is not there.
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
	// Delete removes one object. A missing object is not an error: the step is
	// run on every apply and removing what is already gone is the state it
	// wants.
	Delete(ctx context.Context, group, version, kind, namespace, name string) error
}

// anchorSource is where the certificate the anchor carries comes from.
type anchorSource int

const (
	// anchorNothing: the host's certificate is publicly trusted, so a pod has
	// nothing to add.
	anchorNothing anchorSource = iota
	// anchorFromRoot: the host's certificate is signed by the box root, and
	// the anchor is that root's certificate.
	anchorFromRoot
	// anchorFromFile: a development run hands box the certificate to copy
	// (the e2e's Pebble has no ca.crt in any Secret to read). It is the same
	// writer and the same ConfigMap; only the source differs.
	anchorFromFile
	// anchorUnknown: neither this run's issuer nor a Certificate in the
	// cluster says which issuer the box's host has -- an apply on a box that
	// has never written one. Nothing follows, so nothing is read, written or
	// removed.
	anchorUnknown
)

// anchorFor is the whole decision, as a pure function of the issuer this run
// writes the box's host certificate with (empty when it writes none), the
// issuer the Certificate in the cluster names (and whether there is one at
// all), and whether a development run supplied a file to copy.
//
// The anchor follows the certificate, never DNS. A name that gained a public
// record a minute ago is still served by the certificate box last gave it --
// only init's per-host step moves a host to Let's Encrypt -- so deciding from
// the resolution would take the login away from every app that restarted in
// between. A self-signed host always wins when this run writes the
// certificate: its certificate chains to the box root, so the root is the only
// correct anchor whatever the file says.
func anchorFor(written, certificate string, certificateThere bool, caFile string) (anchorSource, error) {
	follows := ""
	switch {
	case strings.TrimSpace(written) != "":
		name, err := clusterIssuerName(written)
		if err != nil {
			return anchorNothing, err
		}
		follows = name
	case certificateThere:
		// The Certificate's issuerRef is whatever cert-manager accepted
		// there. box only has to recognise its own root; any other name is a
		// certificate the box root did not sign, so there is nothing of
		// box's to carry.
		follows = certificate
	default:
		return anchorUnknown, nil
	}
	if follows == SelfSignedIssuer {
		return anchorFromRoot, nil
	}
	if strings.TrimSpace(caFile) != "" {
		return anchorFromFile, nil
	}
	return anchorNothing, nil
}

// HostCADecision is root's host-CA step for one seedbox: write the anchor the
// box's certificate needs, remove it when the certificate is publicly trusted,
// and keep every self-signed host certificate signed by the root.
type HostCADecision struct {
	// Namespace is the seedbox's namespace: where the anchor goes. Empty is a
	// refusal: this is one box's object, and an empty namespace would be a
	// cluster-wide mistake.
	Namespace string
	// Host is the box's own host name. It names the Certificate this run
	// reads when it writes none, and is what box's output names.
	Host string
	// Issuer is the ClusterIssuer this run writes the box's host certificate
	// with: init's value ("selfsigned", "letsencrypt") or the ClusterIssuer
	// name itself. Empty means this run writes no certificate, and the anchor
	// follows the Certificate already in s5n-gateway.
	Issuer string
	// CAFile is a PEM file a development run supplies instead of reading the
	// root out of cert-manager. Empty in every real run.
	CAFile string
	// Open builds the cluster client; it is only called when there is work.
	Open func() (HostCACluster, error)
	Say  say.Reporter
}

// HostCAResult is what happened, for the caller and for tests.
type HostCAResult struct {
	Namespace string
	// Source is where the certificate came from: "root" or "file".
	Source string
	// Wrote says this run wrote or removed the ConfigMap.
	Wrote bool
	// Removed says it removed it (the host is publicly trusted now).
	Removed bool
}

func (d *HostCADecision) report() say.Reporter { return say.Or(d.Say, nil) }

// Apply writes or removes the anchor. It is idempotent: a run whose ConfigMap
// already holds the certificate writes nothing, so a re-run is empty in box's
// output and in the cluster's resource versions.
func (d *HostCADecision) Apply(ctx context.Context) (HostCAResult, error) {
	res := HostCAResult{Namespace: d.Namespace}
	if strings.TrimSpace(d.Namespace) == "" {
		return res, errors.New("no namespace: the host CA is one seedbox's object")
	}
	cl, err := d.Open()
	if err != nil {
		return res, err
	}
	source, err := d.source(ctx, cl)
	if err != nil {
		return res, err
	}
	switch source {
	case anchorFromRoot:
		res.Source = "root"
	case anchorFromFile:
		res.Source = "file"
	case anchorUnknown:
		// Nothing says what the box's host certificate is issued by, so
		// there is nothing to follow: this run leaves the anchor alone.
		return res, nil
	}
	have, present, err := hostCA(ctx, cl, d.Namespace)
	if err != nil {
		return res, err
	}
	if source == anchorNothing {
		if !present {
			return res, nil
		}
		step := d.report().Start(say.Op{Label: "removing ConfigMap " + d.Namespace + "/" + HostCAName})
		if err := cl.Delete(ctx, "", "v1", "ConfigMap", d.Namespace, HostCAName); err != nil {
			step.Failed(err)
			return res, fmt.Errorf("removing the host CA from %s: %w", d.Namespace, err)
		}
		step.Done(fmt.Sprintf("removed ConfigMap %s/%s: the host's certificate is publicly trusted, so there is nothing to copy", d.Namespace, HostCAName))
		res.Wrote, res.Removed = true, true
		return res, nil
	}
	want, err := d.certificate(ctx, cl, source)
	if err != nil {
		return res, err
	}
	if present && string(have) == string(want) {
		return res, nil
	}
	step := d.report().Start(say.Op{Label: "writing ConfigMap " + d.Namespace + "/" + HostCAName})
	if err := cl.Apply(ctx, []map[string]any{hostCADocument(d.Namespace, want)}); err != nil {
		step.Failed(err)
		return res, fmt.Errorf("writing the host CA into %s: %w", d.Namespace, err)
	}
	step.Done(fmt.Sprintf("wrote ConfigMap %s/%s: the box's root certificate, for an app that has to trust %s", d.Namespace, HostCAName, d.hostName()))
	res.Wrote = true
	return res, nil
}

// Plan is apply's read, before the confirm: the one line a run that has to
// write or remove the anchor adds to its plan, or "" when the anchor is
// already what this run would leave. It compares presence, not content -- the
// write itself is the idempotent one.
func (d *HostCADecision) Plan(ctx context.Context) (string, error) {
	if strings.TrimSpace(d.Namespace) == "" {
		return "", nil
	}
	cl, err := d.Open()
	if err != nil {
		return "", err
	}
	source, err := d.source(ctx, cl)
	if err != nil {
		return "", err
	}
	if source == anchorUnknown {
		return "", nil
	}
	_, present, err := hostCA(ctx, cl, d.Namespace)
	if err != nil {
		return "", err
	}
	switch {
	case source == anchorNothing && present:
		return "remove the host CA ConfigMap " + d.Namespace + "/" + HostCAName, nil
	case source != anchorNothing && !present:
		return "write the host CA ConfigMap " + d.Namespace + "/" + HostCAName, nil
	}
	return "", nil
}

// source is where the anchor's certificate comes from: the issuer this run
// writes the box's host certificate with, or -- when this run writes none --
// the one the Certificate in s5n-gateway already names.
//
// The Certificate is the input instead of the resolution because it is the
// truth about what the host serves. A run that writes no certificate has no
// say in what that is, and a name's DNS answer is not it either: the record
// can appear hours before init moves the certificate.
func (d *HostCADecision) source(ctx context.Context, cl HostCACluster) (anchorSource, error) {
	if strings.TrimSpace(d.Issuer) != "" {
		return anchorFor(d.Issuer, "", false, d.CAFile)
	}
	issuer, there, err := certificateIssuer(ctx, cl, d.Host)
	if err != nil {
		return anchorNothing, err
	}
	return anchorFor("", issuer, there, d.CAFile)
}

// certificateIssuer is the issuer the box's host Certificate names: what
// cert-manager will sign that host's next certificate with, and so what the
// anchor has to match. A box with no Certificate for its host has none.
func certificateIssuer(ctx context.Context, cl objectReader, host string) (string, bool, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", false, nil
	}
	name := CertificateName(host)
	obj, ok, err := cl.Get(ctx, "cert-manager.io", "v1", "Certificate", GatewayNamespace, name)
	if err != nil {
		return "", false, fmt.Errorf("reading certificate %s/%s: %w", GatewayNamespace, name, err)
	}
	if !ok {
		return "", false, nil
	}
	ref, _, err := unstructured.NestedStringMap(obj, "spec", "issuerRef")
	if err != nil {
		return "", false, fmt.Errorf("certificate %s/%s: spec.issuerRef: %w", GatewayNamespace, name, err)
	}
	// Every Certificate box writes names the ClusterIssuer s5n-selfsigned. A
	// namespaced Issuer there is somebody else's, and never box's root.
	if kind := ref["kind"]; kind != "" && kind != "ClusterIssuer" {
		return "", true, nil
	}
	return ref["name"], true, nil
}

// certificate is the certificate the anchor carries: the box root, or the file
// a development run supplied.
func (d *HostCADecision) certificate(ctx context.Context, cl HostCACluster, source anchorSource) ([]byte, error) {
	if source == anchorFromFile {
		return readCAFile(d.CAFile)
	}
	return rootCertificate(ctx, cl)
}

// objectReader is the one read both the host-CA step and the per-host TLS step
// need beyond the Cluster interface: a named object.
type objectReader interface {
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
}

// rootCertificate reads the box's root out of its Secret in cert-manager, and
// refuses anything that is not a CA certificate: an anchor that is not the CA
// every host's certificate chains to would look like trust and be a lie.
func rootCertificate(ctx context.Context, cl objectReader) ([]byte, error) {
	obj, ok, err := cl.Get(ctx, "", "v1", "Secret", CertManagerNamespace, RootSecretName)
	if err != nil {
		return nil, fmt.Errorf("reading the box root: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("the box root's Secret %s/%s is not there: a self-signed box's certificates are signed by it, so run `box bootstrap` first", CertManagerNamespace, RootSecretName)
	}
	pemBytes, err := secretPEM(obj, "tls.crt")
	if err != nil {
		return nil, fmt.Errorf("the box root's Secret %s/%s: %w", CertManagerNamespace, RootSecretName, err)
	}
	if !isCA(pemBytes) {
		return nil, fmt.Errorf("the box root's Secret %s/%s holds a certificate that is not a CA: %s is not the root box made", CertManagerNamespace, RootSecretName, RootCertificateName)
	}
	return pemBytes, nil
}

// secretPEM reads one key of a Secret as a PEM document, keeping only its
// CERTIFICATE blocks.
func secretPEM(obj map[string]any, key string) ([]byte, error) {
	data, _, _ := unstructured.NestedStringMap(obj, "data")
	raw, err := base64.StdEncoding.DecodeString(data[key])
	if err != nil {
		return nil, fmt.Errorf("%s is not base64: %w", key, err)
	}
	return certificatePEM(raw)
}

// hostName is the box's own host, for box's output.
func (d *HostCADecision) hostName() string {
	if strings.TrimSpace(d.Host) == "" {
		return "the box's own certificate"
	}
	return d.Host
}

// readCAFile reads the certificate a development run supplied. The file is
// passed through the same extractor as the root's Secret, so a file carrying a
// key cannot put one in a ConfigMap either.
func readCAFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--ca-file %s: %w", path, err)
	}
	cert, err := certificatePEM(raw)
	if err != nil {
		return nil, fmt.Errorf("--ca-file %s: %w", path, err)
	}
	return cert, nil
}

// hostCA reads the anchor: its certificate and whether it is there at all.
func hostCA(ctx context.Context, cl HostCACluster, namespace string) ([]byte, bool, error) {
	obj, ok, err := cl.Get(ctx, "", "v1", "ConfigMap", namespace, HostCAName)
	if err != nil || !ok {
		return nil, ok, err
	}
	data, _, _ := unstructured.NestedStringMap(obj, "data")
	return []byte(data[HostCAKey]), true, nil
}

// hostCADocument is the ConfigMap: the certificate under one key, with box's
// ownership labels like everything else box writes.
//
//nolint:goconst // the keys are the API's field names, repeated because that is what a ConfigMap's document is.
func hostCADocument(namespace string, cert []byte) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      HostCAName,
			"namespace": namespace,
			"labels": map[string]any{
				kube.PartOfLabel:    kube.PartOf,
				kube.ManagedByLabel: kube.ManagedBy,
			},
		},
		"data": map[string]any{HostCAKey: string(cert)},
	}
}

// certificatePEM keeps only the CERTIFICATE blocks of a PEM document, in order,
// and refuses a file with none. It is how a key can never reach the anchor: the
// one reader of both the root's Secret and a development file passes through
// here, and a PRIVATE KEY block is dropped rather than copied into a ConfigMap.
func certificatePEM(raw []byte) ([]byte, error) {
	var out []byte
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != pemCertificate {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("a certificate in the PEM does not parse: %w", err)
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: block.Bytes})...)
	}
	if len(out) == 0 {
		return nil, errors.New("no CERTIFICATE block: the file holds no certificate to trust")
	}
	return out, nil
}

// isCA reports whether a single-certificate PEM is a CA's certificate. A root
// that is not a CA would sign nothing, and copying it into a ConfigMap would
// look like an anchor that works.
func isCA(pemBytes []byte) bool {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return cert.IsCA
}

// signedByRoot is the upgrade decision, as a pure function of the two
// certificates: was this host's certificate issued by the CA that is in the
// leaf's Secret today, or by something else?
//
// 0.3.0 wrote every host certificate with a bare selfSigned issuer, so the
// Secret's ca.crt is the leaf itself; the box root is a different certificate,
// and nothing in cert-manager re-issues a Certificate because the issuer's
// spec changed -- only a new CertificateRequest does (the Issuing condition,
// which is what `cmctl renew` sets). So box compares and asks for a new
// certificate itself.
//
// A Secret with no ca.crt at all (an ACME certificate) is not signed by the
// root either, and that is answered true: a caller asks this only about a
// certificate that should chain to the root.
func signedByRoot(secretCACert, rootPEM []byte) (bool, error) {
	root, err := firstCertificate(rootPEM)
	if err != nil {
		return false, fmt.Errorf("the box root: %w", err)
	}
	if len(secretCACert) == 0 {
		return false, nil
	}
	ca, err := firstCertificate(secretCACert)
	if err != nil {
		return false, nil
	}
	return ca.Equal(root), nil
}

// firstCertificate parses the first CERTIFICATE block of a PEM document.
func firstCertificate(pemBytes []byte) (*x509.Certificate, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no CERTIFICATE block")
		}
		if block.Type != pemCertificate {
			continue
		}
		return x509.ParseCertificate(block.Bytes)
	}
}
