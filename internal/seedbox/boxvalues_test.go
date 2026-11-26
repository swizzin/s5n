package seedbox

import (
	"reflect"
	"strings"
	"testing"
)

// Two values reach the release from the grant rather than a file: login (one
// app's own account is named after it) and knownProxies (the pod network the app
// trusts a forwarded address from). Both are written only for a chart that
// declares them, because box reads releases back to its oldest upgrade-from.
func TestEffectiveCarriesTheGrantsLoginAndPodNetwork(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	// The fixture grant has no login; the one this box was made for does.
	s.Grant.data["login"] = "alice"
	s.Grant.data["apps"] = map[string]any{
		"jellyfin": map[string]any{
			"enabled":      true,
			"knownProxies": []any{"10.244.1.0/24", "10.244.0.0/24"},
		},
	}
	values, err := s.Effective(realChart(t))
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	if got := values["login"]; got != "alice" {
		t.Fatalf("login = %v, want the grant's account", got)
	}
	jellyfin, _ := values["apps"].(map[string]any)["jellyfin"].(map[string]any)
	if jellyfin == nil {
		t.Fatal("apps.jellyfin is not in the effective values")
	}
	want := []any{"10.244.1.0/24", "10.244.0.0/24"}
	if !reflect.DeepEqual(jellyfin["knownProxies"], want) {
		t.Fatalf("apps.jellyfin.knownProxies = %v, want %v", jellyfin["knownProxies"], want)
	}
}

// Both keys are root's, so a user editing their own file is refused by name.
func TestAValuesFileMayNotNameTheGrantsLoginOrPodNetwork(t *testing.T) {
	s := loadFixture(t, "values.yaml")
	for _, key := range []string{"login", "apps.jellyfin.knownProxies"} {
		err := s.SetValue(key, "x")
		if err == nil {
			t.Fatalf("%s was written into the user's file", key)
		}
		if !strings.Contains(err.Error(), "set by") {
			t.Fatalf("%s: error = %v, want it to say the key is not the file's", key, err)
		}
	}
}
