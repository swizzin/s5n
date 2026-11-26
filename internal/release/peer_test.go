package release

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

// peerChart adds qBittorrent's peers Service branch, with no port.
func peerChart(t *testing.T) Chart {
	t.Helper()
	c, err := loadChartDir(DevVersion, "testdata/chart-peer")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// peerFakes uses that chart on both sides; only the port differs.
func peerFakes(t *testing.T) (*fakes, Chart) {
	t.Helper()
	f := newFakes(t)
	chart := peerChart(t)
	f.chart = &chart
	f.rel.chartVersion = chart.Version
	// Helm is asked only when the namespace exists; these tests have one.
	f.store.nsExists = true
	f.store.nsEnforce = "restricted"
	return f, chart
}

// deployedFor is what the release holds; a test edits it for an old box.
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

// portIn is the peer port in a values document.
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

func TestABoxWithNoRecordedPortKeepsThePortItsReleaseHas(t *testing.T) {
	f, chart := peerFakes(t)
	deployed := deployedFor(t, f, chart)
	// Installed while the chart defaulted it: what Helm stored and peers use.
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
	// The box is on that port already, so the diff is empty.
	for _, ch := range res.Changes {
		if ch.Path == peerPortKey {
			t.Fatalf("the port this box already had is shown as a change: %+v", ch)
		}
	}
	if got := f.out.String(); !strings.Contains(got, "torrent port: 31111 (already this box's; kept)") {
		t.Fatalf("box did not say it kept the port:\n%s", got)
	}
}

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

func TestAFirstInstallWithNoRecordedPortIsRefused(t *testing.T) {
	f, _ := peerFakes(t)
	f.rel.exists = false

	_, err := f.apply(t)
	if err == nil || !strings.Contains(err.Error(), "box init") || !strings.Contains(err.Error(), peerPortKey) {
		t.Fatalf("apply = %v, want a refusal naming %s and box init\n%s", err, peerPortKey, f.out)
	}
	if len(f.rel.upgraded) != 0 {
		t.Fatalf("a first install with no peer port was installed: %v", f.rel.upgraded)
	}
}

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
