package plan

import (
	"strings"
	"testing"
)

// hostsPlan: the Arrs and the torrent client in one namespace, a Seerr reaching
// them by Service name. Every name is invented.
const hostsPlan = `version: 2
data:
  root: /srv/library
  downloads: Downloads
  media: media
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    namespace: media
    ports: [8080]
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    namespace: media
    ports: [8989]
  seerr:
    kind: kubernetes
    source: overseerr
    config: /srv/apps/overseerr
    namespace: front
    ports: [5055]
hosts:
  - service: sonarr.media
    app: sonarr
  - service: qbittorrent.media
    port: 8080
    app: qbittorrent
  - host: 192.0.2.10
    leave: true
`

func TestResolve(t *testing.T) {
	p := mustParse(t, hostsPlan)
	tests := []struct {
		name, from, host string
		port             int
		want             Resolution
	}{
		{"localhost by port", "sonarr", "localhost", 8080, Resolution{Rewrite, "qbittorrent"}},
		{"loopback v4 by port", "sonarr", "127.0.0.1", 8080, Resolution{Rewrite, "qbittorrent"}},
		{"loopback range", "sonarr", "127.1.2.3", 8989, Resolution{Rewrite, "sonarr"}},
		{"loopback v6 bracketed", "sonarr", "[::1]", 5055, Resolution{Rewrite, "seerr"}},
		{"localhost on a port nobody had", "sonarr", "localhost", 9999, Resolution{}},
		{"localhost with no port", "sonarr", "localhost", 0, Resolution{}},
		{"short name from the same namespace", "qbittorrent", "sonarr", 8989, Resolution{Rewrite, "sonarr"}},
		{"short name from another namespace is not the Service", "seerr", "sonarr", 8989, Resolution{}},
		{"name.ns from anywhere", "seerr", "sonarr.media", 8989, Resolution{Rewrite, "sonarr"}},
		{"name.ns.svc", "seerr", "sonarr.media.svc", 8989, Resolution{Rewrite, "sonarr"}},
		{"full name, trailing dot, any case", "seerr", "Sonarr.Media.svc.cluster.local.", 8989, Resolution{Rewrite, "sonarr"}},
		{"a port-narrowed rule on its port", "sonarr", "qbittorrent", 8080, Resolution{Rewrite, "qbittorrent"}},
		{"a port-narrowed rule off its port", "sonarr", "qbittorrent", 6881, Resolution{}},
		{"a host left alone", "sonarr", "192.0.2.10", 8080, Resolution{Verdict: Leave}},
		{"an address nobody classified", "sonarr", "nas.example.net", 8080, Resolution{}},
		{"a longer name that only starts like the Service", "seerr", "sonarr.media.example.net", 8989, Resolution{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Resolve(tt.from, tt.host, tt.port); got != tt.want {
				t.Errorf("Resolve(%q, %q, %d) = %+v, want %+v", tt.from, tt.host, tt.port, got, tt.want)
			}
		})
	}
}

// pathsPlan: several apps on one public name, each under a path (box's own
// shape), one host serving one app at every path, one address left alone.
const pathsPlan = `version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
  sonarr-4k:
    kind: kubernetes
    source: sonarr4k
    config: /srv/apps/sonarr4k
    ports: [8990]
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
hosts:
  - host: panel.example.com
    path: /qbittorrent
    app: qbittorrent
  - host: panel.example.com
    path: /sonarr
    app: sonarr
  - host: panel.example.com
    path: /sonarr/4k
    app: sonarr-4k
  - host: plex.example.com
    app: plex
  - host: 192.0.2.10
    leave: true
`

// TestResolveURL: the deepest path covering an address wins on a segment
// boundary, and a host that only has path rules never answers one with no path.
func TestResolveURL(t *testing.T) {
	p := mustParse(t, pathsPlan)
	tests := []struct {
		name, host, path string
		port             int
		want             Resolution
	}{
		{"each app by its path", "panel.example.com", "/qbittorrent", 443, Resolution{Rewrite, "qbittorrent"}},
		{"the other app by its path", "panel.example.com", "/sonarr", 443, Resolution{Rewrite, "sonarr"}},
		{"a request under the app's path", "panel.example.com", "/qbittorrent/api/v2/app/version", 443, Resolution{Rewrite, "qbittorrent"}},
		{"the deepest path wins", "panel.example.com", "/sonarr/4k", 443, Resolution{Rewrite, "sonarr-4k"}},
		{"the deepest path wins further down", "panel.example.com", "/sonarr/4k/history", 443, Resolution{Rewrite, "sonarr-4k"}},
		{"the shallower path beside the deeper one", "panel.example.com", "/sonarr/calendar", 443, Resolution{Rewrite, "sonarr"}},
		{"a path that only starts like another", "panel.example.com", "/qbittorrentx", 443, Resolution{}},
		{"no path on a host of paths", "panel.example.com", "", 443, Resolution{}},
		{"the root of a host of paths", "panel.example.com", "/", 443, Resolution{}},
		{"a host with one rule answers any path", "plex.example.com", "/anything/at/all", 80, Resolution{Rewrite, "plex"}},
		{"a host with one rule and no path", "plex.example.com", "", 80, Resolution{Rewrite, "plex"}},
		{"another host", "nas.example.net", "/qbittorrent", 443, Resolution{}},
		{"a host left alone", "192.0.2.10", "/qbittorrent", 443, Resolution{Verdict: Leave}},
		{"localhost is still by port", "localhost", "/qbittorrent", 8080, Resolution{Rewrite, "qbittorrent"}},
		{"the host is case-insensitive, the path is not", "PANEL.Example.COM", "/qbittorrent", 443, Resolution{Rewrite, "qbittorrent"}},
		{"a path in another case is another path", "panel.example.com", "/Qbittorrent", 443, Resolution{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.ResolveURL("prowlarr", tt.host, tt.port, tt.path); got != tt.want {
				t.Errorf("ResolveURL(%q, %d, %q) = %+v, want %+v", tt.host, tt.port, tt.path, got, tt.want)
			}
		})
	}
}

// TestResolveNeverPicksAmongPaths: an address with no path is no evidence for any
// of a host's apps, so Resolve leaves it unclassified.
func TestResolveNeverPicksAmongPaths(t *testing.T) {
	p := mustParse(t, pathsPlan)
	if got := p.Resolve("prowlarr", "panel.example.com", 443); got.Verdict != Unclassified {
		t.Fatalf("Resolve on a host of path rules = %+v; the operator settles which app an address with no path meant", got)
	}
	if got := p.Resolve("prowlarr", "plex.example.com", 80); got != (Resolution{Rewrite, "plex"}) {
		t.Fatalf("Resolve on a host with one rule = %+v, want plex", got)
	}
}

func TestResolveRefusesAnAmbiguousLocalPort(t *testing.T) {
	p := mustParse(t, `version: 2
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/sonarr
    ports: [8989]
  sonarr-4k:
    kind: kubernetes
    source: sonarr4k
    config: /srv/sonarr4k
    ports: [8989]
`)
	if got := p.Resolve("seerr", "localhost", 8989); got.Verdict != Unclassified {
		t.Errorf("two apps on one port resolved to %+v; it is the operator's to settle", got)
	}
}

func TestDataRenames(t *testing.T) {
	p := mustParse(t, hostsPlan)
	got := p.Data.Renames()
	if len(got) != 1 || got[0] != [2]string{"Downloads", "downloads"} {
		t.Errorf("Renames() = %v, want only Downloads -> downloads", got)
	}
}

func TestParseRefusesBadDataAndHosts(t *testing.T) {
	base := "version: 2\napps:\n  q:\n    kind: kubernetes\n    source: q\n    config: /srv/q\n"
	tests := []struct{ name, extra, want string }{
		{"relative root", "data: {root: srv, downloads: d, media: m}\n", `root "srv" is not an absolute path`},
		{"root is /", "data: {root: /, downloads: d, media: m}\n", "root is /"},
		{"missing media", "data: {root: /srv, downloads: d}\n", "media is missing"},
		{"nested downloads", "data: {root: /srv, downloads: a/b, media: m}\n", `downloads "a/b" must be one directory`},
		{"dotdot", "data: {root: /srv, downloads: .., media: m}\n", `downloads ".." must be one directory`},
		{"same dir twice", "data: {root: /srv, downloads: x, media: x}\n", `downloads and media are both "x"`},
		{"swapped", "data: {root: /srv, downloads: media, media: Media}\n", "swapped"},
		{"neither service nor host", "hosts: [{app: q}]\n", "exactly one of service and host"},
		{"both service and host", "hosts: [{service: a.b, host: c, app: q}]\n", "exactly one of service and host"},
		{"service without namespace", "hosts: [{service: a, app: q}]\n", `service "a" is not <name>.<namespace>`},
		{"localhost as a rule", "hosts: [{host: 127.0.0.1, app: q}]\n", "resolved by the apps' ports"},
		{"app and leave", "hosts: [{host: c, app: q, leave: true}]\n", "exactly one of app and leave"},
		{"neither app nor leave", "hosts: [{host: c}]\n", "exactly one of app and leave"},
		{"unknown app", "hosts: [{host: c, app: nope}]\n", `app "nope" is not in apps`},
		{"outside without a namespace", "outside: [unifi]\n", `outside: "unifi" is not <namespace>/<name> or <namespace>/<name>/<container>`},
		{"outside with an empty container", "outside: [media/plex/]\n", "outside:"},
		{"outside with too many parts", "outside: [media/plex/exporter/extra]\n", "outside:"},
		{"bad host port", "hosts: [{host: c, port: 70000, leave: true}]\n", "port 70000"},
		{"a path on a service", "hosts: [{service: a.b, path: /x, app: q}]\n", "only for a host"},
		{"a path that is not one", "hosts: [{host: c, path: x, app: q}]\n", "must start with /"},
		{"a path across two lines", "hosts: [{host: c, path: \"/a\\nb\", app: q}]\n", "must be one line"},
		{"the same rule twice", "hosts: [{host: c, app: q}, {host: c, app: q}]\n", "appears twice"},
		{"the same rule in another case", "hosts: [{host: C, app: q}, {host: c, app: q}]\n", "appears twice"},
		{"the same rule twice at one port", "hosts: [{host: c, port: 80, app: q}, {host: c, port: 80, app: q}]\n", "appears twice"},
		{"the same path twice", "hosts: [{host: c, path: /a, app: q}, {host: c, path: /a, app: q}]\n", "appears twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(base + tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
	// The same host at several paths or ports is several rules, not a repeat.
	const many = "hosts: [{host: c, path: /a, app: q}, {host: c, path: /b, app: q}, {host: c, port: 80, app: q}, {host: c, port: 443, app: q}, {service: s.ns, app: q}]\n"
	if _, err := Parse([]byte(base + many)); err != nil {
		t.Errorf("a host at several paths or ports and a service is several rules, and Parse refused it: %v", err)
	}
	if _, err := Parse([]byte(base + "    ports: [0]\n")); err == nil || !strings.Contains(err.Error(), "port 0 is not a port") {
		t.Errorf("app port 0: error = %v", err)
	}
}

func TestOutsideNamesWorkloadsAndContainers(t *testing.T) {
	p := mustParse(t, "version: 2\napps:\n  q:\n    kind: kubernetes\n    source: q\n    config: /srv/q\n"+
		"outside: [media/gluetun, media/plex/plex-exporter]\n")
	want := []string{"media/gluetun", "media/plex/plex-exporter"}
	if len(p.Outside) != len(want) || p.Outside[0] != want[0] || p.Outside[1] != want[1] {
		t.Fatalf("Outside = %v, want %v", p.Outside, want)
	}
}
