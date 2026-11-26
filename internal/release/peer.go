package release

import (
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// peerPortKey is the peer port, a node port shared with other boxes: a grant
// key (internal/seedbox/grant.go), never a values key.
const peerPortKey = "apps.qbittorrent.torrent.port"

// peerPortApp is the app that holds it. Nothing else in the catalog exposes a
// port on the node.
const peerPortApp = "qbittorrent"

// legacyPeerPort is the chart's old default; a box from then has no port in
// either file and its release renders this one.
const legacyPeerPort = 31111

// keepPeerPort keeps a box's running peer port when neither file records one,
// and validates the result. Moving a port peers already announce would drop
// connections until the next announce, so the stored value (or the chart's old
// 31111 default) is written instead. A first install with no port is refused:
// init picks it. A chart without the key is left alone.
func keepPeerPort(values, deployed map[string]any, exists bool, schema []byte, r say.Reporter) error {
	if !appEnabled(values, peerPortApp) {
		return nil
	}
	if _, recorded := portAt(values); recorded {
		return nil
	}
	if _, declared := valueAt(values, "apps."+peerPortApp+".torrent"); !declared {
		return nil
	}
	if !exists {
		// A first install has nothing to keep; picking needs init, not apply.
		return fmt.Errorf("%s is not recorded in the grant: run `box init` to pick one", peerPortKey)
	}
	if port, ok := portAt(deployed); ok {
		setValue(values, peerPortKey, port)
		r.Line(fmt.Sprintf("torrent port: %d (already this box's; kept)", port))
		return seedbox.Validate(schema, values)
	}
	setValue(values, peerPortKey, legacyPeerPort)
	r.Line(fmt.Sprintf("torrent port: %d (the chart's old default, which this box's release runs; kept)", legacyPeerPort))
	return seedbox.Validate(schema, values)
}

// valueAt reads one dotted values path, if it is there.
func valueAt(values map[string]any, path string) (any, bool) {
	var cur any = values
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[key]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// setValue writes one dotted values path, creating the mappings on the way, so
// a release whose values omit the branch still gets a complete document.
func setValue(values map[string]any, path string, value any) {
	keys := strings.Split(path, ".")
	cur := values
	for i, key := range keys {
		if i == len(keys)-1 {
			cur[key] = value
			return
		}
		next, ok := cur[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[key] = next
		}
		cur = next
	}
}

// portAt is the peer port; YAML gives an int, Helm a float64, so both count.
func portAt(values map[string]any) (int, bool) {
	raw, ok := valueAt(values, peerPortKey)
	if !ok {
		return 0, false
	}
	var port int
	switch v := raw.(type) {
	case int:
		port = v
	case int64:
		port = int(v)
	case float64:
		port = int(v)
	default:
		return 0, false
	}
	return port, port > 0
}

// appEnabled reports whether the values switch one app on.
func appEnabled(values map[string]any, app string) bool {
	entry, _ := valueAt(values, "apps."+app)
	on, _ := entry.(map[string]any)
	enabled, _ := on["enabled"].(bool)
	return enabled
}
