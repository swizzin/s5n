package discover

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// sourceInstall is the shape migrate was written for: two Sonarr instances
// sharing one library mounted at /library, qBittorrent seeing the same downloads
// as /downloads, Plex with only the library mounted, and something box has no
// app for running beside them.
func sourceInstall(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.app(t, appSpec{
		name: "sonarr", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8989,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.file(t, "/srv/apps/sonarr/config.xml", `<?xml version="1.0" encoding="utf-8"?>
<Config>
  <BindAddress>*</BindAddress>
  <Port>8989</Port>
  <SslPort>9898</SslPort>
  <EnableSsl>False</EnableSsl>
  <UrlBase></UrlBase>
</Config>
`)
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDB(t, "/library/media/TV"))

	f.app(t, appSpec{
		name: "sonarr4k", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8990,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr4k"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.file(t, "/srv/apps/sonarr4k/config.xml", "<Config><Port>8990</Port></Config>\n")
	f.bytes(t, "/srv/apps/sonarr4k/sonarr.db", arrDB(t, "/library/media/TV-4K"))

	f.app(t, appSpec{
		name: "qbittorrent", image: "ghcr.io/home-operations/qbittorrent:5.3.0", port: 8080,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/qbittorrent"},
			{volume: "downloads", path: "/downloads", host: "/srv/library/Downloads"},
			// A watched folder outside the data root: worth migrating only as
			// a fact the operator has to hear about.
			{volume: "watch", path: "/watch", host: "/srv/incoming"},
		},
	})
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", `[BitTorrent]
Session\DefaultSavePath=/downloads
Session\TempPath=/downloads/incomplete

[WebUI]
Port=8080
`)
	f.app(t, appSpec{
		name: "plex", image: "linuxserver/plex:1.41.5", port: 32400,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/plex"},
			{volume: "media", path: "/media", host: "/srv/library/media"},
		},
		emptyDirs: []mount{{volume: "transcode", path: "/transcode"}},
	})
	f.app(t, appSpec{name: "gluetun", image: "ghcr.io/qdm12/gluetun:v3.39.1", port: 8888})
	f.service(t, "sonarr", "media", "sonarr")
	f.service(t, "qbittorrent", "media", "qbittorrent")
	f.service(t, "plex", "media", "plex")
	return f
}

// TestDiscoverTwoArrs is the whole answer for the source install: every app with
// its own path table, the data root with its two directories, one rule per
// Service, and the workload left behind.
func TestDiscoverTwoArrs(t *testing.T) {
	f := sourceInstall(t)
	p, rep := f.discover(t)

	want := &plan.Plan{
		Version: plan.Version,
		Data: &plan.Data{
			Root: "/srv/library",
			// The source calls it Downloads; apply renames it to the
			// contract's downloads, which is why the rules below say
			// /data/downloads already.
			Downloads: "Downloads",
			Media:     "media",
		},
		Apps: map[string]plan.App{
			"sonarr": {
				Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media", Ports: []int{8989},
				Paths: []plan.PathRule{
					{From: "/config", To: "/config"},
					{From: "/library/Downloads", To: "/data/downloads"},
					{From: "/library/media", To: "/data/media"},
				},
			},
			"sonarr-4k": {
				Source: "sonarr4k", Config: "/srv/apps/sonarr4k", Namespace: "media", Ports: []int{8990},
				Paths: []plan.PathRule{
					{From: "/config", To: "/config"},
					{From: "/library/Downloads", To: "/data/downloads"},
					{From: "/library/media", To: "/data/media"},
				},
			},
			"qbittorrent": {
				Source: "qbittorrent", Config: "/srv/apps/qbittorrent", Namespace: "media", Ports: []int{8080},
				Paths: []plan.PathRule{
					{From: "/config", To: "/config"},
					{From: "/downloads", To: "/data/downloads"},
				},
			},
			"plex": {
				Source: "plex", Config: "/srv/apps/plex", Namespace: "media", Ports: []int{32400},
				Paths: []plan.PathRule{
					{From: "/config", To: "/config"},
					{From: "/media", To: "/data/media"},
					// The source's emptyDir is scratch, and the release
					// mounts the same /transcode.
					{From: "/transcode", To: "/transcode"},
				},
			},
		},
		Hosts: []plan.HostRule{
			{Service: "plex.media", App: "plex", Port: 80},
			{Service: "qbittorrent.media", App: "qbittorrent", Port: 80},
			{Service: "sonarr.media", App: "sonarr", Port: 80},
		},
		Outside: []string{"media/gluetun"},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("plan:\ngot  %#v\nwant %#v", p, want)
	}
	// What apply reads is the file, not the value.
	roundTrip(t, p)

	if len(rep.Skipped) != 1 || rep.Skipped[0].Workload != "media/gluetun" {
		t.Fatalf("skipped: %#v", rep.Skipped)
	}
	if !strings.Contains(rep.Skipped[0].Why, "gluetun") {
		t.Fatalf("the skip does not name the image it did not recognise: %q", rep.Skipped[0].Why)
	}
	if len(rep.Ignored) != 1 {
		t.Fatalf("ignored: %#v", rep.Ignored)
	}
	if got := rep.Ignored[0]; got.App != "qbittorrent" || got.Mount != "/watch" || got.Host != "/srv/incoming" {
		t.Fatalf("ignored mount: %#v", got)
	}
	if !strings.Contains(rep.Ignored[0].Why, "/srv/library") {
		t.Fatalf("the ignored mount does not name the data root: %q", rep.Ignored[0].Why)
	}
	if len(rep.Notes) != 0 {
		t.Fatalf("notes nothing should need: %#v", rep.Notes)
	}
	if len(rep.Raw) == 0 {
		t.Fatal("no manifests were recorded for the plan directory")
	}
	if _, ok := rep.Raw["StatefulSet/media/sonarr"]; !ok {
		t.Fatalf("the source's manifests are not keyed as read: %#v", keys(rep.Raw))
	}
}

// TestDiscoverRefusesDataToMove is the one layout box cannot adopt: the library
// and the downloads are not two directories under one parent, so a rename can no
// longer keep the inode a download and its import share.
func TestDiscoverRefusesDataToMove(t *testing.T) {
	f := sourceInstall(t)
	// The same Sonarr, with its library on a different filesystem from the
	// downloads.
	f.app(t, appSpec{
		name: "radarr", image: "lscr.io/linuxserver/radarr:5.20.1", port: 7878,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/radarr"},
			{volume: "library", path: "/library", host: "/srv/media"},
		},
	})
	f.file(t, "/srv/apps/radarr/config.xml", "<Config><Port>7878</Port></Config>\n")
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDB(t, "/library/movies"))

	err := f.discoverErr(t)
	msg := err.Error()
	if !strings.Contains(msg, "moved by hand") {
		t.Fatalf("the refusal does not say the data has to move: %v", err)
	}
	for _, want := range []string{"/srv/library", "/srv/media/movies"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal does not name %s: %v", want, err)
		}
	}
}

// TestDiscoverRefusesOneInstanceTwice: two workloads cannot both be box's
// sonarr-4k, and the refusal names both so the operator knows which to remove.
func TestDiscoverRefusesOneInstanceTwice(t *testing.T) {
	f := sourceInstall(t)
	f.app(t, appSpec{
		name: "sonarr-4k", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8991,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr-4k"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	err := f.discoverErr(t)
	for _, want := range []string{"media/sonarr4k", "media/sonarr-4k", "sonarr-4k"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %s: %v", want, err)
		}
	}
}

// TestDiscoverOutside: a workload of an app box installs but which takes no
// second instance is not migrated either, and says so.
func TestDiscoverOutside(t *testing.T) {
	f := sourceInstall(t)
	f.app(t, appSpec{
		name: "plex2", image: "linuxserver/plex:1.41.5", port: 32401,
		mounts: []mount{{volume: "config", path: "/config", host: "/srv/apps/plex2"}},
	})
	p, rep := f.discover(t)
	if !reflect.DeepEqual(p.Outside, []string{"media/gluetun", "media/plex2"}) {
		t.Fatalf("outside: %#v", p.Outside)
	}
	var why string
	for _, s := range rep.Skipped {
		if s.Workload == "media/plex2" {
			why = s.Why
		}
	}
	if !strings.Contains(why, "takes no second instance") {
		t.Fatalf("the skip does not say why: %q", why)
	}
	roundTrip(t, p)
}

// TestDiscoverConfigMountPerApp: Seerr keeps its config at /app/config and
// PeerBanHelper at /app/data under the upstream images, and each is the
// directory the app will see as /config.
func TestDiscoverConfigMountPerApp(t *testing.T) {
	f := sourceInstall(t)
	f.app(t, appSpec{
		name: "overseerr", image: "sctx/overseerr:1.33.2", port: 5055,
		mounts: []mount{
			{volume: "config", path: "/app/config", host: "/srv/apps/overseerr"},
			{volume: "media", path: "/media", host: "/srv/library/media"},
		},
	})
	f.file(t, "/srv/apps/overseerr/settings.json", `{"main":{"apiKey":"x"}}`)
	f.app(t, appSpec{
		name: "peerbanhelper", image: "ghcr.io/ghostchu/peerbanhelper:v7.3.0", port: 9898,
		mounts: []mount{
			{volume: "data", path: "/app/data", host: "/srv/apps/pbh"},
			{volume: "downloads", path: "/downloads", host: "/srv/library/Downloads"},
		},
	})
	f.file(t, "/srv/apps/pbh/config/config.yml", "server:\n  http: 9898\n")

	p, _ := f.discover(t)
	seerr, ok := p.Apps["seerr"]
	if !ok {
		t.Fatalf("the source's Overseerr is not box's seerr: %#v", keys(p.Apps))
	}
	if seerr.Source != "overseerr" || seerr.Config != "/srv/apps/overseerr" || seerr.Ports[0] != 5055 {
		t.Fatalf("seerr: %#v", seerr)
	}
	if !reflect.DeepEqual(seerr.Paths, []plan.PathRule{
		{From: "/app/config", To: "/config"},
		{From: "/media", To: "/data/media"},
	}) {
		t.Fatalf("seerr paths: %#v", seerr.Paths)
	}
	pbh := p.Apps["pbh"]
	if pbh.Source != "peerbanhelper" || pbh.Config != "/srv/apps/pbh" || pbh.Ports[0] != 9898 {
		t.Fatalf("pbh: %#v", pbh)
	}
	if !reflect.DeepEqual(pbh.Paths, []plan.PathRule{
		{From: "/app/data", To: "/config"},
		{From: "/downloads", To: "/data/downloads"},
	}) {
		t.Fatalf("pbh paths: %#v", pbh.Paths)
	}
	roundTrip(t, p)
}

// TestDiscoverRefusesConfigWithoutHostDirectory: a config in a volume that is
// not a host directory has nothing to copy, and the refusal names the claim.
func TestDiscoverRefusesConfigWithoutHostDirectory(t *testing.T) {
	f := newFixture(t)
	f.object(t, "sonarr", `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: sonarr
  namespace: media
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: sonarr
    spec:
      containers:
        - name: sonarr
          image: lscr.io/linuxserver/sonarr:4.0.14
          volumeMounts:
            - name: config
              mountPath: /config
      volumes:
        - name: config
          persistentVolumeClaim:
            claimName: sonarr-config
`)
	err := f.discoverErr(t)
	for _, want := range []string{"media/sonarr", "sonarr-config", "host directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %s: %v", want, err)
		}
	}
}

// TestDiscoverReportsADataClaimItCannotReach: a persistent volume nothing on this
// host backs is data box cannot adopt, and it is reported rather than dropped.
func TestDiscoverReportsADataClaimItCannotReach(t *testing.T) {
	f := newFixture(t)
	f.object(t, "qbittorrent", `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: qbittorrent
  namespace: media
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: qbittorrent
    spec:
      containers:
        - name: qbittorrent
          image: ghcr.io/home-operations/qbittorrent:5.3.0
          ports:
            - containerPort: 8080
          volumeMounts:
            - name: config
              mountPath: /config
            - name: downloads
              mountPath: /downloads
            - name: shares
              mountPath: /share
      volumes:
        - name: config
          hostPath: {path: /srv/apps/qbittorrent}
        - name: downloads
          hostPath: {path: /srv/library/Downloads}
        - name: shares
          persistentVolumeClaim:
            claimName: qbittorrent-shares
`)
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	f.app(t, appSpec{
		name: "sonarr", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8989,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDB(t, "/library/media/TV"))
	// The shares volume is a claim a provisioner filled: it exists, it is
	// mounted, and no host directory holds it.
	f.object(t, "shares", `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: qbittorrent-shares
  namespace: media
spec:
  volumeName: qbittorrent-shares
`)
	f.object(t, "shares-volume", `apiVersion: v1
kind: PersistentVolume
metadata:
  name: qbittorrent-shares
spec:
  capacity:
    storage: 1Ti
  accessModes: [ReadWriteOnce]
  csi:
    driver: example.com/csi
    volumeHandle: shares
`)
	_, rep := f.discover(t)
	var found bool
	for _, ig := range rep.Ignored {
		if ig.App == "qbittorrent" && ig.Mount == "/share" && strings.Contains(ig.Why, "not a host directory") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the claim nothing backs is not reported: %#v", rep.Ignored)
	}
}

// TestDiscoverPorts is where each app keeps the port it listened on, and what
// happens when its config does not say.
func TestDiscoverPorts(t *testing.T) {
	tests := []struct {
		name  string
		app   string
		files map[string]string
		env   map[string]string
		want  int
		ok    bool
	}{
		{name: "an Arr's config.xml", app: "sonarr", files: map[string]string{"config.xml": "<Config><Port>8989</Port></Config>"}, want: 8989, ok: true},
		{name: "an Arr's environment wins over its file", app: "sonarr", files: map[string]string{"config.xml": "<Config><Port>8989</Port></Config>"}, env: map[string]string{"SONARR__SERVER__PORT": "8990"}, want: 8990, ok: true},
		{name: "qBittorrent's WebUI port", app: "qbittorrent", files: map[string]string{"qBittorrent/qBittorrent.conf": "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n\n[WebUI]\nPort=8080\n"}, want: 8080, ok: true},
		{name: "qBittorrent's port from the environment", app: "qbittorrent", files: map[string]string{"qBittorrent/qBittorrent.conf": "[BitTorrent]\n"}, env: map[string]string{"QBT_WEBUI_PORT": "8090"}, want: 8090, ok: true},
		{name: "Tautulli's config.ini", app: "tautulli", files: map[string]string{"config.ini": "[General]\nhttp_port = 8181\n"}, want: 8181, ok: true},
		{name: "a Tautulli config in the newer spelling", app: "tautulli", files: map[string]string{"config.ini": "[General]\nHTTP_PORT = 8182\n"}, want: 8182, ok: true},
		{name: "qui's config.toml", app: "qui", files: map[string]string{"config.toml": "host = \"0.0.0.0\"\nport = 7476\n"}, want: 7476, ok: true},
		{name: "autobrr's config.toml", app: "autobrr", files: map[string]string{"config.toml": "host = \"0.0.0.0\"\nport = 7474\n"}, want: 7474, ok: true},
		{name: "PeerBanHelper's config.yml", app: "pbh", files: map[string]string{"config/config.yml": "server:\n  http: 9898\n  address: 0.0.0.0\n"}, want: 9898, ok: true},
		{name: "Seerr's PORT", app: "seerr", env: map[string]string{"PORT": "5056"}, want: 5056, ok: true},
		{name: "Seerr's default", app: "seerr", want: 5055, ok: true},
		{name: "Plex's fixed port", app: "plex", want: 32400, ok: true},
		{name: "Maintainerr's UI_PORT", app: "maintainerr", env: map[string]string{"UI_PORT": "6247"}, want: 6247, ok: true},
		{name: "an app whose config has never been written", app: "sonarr", want: 0, ok: false},
		{name: "an app with no port setting", app: "gluetun", want: 0, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := mapFS(tt.files)
			got, ok, err := listenPort(tt.app, files, ".", tt.env)
			if err != nil {
				t.Fatalf("listenPort: %v", err)
			}
			if ok != tt.ok || got != tt.want {
				t.Fatalf("listenPort = %d, %v; want %d, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestDiscoverPortFromThePod: an app whose config says nothing takes the ports
// its pod declares, and says so, because a port guessed from a pod is a weaker
// answer than one the app wrote.
func TestDiscoverPortFromThePod(t *testing.T) {
	f := sourceInstall(t)
	f.app(t, appSpec{
		name: "tautulli", image: "linuxserver/tautulli:2.14.6", port: 8181,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/tautulli"},
			{volume: "media", path: "/media", host: "/srv/library/media"},
		},
	})
	p, rep := f.discover(t)
	if !reflect.DeepEqual(p.Apps["tautulli"].Ports, []int{8181}) {
		t.Fatalf("tautulli ports: %#v", p.Apps["tautulli"].Ports)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "pod's own") {
		t.Fatalf("the fallback is not reported: %#v", rep.Notes)
	}
}

// TestDiscoverPortFromThePodAndItsService: a pod's declared port can be stale --
// a real source declared 5006 while the app served 9898, which its Service
// forwarded to -- so a Service that selects only this app is evidence of the
// port too, and the note says where each one came from.
func TestDiscoverPortFromThePodAndItsService(t *testing.T) {
	f := sourceInstall(t)
	f.app(t, appSpec{
		name: "tautulli", image: "linuxserver/tautulli:2.14.6", port: 5006,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/tautulli"},
			{volume: "media", path: "/media", host: "/srv/library/media"},
		},
	})
	f.servicePorts(t, "tautulli-web", "media", "tautulli", servicePort{port: 9898, target: "9898"})
	p, rep := f.discover(t)
	if want := []int{5006, 9898}; !reflect.DeepEqual(p.Apps["tautulli"].Ports, want) {
		t.Fatalf("tautulli ports = %#v, want %#v", p.Apps["tautulli"].Ports, want)
	}
	var note string
	for _, n := range rep.Notes {
		if strings.Contains(n, "tautulli keeps no port") {
			note = n
		}
	}
	if note == "" {
		t.Fatalf("the fallback is not reported: %#v", rep.Notes)
	}
	for _, want := range []string{"pod's own (5006)", "tautulli-web.media 9898->9898"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not say %q: %q", want, note)
		}
	}
}

// TestDiscoverSidecarContainer: the chart's pod carries the app and nothing
// else, so a second container in a migrated app's pod -- a real source ran a
// metrics exporter beside Plex -- is left behind and listed as its own outside
// entry, with the image, rather than disappearing with the pod.
func TestDiscoverSidecarContainer(t *testing.T) {
	f := newFixture(t)
	f.app(t, appSpec{
		name: "qbittorrent", image: "ghcr.io/home-operations/qbittorrent:5.3.0", port: 8080,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/qbittorrent"},
			{volume: "downloads", path: "/downloads", host: "/srv/library/Downloads"},
		},
	})
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	f.object(t, "plex", `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: plex
  namespace: media
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: plex
    spec:
      containers:
        - name: plex
          image: linuxserver/plex:1.41.5
          ports:
            - name: http
              containerPort: 32400
          volumeMounts:
            - name: config
              mountPath: /config
            - name: media
              mountPath: /media
        - name: plex-exporter
          image: docker.io/example/plex-exporter:1.2.3
          ports:
            - containerPort: 9000
      volumes:
        - name: config
          hostPath: {path: /srv/apps/plex}
        - name: media
          hostPath: {path: /srv/library/media}
`)
	p, rep := f.discover(t)
	if _, ok := p.Apps["plex"]; !ok {
		t.Fatalf("the app's own container was not migrated: %#v", keys(p.Apps))
	}
	if want := []string{"media/plex/plex-exporter"}; !reflect.DeepEqual(p.Outside, want) {
		t.Fatalf("outside = %#v, want %#v", p.Outside, want)
	}
	if len(rep.Skipped) != 1 || rep.Skipped[0].Workload != "media/plex/plex-exporter" {
		t.Fatalf("Skipped = %#v", rep.Skipped)
	}
	if !strings.Contains(rep.Skipped[0].Why, "docker.io/example/plex-exporter:1.2.3") {
		t.Fatalf("the skip does not name the image it dropped: %q", rep.Skipped[0].Why)
	}
	roundTrip(t, p)
}

// TestDiscoverNoArrNamesItsLibrary: with no Arr in the source, the library is the
// one directory beside the downloads, and that is reported as the shape it is.
func TestDiscoverNoArrNamesItsLibrary(t *testing.T) {
	f := newFixture(t)
	f.app(t, appSpec{
		name: "qbittorrent", image: "ghcr.io/home-operations/qbittorrent:5.3.0", port: 8080,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/qbittorrent"},
			{volume: "downloads", path: "/downloads", host: "/srv/library/Downloads"},
		},
	})
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	f.app(t, appSpec{
		name: "plex", image: "linuxserver/plex:1.41.5", port: 32400,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/plex"},
			{volume: "media", path: "/media", host: "/srv/library/media"},
		},
	})
	p, rep := f.discover(t)
	if !reflect.DeepEqual(p.Data, &plan.Data{Root: "/srv/library", Downloads: "Downloads", Media: "media"}) {
		t.Fatalf("data: %#v", p.Data)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "/srv/library/media") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the library taken from a mount is not reported: %#v", rep.Notes)
	}
	roundTrip(t, p)
}

// TestDiscoverRenamesTheContractDirs: a source whose directories are already
// named as the contract wants needs no rename, and its rules say so.
func TestDiscoverRenamesTheContractDirs(t *testing.T) {
	f := newFixture(t)
	f.app(t, appSpec{
		name: "qbittorrent", image: "ghcr.io/home-operations/qbittorrent:5.3.0", port: 8080,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/qbittorrent"},
			{volume: "downloads", path: "/downloads", host: "/srv/data/downloads"},
		},
	})
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	f.app(t, appSpec{
		name: "sonarr", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8989,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr"},
			{volume: "data", path: "/data", host: "/srv/data"},
		},
	})
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDB(t, "/data/media/tv"))

	p, _ := f.discover(t)
	if !reflect.DeepEqual(p.Data, &plan.Data{Root: "/srv/data", Downloads: "downloads", Media: "media"}) {
		t.Fatalf("data: %#v", p.Data)
	}
	if got := p.Data.Renames(); len(got) != 0 {
		t.Fatalf("a layout already named as the contract wants would be renamed: %#v", got)
	}
	// A mount of the root itself gives the app one rule per contract
	// directory, not one for the root.
	want := []plan.PathRule{
		{From: "/config", To: "/config"},
		{From: "/data/downloads", To: "/data/downloads"},
		{From: "/data/media", To: "/data/media"},
	}
	if !reflect.DeepEqual(p.Apps["sonarr"].Paths, want) {
		t.Fatalf("sonarr paths:\n got %#v\nwant %#v", p.Apps["sonarr"].Paths, want)
	}
	roundTrip(t, p)
}

// TestDiscoverRefusesASourceWithNoTorrentClient: without qBittorrent there is no
// downloads directory, and a plan with no data adopts nothing.
func TestDiscoverRefusesASourceWithNoTorrentClient(t *testing.T) {
	f := newFixture(t)
	f.app(t, appSpec{
		name: "sonarr", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8989,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDB(t, "/library/media/TV"))
	err := f.discoverErr(t)
	if !strings.Contains(err.Error(), "qBittorrent") {
		t.Fatalf("the refusal does not name what is missing: %v", err)
	}
}

func TestInstanceName(t *testing.T) {
	tests := []struct {
		app, workload string
		want          string
		ok            bool
	}{
		{app: "sonarr", workload: "sonarr", want: "sonarr", ok: true},
		{app: "sonarr", workload: "sonarr4k", want: "sonarr-4k", ok: true},
		{app: "sonarr", workload: "sonarr-4k", want: "sonarr-4k", ok: true},
		{app: "sonarr", workload: "Sonarr4K", want: "sonarr-4k", ok: true},
		{app: "radarr", workload: "sonarr4k", ok: false},
		{app: "plex", workload: "plex", want: "plex", ok: true},
		{app: "plex", workload: "plex2", ok: false},
		// An app under the name its upstream used to publish is the app.
		{app: "seerr", workload: "overseerr", want: "seerr", ok: true},
		{app: "pbh", workload: "peerbanhelper", want: "pbh", ok: true},
		{app: "pbh", workload: "peerbanhelper2", ok: false},
	}
	for _, tt := range tests {
		got, ok := instanceName(tt.app, catalogApps[tt.app], tt.workload)
		if ok != tt.ok || got != tt.want {
			t.Errorf("instanceName(%q, %q) = %q, %v; want %q, %v", tt.app, tt.workload, got, ok, tt.want, tt.ok)
		}
	}
}

func TestImageApp(t *testing.T) {
	tests := map[string]string{
		"lscr.io/linuxserver/sonarr:4.0.14":          "sonarr",
		"ghcr.io/seerr-team/seerr@sha256:0000":       "seerr",
		"docker.io/sctx/overseerr:1.33":              "seerr",
		"fallenbagel/jellyseerr:latest":              "seerr",
		"ghcr.io/ghostchu/peerbanhelper:v7.3.0":      "pbh",
		"ghcr.io/home-operations/qbittorrent:5.3.0":  "qbittorrent",
		"ghcr.io/qdm12/gluetun:v3.39.1":              "",
		"registry.example.com:5000/media/radarr:5.2": "radarr",
	}
	for image, want := range tests {
		got, ok := imageApp(image)
		if want == "" {
			if ok {
				t.Errorf("imageApp(%q) = %q, true; want no app", image, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("imageApp(%q) = %q, %v; want %q", image, got, ok, want)
		}
	}
}

// mapFS is a filesystem of literal files, rooted where the test says.
func mapFS(files map[string]string) fstest.MapFS {
	out := make(fstest.MapFS, len(files))
	for name, content := range files {
		out[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return out
}

// keys is a map's keys, sorted, for a failure message that reads.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestDiscoverSideServiceReachesNoApp: a Service that selects an app's pod is
// not automatically an address of that app. A metrics Service forwarding to a
// sidecar's port and one forwarding to the torrent port both select the pod and
// reach neither app, so a rule for either would rewrite an address that was
// never the app's -- a stored plex-exporter:9000 into Plex's web endpoint.
func TestDiscoverSideServiceReachesNoApp(t *testing.T) {
	f := sourceInstall(t)
	// Plex's metrics sidecar, and qBittorrent's torrent port: both Services
	// select the app's pod and forward past every port the app itself serves.
	f.servicePorts(t, "plex-exporter", "media", "plex", servicePort{port: 9000, target: "9000"})
	f.servicePorts(t, "qbittorrent-node", "media", "qbittorrent", servicePort{port: 31111, target: "31111"})
	// A target port naming a container port that does not exist reaches
	// nothing either.
	f.servicePorts(t, "plex-metrics", "media", "plex", servicePort{port: 9100, target: "metrics"})
	// A Service with no ports at all reaches nothing: an ExternalName-style
	// Service beside the apps, or one an operator added by hand.
	f.servicePorts(t, "plex-headless", "media", "plex")

	p, rep := f.discover(t)
	// The apps' own Services are untouched by the fix, and no side Service
	// earned a rule.
	want := []plan.HostRule{
		{Service: "plex.media", App: "plex", Port: 80},
		{Service: "qbittorrent.media", App: "qbittorrent", Port: 80},
		{Service: "sonarr.media", App: "sonarr", Port: 80},
	}
	if !reflect.DeepEqual(p.Hosts, want) {
		t.Fatalf("hosts:\n got %#v\nwant %#v", p.Hosts, want)
	}
	for _, name := range []string{"plex-exporter", "qbittorrent-node", "plex-metrics", "plex-headless"} {
		var noted bool
		for _, n := range rep.Notes {
			if strings.Contains(n, name+".media") {
				noted = true
			}
		}
		if !noted {
			t.Fatalf("the Service %s that reaches no app is not reported: %#v", name, rep.Notes)
		}
	}
	// The note names the ports the Service has, so the operator sees why the
	// address was left for them to classify.
	for _, want := range []string{"9000->9000", "31111->31111", "9100->metrics", "declares no ports"} {
		var noted bool
		for _, n := range rep.Notes {
			if strings.Contains(n, want) {
				noted = true
			}
		}
		if !noted {
			t.Fatalf("no note says %q: %#v", want, rep.Notes)
		}
	}
}

// TestDiscoverServiceRuleCarriesTheServicePort: a Service on 80 forwarding to
// 8080 is reached as :80, so that is the port the rule narrows to. A rule on the
// target port would not match the address any app stored.
func TestDiscoverServiceRuleCarriesTheServicePort(t *testing.T) {
	f := sourceInstall(t)
	f.servicePorts(t, "qbittorrent-web", "media", "qbittorrent", servicePort{port: 80, target: "8080"})
	p, _ := f.discover(t)
	var found bool
	for _, h := range p.Hosts {
		if h.Service != "qbittorrent-web.media" {
			continue
		}
		found = true
		if h.App != "qbittorrent" || h.Port != 80 {
			t.Fatalf("the rule does not carry the Service's port: %#v", h)
		}
	}
	if !found {
		t.Fatalf("a Service reaching the app got no rule: %#v", p.Hosts)
	}
}

// TestDiscoverServiceWithSeveralPortsReachesOne: one Service can answer on more
// than one port and forward only one of them to the app, so the rule set is per
// port, not per Service.
func TestDiscoverServiceWithSeveralPortsReachesOne(t *testing.T) {
	f := sourceInstall(t)
	f.servicePorts(t, "plex-all", "media", "plex",
		servicePort{port: 80, target: "32400"},
		servicePort{port: 9000, target: "9000"},
	)
	p, _ := f.discover(t)
	want := []plan.HostRule{{Service: "plex-all.media", App: "plex", Port: 80}}
	var got []plan.HostRule
	for _, h := range p.Hosts {
		if h.Service == "plex-all.media" {
			got = append(got, h)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules for the Service:\n got %#v\nwant %#v", got, want)
	}
}

// TestDiscoverNamespaces: a source install can run beside things the operator
// is not migrating, and a namespaced read is what a user's kubeconfig is
// allowed.
func TestDiscoverNamespaces(t *testing.T) {
	f := sourceInstall(t)
	f.app(t, appSpec{
		name: "radarr", namespace: "films", image: "lscr.io/linuxserver/radarr:5.20.1", port: 7878,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/radarr"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.file(t, "/srv/apps/radarr/config.xml", "<Config><Port>7878</Port></Config>\n")
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDB(t, "/library/media/Movies"))
	f.service(t, "radarr", "films", "radarr")

	p, rep, err := Discover(context.Background(), NewDirectory(f.manifests), Options{
		Namespaces: []string{"media"},
		ConfigFS:   f.files,
		TempDir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, ok := p.Apps["radarr"]; ok {
		t.Fatalf("an app outside the chosen namespaces was migrated: %#v", keys(p.Apps))
	}
	if len(p.Outside) != 1 || p.Outside[0] != "media/gluetun" {
		t.Fatalf("outside: %#v", p.Outside)
	}
	for _, h := range p.Hosts {
		if strings.HasPrefix(h.Service, "radarr.") {
			t.Fatalf("a Service outside the chosen namespaces became a rule: %#v", h)
		}
	}
	if len(rep.Skipped) != 1 {
		t.Fatalf("a workload outside the chosen namespaces was reported: %#v", rep.Skipped)
	}
}

// TestDiscoverServiceThatSelectsTwoApps: a rule has to name one app, so an
// ambiguous selector gets none and the operator hears about it. Apply then lists
// every stored address that meant it, which is the honest consequence.
func TestDiscoverServiceThatSelectsTwoApps(t *testing.T) {
	f := newFixture(t)
	shared := map[string]string{"app": "media"}
	f.app(t, appSpec{
		name: "qbittorrent", image: "ghcr.io/home-operations/qbittorrent:5.3.0", port: 8080,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/qbittorrent"},
			{volume: "downloads", path: "/downloads", host: "/srv/library/Downloads"},
		},
	})
	f.file(t, "/srv/apps/qbittorrent/qBittorrent/qBittorrent.conf", "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	f.app(t, appSpec{
		name: "sonarr", image: "lscr.io/linuxserver/sonarr:4.0.14", port: 8989, podLabels: shared,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/sonarr"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.bytes(t, "/srv/apps/sonarr/sonarr.db", arrDB(t, "/library/media/TV"))
	f.app(t, appSpec{
		name: "radarr", image: "lscr.io/linuxserver/radarr:5.20.1", port: 7878, podLabels: shared,
		mounts: []mount{
			{volume: "config", path: "/config", host: "/srv/apps/radarr"},
			{volume: "library", path: "/library", host: "/srv/library"},
		},
	})
	f.bytes(t, "/srv/apps/radarr/radarr.db", arrDB(t, "/library/media/Movies"))
	f.object(t, "media", `apiVersion: v1
kind: Service
metadata:
  name: media
  namespace: media
spec:
  selector:
    app: media
  ports:
    - port: 80
`)
	p, rep := f.discover(t)
	if len(p.Hosts) != 0 {
		t.Fatalf("an ambiguous Service became a rule: %#v", p.Hosts)
	}
	var noted bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "selects both radarr and sonarr") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the ambiguity is not reported: %#v", rep.Notes)
	}
}

// TestContractPath: a path below the data root maps to what apply's renames
// leave behind, so an app that stored the source's directory name reads the
// contract's.
func TestContractPath(t *testing.T) {
	data := &plan.Data{Root: "/srv/library", Downloads: "Downloads", Media: "media"}
	tests := map[string]string{
		"Downloads":          "/data/downloads",
		"Downloads/tv":       "/data/downloads/tv",
		"Downloads/tv/a.mkv": "/data/downloads/tv/a.mkv",
		"media":              "/data/media",
		"media/4K/Movies":    "/data/media/4K/Movies",
		"backups":            "/data/backups",
	}
	for rel, want := range tests {
		if got := contractPath(rel, data); got != want {
			t.Errorf("contractPath(%q) = %q; want %q", rel, got, want)
		}
	}
	// A mount of the root itself is the whole data directory under another
	// name.
	want := []plan.PathRule{
		{From: "/library/Downloads", To: "/data/downloads"},
		{From: "/library/media", To: "/data/media"},
	}
	if got := mountRules("/library", "", data); !reflect.DeepEqual(got, want) {
		t.Fatalf("mountRules of the root:\n got %#v\nwant %#v", got, want)
	}
	if got := mountRules("/downloads", "Downloads", data); !reflect.DeepEqual(got, []plan.PathRule{{From: "/downloads", To: "/data/downloads"}}) {
		t.Fatalf("mountRules of the downloads: %#v", got)
	}
}

// TestDiscoverRefusesOneWorkloadTwoApps: one pod cannot be two of box's apps, so
// the source has to be split before discovery can name it.
func TestDiscoverRefusesOneWorkloadTwoApps(t *testing.T) {
	f := newFixture(t)
	f.object(t, "arrs", `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: arrs
  namespace: media
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: arrs
    spec:
      containers:
        - name: sonarr
          image: lscr.io/linuxserver/sonarr:4.0.14
          volumeMounts:
            - name: sonarr
              mountPath: /sonarr
        - name: radarr
          image: lscr.io/linuxserver/radarr:5.20.1
          volumeMounts:
            - name: radarr
              mountPath: /radarr
      volumes:
        - name: sonarr
          hostPath: {path: /srv/apps/sonarr}
        - name: radarr
          hostPath: {path: /srv/apps/radarr}
`)
	err := f.discoverErr(t)
	for _, want := range []string{"media/arrs", "sonarr", "radarr", "two catalog apps"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %s: %v", want, err)
		}
	}
}

// TestDiscoverNeedsTheHost: the mounts name host directories, and a caller that
// brings no way to read them is refused before the source is read.
func TestDiscoverNeedsTheHost(t *testing.T) {
	f := sourceInstall(t)
	_, _, err := Discover(context.Background(), NewDirectory(f.manifests), Options{TempDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "config filesystem") {
		t.Fatalf("Discover with no config filesystem: %v", err)
	}
	if _, _, err := Discover(context.Background(), nil, Options{ConfigFS: f.files}); err == nil {
		t.Fatal("Discover with no source was accepted")
	}
}
