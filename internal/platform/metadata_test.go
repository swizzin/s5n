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

// The ownership strings are constants in internal/kube, but an embedded
// manifest cannot reference a Go constant: the cluster needs the literal in the
// file, and the e2e's `kubectl apply` of the same file needs it too. These
// tests are the link instead. A rename that moves the constant and not the
// manifest -- or the manifest and not the constant -- fails here, rather than on
// a cluster where box stops recognising a GatewayClass it created.
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
				// The namespace is box's plumbing rather than one of its owned
				// objects: it carries part-of only, and nothing reads a
				// managed-by label off it.
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

// TestAdmissionPolicyCarriesTheOwnershipStrings is the third embedded manifest:
// the policy itself has no labels box reads, but the namespace it lives in does,
// and the e2e asserts that namespace's labels.
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

// TestPerHostManifestsCarryTheOwnershipStrings covers the two documents box
// builds in code rather than embedding: the fmt string is the manifest, so the
// constants have to be the values it prints.
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

// TestPlatformOwnershipConstantsMatchKube is the seam that has not collapsed
// yet: platform.go's const block also declares the field manager and the
// release-ownership pair, and that block belongs to the versions lane while the
// pins move out of it. The values cannot drift in the meantime.
func TestPlatformOwnershipConstantsMatchKube(t *testing.T) {
	if FieldManager != kube.FieldManager {
		t.Errorf("platform.FieldManager = %q, kube.FieldManager = %q", FieldManager, kube.FieldManager)
	}
	if ReleaseOwnerLabel != kube.ManagedByLabel {
		t.Errorf("platform.ReleaseOwnerLabel = %q, kube.ManagedByLabel = %q", ReleaseOwnerLabel, kube.ManagedByLabel)
	}
	if ReleaseOwner != kube.ManagedBy {
		t.Errorf("platform.ReleaseOwner = %q, kube.ManagedBy = %q", ReleaseOwner, kube.ManagedBy)
	}
}
