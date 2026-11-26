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

// This file answers one question for the host box installs k0s on: which address
// should the node register with? The address of the link carrying the default route,
// which box pins.
//
// The reason is the CNI's MTU, not the address. kube-router runs with auto-MTU and
// takes the MTU from the interface holding the node's primary IP
// (cloudnativelabs/kube-router v2.6.3, pkg/utils/node.go:180 GetNodeMTU, the CNI k0s
// ships), so a node registering on a tailnet address gives kube-bridge that
// interface's MTU -- 1280 for tailscale0 -- and everything a pod sends through the
// tunnel fragments. box never sets an MTU: it pins the address the MTU derives from,
// so the MTU follows whatever the uplink is.

const (
	// ARPHRD device types as /sys/class/net/<name>/type reports them: a tun device,
	// a wireguard interface and an ipip tunnel all report ARPHRD_NONE, and the TUNNEL
	// types are the older ipip/sit shapes.
	arphrdPPP     = 512
	arphrdTunnel  = 768
	arphrdTunnel6 = 769
	arphrdNone    = 65534

	// rtfUp is RTF_UP: both route tables print flags in hex, and a route without it
	// is not usable.
	rtfUp = 0x1
)

// Link is one local interface: the MTU an auto-detecting CNI would take, the device
// type that tells a tunnel from a wire, and its addresses.
type Link struct {
	Name         string
	MTU          int
	Type         int  // ARPHRD_*, 0 when /sys did not say
	PointToPoint bool // IFF_POINTOPOINT
	Addrs        []netip.Addr
}

// Tunnel reports whether pods on this interface would talk through a tunnel rather
// than the uplink.
//
// The device type decides, not the name: nothing reads "tailscale" or "wg" from an
// interface name. IFF_POINTOPOINT is the second signal, but a PPPoE link is
// point-to-point and a real uplink -- a home line with 1492 on ppp0 must install
// normally -- so ARPHRD_PPP is excluded by type.
func (l Link) Tunnel() bool {
	switch l.Type {
	case arphrdNone, arphrdTunnel, arphrdTunnel6:
		return true
	case arphrdPPP:
		return false
	}
	return l.PointToPoint
}

// Uplink is where the node IP comes from: the link holding the address the kernel
// sources from on the default route, its MTU, and whether it is a tunnel.
type Uplink struct {
	Interface string
	Address   string
	MTU       int
	Tunnel    bool
}

// Net reports this host's default route and links, read-only: consulted where box
// installs k0s and where it warns about a node address it can see locally.
type Net interface {
	// Uplink is the link carrying the default route and the address the kernel
	// prefers on it.
	Uplink() (Uplink, error)
	// InterfaceFor reports the local link holding addr; false means this host does not
	// hold it, so box is not on that node.
	InterfaceFor(addr string) (Link, bool)
}

// LinuxNet is the running host, read from the kernel's own files: the route tables
// say which family has a default route, a connected UDP socket says which address the
// kernel would source (connecting sends no packet), and /sys/class/net says what each
// device is. No netlink, no new dependency, no write. The fields let a test run
// against a fixture; the zero value is the real host.
type LinuxNet struct {
	procRoute, procRoute6, sysNet string
	links                         func() ([]Link, error)
	source                        func(network, destination string) (netip.Addr, error)
}

// Uplink finds the default route's family, the address the kernel sources for it and
// the link that address sits on: the tables decide the family, and the kernel's own
// choice of source address decides the link, policy routing included.
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

// defaultFamily is IPv4 first (dual-stack is a later spike), and a host whose only
// default route is IPv6 uses that. The node is one address, pinned, in either family.
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

// sourceFor asks the local address of a route to a documentation address in the
// family: a UDP connect resolves route and source without sending anything.
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

// hostLinks reads every interface with its MTU, addresses and /sys device type.
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

// deviceType reads /sys/class/net/<name>/type. Zero means the file did not say,
// which is not a tunnel type: a host hiding sysfs would otherwise turn every
// point-to-point uplink into a refusal.
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

// hasDefaultRouteIPv4 reads /proc/net/route: a default route's destination and mask
// are both zero, and the flags say it is up.
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

// hasDefaultRouteIPv6 reads /proc/net/ipv6_route: destination ::/0, flags hex in the
// ninth field.
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

// tunnelRefusal is the paragraph box stops with when the default route leaves through
// a tunnel: what it would have pinned, what that does to every pod, and the two ways
// out. No override flag on purpose: a full tunnel on the host is the operator's
// decision about the host.
func tunnelRefusal(up Uplink) string {
	if !up.Tunnel {
		return ""
	}
	return fmt.Sprintf("box pins the node IP to this host's uplink, and this host's default route leaves through %s, a tunnel with an MTU of %d: the node would register %s there, every pod would inherit that MTU, and anything they send through the tunnel would fragment. Bring the uplink's default route up and run bootstrap again, or install with the tunnel down.", up.Interface, up.MTU, up.Address)
}

// nodeIPWarning is the line box prints for a cluster it did not install whose node
// registered on a tunnel it can see: a warning, since that cluster is not box's to
// change.
func nodeIPWarning(addr string, l Link) string {
	return fmt.Sprintf("the node registered %s, the address on %s, a tunnel: every pod inherits %s's MTU %d. Pin the kubelet node IP to the uplink (--node-ip) and restart kubelet.", addr, l.Name, l.Name, l.MTU)
}

// nodeIPWarnings is nodeIPWarning over a cluster's addresses; one this host does not
// hold is skipped, being another node.
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
