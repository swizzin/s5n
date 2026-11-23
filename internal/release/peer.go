package release

import (
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/seedbox"
)

// peerPortKey is where a seedbox's peer port lives: a port on the node, shared
// with every other box on it, so it is a grant key (internal/seedbox/grant.go)
// and a values file may not set it.
const peerPortKey = "apps.qbittorrent.torrent.port"

// peerPortApp is the app that holds it. Nothing else in the catalog exposes a
// port on the node.
const peerPortApp = "qbittorrent"

// legacyPeerPort is the port the chart defaulted to before it shipped no
// default at all. A box installed while it did has no port in either of its
// files, and its release renders this one.
const legacyPeerPort = 31111

// keepPeerPort writes the peer port into the values a box with none recorded is
// about to install, and checks the document it wrote against the chart's own
// schema.
//
// A box has no port recorded for one of two reasons, and both must keep running
// rather than be refused by a chart that ships no default: it was installed
// while the chart defaulted the port to 31111, or its release was installed
// without box. The number such a box already runs is in the release's own
// values -- Helm stores what it was given, and an old box's stored values carry
// the chart's old default -- so that is the number this run keeps: moving the
// port a box's peers already announce would drop every connection until the
// next tracker announce, and moving it is never this command's business. A
// release that records no port either falls back to the same old default, which
// is the port its Service is on.
//
// Nothing is written for a release that has no peer port at all (a chart from
// before the key, or one without qBittorrent): its own schema would refuse it,
// and box does not invent a key its release cannot take.
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
	if exists {
		if port, ok := portAt(deployed); ok {
			setValue(values, peerPortKey, port)
			r.Line(fmt.Sprintf("torrent port: %d (already this box's; kept)", port))
			return seedbox.Validate(schema, values)
		}
	}
	setValue(values, peerPortKey, legacyPeerPort)
	r.Line(fmt.Sprintf("torrent port: %d (the chart's old default; run `box init` to pick one)", legacyPeerPort))
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

// portAt is the peer port one values document records. A document read from
// YAML has an int and one read back from Helm has a float64, so both count.
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
