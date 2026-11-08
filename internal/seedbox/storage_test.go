package seedbox

import (
	"os"
	"strings"
	"testing"
)

// TestStorageDerivesClaims: the grant records two host paths and nothing else.
// The claim names come from the namespace, so that two boxes on one host can
// never ask for the same cluster-scoped PersistentVolume.
func TestStorageDerivesClaims(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	vols, err := s.Storage()
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}
	want := []Volume{
		{Kind: "data", Path: "/srv/s5n/data", Claim: "alice-s5n-data"},
		{Kind: "config", Path: "/srv/s5n/config", Claim: "alice-s5n-config"},
	}
	if len(vols) != len(want) {
		t.Fatalf("volumes = %+v, want %+v", vols, want)
	}
	for i := range want {
		if vols[i] != want[i] {
			t.Errorf("volume %d = %+v, want %+v", i, vols[i], want[i])
		}
	}
}

// TestEffectiveCarriesBoxClaims: the claim names are in the values handed to
// Helm, and they are box's -- the chart's defaults never reach the release.
func TestEffectiveCarriesBoxClaims(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	values, err := s.Effective(realChart(t))
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	storage := values["storage"].(map[string]any)
	for kind, want := range map[string]string{"data": "alice-s5n-data", "config": "alice-s5n-config"} {
		got := storage[kind].(map[string]any)["existingClaim"]
		if got != want {
			t.Errorf("storage.%s.existingClaim = %v, want %s", kind, got, want)
		}
	}
}

func TestStorageRefusesABadPath(t *testing.T) {
	cases := []struct {
		name  string
		grant string
		want  string
	}{
		{
			name:  "missing",
			grant: "namespace: alice\ngateway:\n  host: alice.example.com\nstorage:\n  data:\n    path: /srv/s5n/data\n  config: {}\n",
			want:  "storage.config.path is missing",
		},
		{
			name:  "relative",
			grant: "namespace: alice\ngateway:\n  host: alice.example.com\nstorage:\n  data:\n    path: data\n  config:\n    path: /srv/s5n/config\n",
			want:  "a host path must be absolute",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Load(writeTemp(t, "grant.yaml", c.grant), "")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			_, err = s.Storage()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
		})
	}
}

// TestStorageRulesAreBoxOwned: the host path is the grant's record, and the
// claim name is box's own. A user who writes either one gets an error that
// names the key, not a silent overwrite.
func TestStorageRulesAreBoxOwned(t *testing.T) {
	path, ok := GrantRule([]string{"storage", "data", "path"})
	if !ok || !path.BoxOnly {
		t.Errorf("storage.data.path rule = %+v (found %v), want BoxOnly", path, ok)
	}
	for _, keys := range [][]string{
		{"storage", "data", "existingClaim"},
		{"storage", "config", "existingClaim"},
		{"storage", "data", "size"},
	} {
		rule, ok := GrantRule(keys)
		if !ok || !rule.BoxSets {
			t.Errorf("%v rule = %+v (found %v), want BoxSets", keys, rule, ok)
		}
	}

	// The grant file itself may not name a claim either.
	s := loadFixture(t, "values.yaml")
	grant := copyFixture(t, "grant.yaml")
	before, err := os.ReadFile(grant)
	if err != nil {
		t.Fatal(err)
	}
	s.Grant.Path = grant
	s.Grant.Src = before
	if err := s.SetGrant("storage.data.existingClaim", "alice-s5n-data"); err == nil ||
		!strings.Contains(err.Error(), "is set by box") {
		t.Errorf("SetGrant(existingClaim) error = %v, want a box-sets refusal", err)
	}
	after, err := os.ReadFile(grant)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the grant changed on a refused edit")
	}

	// ... and the user's file may not, which is checked with the line.
	values := writeTemp(t, "values.yaml", "storage:\n  data:\n    existingClaim: mine-data\n")
	s, err = Load(fixture(t, "grant.yaml"), values)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = s.Merge()
	if err == nil {
		t.Fatal("Merge accepted a values file that names a claim")
	}
	if !strings.Contains(err.Error(), "values.yaml:3: storage.data.existingClaim is set by box") {
		t.Errorf("error = %v, want it to name the key and line", err)
	}
}
