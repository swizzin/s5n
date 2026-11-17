package plan

import (
	"errors"
	"strings"
	"testing"
)

// sourceV2 is a plan with the whole version 2 shape: the source it was read
// from, one app per platform, an image and the variant it maps to, and the two
// kinds of rule discovery can write as a default. Every name is invented.
const sourceV2 = `version: 2
source:
  host: media-host.example.test
  taken: 2026-11-16T12:00:00Z
  owner:
    uid: 1000
    gid: 1000
  kinds: [kubernetes, swizzin]
apps:
  sonarr:
    kind: kubernetes
    source: Sonarr
    image: lscr.io/linuxserver/sonarr:4.0.14
    variant: lt1.2
    config: /srv/apps/sonarr
    namespace: media
    ports: [8989]
    paths:
      - from: /library/media
        to: /data/media
      - from: /home/user/media
        to: /data/media
        why: the swizzin unit kept its library under the user's home
        default: true
  qbittorrent:
    kind: swizzin
    source: qbittorrent@user
    config: /home/user/.config/qBittorrent
    ports: [8080]
    paths:
      - from: /home/user/torrents
        to: /data/downloads
  plex:
    kind: docker
    source: plexinc/pms-docker
    config: /srv/apps/plex
    ports: [32400]
    paths: []
hosts:
  - host: 10.0.0.5
    port: 32400
    app: plex
    why: a pod address that does not survive the move; only plex listens on 32400
    default: true
`

func TestParseReadsTheSourceBlock(t *testing.T) {
	p := mustParse(t, sourceV2)
	s := p.Source
	if s == nil {
		t.Fatal("source block was not read")
	}
	if s.Host != "media-host.example.test" || s.Taken != "2026-11-16T12:00:00Z" {
		t.Errorf("source = %+v, want the host and the time from the file", s)
	}
	if s.Owner == nil || s.Owner.UID != 1000 || s.Owner.GID != 1000 {
		t.Errorf("owner = %+v, want 1000:1000", s.Owner)
	}
	if len(s.Kinds) != 2 || s.Kinds[0] != Kubernetes || s.Kinds[1] != Swizzin {
		t.Errorf("kinds = %v, want the two platforms read", s.Kinds)
	}
	sonarr := p.Apps["sonarr"]
	if sonarr.Kind != Kubernetes || sonarr.Image != "lscr.io/linuxserver/sonarr:4.0.14" || sonarr.Variant != "lt1.2" {
		t.Errorf("sonarr = %+v, want its kind, image and variant", sonarr)
	}
	if got := p.Apps["qbittorrent"].Kind; got != Swizzin {
		t.Errorf("qbittorrent kind = %q, want %q", got, Swizzin)
	}
	// A default is a decision with a reason, on a path rule and on an address.
	want := []PathRule{
		{From: "/library/media", To: "/data/media"},
		{From: "/home/user/media", To: "/data/media",
			Why: "the swizzin unit kept its library under the user's home", Default: true},
	}
	if got := sonarr.Paths; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("paths = %+v, want %+v", got, want)
	}
	if h := p.Hosts[0]; h.Why == "" || !h.Default || h.Port != 32400 {
		t.Errorf("host rule = %+v, want the default's reason and port", h)
	}
}

// TestPlanHoldsAppsFromEveryPlatform: one plan carries the apps of several
// sources, which is what "combinations in one plan" means for the file.
func TestPlanHoldsAppsFromEveryPlatform(t *testing.T) {
	p := mustParse(t, sourceV2)
	kinds := map[string]bool{}
	for _, app := range p.Apps {
		kinds[app.Kind] = true
	}
	if !kinds[Kubernetes] || !kinds[Swizzin] {
		t.Fatalf("apps' kinds = %v, want apps from both platforms", kinds)
	}
	// The path tables stay per app: a swizzin app's rule does not touch the
	// cluster app's paths.
	if got, ok := p.Rewrite("qbittorrent", "/home/user/torrents/a.mkv"); !ok || got != "/data/downloads/a.mkv" {
		t.Errorf("qbittorrent rewrite = (%q, %v), want its own rule", got, ok)
	}
	if _, ok := p.Rewrite("sonarr", "/home/user/torrents/a.mkv"); ok {
		t.Error("a swizzin app's rule applied to the cluster app's paths")
	}
}

func TestVersion1IsRefusedWithTheWayForward(t *testing.T) {
	_, err := Parse([]byte("version: 1\napps: {}\n"))
	if err == nil {
		t.Fatal("Parse accepted a version 1 plan")
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("error is %T, want *plan.Error: %v", err, err)
	}
	for _, want := range []string{"version 1", "this box reads version 2", "re-run `box migrate discover`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestV2Refusals: what a version 2 plan has to say, one test per way it can be
// wrong. Each case is the smallest file that shows one problem.
func TestV2Refusals(t *testing.T) {
	app := func(fields string) string {
		return "version: 2\napps:\n  q:\n    kind: kubernetes\n    source: q\n    config: /srv/q\n" + fields
	}
	tests := []struct{ name, src, want string }{
		{
			"an app with no kind",
			"version: 2\napps:\n  q:\n    source: q\n    config: /srv/q\n",
			"apps.q: kind is missing",
		},
		{
			"an app from a platform box cannot read",
			"version: 2\napps:\n  q:\n    kind: podman\n    source: q\n    config: /srv/q\n",
			`apps.q: kind "podman" is not one of kubernetes, swizzin or docker`,
		},
		{
			"a variant with no image behind it",
			app("    variant: lt1.2\n"),
			`apps.q: variant "lt1.2" is a mapping from an image, and image is empty`,
		},
		{
			"a defaulted path rule with no reason",
			app("    paths:\n      - from: /a\n        to: /b\n        default: true\n"),
			"default: true needs a why",
		},
		{
			"a path rule's reason on two lines",
			app("    paths:\n      - from: /a\n        to: /b\n        why: \"first\\nsecond\"\n"),
			"why \"first\\nsecond\" must be one line",
		},
		{
			"a defaulted address with no reason",
			app("    paths: []\nhosts: [{host: c.example.test, app: q, default: true}]\n"),
			"default: true needs a why",
		},
		{
			"an address rule's reason on two lines",
			app("    paths: []\nhosts: [{host: c.example.test, app: q, why: \"first\\nsecond\"}]\n"),
			"must be one line",
		},
		{
			"a source with no kinds",
			app("    paths: []\nsource: {taken: 2026-11-16T12:00:00Z}\n"),
			"source: kinds is empty",
		},
		{
			"a source kind box cannot read",
			app("    paths: []\nsource: {taken: 2026-11-16T12:00:00Z, kinds: [podman]}\n"),
			`source: kind "podman" is not one of`,
		},
		{
			"a source kind named twice",
			app("    paths: []\nsource: {taken: 2026-11-16T12:00:00Z, kinds: [docker, docker]}\n"),
			`source: kind "docker" appears twice`,
		},
		{
			"a source with no time",
			app("    paths: []\nsource: {kinds: [kubernetes]}\n"),
			"source: taken is missing",
		},
		{
			"a source time that is not one",
			app("    paths: []\nsource: {taken: yesterday, kinds: [kubernetes]}\n"),
			`source: taken "yesterday" is not a time`,
		},
		{
			"a source host on two lines",
			app("    paths: []\nsource: {host: \"a\\nb\", taken: 2026-11-16T12:00:00Z, kinds: [kubernetes]}\n"),
			"source: host",
		},
		{
			"an owner that is not a uid:gid",
			app("    paths: []\nsource: {taken: 2026-11-16T12:00:00Z, kinds: [kubernetes], owner: {uid: -1, gid: 0}}\n"),
			"source: owner -1:0 is not a uid:gid",
		},
		{
			"an unknown key in the source block",
			app("    paths: []\nsource: {taken: 2026-11-16T12:00:00Z, kinds: [kubernetes], hostname: x}\n"),
			"hostname",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("Parse accepted:\n%s", tt.src)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

// TestSourceBlockIsOptional: a plan for one app's path table needs no source
// behind it, which is what the rewriters' own fixtures are.
func TestSourceBlockIsOptional(t *testing.T) {
	p := mustParse(t, `version: 2
apps:
  q:
    kind: docker
    source: q
    config: /srv/q
    paths: []
`)
	if p.Source != nil {
		t.Errorf("source = %+v, want none", p.Source)
	}
}
