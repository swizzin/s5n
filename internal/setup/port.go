package setup

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// The peer port is a port on the node, shared by every seedbox, so it is the
// grant's and root assigns it (docs/box.md, "Root assigns the torrent port"). box
// picks it once, from the band Kubernetes does not auto-assign from, and every
// later run keeps what the grant says.
//
// The band: with ServiceNodePortStaticSubrange (GA in 1.29) the API server hands
// out dynamic nodePorts from the top of the range, falling into the bottom band
// only when the top is exhausted, so a port there is never taken by a Service that
// asked for "any free port" -- as happened to the chart's fixed 31111, which an
// implementation's LoadBalancer Service got first.
const (
	// torrentPortKey is the values path the port lives at, a grant key
	// (internal/seedbox/grant.go): it is exposed on the node, so no values file may
	// set it.
	torrentPortKey = "apps.qbittorrent.torrent.port"
	// torrentApp is that key's app: nothing else in the catalog exposes a peer port.
	torrentApp = "qbittorrent"

	// nodePortRangeStart and nodePortRangeEnd are the default --service-node-port-range.
	// box cannot read an apiserver's own range, so it assumes the default: a
	// narrowed cluster refuses the port at apply, and cmd/box's portRangeHint turns
	// that refusal into the flag that widens it.
	nodePortRangeStart = 30000
	nodePortRangeEnd   = 32767
)

// NodePort is one nodePort a Service holds. Service is that Service
// (namespace/name), so a refusal counts Services rather than numbers.
type NodePort struct {
	Port    int
	Service string
}

// staticBand is the part of a range reserved for ports picked by hand,
// [start, start+offset-1]. It mirrors calculateRangeOffset in
// pkg/registry/core/service/portallocator/allocator.go at the version
// charts/s5n/platform.yaml pins: `min(max(16, size/32), 128)`, and nothing for 16
// ports or fewer. The default range's band is its first 86: 30000-30085.
func staticBand(start, end int) (lo, hi int) {
	size := end - start + 1
	const (
		min  = 16
		max  = 128
		step = 32
	)
	if size <= min {
		// Kubernetes reserves nothing, so box has no band to pick from.
		return start, start - 1
	}
	offset := size / step
	if offset < min {
		offset = min
	}
	if offset > max {
		offset = max
	}
	return start, start + offset - 1
}

// pickPort answers one free port in [lo, hi], at random rather than the lowest:
// boxes installed the same day would otherwise walk the band in step and collide on
// the next install. held is every nodePort the cluster has; a Service holds two
// entries when a number is shared over TCP and UDP, so a refusal counts names.
func pickPort(lo, hi int, held []NodePort, randInt func(int) int) (int, error) {
	used := make(map[int]bool, len(held))
	blocking := make(map[string]bool, len(held))
	for _, h := range held {
		used[h.Port] = true
		if h.Port >= lo && h.Port <= hi && h.Service != "" {
			blocking[h.Service] = true
		}
	}
	free := make([]int, 0, hi-lo+1)
	for port := lo; port <= hi; port++ {
		if !used[port] {
			free = append(free, port)
		}
	}
	if len(free) == 0 {
		return 0, fmt.Errorf("the torrent port cannot be picked: every port box may pick from (%d-%d, the part of the NodePort range Kubernetes does not assign by itself) is held by %d %s in this cluster",
			lo, hi, len(blocking), plural(len(blocking), "Service", "Services"))
	}
	return free[randInt(len(free))], nil
}

// torrentPortRow is the row the decided port is printed as.
const torrentPortRow = "torrent port"

// ownPeerPort is the nodePort this seedbox's peers Service holds, if the release is
// installed. qBittorrent takes no instances, so the name is fixed.
func ownPeerPort(namespace string, held []NodePort) (int, bool) {
	if namespace == "" {
		return 0, false
	}
	want := namespace + "/" + torrentApp + "-peers"
	for _, h := range held {
		if h.Service == want && h.Port > 0 {
			return h.Port, true
		}
	}
	return 0, false
}

// pickRandom is the default draw: crypto/rand, because a predictable port is one
// two boxes booting together can both pick.
func pickRandom(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand does not fail on a running host; guessing beats refusing an
		// install over a random source.
		return 0
	}
	return int(v.Int64())
}

// torrentPort settles the grant's torrent port, or nil when there is nothing to
// write. It keeps what the grant says and picks only for a box with no port: a
// working port is never moved, because peers learned it from the tracker and a new
// number drops every connection until the next announce.
//
// The app that holds the port is the box's, not this run's: a migrate install
// answers with every app off and switches the plan's on later, so SwitchesOn counts.
func (d *Decision) torrentPort(ctx context.Context, namespace string, apps []string, grantNow map[string]any, opts Options) (any, error) {
	// The release says whether the key exists: the chart ships no default for the
	// port, and one from before it has no key in its schema to fill in.
	declared, err := seedbox.Declares(d.Schema, strings.Split(torrentPortKey, ".")...)
	if err != nil {
		return nil, err
	}
	if !declared {
		return nil, nil
	}
	running := apps
	if len(opts.SwitchesOn) > 0 {
		running = append(append([]string{}, apps...), opts.SwitchesOn...)
	}
	if !contains(running, torrentApp) {
		return nil, nil
	}
	if port, ok := intAt(grantNow, torrentPortKey); ok && port > 0 {
		return port, nil
	}
	if d.NodePorts == nil {
		return nil, fmt.Errorf("%s is not set and box cannot read this cluster's Services to pick a free port: run `box init` on the host `box bootstrap` set up, or as root", torrentPortKey)
	}
	held, err := d.NodePorts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read this cluster's Services to pick a free torrent port: %w", err)
	}
	// A seedbox installed before box picked the port has none in its grant, but its
	// peers Service holds the one its peers know: keep it.
	if port, ok := ownPeerPort(namespace, held); ok {
		d.report().Rows([]say.Row{{Mark: say.MarkNote, Name: torrentPortRow, State: "kept", Facts: []string{fmt.Sprintf("%d", port)}}})
		return port, nil
	}
	// A migrated box's peers, and any forward the operator set, point at the port the
	// source announced. Keep it when a Service could hold it and nothing does: the
	// whole NodePort range is the test, not box's band, since this number was not
	// picked here and moving it costs the connections.
	if port := opts.TorrentPort; port >= nodePortRangeStart && port <= nodePortRangeEnd && !portHeld(port, held) {
		d.report().Rows([]say.Row{{Mark: say.MarkNote, Name: torrentPortRow, State: "kept", Facts: []string{fmt.Sprintf("%d from the source", port)}}})
		return port, nil
	}
	lo, hi := staticBand(nodePortRangeStart, nodePortRangeEnd)
	randInt := d.Rand
	if randInt == nil {
		randInt = pickRandom
	}
	port, err := pickPort(lo, hi, held, randInt)
	if err != nil {
		return nil, err
	}
	d.report().Rows([]say.Row{{Mark: say.MarkNote, Name: torrentPortRow, State: "picked", Facts: []string{fmt.Sprintf("%d", port)}}})
	return port, nil
}

// portHeld reports whether a Service in the cluster already holds port.
func portHeld(port int, held []NodePort) bool {
	for _, h := range held {
		if h.Port == port {
			return true
		}
	}
	return false
}
