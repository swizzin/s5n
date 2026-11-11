package bootstrap

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file answers one question for the host box installs k0s on: which
// address should the node register with? The answer is the address of the link
// that carries the default route, and box pins it.
//
// The reason is the CNI's MTU, not the address itself. kube-router runs with
// auto-MTU on and takes the MTU from the interface that holds the node's
// primary IP, not from the smallest MTU on the host:
//
//	// GetNodeMTU returns the MTU of the interface that the node's primary IP
//	// address is assigned to. ...
//	func (n *LocalKRNode) GetNodeMTU() (int, error)
//
// (cloudnativelabs/kube-router v2.6.3, pkg/utils/node.go:180, the CNI k0s
// ships). A node that registers on a tailnet or VPN address therefore gives
// kube-bridge that interface's MTU -- 1280 for tailscale0 -- and everything a
// pod sends through the tunnel fragments. box never sets an MTU: it pins the
// address the MTU is derived from, so the MTU follows whatever the uplink
// really is (1500 on ethernet, 1492 on PPPoE, whatever a cloud hands out).

const (
	// ARPHRD device types, as /sys/class/net/<name>/type reports them. A tun
	// device, a wireguard interface and an ipip tunnel all report ARPHRD_NONE;
	// the two TUNNEL types are the older ipip/sit shapes.
	arphrdPPP     = 512
	arphrdTunnel  = 768
	arphrdTunnel6 = 769
	arphrdNone    = 65534

	// rtfUp is RTF_UP. Both kernel route tables print flags in hex, and a
	// route without this one is not usable.
	rtfUp = 0x1
)

// Link is one local interface as the kernel reports it: the MTU an
// auto-detecting CNI would take from it, the device type that tells a tunnel
// from a wire, and the addresses it holds.
type Link struct {
	Name         string
	MTU          int
	Type         int  // ARPHRD_*, 0 when /sys did not say
	PointToPoint bool // IFF_POINTOPOINT
	Addrs        []netip.Addr
}

// Tunnel reports whether pods on this interface would be talking through a
// tunnel rather than the uplink.
//
// The device type decides, not the name: nothing here reads "tailscale" or
// "wg" out of an interface name. IFF_POINTOPOINT is the second signal, because
// a tun device is point-to-point and its ARPHRD is often the only other sign.
// A PPPoE link is point-to-point too and is a real uplink -- a home line with
// 1492 on ppp0 must install normally -- so ARPHRD_PPP is excluded from that
// rule by name of type, not by interface name.
func (l Link) Tunnel() bool {
	switch l.Type {
	case arphrdNone, arphrdTunnel, arphrdTunnel6:
		return true
	case arphrdPPP:
		return false
	}
	return l.PointToPoint
}

// Uplink is where the node IP comes from: the link holding the address the
// kernel sources from on the default route, its MTU, and whether that link is
// a tunnel (which box refuses to install on).
type Uplink struct {
	Interface string
	Address   string
	MTU       int
	Tunnel    bool
}

// Net reports this host's default route and its links, read-only. Decision
// consults it only where box installs k0s and where box warns about a node
// address it can see locally; the tests fake it.
type Net interface {
	// Uplink is the link carrying the default route and the address the
	// kernel prefers on it.
	Uplink() (Uplink, error)
	// InterfaceFor reports the local link holding addr. ok is false when this
	// host does not hold it, which is how box knows it is not on that node.
	InterfaceFor(addr string) (Link, bool)
}

// LinuxNet is the running host, read from the kernel's own files: the two
// route tables say which address family has a default route, a connected UDP
// socket says which address the kernel would source (connecting a UDP socket
// sends no packet), and /sys/class/net says what each device is. None of it
// needs netlink, a new dependency, or a write.
//
// The five fields exist so a test can run against a fixture instead of the
// machine it happens to be on; the zero value is the real host.
type LinuxNet struct {
	procRoute, procRoute6, sysNet string
	links                         func() ([]Link, error)
	source                        func(network, destination string) (netip.Addr, error)
}

// Uplink finds the default route's family, the address the kernel sources for
// it, and the link that address sits on. The route tables decide the family
// and whether a default route exists at all; the address the kernel picks is
// what decides the link, because that is the same choice it makes for a
// packet, policy routing included.
func (n LinuxNet) Uplink() (Uplink, error) {
	network, err := n.defaultFamily()
	if err != nil {
		return Uplink{}, err
	}
	addr, err := n.sourceFor(network)
	if err != nil {
		return Uplink{}, fmt.Errorf("ask the kernel which address it sources from: %w", err)
	}
	link, ok := n.InterfaceFor(addr.String())
	if !ok {
		return Uplink{}, fmt.Errorf("the default route sources %s and no interface on this host holds it", addr)
	}
	return Uplink{
		Interface: link.Name,
		Address:   addr.String(),
		MTU:       link.MTU,
		Tunnel:    link.Tunnel(),
	}, nil
}

// InterfaceFor finds the link that holds addr by asking the kernel for every
// interface and its addresses.
func (n LinuxNet) InterfaceFor(addr string) (Link, bool) {
	want, err := netip.ParseAddr(addr)
	if err != nil {
		return Link{}, false
	}
	want = want.Unmap()
	links, err := n.linkList()
	if err != nil {
		return Link{}, false
	}
	for _, l := range links {
		for _, a := range l.Addrs {
			if a.Unmap() == want {
				return l, true
			}
		}
	}
	return Link{}, false
}

// defaultFamily is IPv4 first: a host with both is an IPv4 host for this
// choice (dual-stack is a later spike), and a host whose only default route is
// IPv6 uses that. The s5n node is one address, pinned, in either family.
func (n LinuxNet) defaultFamily() (string, error) {
	if data, err := os.ReadFile(n.routePath()); err == nil {
		if hasDefaultRouteIPv4(data) {
			return "udp4", nil
		}
	}
	if data, err := os.ReadFile(n.route6Path()); err == nil {
		if hasDefaultRouteIPv6(data) {
			return "udp6", nil
		}
	}
	return "", errors.New("this host has no IPv4 and no IPv6 default route")
}

// sourceFor asks for the local address of a route to a documentation address
// in the route's family. A UDP connect resolves the route and the source
// address without sending anything, so this is as read-only as the tables.
func (n LinuxNet) sourceFor(network string) (netip.Addr, error) {
	destination := "192.0.2.1:9" // RFC 5737, TEST-NET-1
	if network == "udp6" {
		destination = "[2001:db8::1]:9" // RFC 3849, documentation
	}
	source := n.source
	if source == nil {
		source = udpSource
	}
	return source(network, destination)
}

func udpSource(network, destination string) (netip.Addr, error) {
	conn, err := net.Dial(network, destination)
	if err != nil {
		return netip.Addr{}, err
	}
	defer func() { _ = conn.Close() }()
	udp, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, fmt.Errorf("the kernel gave %s as a local address, want an IP address", conn.LocalAddr())
	}
	return udp.AddrPort().Addr().Unmap(), nil
}

func (n LinuxNet) linkList() ([]Link, error) {
	if n.links != nil {
		return n.links()
	}
	return hostLinks(n.sysPath())
}

// hostLinks reads the running host: every interface, with the MTU, the
// addresses and the device type /sys reports for it.
func hostLinks(sysNet string) ([]Link, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list this host's interfaces: %w", err)
	}
	out := make([]Link, 0, len(ifs))
	for _, i := range ifs {
		l := Link{
			Name:         i.Name,
			MTU:          i.MTU,
			Type:         deviceType(filepath.Join(sysNet, i.Name, "type")),
			PointToPoint: i.Flags&net.FlagPointToPoint != 0,
		}
		addrs, err := i.Addrs()
		if err == nil {
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				addr, ok := netip.AddrFromSlice(ipn.IP)
				if !ok {
					continue
				}
				l.Addrs = append(l.Addrs, addr.Unmap())
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// deviceType reads /sys/class/net/<name>/type. Zero means "the file did not
// say", which is not a tunnel type: a host that hides sysfs would otherwise
// turn every point-to-point uplink into a refusal.
func deviceType(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	t, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return t
}

// hasDefaultRouteIPv4 reads one /proc/net/route table. The destination and the
// mask of a default route are both zero; the flags say whether it is up.
func hasDefaultRouteIPv4(data []byte) bool {
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 { // header
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		if f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		if flags, err := strconv.ParseUint(f[3], 16, 32); err == nil && flags&rtfUp != 0 {
			return true
		}
	}
	return false
}

// hasDefaultRouteIPv6 reads one /proc/net/ipv6_route table: the destination is
// ::/0, and the flags are hex in the ninth field.
func hasDefaultRouteIPv6(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		if strings.Trim(f[0], "0") != "" || f[1] != "00" {
			continue
		}
		if flags, err := strconv.ParseUint(f[8], 16, 32); err == nil && flags&rtfUp != 0 {
			return true
		}
	}
	return false
}

func (n LinuxNet) routePath() string {
	if n.procRoute != "" {
		return n.procRoute
	}
	return "/proc/net/route"
}

func (n LinuxNet) route6Path() string {
	if n.procRoute6 != "" {
		return n.procRoute6
	}
	return "/proc/net/ipv6_route"
}

func (n LinuxNet) sysPath() string {
	if n.sysNet != "" {
		return n.sysNet
	}
	return "/sys/class/net"
}

// tunnelRefusal is the one paragraph box stops with when the default route
// leaves through a tunnel: what box would have pinned, what that does to every
// pod, and the two ways out. There is no override flag on purpose -- a full
// tunnel on the host is a decision the operator has to make about the host,
// not a string box should accept.
func tunnelRefusal(up Uplink) string {
	if !up.Tunnel {
		return ""
	}
	return fmt.Sprintf("box pins the node IP to this host's uplink, and this host's default route leaves through %s, a tunnel with an MTU of %d: the node would register %s there, every pod would inherit that MTU, and anything they send through the tunnel would fragment. Bring the uplink's default route up and run bootstrap again, or install with the tunnel down.", up.Interface, up.MTU, up.Address)
}

// nodeIPWarning is the one line box prints on a cluster it did not install
// whose node registered on a tunnel it can see. It is a warning, not a
// refusal: that cluster is somebody else's to change, and box does not change
// it.
func nodeIPWarning(addr string, l Link) string {
	return fmt.Sprintf("the node registered %s, the address on %s, a tunnel: every pod inherits %s's MTU %d. Pin the kubelet node IP to the uplink (--node-ip) and restart kubelet.", addr, l.Name, l.Name, l.MTU)
}

// nodeIPWarnings is nodeIPWarning over a cluster's node addresses. An address
// this host does not hold is skipped silently: that node is not this host, and
// box cannot see its interfaces.
func nodeIPWarnings(cl Cluster, n Net) []string {
	for _, addr := range cl.NodeIPs {
		link, ok := n.InterfaceFor(addr)
		if !ok || !link.Tunnel() {
			continue
		}
		return []string{nodeIPWarning(addr, link)}
	}
	return nil
}
