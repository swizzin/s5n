package rewrite

import (
	"context"
	"net/url"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

type nop struct{}

func (nop) Check(context.Context, Input) (Report, error) { return Report{}, nil }
func (nop) Write(context.Context, Input) (Report, error) { return Report{}, nil }

func TestRegistry(t *testing.T) {
	t.Cleanup(func() { delete(registry, "testapp") })
	Register("testapp", nop{})
	if _, ok := For("testapp"); !ok {
		t.Fatal("For(testapp) after Register: not found")
	}
	if _, ok := For("sonarr-4k"); ok {
		t.Error("For finds an instance name; rewriters are filed by catalog app")
	}
	defer func() {
		if recover() == nil {
			t.Error("a second Register for one app did not panic")
		}
	}()
	Register("testapp", nop{})
}

func TestBlocked(t *testing.T) {
	if (Report{Notes: []string{"x"}, Changes: []Change{{Count: 1}}}).Blocked() {
		t.Error("notes and changes blocked apply")
	}
	// A value left on purpose or an address proposed for are decisions, not
	// gaps.
	if (Report{Left: []Left{{What: "192.0.2.10:9091"}}, Defaults: []Default{{Host: "192.0.2.10", Port: 9091}}}).Blocked() {
		t.Error("a value left on purpose, or a proposed default, blocked apply")
	}
	if !(Report{Uncovered: []plan.Uncovered{{Path: "/x"}}}).Blocked() {
		t.Error("an uncovered path did not block apply")
	}
	if !(Report{Unclassified: []Address{{Host: "nas"}}}).Blocked() {
		t.Error("an unclassified address did not block apply")
	}
}

// TestAddressText: the port appears only when the app stored one.
func TestAddressText(t *testing.T) {
	if got := (Address{Host: "192.0.2.10", Port: 32400}).Text(); got != "192.0.2.10:32400" {
		t.Errorf("Text() = %q", got)
	}
	if got := (Address{Host: "qbt.example.org"}).Text(); got != "qbt.example.org" {
		t.Errorf("Text() with no port = %q", got)
	}
}

// TestURLPort: a URL addresses the port it names, else its scheme's.
func TestURLPort(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{"https://qbt.example.org/qbittorrent", 443},
		{"https://qbt.example.org:/x", 443},
		{"http://qbt.example.org/x", 80},
		{"https://qbt.example.org:8443/x", 8443},
		{"http://192.0.2.10:8080", 8080},
		{"ftp://qbt.example.org/x", 0},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := URLPort(u); got != tt.want {
				t.Errorf("URLPort(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
	// A URL built without a port reads as the scheme's; a malformed one is
	// refused by url.Parse.
	if got := URLPort(&url.URL{Scheme: "https", Host: "qbt.example.org"}); got != 443 {
		t.Errorf("URLPort of a hand-built URL = %d, want 443", got)
	}
}

// TestResolveFallsBackToTheBoxEndpoints: an address naming one of the box's own
// endpoints is that app, which lets Write follow the Check that rewrote it.
func TestResolveFallsBackToTheBoxEndpoints(t *testing.T) {
	p, err := plan.Parse([]byte(`version: 2
apps:
  sonarr:
    kind: kubernetes
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths: []
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths: []
  plex:
    kind: kubernetes
    source: plex
    config: /srv/apps/plex
    ports: [32400]
    paths: []
hosts:
  - service: plex.media
    app: plex
`))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	in := Input{
		App:     "sonarr",
		Plan:    p,
		Targets: map[string]Endpoint{"qbittorrent": {Host: "qbittorrent", Port: 8080}},
	}
	tests := []struct {
		name string
		host string
		port int
		want string // the app it rewrites to, "" for unclassified
	}{
		{"a localhost port the plan resolves", "127.0.0.1", 8080, "qbittorrent"},
		{"a Service rule the plan holds", "plex.media.svc", 32400, "plex"},
		{"the box's own host and port", "qbittorrent", 8080, "qbittorrent"},
		{"the same, in another case and with a trailing dot", "QBittorrent.", 8080, "qbittorrent"},
		{"the box's host on another port", "qbittorrent", 9999, ""},
		{"the box's host with no port", "qbittorrent", 0, ""},
		{"a LAN address", "192.0.2.10", 8080, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := in.Resolve(tt.host, tt.port)
			if tt.want == "" {
				if res.Verdict != plan.Unclassified {
					t.Fatalf("Resolve(%q, %d) = %+v, want unclassified", tt.host, tt.port, res)
				}
				return
			}
			if res.Verdict != plan.Rewrite || res.App != tt.want {
				t.Fatalf("Resolve(%q, %d) = %+v, want a rewrite to %s", tt.host, tt.port, res, tt.want)
			}
		})
	}
}
