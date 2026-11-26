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

// The host CA answers one question: how does something inside the cluster trust a
// certificate box made for itself? For Let's Encrypt there is nothing to do; for a
// self-signed box the root travels into the seedbox's namespace as a ConfigMap, which
// an app signing in through OIDC mounts (docs/box.md, "Certificates").
//
// The ConfigMap holds the certificate and only the certificate: never the key.

// pemCertificate is the only PEM block type the anchor ever carries.
const pemCertificate = "CERTIFICATE"

// HostCACluster is the platform Cluster plus the two calls the host-CA step needs, so
// a client that cannot make them does not compile.
type HostCACluster interface {
	Cluster
	// Get reads one object, or reports that it is not there.
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
	// Delete removes one object; a missing one is not an error, since the step runs on
	// every apply.
	Delete(ctx context.Context, group, version, kind, namespace, name string) error
}

// anchorSource is where the anchor's certificate comes from.
type anchorSource int

const (
	// anchorNothing: the host's certificate is publicly trusted.
	anchorNothing anchorSource = iota
	// anchorFromRoot: the host's certificate is signed by box's root, which is the
	// anchor.
	anchorFromRoot
	// anchorFromFile: a development run hands box the certificate to copy (the e2e's
	// Pebble has no ca.crt). Same writer and ConfigMap, another source.
	anchorFromFile
	// anchorUnknown: neither this run's issuer nor a Certificate says which issuer the
	// box's host has, so nothing is read, written or removed.
	anchorUnknown
)

// anchorFor is the whole decision, from the issuer this run writes with (empty when
// it writes none), the issuer the cluster's Certificate names, and whether a
// development run supplied a file.
//
// The anchor follows the certificate, never DNS: a name with a public record an hour
// old is still served by the certificate box last gave it, so deciding from the
// resolution would take the login away from every app that restarted meanwhile. A
// self-signed host always wins when this run writes the certificate.
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
		// The issuerRef is whatever cert-manager accepted; box only recognises its own
		// root, and any other name has nothing of box's to carry.
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

// HostCADecision is root's host-CA step for one seedbox: write the anchor the box's
// certificate needs, or remove it when the host is publicly trusted.
type HostCADecision struct {
	// Namespace is the seedbox's namespace, where the anchor goes. Empty is a refusal:
	// one box's object, and an empty namespace would be a cluster-wide mistake.
	Namespace string
	// Host is the box's own host name, naming the Certificate this run reads when it
	// writes none.
	Host string
	// Issuer is the issuer this run writes the box's certificate with (init's value or
	// the ClusterIssuer name). Empty means this run writes none, and the anchor follows
	// the Certificate in s5n-gateway.
	Issuer string
	// CAFile is a PEM file a development run supplies instead of the root; empty in
	// every real run.
	CAFile string
	// Open builds the cluster client, called only when there is work.
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

// Apply writes or removes the anchor. Idempotent: a ConfigMap already holding the
// certificate writes nothing, so a re-run changes no resource version.
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
		// Nothing says what the host certificate is issued by: leave the anchor alone.
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
		step.Done("")
		d.report().Rows([]say.Row{{Mark: say.MarkNote, Name: hostCARow, State: "removed", Facts: []string{"the host's certificate is publicly trusted"}}})
		d.report().Detail(fmt.Sprintf("host CA: removed ConfigMap %s/%s", d.Namespace, HostCAName))
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
	step.Done("")
	d.report().Rows([]say.Row{{Mark: say.MarkDone, Name: hostCARow, State: "written", Facts: []string{"box's root certificate"}}})
	d.report().Detail(fmt.Sprintf("host CA: wrote ConfigMap %s/%s from %s", d.Namespace, HostCAName, d.hostName()))
	res.Wrote = true
	return res, nil
}

// hostCARow is the host CA's row: the object's name is the log's (rules 1 and 5).
const hostCARow = "host CA"

// Plan is apply's read before the confirm: the line a run that must write, replace
// or remove the anchor adds, or "" when it would leave it as it is.
//
// It compares the certificate, not just its presence: a root replaced under the box
// is a change the confirm has to show.
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
	have, present, err := hostCA(ctx, cl, d.Namespace)
	if err != nil {
		return "", err
	}
	if source == anchorNothing {
		if present {
			return "remove the host CA ConfigMap " + d.Namespace + "/" + HostCAName, nil
		}
		return "", nil
	}
	if !present {
		return "write the host CA ConfigMap " + d.Namespace + "/" + HostCAName, nil
	}
	// The file is read only when there is something to compare: a missing anchor is
	// a write whatever it holds.
	want, err := d.certificate(ctx, cl, source)
	if err != nil {
		return "", err
	}
	if string(have) == string(want) {
		return "", nil
	}
	return "replace the host CA ConfigMap " + d.Namespace + "/" + HostCAName, nil
}

// source is where the anchor's certificate comes from: this run's issuer, or the one
// the Certificate in s5n-gateway names.
//
// The Certificate is the input, not the resolution: it is the truth about what the
// host serves, and a DNS record can appear hours before init moves the certificate.
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

// certificateIssuer is the issuer the host Certificate names, which the anchor has to
// match; a box without one has none.
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
	// Every Certificate box writes names the ClusterIssuer s5n-selfsigned; a namespaced
	// Issuer is somebody else's.
	if kind := ref["kind"]; kind != "" && kind != "ClusterIssuer" {
		return "", true, nil
	}
	return ref["name"], true, nil
}

// certificate is the certificate the anchor carries: box's root or the supplied file.
func (d *HostCADecision) certificate(ctx context.Context, cl HostCACluster, source anchorSource) ([]byte, error) {
	if source == anchorFromFile {
		return readCAFile(d.CAFile)
	}
	return rootCertificate(ctx, cl)
}

// objectReader is the one read the host-CA and per-host TLS steps need: a named object.
type objectReader interface {
	Get(ctx context.Context, group, version, kind, namespace, name string) (map[string]any, bool, error)
}

// rootCertificate reads box's root out of its Secret and refuses anything that is not
// a CA: an anchor that is not the CA the certificates chain to would be a lie.
func rootCertificate(ctx context.Context, cl objectReader) ([]byte, error) {
	obj, ok, err := cl.Get(ctx, "", "v1", "Secret", CertManagerNamespace, RootSecretName)
	if err != nil {
		return nil, fmt.Errorf("reading box's root: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("box's root's Secret %s/%s is not there: a self-signed box's certificates are signed by it, so run `box bootstrap` first", CertManagerNamespace, RootSecretName)
	}
	pemBytes, err := secretPEM(obj, "tls.crt")
	if err != nil {
		return nil, fmt.Errorf("box's root's Secret %s/%s: %w", CertManagerNamespace, RootSecretName, err)
	}
	if !isCA(pemBytes) {
		return nil, fmt.Errorf("box's root's Secret %s/%s holds a certificate that is not a CA: %s is not the root box made", CertManagerNamespace, RootSecretName, RootCertificateName)
	}
	return pemBytes, nil
}

// secretPEM reads one key of a Secret as a PEM, keeping only CERTIFICATE blocks.
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
		return "box's own certificate"
	}
	return d.Host
}

// readCAFile reads the development certificate through the same extractor as the
// root's Secret, so a file carrying a key cannot put one in a ConfigMap either.
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

// hostCADocument is the ConfigMap: the certificate under one key, with box's labels.
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

// certificatePEM keeps only the CERTIFICATE blocks, in order, refusing a document
// with none: the one reader of the root's Secret and a development file passes here,
// so a PRIVATE KEY block is dropped rather than copied into a ConfigMap.
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

// isCA reports whether a single-certificate PEM is a CA's: one that is not would sign
// nothing, and copying it in would look like a working anchor.
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

// signedByRoot is the upgrade decision, from the two certificates: was this host's
// certificate issued by the CA in the leaf's Secret today?
//
// A certificate from a bare selfSigned issuer has ca.crt == the leaf, and cert-manager
// re-issues only for a new CertificateRequest (the Issuing condition `cmctl renew`
// sets), so box compares and asks itself. A Secret with no ca.crt is not signed by the
// root either, which is answered true: callers ask only about certificates that should
// chain to it.
func signedByRoot(secretCACert, rootPEM []byte) (bool, error) {
	root, err := firstCertificate(rootPEM)
	if err != nil {
		return false, fmt.Errorf("box's root: %w", err)
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
