package apply

// versions_test.go is the confirm's version column and the refusal it can make:
// what a tag says as a person reads it, which pairs are an upgrade, which are a
// downgrade, and the row each becomes.

import (
	"strings"
	"testing"
	"time"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/say"
)

func TestATagReadsAsTheVersionAPersonSeesIt(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  string
	}{
		{"ghcr.io/home-operations/sonarr:4.0.14.2938", "4.0.14"},
		{"ghcr.io/linuxserver/sonarr:version-4.0.14.2939", "4.0.14"},
		{"ghcr.io/autobrr/autobrr:v1.91.1", "1.91.1"},
		{"ghcr.io/home-operations/qui:v1.36.1", "1.36.1"},
		{"ghcr.io/home-operations/qbittorrent:5.3.0", "5.3"},
		{"ghcr.io/swizzin/qbittorrent:5.3.0-lt1.2", "5.3"},
		{"ghcr.io/home-operations/plex:1.43.5.11079", "1.43.5"},
		{"ghcr.io/ghostchu/peerbanhelper:v7.3.0", "7.3"},
		{"ghcr.io/example/maintainerr:release-5.0", "5.0"},
		{"ghcr.io/example/app:2.x", "2.x"},
		// No version at all: a tag that names a channel, and an image pinned by
		// its digest alone.
		{"ghcr.io/example/app:latest", ""},
		{"ghcr.io/example/app@sha256:0123456789abcdef", ""},
		{"", ""},
	} {
		if got := tagVersion(tc.image); got != tc.want {
			t.Errorf("tagVersion(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

func TestTheReleaseAgainstTheSource(t *testing.T) {
	for _, tc := range []struct {
		source, release string
		want            int
		known           bool
	}{
		{"ghcr.io/home-operations/sonarr:4.0.14.2938", "ghcr.io/home-operations/sonarr:5.0.0.3041", 1, true},
		{"ghcr.io/ghostchu/peerbanhelper:v7.3.0", "ghcr.io/ghostchu/peerbanhelper:v9.6.0", 1, true},
		{"ghcr.io/home-operations/radarr:5.20.1.9773", "ghcr.io/home-operations/radarr:7.0.3.10918", 1, true},
		{"ghcr.io/home-operations/qbittorrent:5.3.0", "ghcr.io/swizzin/qbittorrent:5.3.0-lt1.2", 0, true},
		{"ghcr.io/home-operations/plex:1.43.5.11079", "ghcr.io/home-operations/plex:1.43.5.12000", 0, true},
		{"ghcr.io/example/app:release-5.0", "ghcr.io/example/app:5.0.0", 0, true},
		{"ghcr.io/example/app:2.x", "ghcr.io/example/app:3.33.0", 1, true},
		{"ghcr.io/autobrr/autobrr:v9.6.0", "ghcr.io/autobrr/autobrr:v1.91.1", -1, true},
		{"ghcr.io/linuxserver/sonarr:version-4.0.14.2939", "ghcr.io/home-operations/sonarr:4.0.14.2938", 0, true},
		// One side unknown is no answer: box does not stop a migration over a
		// tag it cannot read.
		{"ghcr.io/example/app:latest", "ghcr.io/example/app:1.2.3", 0, false},
		{"ghcr.io/example/app@sha256:0123456789abcdef", "ghcr.io/example/app:1.2.3", 0, false},
	} {
		got := versionOf("app", tc.source, tc.release)
		if got.Known != tc.known || (tc.known && got.Cmp != tc.want) {
			t.Errorf("versionOf(%q, %q) = %+v, want known=%v cmp=%d", tc.source, tc.release, got, tc.known, tc.want)
		}
	}
}

func TestTheColumnSaysOneVersionOrTwo(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    AppVersion
		want string
	}{
		{"same", versionOf("sonarr", "x/sonarr:5.0.0.3041", "x/sonarr:5.0.0.3041"), "5.0"},
		{"upgrade", versionOf("pbh", "x/pbh:v7.3.0", "x/pbh:v9.6.0"), "7.3 → 9.6  ↑"},
		{"downgrade", versionOf("pbh", "x/pbh:v9.6.0", "x/pbh:v7.3.0"), "9.6 → 7.3  ↓"},
		{"unknown", versionOf("sonarr", "x/sonarr:latest", "x/sonarr:5.0.0.3041"), "?"},
		{"no chart", AppVersion{App: "sonarr"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.Cell(); got != tc.want {
				t.Errorf("the cell is %q, want %q", got, tc.want)
			}
		})
	}
}

// versionFixture is two apps moving up, one staying, one moving down and one
// whose source version box could not read.
func versionFixture() Summary {
	s := Summary{Instances: []InstanceSummary{
		{App: "pbh", Staged: Staged{App: "pbh", Files: 3, Bytes: 1 << 30}, Version: versionOf("pbh", "x/pbh:v7.3.0", "x/pbh:v9.6.0")},
		{App: "qbittorrent", Staged: Staged{App: "qbittorrent", Files: 9, Bytes: 570 << 20}, Version: versionOf("qbittorrent", "x/qbittorrent:5.3.0", "x/qbittorrent:5.3.0")},
		{App: "seerr", Staged: Staged{App: "seerr", Files: 2, Bytes: 8 << 20}, Version: versionOf("seerr", "x/seerr:latest", "x/seerr:v3.5.2")},
		{App: "sonarr", Staged: Staged{App: "sonarr", Files: 9, Bytes: 1 << 30}, Version: versionOf("sonarr", "x/sonarr:9.9.9.1", "x/sonarr:5.0.0.3041")},
	}}
	return s
}

func TestTheVersionColumnIsATableColumn(t *testing.T) {
	var out strings.Builder
	for _, block := range versionFixture().Grouped().Blocks {
		(&say.Plain{W: &out}).Group(block)
	}
	want := strings.Join([]string{
		"apps           carries    version         changes",
		"  pbh          1.0 GiB    7.3 → 9.6  ↑",
		"  qbittorrent  570.0 MiB  5.3",
		"  seerr        8.0 MiB    ?",
		"  sonarr       1.0 GiB    9.9.9 → 5.0  ↓",
		"  ! sonarr  9.9.9 → 5.0 · the config is newer than the app",
		"",
	}, "\n")
	if got := out.String(); got != want {
		t.Fatalf("the block:\n%s\nwant:\n%s", got, want)
	}
	downs := versionFixture().Downgrades()
	if len(downs) != 1 || downs[0].App != "sonarr" {
		t.Fatalf("the downgrades are %+v, want sonarr alone", downs)
	}
}

func TestTheReportCarriesTheSameColumn(t *testing.T) {
	doc := versionFixture().Report("final", "/var/lib/s5n/migrate/x.tar.gz", time.Date(2026, 11, 23, 4, 50, 0, 0, time.UTC), "0.6.3")
	var apps string
	for _, sec := range doc.Sections {
		if sec.Title != "apps" {
			continue
		}
		var b strings.Builder
		for _, row := range sec.Rows {
			b.WriteString(row.Name + " " + strings.Join(row.Facts, " · ") + "\n")
		}
		apps = b.String()
	}
	for _, want := range []string{
		"pbh 1.0 GiB · 7.3 → 9.6  ↑",
		"qbittorrent 570.0 MiB · 5.3",
		"sonarr 1.0 GiB · 9.9.9 → 5.0  ↓",
		"seerr 8.0 MiB · ?",
	} {
		if !strings.Contains(apps, want) {
			t.Errorf("the apps section is missing %q:\n%s", want, apps)
		}
	}
}

func TestAPlanWithNoChartSaysNothingAboutVersions(t *testing.T) {
	m := &migration{p: &plan.Plan{Apps: map[string]plan.App{"sonarr": {Kind: plan.Kubernetes}}}, env: Env{}}
	if got := m.version("sonarr"); got.Cell() != "" {
		t.Errorf("a version with no chart is %q, want nothing", got.Cell())
	}
}
