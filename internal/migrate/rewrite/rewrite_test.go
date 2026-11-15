package rewrite

import (
	"context"
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
	if !(Report{Uncovered: []plan.Uncovered{{Path: "/x"}}}).Blocked() {
		t.Error("an uncovered path did not block apply")
	}
	if !(Report{Unclassified: []Address{{Host: "nas"}}}).Blocked() {
		t.Error("an unclassified address did not block apply")
	}
}

// TestResolveFallsBackToTheBoxEndpoints: an address no source rule settles but
// that already names one of the box's own endpoints is that app, which is what
// lets a second Check read a copy the first pass rewrote, and lets Write follow
// the Check that made the rewrite.
func TestResolveFallsBackToTheBoxEndpoints(t *testing.T) {
	p, err := plan.Parse([]byte(`version: 1
apps:
  sonarr:
    source: sonarr
    config: /srv/apps/sonarr
    ports: [8989]
    paths: []
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths: []
  plex:
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
