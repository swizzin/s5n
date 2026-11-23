package rewrite

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestWiringAccountIsTheCharts: the account a migration renames a source's row
// to is the one the chart's init containers create and sign in as, and it is the
// chart's to spell (charts/s5n/templates/_helpers.tpl, s5n.wiringAccount). If the
// chart ever renames it, a migration that still renamed to the old name would
// leave an app whose account the init then cannot find -- the exact failure this
// rename exists to prevent -- so the two spellings are compared here rather than
// left to a reviewer.
func TestWiringAccountIsTheCharts(t *testing.T) {
	raw, err := os.ReadFile("../../../charts/s5n/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	// The template's body is `{{- define "s5n.wiringAccount" -}}\ns5n\n{{- end -}}`.
	m := regexp.MustCompile(`(?s)\{\{-\s*define "s5n\.wiringAccount"\s*-\}\}(.*?)\{\{-\s*end`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("charts/s5n/templates/_helpers.tpl has no s5n.wiringAccount define")
	}
	if got := strings.TrimSpace(string(m[1])); got != WiringAccount {
		t.Errorf("the chart's account is %q and this package renames to %q: a migrated app would keep an account the init container cannot sign in as",
			got, WiringAccount)
	}
}
