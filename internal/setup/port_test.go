package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/seedbox"
)

// The band is Kubernetes' own: min(max(16, size/32), 128) ports from the start
// of the range (pkg/registry/core/service/portallocator's calculateRangeOffset
// at the pinned version). The default range is 32768 ports, so the band is its
// first 128 -- not the whole range, and not the upper part the API server
// assigns from.
func TestStaticBandMatchesKubernetes(t *testing.T) {
	cases := []struct {
		name       string
		start, end int
		lo, hi     int
	}{
		{"the default range", 30000, 32767, 30000, 30085},
		{"a thousand ports", 30000, 31000, 30000, 30030},
		{"fifteen hundred ports", 30000, 31500, 30000, 30045},
		{"at the minimum", 30000, 30016, 30000, 30015},
		{"below the minimum", 30000, 30010, 30000, 29999},
		{"a range wider than the cap", 20000, 60000, 20000, 20127},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lo, hi := staticBand(c.start, c.end)
			if lo != c.lo || hi != c.hi {
				t.Fatalf("staticBand(%d, %d) = %d-%d, want %d-%d", c.start, c.end, lo, hi, c.lo, c.hi)
			}
		})
	}
}

func inBand(port int) bool {
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	return port >= lo && port <= hi
}

// An empty cluster: any of the band's ports, and the draw decides which.
func TestPickPortEmptyCluster(t *testing.T) {
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	for _, want := range []int{lo, lo + 1, hi} {
		got, err := pickPort(lo, hi, nil, func(n int) int {
			if n != hi-lo+1 {
				t.Fatalf("the draw was offered %d ports, want the band's %d", n, hi-lo+1)
			}
			return want - lo
		})
		if err != nil {
			t.Fatalf("pickPort: %v", err)
		}
		if got != want {
			t.Fatalf("pickPort = %d, want %d", got, want)
		}
		if !inBand(got) {
			t.Fatalf("%d is outside the band", got)
		}
	}
}

// A band with one free port left: that port, whatever the draw would have said.
func TestPickPortAlmostFull(t *testing.T) {
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	var held []NodePort
	for port := lo; port <= hi; port++ {
		if port == hi-3 {
			continue
		}
		held = append(held, NodePort{Port: port, Service: fmt.Sprintf("other/svc-%d", port)})
	}
	got, err := pickPort(lo, hi, held, func(int) int { return 0 })
	if err != nil {
		t.Fatalf("pickPort: %v", err)
	}
	if got != hi-3 {
		t.Fatalf("pickPort = %d, want the one free port %d", got, hi-3)
	}
}

// A full band is a refusal, and the refusal names the band and how many
// Services are in it, so an operator can go and look.
func TestPickPortFullNamesTheBandAndTheServices(t *testing.T) {
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	var held []NodePort
	for port := lo; port <= hi; port++ {
		held = append(held, NodePort{Port: port, Service: fmt.Sprintf("other/svc-%d", port)})
	}
	_, err := pickPort(lo, hi, held, func(int) int { return 0 })
	if err == nil {
		t.Fatal("a full band was picked from")
	}
	for _, want := range []string{"30000-30085", "86 Services"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// Two ports of one Service (TCP and UDP on the same number) are one Service in
// the way, not two.
func TestPickPortCountsServicesNotPorts(t *testing.T) {
	_, err := pickPort(30000, 30000, []NodePort{
		{Port: 30000, Service: "other/peers"},
		{Port: 30000, Service: "other/peers"},
	}, func(int) int { return 0 })
	if err == nil {
		t.Fatal("a full band was picked from")
	}
	if !strings.Contains(err.Error(), "1 Service in this cluster") {
		t.Fatalf("error = %q, want one Service", err)
	}
}

// A port outside the band is not "in the way": the band is what box picks from,
// and the upper part of the range is where the API server puts its own
// allocations.
func TestPickPortIgnoresPortsOutsideTheBand(t *testing.T) {
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	got, err := pickPort(lo, hi, []NodePort{
		{Port: 31111, Service: "envoy-gateway-system/envoy"},
		{Port: 32767, Service: "default/kubernetes"},
	}, func(n int) int {
		if n != hi-lo+1 {
			t.Fatalf("the draw was offered %d ports, want %d", n, hi-lo+1)
		}
		return 0
	})
	if err != nil {
		t.Fatalf("pickPort: %v", err)
	}
	if got != lo {
		t.Fatalf("pickPort = %d, want the band's first port %d", got, lo)
	}
}

// catalogAt is the fixture chart's manifest, loaded the way cmd/box loads the
// release's.
func catalogAt(t *testing.T, dir string) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	return cat
}

// torrentHarness is a harness on the fixture chart that has qBittorrent, with
// the cluster holding the two ports a test says it holds.
func torrentHarness(t *testing.T, held []NodePort) *harness {
	t.Helper()
	h := newHarness(t)
	h.chartDir = "testdata/chart-torrent"
	h.decision.Catalog = catalogAt(t, h.chartDir)
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha", torrentApp}}
	h.decision.NodePorts = func(context.Context) ([]NodePort, error) { return held, nil }
	// The first free port in the band, so the assertion is about the pick and
	// not about the random source.
	h.decision.Rand = func(int) int { return 0 }
	return h
}

// The port is decided at init, from the cluster's own Services, and recorded in
// the grant: that is the whole fix for a fixed port that the API server had
// already given away.
func TestTorrentPortIsPickedAndRecorded(t *testing.T) {
	h := torrentHarness(t, []NodePort{
		{Port: 30000, Service: "envoy-gateway-system/envoy"},
		{Port: 31111, Service: "envoy-gateway-system/envoy"},
	})
	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if !contains(res.GrantChanged, torrentPortKey) {
		t.Fatalf("grant keys changed = %v, want %s", res.GrantChanged, torrentPortKey)
	}
	grant := h.read(h.grantPath)
	// 30000 is held, so the band's first free port is 30001.
	if !strings.Contains(grant, "torrent:\n      port: 30001") {
		t.Fatalf("the grant does not record the picked port:\n%s", grant)
	}
	if got := h.out.String(); !strings.Contains(got, "torrent port: 30001") {
		t.Fatalf("init did not report the port it picked:\n%s", got)
	}
}

// A seedbox installed before box picked the port has no port in its grant, but
// its peers Service already holds one, outside the band: that one is kept, not
// moved to a fresh pick. Another namespace's peers Service is not this box's.
func TestTorrentPortAdoptsTheInstalledPeersService(t *testing.T) {
	h := torrentHarness(t, []NodePort{
		{Port: 31111, Service: "other/qbittorrent-peers"},
		{Port: 31112, Service: "seedbox/qbittorrent-peers"},
	})
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if grant := h.read(h.grantPath); !strings.Contains(grant, "torrent:\n      port: 31112") {
		t.Fatalf("the grant does not keep the installed port:\n%s", grant)
	}
	if got := h.out.String(); !strings.Contains(got, "torrent port: 31112 (already this seedbox's; kept)") {
		t.Fatalf("init did not say it kept the port:\n%s", got)
	}
}

// A grant that already holds a port keeps it, and nothing reads the cluster:
// the peers learned that number from the tracker, and this is also the path a
// user's run takes, where a cluster-wide LIST is not permitted at all.
func TestTorrentPortIsKeptAndTheClusterIsNotRead(t *testing.T) {
	h := newHarness(t)
	h.chartDir = "testdata/chart-torrent"
	h.decision.Catalog = catalogAt(t, h.chartDir)
	write(t, h.grantPath, torrentGrant)
	reads := 0
	h.decision.NodePorts = func(context.Context) ([]NodePort, error) {
		reads++
		return nil, fmt.Errorf("the cluster must not be read when the grant has a port")
	}

	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if reads != 0 {
		t.Fatalf("the cluster was read %d times, want 0", reads)
	}
	if contains(res.GrantChanged, torrentPortKey) {
		t.Fatalf("the grant's port was rewritten: %v", res.GrantChanged)
	}
	if !strings.Contains(h.read(h.grantPath), "port: 30042") {
		t.Fatalf("the grant lost its port:\n%s", h.read(h.grantPath))
	}
}

// A full band is refused before anything is written: no grant, no values file,
// no apply.
func TestTorrentPortFullBandRefusesBeforeAnyWrite(t *testing.T) {
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	var held []NodePort
	for port := lo; port <= hi; port++ {
		held = append(held, NodePort{Port: port, Service: fmt.Sprintf("other/svc-%d", port)})
	}
	h := torrentHarness(t, held)

	_, err := h.run(h.options())
	if err == nil {
		t.Fatal("init installed a box with no free port in the band")
	}
	for _, want := range []string{"30000-30085", "86 Services"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
	if h.exists(h.grantPath) {
		t.Fatalf("the grant was written anyway:\n%s", h.read(h.grantPath))
	}
	if len(h.applied) != 0 {
		t.Fatalf("apply ran %d times, want 0", len(h.applied))
	}
	if len(h.host.mkdirs) != 0 {
		t.Fatalf("directories were created anyway: %v", h.host.mkdirs)
	}
}

// The first install's plan shows the port as a grant addition, and the second
// run has nothing to add: the grant is where the decision lives, so a re-run
// cannot move a port peers are already announcing.
func TestPlanShowsTheTorrentPortOnce(t *testing.T) {
	h := torrentHarness(t, []NodePort{{Port: 30000, Service: "other/svc"}})
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}

	chart := chartAt(t, h.chartDir)
	sb, err := seedbox.Load(h.grantPath, h.valuesPath)
	if err != nil {
		t.Fatalf("load the files init wrote: %v", err)
	}
	effective, err := sb.Effective(chart)
	if err != nil {
		t.Fatalf("effective values: %v", err)
	}
	// A first install has no deployed release, so every effective value is an
	// addition -- the port among them.
	var shown *seedbox.Change
	for _, change := range seedbox.Diff(map[string]any{}, effective) {
		if change.Path == torrentPortKey {
			c := change
			shown = &c
		}
	}
	if shown == nil {
		t.Fatal("the first install's plan does not show the torrent port")
	}
	if shown.Kind != seedbox.Added {
		t.Fatalf("the port is shown as %v, want an addition", shown.Kind)
	}
	if got, ok := asInt(shown.New); !ok || got != 30001 {
		t.Fatalf("the plan adds %v, want 30001", shown.New)
	}

	// The same answers again: the grant keeps the port, so the plan is empty.
	h.prompt = &fakePrompt{inputs: []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}}
	h.prompt.multis = [][]string{{"alpha", torrentApp}}
	h.decision.Prompt = h.prompt
	res, err := h.run(h.options())
	if err != nil {
		t.Fatalf("second init: %v\n%s", err, h.out)
	}
	if contains(res.GrantChanged, torrentPortKey) {
		t.Fatalf("the second run spliced the port: %v", res.GrantChanged)
	}
	again, err := seedbox.Load(h.grantPath, h.valuesPath)
	if err != nil {
		t.Fatalf("load the files after the second run: %v", err)
	}
	effective2, err := again.Effective(chart)
	if err != nil {
		t.Fatalf("effective values: %v", err)
	}
	if changes := seedbox.Diff(effective, effective2); len(changes) != 0 {
		t.Fatalf("the second run's plan is not empty: %v", changes)
	}
}

// A chart with no peer port at all (or one from before the key existed) is not
// a reason to read the cluster or to write anything.
func TestTorrentPortUnaskedWithoutTheKey(t *testing.T) {
	h := newHarness(t) // testdata/chart has no qbittorrent
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha"}}
	h.decision.NodePorts = func(context.Context) ([]NodePort, error) {
		t.Fatal("the cluster was read for a chart with no peer port")
		return nil, nil
	}
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
}

// A box that does not run qBittorrent has no peer port to decide, and init does
// not write one: nothing listens on it, so nothing may hold it either.
func TestTorrentPortUnaskedWithoutTheApp(t *testing.T) {
	h := torrentHarness(t, nil)
	h.prompt.multis = [][]string{{"alpha"}}
	h.decision.NodePorts = func(context.Context) ([]NodePort, error) {
		t.Fatal("the cluster was read for a box with no qBittorrent")
		return nil, nil
	}
	if _, err := h.run(h.options()); err != nil {
		t.Fatalf("init: %v\n%s", err, h.out)
	}
	if strings.Contains(h.read(h.grantPath), torrentPortKey) {
		t.Fatalf("the grant holds a peer port for a box without qBittorrent:\n%s", h.read(h.grantPath))
	}
}

// torrentGrant is a box that already has a peer port, as an operator's first
// install left it.
const torrentGrant = `# root's grant for this box
namespace: seedbox
login: alice

global:
  puid: 1000
  pgid: 1000

storage:
  data:
    path: /srv/s5n/data
  config:
    path: /srv/s5n/config

gateway:
  host: box.example.com

apps:
  alpha:
    enabled: true
  beta:
    enabled: false
  gamma:
    enabled: false
  qbittorrent:
    enabled: true
    torrent:
      port: 30042
`
