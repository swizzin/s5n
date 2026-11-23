package release

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

// peerChart is testdata/chart-peer: the fixture chart plus the one values
// branch the real chart has and it does not, qBittorrent's peers Service, with
// no port in it.
func peerChart(t *testing.T) Chart {
	t.Helper()
	c, err := loadChartDir(DevVersion, "testdata/chart-peer")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// peerFakes is newFakes with that chart, and with the release Helm has being
// the same chart: the difference between the two sides is the port alone.
func peerFakes(t *testing.T) (*fakes, Chart) {
	t.Helper()
	f := newFakes(t)
	chart := peerChart(t)
	f.chart = &chart
	f.rel.chartVersion = chart.Version
	// The release lives in its namespace, so apply asks Helm about it only
	// when the namespace is there: an existing box is the case these tests are.
	f.store.nsExists = true
	f.store.nsEnforce = "restricted"
	return f, chart
}

// deployedFor is the values an already-installed box's release holds: what the
// same files render, which is what an apply would install. A test edits it to
// stand for a box installed while the chart still defaulted the peer port.
func deployedFor(t *testing.T, f *fakes, chart Chart) map[string]any {
	t.Helper()
	sb, err := seedbox.Load(f.grantPath, f.values)
	if err != nil {
		t.Fatalf("load the fixture files: %v", err)
	}
	effective, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		t.Fatalf("effective values: %v", err)
	}
	return effective
}

// portIn is the peer port one values document carries.
func portIn(t *testing.T, values map[string]any) int {
	t.Helper()
	qbt, _ := values["apps"].(map[string]any)["qbittorrent"].(map[string]any)
	torrent, _ := qbt["torrent"].(map[string]any)
	if torrent == nil {
		return 0
	}
	switch p := torrent["port"].(type) {
	case int:
		return p
	case float64:
		return int(p)
	}
	return 0
}

// An old box has no peer port in its files and its release renders the one the
// chart used to default to. box keeps that number: the chart ships no default
// any more, so the value the release already holds is what this apply installs,
// and nothing about the peers Service moves.
func TestABoxWithNoRecordedPortKeepsThePortItsReleaseHas(t *testing.T) {
	f, chart := peerFakes(t)
	deployed := deployedFor(t, f, chart)
	// The box was installed while the chart defaulted the port, so this is what
	// Helm stored and what its peers Service is on.
	qbt := deployed["apps"].(map[string]any)["qbittorrent"].(map[string]any)
	qbt["torrent"].(map[string]any)["port"] = legacyPeerPort
	f.rel.values = deployed
	f.rel.exists = true

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.rel.upgraded) != 1 {
		t.Fatalf("the release was applied %d times, want once", len(f.rel.upgraded))
	}
	if got := portIn(t, f.rel.upgraded[0]); got != legacyPeerPort {
		t.Fatalf("the peer port this apply installed = %d, want the release's %d", got, legacyPeerPort)
	}
	// Nothing about it is a change: the box is on that port already, and the
	// diff is what says so.
	for _, ch := range res.Changes {
		if ch.Path == peerPortKey {
			t.Fatalf("the port this box already had is shown as a change: %+v", ch)
		}
	}
	if got := f.out.String(); !strings.Contains(got, "torrent port: 31111 (already this box's; kept)") {
		t.Fatalf("box did not say it kept the port:\n%s", got)
	}
}

// A box whose grant records a port keeps it: the adoption is for a box that has
// none, and a working port is never moved to the number the chart used to ship.
func TestARecordedPortIsNotMovedByTheAdoption(t *testing.T) {
	f, chart := peerFakes(t)
	rewrite(t, f.grantPath, map[string]string{
		"  qbittorrent:\n    enabled: true": "  qbittorrent:\n    enabled: true\n    torrent:\n      port: 30042",
	})
	deployed := deployedFor(t, f, chart)
	qbt := deployed["apps"].(map[string]any)["qbittorrent"].(map[string]any)
	qbt["torrent"].(map[string]any)["port"] = legacyPeerPort
	f.rel.values = deployed
	f.rel.exists = true

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if got := portIn(t, f.rel.upgraded[0]); got != 30042 {
		t.Fatalf("the peer port this apply installed = %d, want the grant's 30042", got)
	}
	if strings.Contains(f.out.String(), "torrent port:") {
		t.Fatalf("box reported a port decision for a box that records one:\n%s", f.out)
	}
}

// A first install with no recorded port is still a release that renders: the
// chart ships no default, so box writes the one it left behind, and says where
// the decision belongs.
func TestAFirstInstallWithNoRecordedPortGetsTheOldDefault(t *testing.T) {
	f, _ := peerFakes(t)
	f.rel.exists = false

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if got := portIn(t, f.rel.upgraded[0]); got != legacyPeerPort {
		t.Fatalf("the peer port this first install rendered = %d, want %d", got, legacyPeerPort)
	}
	if got := f.out.String(); !strings.Contains(got, "torrent port: 31111 (the chart's old default; run `box init` to pick one)") {
		t.Fatalf("box did not say where the port came from:\n%s", got)
	}
}

// A release from before the peer port, or one without qBittorrent, is never
// given the key: its own schema would refuse the install.
func TestAChartWithoutThePeerPortIsNotGivenOne(t *testing.T) {
	f := newFakes(t)
	f.rel.exists = false

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	values := f.rel.upgraded[0]
	qbt := values["apps"].(map[string]any)["qbittorrent"].(map[string]any)
	if _, ok := qbt["torrent"]; ok {
		t.Fatalf("a chart with no peer port was given one: %v", qbt["torrent"])
	}
	if strings.Contains(f.out.String(), "torrent port:") {
		t.Fatalf("box reported a port decision for a chart that has none:\n%s", f.out)
	}
}
