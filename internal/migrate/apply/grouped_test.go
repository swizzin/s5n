package apply

// grouped_test.go pins the confirm's summary: one row per app, the data it
// renames, what needs a person, every note once, and what is not migrated
// folded by namespace. It is read through say's plain renderer, which is the
// transcript a logged run and this golden read.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/say"
)

// groupedFixture is a rehearsal of five apps: one with an account of its own,
// one whose config a snapshot does not carry whole, one with torrents and
// paths, and two that share a download client they keep and one they leave.
func groupedFixture() Summary {
	leaveWhy := `"Deluge" is a Deluge client and this box runs only qBittorrent, so a rewrite would point it at an app that does not speak its protocol; delete it in the Arr if you do not need it`
	return Summary{
		Rehearsal: true,
		Instances: []InstanceSummary{
			{
				App:      "autobrr",
				Staged:   Staged{App: "autobrr", Files: 5, Bytes: 1610612736},
				Changes:  []rewrite.Change{{File: "config.toml", Where: "baseUrl", What: "url base", Count: 1}, {File: "config.toml", Where: "baseUrlModeLegacy", What: "url base mode", Count: 1}},
				Accounts: []rewrite.Account{{From: "the-source-owner", To: "s5n"}},
			},
			{
				App:     "plex",
				Staged:  Staged{App: "plex", Files: 7, Bytes: 518029312},
				Changes: []rewrite.Change{{File: "db", Where: "section_locations.root_path", What: "path", Count: 8264}},
				Notes:   []string{`"RemotePathMappings" is not read by anything box runs, so it is left as it is`},
			},
			{
				App:     "qbittorrent",
				Staged:  Staged{App: "qbittorrent", Files: 10, Bytes: 598736896},
				Changes: []rewrite.Change{{File: "qBittorrent.conf", Where: "BitTorrent Session\\DefaultSavePath", What: "path", Count: 1562}},
				Stopped: 1532,
			},
			{
				App:     "radarr",
				Staged:  Staged{App: "radarr", Files: 10, Bytes: 35651584},
				Changes: []rewrite.Change{{File: "radarr.db", Where: "RootFolders.Path", What: "path", Count: 2930}, {File: "radarr.db", Where: "DownloadClients.Settings.host", What: "host", Count: 1}},
				Owned:   []rewrite.Owned{{Kind: "downloadclient", ID: 1, Name: "qBT", Target: "qbittorrent"}},
				Keys:    []string{"radarr-apikey"},
				Left:    []rewrite.Left{{File: "radarr.db", Where: "DownloadClients.Settings.host", What: `"Deluge" (172.17.0.1:10365)`, Why: leaveWhy}},
				Enable:  true,
			},
			{
				App:     "sonarr",
				Staged:  Staged{App: "sonarr", Files: 10, Bytes: 5452595},
				Changes: []rewrite.Change{{File: "sonarr.db", Where: "RootFolders.Path", What: "path", Count: 1}},
				Notes:   []string{`"RemotePathMappings" is not read by anything box runs, so it is left as it is`},
				Left:    []rewrite.Left{{File: "sonarr.db", Where: "DownloadClients.Settings.host", What: `"rT" (172.17.0.1:443)`, Why: leaveWhy}},
				Enable:  true,
			},
		},
		Renames: []string{"Downloads to downloads", "library to media"},
		Defaults: []string{
			"10.244.0.5:8989 (rewrite to sonarr: it is the address its own client stored)",
			"old/library (the source never kept a media root this app writes to)",
		},
		DefaultRules: []plan.Default{
			{App: "sonarr", What: "10.244.0.5:8989", Why: "it is the address its own client stored", Kind: "address"},
			{What: "sonarr: old/library", Why: "the source never kept a media root this app writes to", Kind: "path"},
		},
		Carried: []Carried{{App: "plex", Note: "carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)"}},
		Outside: []string{
			"cluster/cert-manager",
			"default/actualbudget",
			"default/changedetection",
			"default/prowlarr/sidecar",
		},
	}
}

// renderGrouped is what the one confirm does with a summary in grouped mode.
func renderGrouped(s Summary) string {
	var buf bytes.Buffer
	out := &say.Plain{W: &buf}
	g := s.Grouped()
	if g.Lead != "" {
		out.Muted(g.Lead)
	}
	for _, block := range g.Blocks {
		out.Table(block.Title, block.Rows)
	}
	if g.Footer != "" {
		out.Muted(g.Footer)
	}
	return buf.String()
}

func TestTheGroupedSummaryGolden(t *testing.T) {
	want := `rehearsal · nothing leaves this box · no data is renamed
5 apps
  ✓ autobrr      1.5 GiB · 2 addresses · account "the-source-owner" becomes s5n (sign in through the box)
  ✓ plex         494.0 MiB · 8,264 paths
  ✓ qbittorrent  571.0 MiB · 1,562 paths · 1,532 torrents stopped
  ✓ radarr       34.0 MiB · 2,930 paths · 1 address · keeps download client "qBT" · keeps its API key
  ✓ sonarr       5.2 MiB · 1 path
data
  ✓ data  Downloads → downloads · library → media
needs you
  ! radarr, sonarr  "Deluge" (172.17.0.1:10365) and "rT" (172.17.0.1:443) stay as it is: "Deluge" is a Deluge client and this box runs only qBittorrent, so a rewrite would point it at an app that does not speak its protocol; delete it in the Arr if you do not need it
good to know
  · plex, sonarr  "RemotePathMappings" is not read by anything box runs, so it is left as it is
  · plex          carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)
  · 1 path        the source never kept a media root this app writes to
not migrated (4)
  · cluster  cert-manager
  · default  actualbudget, changedetection, prowlarr/sidecar
every rewrite, column by column: report.txt in the snapshot, or --details
`
	if got := renderGrouped(groupedFixture()); got != want {
		t.Fatalf("the grouped summary is:\n%s\nwant:\n%s", got, want)
	}
}

// TestTheNotesAreSaidOnce: a note two apps report is one row naming both, not
// two rows saying the same thing -- the report's duplicate that made the old
// confirm four lines about one RemotePathMappings.
func TestTheNotesAreSaidOnce(t *testing.T) {
	rendered := renderGrouped(groupedFixture())
	if got := strings.Count(rendered, "RemotePathMappings"); got != 1 {
		t.Errorf("the shared note is printed %d times, want once:\n%s", got, rendered)
	}
	// And it names both apps that said it.
	if !strings.Contains(rendered, "· plex, sonarr  \"RemotePathMappings\"") {
		t.Errorf("the shared note does not name the apps it came from:\n%s", rendered)
	}
}

// TestADefaultThatRewroteAnAddressIsCounted: the plan's own address decision is
// one of that app's rewrites, so it is in the count and is not a line of its
// own; a default that left a value alone is said once, by what it is.
func TestADefaultThatRewroteAnAddressIsCounted(t *testing.T) {
	rendered := renderGrouped(groupedFixture())
	if strings.Contains(rendered, "10.244.0.5:8989") {
		t.Errorf("a default that rewrote an address is printed as its own line:\n%s", rendered)
	}
	if !strings.Contains(rendered, "· 1 path        the source never kept a media root this app writes to") {
		t.Errorf("a default that left a value alone is not said:\n%s", rendered)
	}
}

// TestTheNotMigratedListIsFoldedByNamespace: one line per namespace, so a box
// running fifteen things box has no app for is fifteen names on a line rather
// than fifteen lines before the question.
func TestTheNotMigratedListIsFoldedByNamespace(t *testing.T) {
	rendered := renderGrouped(groupedFixture())
	if got := strings.Count(rendered, "actualbudget"); got != 1 {
		t.Errorf("the not-migrated list repeats a name %d times", got)
	}
	for _, want := range []string{
		"not migrated (4)",
		"· cluster  cert-manager",
		"· default  actualbudget, changedetection, prowlarr/sidecar",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the folded list is missing %q:\n%s", want, rendered)
		}
	}
}

// TestAFinalRunHasNoRehearsalLead: the lead line is what a rehearsal is; a real
// run's own facts are the renames under data.
func TestAFinalRunHasNoRehearsalLead(t *testing.T) {
	s := groupedFixture()
	s.Rehearsal = false
	if got := renderGrouped(s); strings.Contains(got, "nothing leaves this box") {
		t.Errorf("a final run says it is a rehearsal:\n%s", got)
	}
}

// TestANoteTheConfirmShowedIsNotPrintedAgain: the Write pass reports the same
// notes its Check did, and the confirm already showed those once. Only a note
// nobody could see before the write belongs after the run.
func TestANoteTheConfirmShowedIsNotPrintedAgain(t *testing.T) {
	shown := []string{"its own login is switched off; you sign in through the box"}
	wrote := append(append([]string(nil), shown...), "one table was emptied on purpose")
	got := appendNotes(nil, wrote, shown)
	if len(got) != 1 || got[0] != "one table was emptied on purpose" {
		t.Fatalf("notes after the run = %v, want only the one the confirm had not shown", got)
	}
}
