package platform

import (
	"testing"

	admissionassets "github.com/swizzin/s5n/install/platform/admission"
	certmanagerassets "github.com/swizzin/s5n/install/platform/cert-manager"
	gatewayassets "github.com/swizzin/s5n/install/platform/gateway"
	"github.com/swizzin/s5n/internal/kube"
)

// labelsOf is a decoded document's metadata.labels, or nil.
func labelsOf(doc map[string]any) map[string]any {
	meta, _ := doc["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	return labels
}

// An embedded manifest cannot reference a Go constant: the cluster and the e2e's
// kubectl need the literal. These tests are the link, so a rename that moves one and
// not the other fails here rather than on a cluster where box stops recognising its own
// GatewayClass.
func TestEmbeddedManifestsCarryTheOwnershipStrings(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "gateway.yaml", data: gatewayassets.Manifest},
		{name: "issuers.yaml", data: certmanagerassets.Issuers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := ParseManifests(tc.data)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for _, doc := range docs {
				kind, _, name := objectMeta(doc)
				labels := labelsOf(doc)
				if got := labels[kube.PartOfLabel]; got != kube.PartOf {
					t.Errorf("%s %s: %s = %v, want %q", kind, name, kube.PartOfLabel, got, kube.PartOf)
				}
				// The namespace is plumbing, not an owned object: part-of only.
				if kind == "Namespace" {
					continue
				}
				if got := labels[kube.ManagedByLabel]; got != kube.ManagedBy {
					t.Errorf("%s %s: %s = %v, want %q", kind, name, kube.ManagedByLabel, got, kube.ManagedBy)
				}
			}
		})
	}
}

// The policy has no labels box reads, but the namespace it lives in does.
func TestAdmissionPolicyCarriesTheOwnershipStrings(t *testing.T) {
	docs, err := ParseManifests(admissionassets.Policy)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, doc := range docs {
		kind, _, name := objectMeta(doc)
		if kind != "Namespace" {
			continue
		}
		found = true
		if got := labelsOf(doc)[kube.PartOfLabel]; got != kube.PartOf {
			t.Errorf("Namespace %s: %s = %v, want %q", name, kube.PartOfLabel, got, kube.PartOf)
		}
	}
	if !found {
		t.Fatal("the admission policy has no Namespace document")
	}
}

// The two documents box builds in code rather than embedding: the constants must be
// the values the format string prints.
func TestPerHostManifestsCarryTheOwnershipStrings(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "Certificate", data: certificateManifest("box.example.test", "s5n-letsencrypt", CertificateName("box.example.test"))},
		{name: "HTTPRoute", data: redirectManifest("box.example.test", CertificateName("box.example.test"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := ParseManifests(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			labels := labelsOf(docs[0])
			if got := labels[kube.PartOfLabel]; got != kube.PartOf {
				t.Errorf("%s = %v, want %q", kube.PartOfLabel, got, kube.PartOf)
			}
			if got := labels[kube.ManagedByLabel]; got != kube.ManagedBy {
				t.Errorf("%s = %v, want %q", kube.ManagedByLabel, got, kube.ManagedBy)
			}
		})
	}
}
