package report

// report_test.go is the migration report's golden: the file a person reads in
// full, section by section, from a document the two commands build. It pins the
// shape -- the summary first, lowercase headings, columns, 80 columns -- and the
// file's name beside its snapshot.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doc is one finished report, the way `box migrate cutover` builds it: a source
// read on one day, six apps, the data renamed onto box's layout, three addresses
// left as they were, and a client the box cannot run.
func doc() Doc {
	return Doc{
		Kind:     "final",
		Snapshot: "/var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz",
		Taken:    time.Date(2026, 11, 23, 4, 50, 38, 0, time.UTC),
		Box:      "0.6.2",
		Counts: []Count{
			{"apps", "6"},
			{"addresses", "11 rewritten · 3 left as they are"},
			{"data", "2 renames · 4 library directories"},
			{"notes", "2"},
			{"needs you", "1"},
			{"left", "31 workloads"},
		},
		Sections: []Section{
			{"apps", []Row{
				{"autobrr", []string{"from autobrr in legacy", "config /var/data/autobrr"}},
				{"qbittorrent", []string{"from qbittorrent in legacy", "config /var/data/qbittorrent"}},
				{"radarr-4k", []string{"from radarr4k in legacy", "config /var/data/radarr4k", "category movies-4k"}},
				{"sonarr", []string{"from sonarr in legacy", "config /var/data/sonarr", "category tv"}},
			}},
			{"data", []Row{
				{"root", []string{"/var/data/library"}},
				{"renames", []string{"Downloads → downloads", "library → media"}},
				{"library", []string{"Movies → movies", "TV → tv", "4K/Movies → movies-4k", "4K/TV → tv-4k"}},
			}},
			{"addresses", []Row{
				{"qbittorrent.legacy:8080", []string{"→ qbittorrent", "a Service of the source cluster"}},
				{"10.244.0.94:32400", []string{"→ plex", "Maintainerr's Plex connection"}},
				{"172.17.0.1:10365", []string{"left as it is", "a Deluge client, and this box runs qBittorrent"}},
			}},
			{"checks", []Row{
				{"plex", []string{"checked at apply", "its database is only readable through its own image"}},
				{"/transcode", []string{"left as it is", "the release mounts the same /transcode"}},
			}},
			{"notes", []Row{
				{"tautulli", []string{"its own login is switched off, and you sign in through the box"}},
				{"seerr", []string{"sign in once with Plex after the first start"}},
			}},
			{"not migrated", []Row{
				{"ci", []string{"qbt-ci-20261123-044730"}},
				{"cert-manager", []string{"cert-manager", "cert-manager-webhook"}},
			}},
		},
	}
}

// TestTheReportIsThePageAPersonReads is the golden: the summary a person takes
// in at a glance, then the sections, every row in its column, nothing wrapped
// inside a path.
func TestTheReportIsThePageAPersonReads(t *testing.T) {
	want := `s5n migration report · final · migrate-20261123T045038Z-final.tar.gz
read 2026-11-23 04:50 UTC by box 0.6.2

summary
  apps       6
  addresses  11 rewritten · 3 left as they are
  data       2 renames · 4 library directories
  notes      2
  needs you  1
  left       31 workloads

apps
  autobrr      from autobrr in legacy · config /var/data/autobrr
  qbittorrent  from qbittorrent in legacy · config /var/data/qbittorrent
  radarr-4k    from radarr4k in legacy · config /var/data/radarr4k · category
               movies-4k
  sonarr       from sonarr in legacy · config /var/data/sonarr · category tv

data
  root     /var/data/library
  renames  Downloads → downloads · library → media
  library  Movies → movies · TV → tv · 4K/Movies → movies-4k · 4K/TV → tv-4k

addresses
  qbittorrent.legacy:8080  → qbittorrent · a Service of the source cluster
  10.244.0.94:32400        → plex · Maintainerr's Plex connection
  172.17.0.1:10365         left as it is · a Deluge client, and this box runs
                           qBittorrent

checks
  plex        checked at apply · its database is only readable through its own
              image
  /transcode  left as it is · the release mounts the same /transcode

notes
  tautulli  its own login is switched off, and you sign in through the box
  seerr     sign in once with Plex after the first start

not migrated
  ci            qbt-ci-20261123-044730
  cert-manager  cert-manager · cert-manager-webhook
`
	if got := doc().Text(); got != want {
		t.Fatalf("the report:\n%s\nwant:\n%s", got, want)
	}
}

// TestEveryLineFitsTheColumn: a reason long enough to wrap hangs under its own
// text, and a path longer than the column goes on a line of its own whole.
func TestEveryLineFitsTheColumn(t *testing.T) {
	d := Doc{
		Kind: "survey",
		Counts: []Count{
			{"apps", "3"},
		},
		Sections: []Section{
			{"addresses", []Row{
				{"10.244.0.94:32400", []string{
					"left as it is",
					"box cannot tell what it is, and a rewrite would point an app at a port nothing answers on",
				}},
				{"/a/path/that/is/longer/than/the/whole/column/by/some/characters/and/must/not/be/broken", []string{"left as it is"}},
			}},
		},
	}
	for _, line := range strings.Split(strings.TrimRight(d.Text(), "\n"), "\n") {
		// One unbreakable token -- a path, an address -- is longer than the
		// column and stays whole; every other line fits.
		if Cols(line) > Width && strings.Contains(strings.TrimSpace(line), " ") {
			t.Errorf("%d columns: %q", len(line), line)
		}
	}
	// A word wider than the column is kept whole rather than split: half a path
	// is a different path.
	if !strings.Contains(d.Text(), "  /a/path/that/is/longer/than/the/whole/column/by/some/characters/and/must/not/be/broken\n") {
		t.Errorf("the long path was broken up:\n%s", d.Text())
	}
}

// TestTheReportSitsBesideItsSnapshot: the file is named for the snapshot it
// explains, in the same directory, 0600, and it holds the snapshot's name so a
// person reading a copy knows which run it is.
func TestTheReportSitsBesideItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "migrate-20261123T045038Z-survey.tar.gz")
	path := Name(snapshot)
	if want := filepath.Join(dir, "migrate-20261123T045038Z-survey-report.txt"); path != want {
		t.Fatalf("report name = %q, want %q", path, want)
	}
	d := doc()
	d.Snapshot = snapshot
	if err := Write(path, d); err != nil {
		t.Fatalf("write the report: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the report is mode %o, want 600: it names the source's paths, beside a file that holds its keys", mode)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != d.Text() {
		t.Errorf("the file is not the report:\n%s", back)
	}
}

// TestAnEmptySectionIsNotAHeading: a report with nothing to say about a section
// leaves it out rather than printing a heading over nothing.
func TestAnEmptySectionIsNotAHeading(t *testing.T) {
	d := Doc{Sections: []Section{{"notes", nil}, {"apps", []Row{{"sonarr", []string{"from sonarr in legacy"}}}}}}
	text := d.Text()
	if strings.Contains(text, "\nnotes\n") {
		t.Errorf("an empty section kept its heading:\n%s", text)
	}
	if !strings.Contains(text, "\napps\n  sonarr  from sonarr in legacy\n") {
		t.Errorf("the section with rows is not there:\n%s", text)
	}
}

// TestTheRunsPageDoesNotReplaceTheReads: the snapshot's report is the read's, and
// a run that applies it writes its own beside the same file rather than over it.
func TestTheRunsPageDoesNotReplaceTheReads(t *testing.T) {
	snapshot := "/var/lib/s5n/migrate/migrate-20261123T045038Z-final.tar.gz"
	if got, want := Name(snapshot), "/var/lib/s5n/migrate/migrate-20261123T045038Z-final-report.txt"; got != want {
		t.Errorf("the read's report = %q, want %q", got, want)
	}
	if got, want := ApplyName(snapshot), "/var/lib/s5n/migrate/migrate-20261123T045038Z-final-apply-report.txt"; got != want {
		t.Errorf("the run's report = %q, want %q", got, want)
	}
}
