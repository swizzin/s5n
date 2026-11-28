package seedbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const editValuesFile = `# alice's box: everything here is hers to tune.
global:
    # Four spaces, on purpose.
    tz: "Europe/Berlin"

apps:
  # qBittorrent is root's choice of app; this is her choice of build.
  qbittorrent:
    image:
      variant: default
      tag: "5.3.0"
    env:
      TZ: "Europe/Berlin"

  sonarr:
    env:
      SONARR__LOG__LEVEL: debug

wiring:
  enabled: true
`

func loadEditValues(t *testing.T, body string) (*Seedbox, string) {
	t.Helper()
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(values, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grant.yaml"), []byte("namespace: testbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(filepath.Join(dir, "grant.yaml"), values)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, values
}

func TestEditValuesWritesAndRemovesInOneSave(t *testing.T) {
	sb, values := loadEditValues(t, editValuesFile)
	err := sb.EditValues(
		[]Edit{{Path: "apps.qbittorrent.image.tag", Value: "5.4-lt2.1"}},
		[]string{"apps.qbittorrent.image.digest"},
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(values)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `tag: "5.4-lt2.1"`) {
		t.Fatalf("the tag was not written:\n%s", got)
	}
	for _, kept := range []string{
		"# alice's box: everything here is hers to tune.",
		"    # Four spaces, on purpose.",
		"  # qBittorrent is root's choice of app; this is her choice of build.",
		`tz: "Europe/Berlin"`,
		"      SONARR__LOG__LEVEL: debug",
		"wiring:",
	} {
		if !strings.Contains(got, kept) {
			t.Fatalf("the edit lost %q:\n%s", kept, got)
		}
	}
}

func TestEditValuesRemovesBothHalvesOfAPin(t *testing.T) {
	sb, values := loadEditValues(t, editValuesFile)
	if err := sb.EditValues([]Edit{{Path: "apps.qbittorrent.image.digest", Value: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sb.EditValues(nil, []string{"apps.qbittorrent.image.tag", "apps.qbittorrent.image.digest"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(values)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, "tag:") || strings.Contains(got, "digest:") {
		t.Fatalf("a pin is left behind:\n%s", got)
	}
	if !strings.Contains(got, "variant: default") {
		t.Fatalf("the unpin took the variant with it:\n%s", got)
	}
	if !strings.Contains(got, "# qBittorrent is root's choice of app") {
		t.Fatalf("the unpin took the comment above the app:\n%s", got)
	}
}

func TestEditValuesRefusesAGrantKey(t *testing.T) {
	sb, _ := loadEditValues(t, editValuesFile)
	err := sb.EditValues([]Edit{{Path: "apps.qbittorrent.enabled", Value: true}}, nil)
	if err == nil || !strings.Contains(err.Error(), "grant") {
		t.Fatalf("EditValues = %v, want the grant-key refusal", err)
	}
	err = sb.EditValues(nil, []string{"apps.qbittorrent.enabled"})
	if err == nil || !strings.Contains(err.Error(), "grant") {
		t.Fatalf("deleting a grant key = %v, want the refusal", err)
	}
}
