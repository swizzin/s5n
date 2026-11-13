package bootstrap

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/release"
)

// testNet is one fixed host for the decision: the uplink it would find, and the
// links it can see. A test that cares about the host's routes uses it, so no
// test reads the machine it runs on.
type testNet struct {
	uplink Uplink
	err    error
	links  []Link
}

func (n testNet) Uplink() (Uplink, error) {
	if n.err != nil {
		return Uplink{}, n.err
	}
	return n.uplink, nil
}

func (n testNet) InterfaceFor(addr string) (Link, bool) {
	want, err := netip.ParseAddr(addr)
	if err != nil {
		return Link{}, false
	}
	for _, l := range n.links {
		for _, a := range l.Addrs {
			if a.Unmap() == want.Unmap() {
				return l, true
			}
		}
	}
	return Link{}, false
}

// The kernel's own tables, with documentation addresses: one default route on
// an ethernet, one connected route that must not be mistaken for it.
const procRouteFixture = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0002A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
eth0	00000000	0102A8C0	0003	0	0	100	00000000	0	0	0
`

const procRouteHeaderOnly = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
`

// /proc/net/ipv6_route: ::/0 out of eth0, flags UP|GATEWAY.
const procRoute6Fixture = "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000003     eth0\n"

// linuxNetFixture is a LinuxNet over fixture tables, a fixed link list and one
// answer for the source address: the host, without being this host.
func linuxNetFixture(t *testing.T, route, route6 string, links []Link, source func(network, destination string) (netip.Addr, error)) LinuxNet {
	t.Helper()
	dir := t.TempDir()
	r4 := filepath.Join(dir, "route")
	r6 := filepath.Join(dir, "ipv6_route")
	if err := os.WriteFile(r4, []byte(route), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r6, []byte(route6), 0o600); err != nil {
		t.Fatal(err)
	}
	return LinuxNet{
		procRoute:  r4,
		procRoute6: r6,
		sysNet:     dir,
		links:      func() ([]Link, error) { return links, nil },
		source:     source,
	}
}

func fixedSource(addr string) func(string, string) (netip.Addr, error) {
	return func(string, string) (netip.Addr, error) { return netip.MustParseAddr(addr), nil }
}

// The real-world case: tailscaled is up and holds an address, but the default
// route leaves through the ethernet, so the node registers on the ethernet and
// the CNI takes 1500. The tailnet interface is present and is still a tunnel --
// it just is not the uplink.
func TestUplinkIsTheDefaultRoutesLinkWithATailnetUp(t *testing.T) {
	links := []Link{
		{Name: "eth0", MTU: 1500, Type: 1, Addrs: []netip.Addr{
			netip.MustParseAddr("192.0.2.10"),
			netip.MustParseAddr("2001:db8::10"),
		}},
		{Name: "tailscale0", MTU: 1280, Type: arphrdNone, PointToPoint: true, Addrs: []netip.Addr{
			netip.MustParseAddr("100.64.0.7"),
		}},
	}
	n := linuxNetFixture(t, procRouteFixture, procRoute6Fixture, links, fixedSource("192.0.2.10"))

	up, err := n.Uplink()
	if err != nil {
		t.Fatalf("uplink: %v", err)
	}
	want := Uplink{Interface: "eth0", Address: "192.0.2.10", MTU: 1500}
	if up != want {
		t.Fatalf("uplink = %+v, want %+v", up, want)
	}

	// The tailnet address is still recognised for what it is, which is what the
	// existing-cluster warning needs.
	l, ok := n.InterfaceFor("100.64.0.7")
	if !ok || !l.Tunnel() || l.MTU != 1280 {
		t.Fatalf("tailscale0 = %+v, %v; want a 1280 tunnel", l, ok)
	}
}

// A host whose only default route is IPv6 pins its IPv6 address: the family the
// default route is in decides, and one address is still the answer.
func TestUplinkWithOnlyAnIPv6DefaultRoute(t *testing.T) {
	links := []Link{
		{Name: "eth0", MTU: 1500, Type: 1, Addrs: []netip.Addr{netip.MustParseAddr("2001:db8::10")}},
	}
	var asked string
	n := linuxNetFixture(t, procRouteHeaderOnly, procRoute6Fixture, links,
		func(network, _ string) (netip.Addr, error) {
			asked = network
			return netip.MustParseAddr("2001:db8::10"), nil
		})

	up, err := n.Uplink()
	if err != nil {
		t.Fatalf("uplink: %v", err)
	}
	if asked != "udp6" {
		t.Errorf("asked the kernel for %q, want udp6: the only default route is IPv6", asked)
	}
	if up.Address != "2001:db8::10" || up.Interface != "eth0" || up.MTU != 1500 || up.Tunnel {
		t.Fatalf("uplink = %+v, want the IPv6 address on eth0", up)
	}
}

// IPv4 is asked first when a host has both: one node address is the design here,
// and a dual-stack choice is a later spike.
func TestUplinkPrefersIPv4WhenBothFamiliesRoute(t *testing.T) {
	links := []Link{
		{Name: "eth0", MTU: 1500, Type: 1, Addrs: []netip.Addr{
			netip.MustParseAddr("192.0.2.10"),
			netip.MustParseAddr("2001:db8::10"),
		}},
	}
	n := linuxNetFixture(t, procRouteFixture, procRoute6Fixture, links, fixedSource("192.0.2.10"))
	if _, err := n.Uplink(); err != nil {
		t.Fatalf("uplink: %v", err)
	}
}

// No default route is no node IP: box says so rather than pinning an address
// that happens to be on some connected route.
func TestUplinkWithoutADefaultRoute(t *testing.T) {
	n := linuxNetFixture(t, procRouteHeaderOnly, "", nil, fixedSource("192.0.2.10"))
	_, err := n.Uplink()
	if err == nil || !strings.Contains(err.Error(), "no IPv4 and no IPv6 default route") {
		t.Fatalf("err = %v, want the missing default route named", err)
	}
}

func TestDefaultRouteTables(t *testing.T) {
	cases := []struct {
		name string
		data string
		v6   bool
		want bool
	}{
		{"ipv4 default", procRouteFixture, false, true},
		{"ipv4 header only", procRouteHeaderOnly, false, false},
		{"ipv4 default not up", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\neth0\t00000000\t0102A8C0\t0002\t0\t0\t100\t00000000\t0\t0\t0\n", false, false},
		{"ipv4 connected route is not a default", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\neth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n", false, false},
		{"ipv6 default", procRoute6Fixture, true, true},
		{"ipv6 default not up", "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000002     eth0\n", true, false},
		{"ipv6 non-default destination", "20010db8000000000000000000000000 20 00000000000000000000000000000000 00 00000000000000000000000000000000 00000400 00000001 00000000 00000003     eth0\n", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasDefaultRouteIPv4([]byte(tc.data))
			if tc.v6 {
				got = hasDefaultRouteIPv6([]byte(tc.data))
			}
			if got != tc.want {
				t.Errorf("default route = %v, want %v", got, tc.want)
			}
		})
	}
}

// Device type decides a tunnel, never the interface name, and a PPPoE uplink is
// point-to-point without being a tunnel: a home line at 1492 must install.
func TestTunnelDeviceTypes(t *testing.T) {
	cases := []struct {
		name string
		link Link
		want bool
	}{
		{"ethernet", Link{Type: 1}, false},
		{"loopback", Link{Type: 772}, false},
		{"tun, ARPHRD_NONE", Link{Type: arphrdNone, PointToPoint: true}, true},
		{"wireguard, ARPHRD_NONE", Link{Type: arphrdNone}, true},
		{"ipip, ARPHRD_TUNNEL", Link{Type: arphrdTunnel}, true},
		{"sit, ARPHRD_TUNNEL6", Link{Type: arphrdTunnel6}, true},
		{"PPPoE: point-to-point, not a tunnel", Link{Type: arphrdPPP, PointToPoint: true}, false},
		{"point-to-point with no type", Link{PointToPoint: true}, true},
		{"a name that says tailscale proves nothing", Link{Name: "tailscale0", Type: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.link.Tunnel(); got != tc.want {
				t.Errorf("%+v.Tunnel() = %v, want %v", tc.link, got, tc.want)
			}
		})
	}
}

// pins are the chart's own pins, from the checkout: the plan a clean host gets
// carries the k0s the release names and the sum of its asset.
func testPins() release.Versions {
	return release.Versions{
		K0s:       testK0sVersion,
		K0sSHA256: map[string]string{runtime.GOARCH: testSum()},
	}
}

// The ethernet case end to end through the plan: the node IP is in the install
// args where kubelet reads it, and in the config that pins the API address.
func TestInstallPinsTheChosenNodeIP(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	plan := k0sPlan(testPins(), Uplink{Interface: "eth0", Address: testNodeIP, MTU: 1500})
	plan.ConfigPath = testConfigPath(k)

	if err := k.Install(context.Background(), plan); err != nil {
		t.Fatalf("install: %v", err)
	}
	args := testInstallArgs(plan)
	if !strings.Contains(args, "--kubelet-extra-args=--node-ip="+testNodeIP) {
		t.Errorf("install args = %q, want the node IP pinned", args)
	}
	if len(stubs.run.calls) == 0 || stubs.run.calls[0] != args {
		t.Errorf("ran %v, want %q first", stubs.run.calls, args)
	}
	config, err := os.ReadFile(plan.ConfigPath)
	if err != nil {
		t.Fatalf("the k0s config was not written: %v", err)
	}
	if !strings.Contains(string(config), "address: "+testNodeIP) {
		t.Errorf("config = %q, want the API address pinned to %s", config, testNodeIP)
	}
}

// A PPPoE uplink is 1492, and box pins the node IP on it and sets no MTU
// anywhere: auto-MTU deriving 1492 is the point, not a number box writes down.
func TestPPPoEUplinkIsPinnedAndNoMTUIsSet(t *testing.T) {
	const (
		addr = "198.51.100.20"
		mtu  = 1492
	)
	k, stubs := testInstaller(t, testPayload)
	up := Uplink{Interface: "ppp0", Address: addr, MTU: mtu}
	plan := k0sPlan(testPins(), up)
	plan.ConfigPath = testConfigPath(k)
	stubs.ready.nodeIPs = []string{addr}

	if err := k.Install(context.Background(), plan); err != nil {
		t.Fatalf("install on a PPPoE uplink: %v", err)
	}
	if len(stubs.run.calls) == 0 || !strings.Contains(stubs.run.calls[0], "--node-ip="+addr) {
		t.Fatalf("ran %v, want the node IP pinned to %s", stubs.run.calls, addr)
	}
	// Nothing box hands k0s may carry an MTU: kube-router derives it from the
	// node address, and a number here would be a second opinion. The config
	// path is stripped first, because a test's own temporary directory is
	// named after this test.
	for _, call := range append(stubs.run.calls, testInstallArgs(plan)) {
		lower := strings.ToLower(strings.ReplaceAll(call, plan.ConfigPath, ""))
		if strings.Contains(lower, "mtu") || strings.Contains(lower, "1492") {
			t.Errorf("%q names an MTU; box never sets one", call)
		}
	}
	config, err := os.ReadFile(plan.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(config)), "mtu") {
		t.Errorf("config = %q, want no MTU in it", config)
	}
}

// A default route through a tunnel stops box before it installs anything, with
// the one paragraph that says what the interface, the MTU and the two fixes
// are. The wording is asserted exactly: an operator reads this and nothing else.
func TestTunnelDefaultRouteStopsBeforeInstalling(t *testing.T) {
	const want = "box pins the node IP to this host's uplink, and this host's default route leaves through tailscale0, a tunnel with an MTU of 1280: the node would register 100.64.0.7 there, every pod would inherit that MTU, and anything they send through the tunnel would fragment. Bring the uplink's default route up and run bootstrap again, or install with the tunnel down."

	src := &fakeSource{candidates: []Candidate{}, files: map[string][]byte{}}
	inst := &fakeInstaller{}
	d := newDecision(src, &fakeProber{}, &fakeHost{}, &fakeRecords{}, inst)
	d.Net = testNet{uplink: Uplink{Interface: "tailscale0", Address: "100.64.0.7", MTU: 1280, Tunnel: true}}

	res, err := d.Bootstrap(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionStop {
		t.Fatalf("action = %q, want stop", res.Action)
	}
	if res.Reason != want {
		t.Fatalf("reason =\n%q\nwant\n%q", res.Reason, want)
	}
	if len(inst.plans) != 0 {
		t.Errorf("the installer was reached with %+v; nothing may be installed on a tunnel", inst.plans)
	}
	if out := FormatResult(res); !strings.Contains(out, want) {
		t.Errorf("the decision output does not carry the paragraph:\n%s", out)
	}
}

// A kubelet that registered something other than the address box pinned fails
// through the left-behind path, naming both addresses, not just the one.
func TestNodeRegisteredAnotherAddressFails(t *testing.T) {
	k, stubs := testInstaller(t, testPayload)
	stubs.ready.nodeIPs = []string{"100.64.0.7"}

	err := k.Install(context.Background(), testPlan(testConfigPath(k)))
	if err == nil {
		t.Fatal("install succeeded, want an InternalIP failure")
	}
	msg := err.Error()
	for _, want := range []string{"100.64.0.7", testNodeIP, "the uplink address box pinned", "MTU of the interface behind that address", "k0s stop; k0s reset"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err = %q, want it to contain %q", msg, want)
		}
	}
	for _, c := range stubs.run.calls {
		if c == "reset" || strings.HasPrefix(c, "stop") {
			t.Errorf("box ran %q: it must never reset a cluster on its own", c)
		}
	}
}

// A cluster box did not install is not box's to change, so a node on a tunnel is
// one warning line and the decision goes on. The warning only appears when box
// is on that node and can see the interface.
func TestExistingClusterOnATunnelWarnsAndIsNotRefused(t *testing.T) {
	const path = "/explicit.yaml"
	cluster := Cluster{Nodes: 1, NodeIPs: []string{"100.64.0.7"}}
	probe := func() *fakeProber {
		return &fakeProber{
			version: Version{GitVersion: "v1.37.2+k3s1"},
			cluster: cluster,
			platform: PlatformProbe{
				GatewayAPI:   OutcomePresent,
				LoadBalancer: OutcomePresent,
				Gateway:      OutcomePresent,
				CertManager:  OutcomePresent,
				Classes:      []GatewayClass{{Name: "eg", ControllerName: "gateway.envoyproxy.io/gatewayclass-controller", Accepted: true, ManagedByBox: true}},
			},
		}
	}
	onTheNode := testNet{links: []Link{
		{Name: "tailscale0", MTU: 1280, Type: arphrdNone, PointToPoint: true, Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.7")}},
	}}

	src := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	d := newDecision(src, probe(), &fakeHost{}, &fakeRecords{}, &fakeInstaller{})
	d.Net = onTheNode

	res, err := d.Bootstrap(context.Background(), Options{Kubeconfig: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionUseCluster {
		t.Fatalf("action = %q, want use-cluster: a warning is not a refusal", res.Action)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings = %v, want one line", res.Warnings)
	}
	want := "the node registered 100.64.0.7, the address on tailscale0, a tunnel: every pod inherits tailscale0's MTU 1280. Pin the kubelet node IP to the uplink (--node-ip) and restart kubelet."
	if res.Warnings[0] != want {
		t.Errorf("warning = %q, want %q", res.Warnings[0], want)
	}
	if out := FormatResult(res); !strings.Contains(out, "warning: "+want) {
		t.Errorf("the decision output does not carry the warning:\n%s", out)
	}

	// The same cluster read from a host that does not hold the address: box is
	// not on that node, so it says nothing.
	src2 := &fakeSource{files: map[string][]byte{path: []byte("kubeconfig")}}
	d2 := newDecision(src2, probe(), &fakeHost{}, &fakeRecords{}, &fakeInstaller{})
	d2.Net = testNet{uplink: Uplink{Interface: "eth0", Address: testNodeIP, MTU: 1500}}
	res2, err := d2.Bootstrap(context.Background(), Options{Kubeconfig: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res2.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for a node this host is not on", res2.Warnings)
	}
}
