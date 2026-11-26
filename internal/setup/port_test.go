package setup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/catalog"
	"github.com/swizzin/s5n/internal/seedbox"
)

// The band is Kubernetes' own, min(max(16, size/32), 128) ports from the start of
// the range (pkg/registry/core/service/portallocator's calculateRangeOffset at the
// pinned version): its first 128, not the upper part the API server assigns from.
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

// An empty cluster: any of the band's ports, the draw decides which.
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

// One free port left is that port, whatever the draw says.
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

// A full band is a refusal naming the band and how many Services are in it.
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

// TCP and UDP on one number are one Service in the way, not two.
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

// A port outside the band is not in the way: the upper range is where the API
// server puts its own allocations.
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

// catalogAt is the fixture chart's manifest, loaded as cmd/box loads the release's.
func catalogAt(t *testing.T, dir string) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	return cat
}

// torrentHarness is a harness on the qBittorrent fixture chart, with the cluster
// holding the ports the test says.
func torrentHarness(t *testing.T, held []NodePort) *harness {
	t.Helper()
	h := newHarness(t)
	h.chartDir = "testdata/chart-torrent"
	h.decision.Catalog = catalogAt(t, h.chartDir)
	// The chart ships no peer port: the schema says the key exists, init decides it.
	h.decision.Schema = chartAt(t, h.chartDir).Schema
	h.prompt.inputs = []string{"box.example.com", "/srv/s5n/data", "/srv/s5n/config"}
	h.prompt.multis = [][]string{{"alpha", torrentApp}}
	h.decision.NodePorts = func(context.Context) ([]NodePort, error) { return held, nil }
	// The first free port in the band, so the assertion is about the pick.
	h.decision.Rand = func(int) int { return 0 }
	return h
}

// The port is decided at init from the cluster's Services and recorded in the
// grant: the fix for a fixed port the API server had already given away.
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
	if got := h.out.String(); !strings.Contains(got, "· torrent port   picked    30001\n") {
		t.Fatalf("init did not report the port it picked:\n%s", got)
	}
}

// A box installed before box picked the port has none in its grant but its peers
// Service holds one outside the band: kept, not moved. Another namespace's peers
// Service is not this box's.
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
	if got := h.out.String(); !strings.Contains(got, "· torrent port   kept      31112\n") {
		t.Fatalf("init did not say it kept the port:\n%s", got)
	}
}

// A grant with a port keeps it and nothing reads the cluster -- the peers learned
// that number from the tracker -- which is also the path a user's run takes, where a
// cluster-wide LIST is not permitted.
func TestTorrentPortIsKeptAndTheClusterIsNotRead(t *testing.T) {
	h := newHarness(t)
	h.chartDir = "testdata/chart-torrent"
	h.decision.Catalog = catalogAt(t, h.chartDir)
	h.decision.Schema = chartAt(t, h.chartDir).Schema
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

// A full band is refused before anything is written.
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

// The first plan shows the port as a grant addition and the second adds nothing: a
// re-run cannot move a port peers are announcing.
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
	// A first install has no deployed release, so every value is an addition.
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

// A chart with no peer port is no reason to read the cluster or write anything.
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

// A box without qBittorrent has no peer port: nothing listens, so nothing holds it.
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

// torrentGrant is a box that already has a peer port.
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

// migratingOptions is what `box migrate apply` gives init on a host with no box:
// every app off, the plan's answers given, and the apps it will switch on named.
func migratingOptions(h *harness, switchesOn ...string) Options {
	opts := h.unattended()
	opts.Apps = []string{"none"}
	opts.Adopting = true
	opts.SwitchesOn = switchesOn
	return opts
}

// A migrate install answers `--apps none` and switches the plan's apps on later, so
// the peer port is decided for the box that is coming, not this run's list:
// otherwise a migrated box keeps 31111, the port a LoadBalancer Service already
// took.
func TestTorrentPortIsDecidedForTheAppsAPlanBrings(t *testing.T) {
	h := torrentHarness(t, []NodePort{
		{Port: 30000, Service: "envoy-gateway-system/envoy"},
		{Port: 31111, Service: "envoy-gateway-system/envoy"},
	})
	res, err := h.run(migratingOptions(h, "alpha", torrentApp))
	if err != nil {
		t.Fatalf("a migrating init: %v\n%s", err, h.out)
	}
	if !contains(res.GrantChanged, torrentPortKey) {
		t.Fatalf("grant keys changed = %v, want %s", res.GrantChanged, torrentPortKey)
	}
	// 30000 is held: the band's first free port is 30001, with every app off.
	grant := h.read(h.grantPath)
	if !strings.Contains(grant, "torrent:\n      port: 30001") {
		t.Fatalf("the grant does not record the port the plan's qBittorrent will use:\n%s", grant)
	}
	if !strings.Contains(grant, "qbittorrent:\n    enabled: false") {
		t.Fatalf("the box did not go in with qBittorrent off:\n%s", grant)
	}
	if got := h.out.String(); !strings.Contains(got, "· torrent port   picked    30001\n") {
		t.Fatalf("the migrating init did not say it picked a port:\n%s", got)
	}
}

// A plan without qBittorrent has no peer port, and init writes none.
func TestTorrentPortUnaskedWhenThePlanBringsNoQbittorrent(t *testing.T) {
	h := torrentHarness(t, nil)
	h.decision.NodePorts = func(context.Context) ([]NodePort, error) {
		t.Fatal("the cluster was read for a plan with no qBittorrent")
		return nil, nil
	}
	if _, err := h.run(migratingOptions(h, "alpha")); err != nil {
		t.Fatalf("a migrating init with no qBittorrent in the plan: %v\n%s", err, h.out)
	}
	if strings.Contains(h.read(h.grantPath), torrentPortKey) {
		t.Fatalf("the grant holds a peer port for a plan without qBittorrent:\n%s", h.read(h.grantPath))
	}
}

// The source's peer port is preferred when this cluster can use it: peers and any
// forward point at it, so moving it drops every connection until the next announce.
// Kept only inside this cluster's NodePort range and unheld.
func TestTorrentPortKeepsTheSourcesPortWhenThisClusterCanUseIt(t *testing.T) {
	h := torrentHarness(t, []NodePort{{Port: 30000, Service: "other/svc"}})
	opts := migratingOptions(h, torrentApp)
	opts.TorrentPort = 30047

	if _, err := h.run(opts); err != nil {
		t.Fatalf("a migrating init with the source's port: %v\n%s", err, h.out)
	}
	if grant := h.read(h.grantPath); !strings.Contains(grant, "torrent:\n      port: 30047") {
		t.Fatalf("the grant does not keep the source's port:\n%s", grant)
	}
	if got := h.out.String(); !strings.Contains(got, "· torrent port   kept      30047 from the source\n") {
		t.Fatalf("init did not say it kept the source's port:\n%s", got)
	}
}

// The source's port is a hint, not an answer: one outside the range or already held
// is picked fresh from the band, and init says so.
func TestTorrentPortPicksWhenTheSourcesPortCannotBeUsed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		port  int
		held  []NodePort
		want  int
		avoid int
	}{
		{
			name: "the source listened outside the NodePort range",
			port: 6881,
			want: 30000,
		},
		{
			name: "the source's port is already a Service's",
			port: 30047,
			held: []NodePort{{Port: 30047, Service: "other/svc"}},
			want: 30000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := torrentHarness(t, tc.held)
			opts := migratingOptions(h, torrentApp)
			opts.TorrentPort = tc.port

			if _, err := h.run(opts); err != nil {
				t.Fatalf("a migrating init: %v\n%s", err, h.out)
			}
			if grant := h.read(h.grantPath); !strings.Contains(grant, fmt.Sprintf("torrent:\n      port: %d", tc.want)) {
				t.Fatalf("the grant does not record the port box picked (%d):\n%s", tc.want, grant)
			}
			got := h.out.String()
			if !strings.Contains(got, fmt.Sprintf("· torrent port   picked    %d\n", tc.want)) {
				t.Fatalf("init did not say it picked %d:\n%s", tc.want, got)
			}
			if strings.Contains(got, "(the source's, kept)") {
				t.Fatalf("init said it kept a port it could not use:\n%s", got)
			}
		})
	}
}
