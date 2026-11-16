package plan

import (
	"strings"
	"testing"
)

// hostsPlan is a source where the Arrs and the torrent client ran in one
// namespace and a Seerr reached them by Service name. Every name is invented.
const hostsPlan = `version: 1
data:
  root: /srv/library
  downloads: Downloads
  media: media
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    namespace: media
    ports: [8080]
  sonarr:
    source: sonarr
    config: /srv/apps/sonarr
    namespace: media
    ports: [8989]
  seerr:
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

func TestResolveRefusesAnAmbiguousLocalPort(t *testing.T) {
	p := mustParse(t, `version: 1
apps:
  sonarr:
    source: sonarr
    config: /srv/sonarr
    ports: [8989]
  sonarr-4k:
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
	base := "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n"
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(base + tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
	if _, err := Parse([]byte(base + "    ports: [0]\n")); err == nil || !strings.Contains(err.Error(), "port 0 is not a port") {
		t.Errorf("app port 0: error = %v", err)
	}
}

// TestOutsideNamesWorkloadsAndContainers: an outside entry is a workload, or one
// container of one -- the sidecar in a catalog app's pod that the chart's pod
// does not carry -- and both read back from the file.
func TestOutsideNamesWorkloadsAndContainers(t *testing.T) {
	p := mustParse(t, "version: 1\napps:\n  q:\n    source: q\n    config: /srv/q\n"+
		"outside: [media/gluetun, media/plex/plex-exporter]\n")
	want := []string{"media/gluetun", "media/plex/plex-exporter"}
	if len(p.Outside) != len(want) || p.Outside[0] != want[0] || p.Outside[1] != want[1] {
		t.Fatalf("Outside = %v, want %v", p.Outside, want)
	}
}
