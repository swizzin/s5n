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
// grant's and root assigns it (docs/box.md, "Root assigns the torrent port").
// box picks it once, from the band Kubernetes does not auto-assign from, and
// records it; every later run keeps what the grant says.
//
// Why a band rather than any port in the range: with the
// ServiceNodePortStaticSubrange behaviour (GA in 1.29) the API server hands out
// dynamic nodePorts from the top of the range and only falls into the bottom
// band once the top is exhausted. A port in that band is therefore never taken
// by a Service that asked for "any free port" -- which is what happened to the
// chart's fixed 31111: an implementation's LoadBalancer Service got 31111 first
// and the seedbox's own peers Service was refused for the rest of the install.
const (
	// torrentPortKey is the values path the port lives at. It is a grant key
	// (internal/seedbox/grant.go): the port is exposed on the node, beside every
	// other box's, so a values file may not set it.
	torrentPortKey = "apps.qbittorrent.torrent.port"
	// torrentApp is that key's app: the port is decided for the box that runs
	// it, and nothing else in the catalog exposes a peer port.
	torrentApp = "qbittorrent"

	// nodePortRangeStart and nodePortRangeEnd are the default
	// --service-node-port-range. box cannot read an apiserver's own range from
	// the API, so it assumes the default: a cluster that narrowed the range
	// refuses the picked port at apply, and that refusal is translated there
	// (cmd/box's portRangeHint) into the flag that widens it.
	nodePortRangeStart = 30000
	nodePortRangeEnd   = 32767
)

// NodePort is one nodePort a Service in the cluster already holds. Service is
// the Service that holds it (namespace/name), so a refusal can name how many
// Services stand in the way rather than how many numbers.
type NodePort struct {
	Port    int
	Service string
}

// staticBand is the part of a port range reserved for ports a person picks by
// hand: [start, start+offset-1], where the offset is the one Kubernetes
// computes. It mirrors calculateRangeOffset in
// pkg/registry/core/service/portallocator/allocator.go at the version
// charts/s5n/platform.yaml pins -- `min(max(16, size/32), 128)`, and nothing at
// all for a range of 16 ports or fewer. The default range holds 2768 ports, so
// the band is its first 86: 30000-30085.
func staticBand(start, end int) (lo, hi int) {
	size := end - start + 1
	const (
		min  = 16
		max  = 128
		step = 32
	)
	if size <= min {
		// Kubernetes reserves nothing, so every port is fair game for its own
		// allocations and box has no band to pick from.
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

// pickPort answers one free port in [lo, hi], chosen at random rather than as
// the lowest free one: boxes installed on the same day would otherwise walk the
// band in step and collide with each other on the next install.
//
// held is every nodePort the cluster already has. A Service holds two of them
// when its ports share a number over TCP and UDP, so the count of Services in a
// refusal is distinct names, not entries.
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

// ownPeerPort is the nodePort this seedbox's own peers Service holds, if the
// release is already installed. The chart names it <instance>-peers, and
// qBittorrent takes no instances, so the name is fixed.
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

// pickRandom is the default draw: crypto/rand, because a port that two boxes
// booting together can predict is a port they can both pick.
func pickRandom(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand does not fail on a running host; a fallback that guesses
		// is still better than refusing an install over a random source.
		return 0
	}
	return int(v.Int64())
}

// torrentPort settles the grant's torrent port, or nil when there is nothing to
// write. It keeps what the grant already says and only picks for a box that has
// no port yet: a working port is never moved, because peers learned it from the
// tracker and a new number drops every connection until the next announce.
//
// The app that holds the port is the box's, not this run's: a migrate install
// answers init with every app off and switches the plan's on afterwards, so
// opts.SwitchesOn is read beside apps.
func (d *Decision) torrentPort(ctx context.Context, namespace string, apps []string, grantNow map[string]any, opts Options) (any, error) {
	// The release being installed is what says whether the key exists at all:
	// it ships no default for the port any more, so the chart's own values do
	// not mention it, and a chart from before the port has no key in its schema
	// for this box to fill in.
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
	// A seedbox installed before box picked the port has none in its grant,
	// but its peers Service already holds the one its peers know. Keep that
	// one: moving a working peer port only makes every tracker relearn it.
	if port, ok := ownPeerPort(namespace, held); ok {
		d.report().Rows([]say.Row{{Mark: say.MarkNote, Name: torrentPortRow, State: "kept", Facts: []string{fmt.Sprintf("%d", port)}}})
		return port, nil
	}
	// A migrated box's peers, and any forward the operator set, already point
	// at the port the source announced. Keep it when a Service could hold it
	// here and nothing does: the whole NodePort range is the test, not the band
	// box picks from, because this number was not picked on this cluster and
	// moving it is exactly what costs the connections.
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
