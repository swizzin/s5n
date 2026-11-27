package authelia

import (
	"reflect"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

// fixtureValues is a seedbox's merged values with both flavours of instance.
func fixtureValues() map[string]any {
	app := func(extra map[string]any) map[string]any {
		out := map[string]any{"enabled": true, "port": 1}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	arr := func(hd, fourKay bool) map[string]any {
		return map[string]any{
			"enabled":   true,
			"envPrefix": "SONARR__",
			"instances": []any{
				map[string]any{"suffix": "", "enabled": hd, "category": "tv"},
				map[string]any{"suffix": "4k", "enabled": fourKay, "category": "4K/TV"},
			},
		}
	}
	return map[string]any{
		"apps": map[string]any{
			"sonarr":      arr(true, true),
			"radarr":      arr(true, false),
			"prowlarr":    app(map[string]any{"envPrefix": "PROWLARR__"}),
			"plex":        app(nil),
			"seerr":       app(nil),
			"tautulli":    app(nil),
			"qbittorrent": app(nil),
		},
	}
}

// selfLogin is the catalog's answer for this fixture: the apps that sign their users in
// themselves.
var selfLogin = []string{"plex", "seerr", "tautulli"}

func TestRulesExact(t *testing.T) {
	got := Rules("box.example.com", "alice", seedbox.Instances(fixtureValues()), nil, selfLogin)
	host := "box.example.com"
	want := []Rule{
		{Domain: host, Policy: "bypass", Resources: []string{`^/auth([/?].*)?$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/plex([/?].*)?$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/prowlarr/api([/?].*)?$`, `^/prowlarr/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/radarr/api([/?].*)?$`, `^/radarr/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/seerr([/?].*)?$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/sonarr/api([/?].*)?$`, `^/sonarr/ping$`}},
		// The 4K instance is its own prefix, so it needs its own rules.
		{Domain: host, Policy: "bypass", Resources: []string{`^/sonarr-4k/api([/?].*)?$`, `^/sonarr-4k/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/tautulli([/?].*)?$`}},
		{Domain: host, Policy: "one_factor", Subject: "alice"},
	}
	if !reflect.DeepEqual(got, want) {
		var b strings.Builder
		for _, r := range got {
			b.WriteString("\n    " + r.Policy + " " + strings.Join(r.Resources, " ") + " " + r.Subject)
		}
		t.Fatalf("rules =%s\nwant %#v", b.String(), want)
	}
}

// qBittorrent's UI and API are behind the login: the Arrs reach it over the
// cluster network, not through the Gateway.
func TestRulesLeaveQBittorrentBehindTheLogin(t *testing.T) {
	for _, r := range Rules("box.example.com", "alice", seedbox.Instances(fixtureValues()), nil, selfLogin) {
		for _, resource := range r.Resources {
			if strings.HasPrefix(resource, `^/qbittorrent`) {
				t.Fatalf("qBittorrent is exempted from the login by %q", resource)
			}
		}
	}
}

// A host with no apps still has a login: the portal is always there.
func TestRulesWithNoApps(t *testing.T) {
	got := Rules("box.example.com", "alice", nil, nil, nil)
	if len(got) != 2 {
		t.Fatalf("rules = %#v, want the portal and the login", got)
	}
	if got[1].Policy != "one_factor" || got[1].Subject != "alice" {
		t.Fatalf("last rule = %#v", got[1])
	}
	if got[1].Domain != "box.example.com" {
		t.Fatalf("login rule domain = %q, want the box's host", got[1].Domain)
	}
}

// An app served at a name of its own leaves its path behind: its rule is the whole
// <app>.<domain>, and a leftover path rule would serve a route the chart no longer
// creates.
func TestRulesFollowAnAppToItsOwnHost(t *testing.T) {
	got := Rules("box.example.com", "alice", seedbox.Instances(fixtureValues()), []string{"plex.box.example.com", "seerr.box.example.com"}, selfLogin)
	host := "box.example.com"
	want := []Rule{
		{Domain: host, Policy: "bypass", Resources: []string{`^/auth([/?].*)?$`}},
		// Sorted instance order: plex, prowlarr, radarr, seerr, sonarr, ...
		{Domain: "plex." + host, Policy: "bypass"},
		{Domain: host, Policy: "bypass", Resources: []string{`^/prowlarr/api([/?].*)?$`, `^/prowlarr/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/radarr/api([/?].*)?$`, `^/radarr/ping$`}},
		{Domain: "seerr." + host, Policy: "bypass"},
		{Domain: host, Policy: "bypass", Resources: []string{`^/sonarr/api([/?].*)?$`, `^/sonarr/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/sonarr-4k/api([/?].*)?$`, `^/sonarr-4k/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/tautulli([/?].*)?$`}},
		{Domain: host, Policy: "one_factor", Subject: "alice"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules = %#v\nwant %#v", got, want)
	}
	for _, r := range got {
		for _, resource := range r.Resources {
			if strings.HasPrefix(resource, `^/plex`) || strings.HasPrefix(resource, `^/seerr`) {
				t.Fatalf("a hosted app still has a path rule: %#v", r)
			}
		}
	}
}

// No rule mentions the ACME challenge: it is answered on the host's port-80
// listener, which has no login, so a bypass would be dead config.
func TestRulesMentionNoACMEChallenge(t *testing.T) {
	for _, r := range Rules("box.example.com", "alice", seedbox.Instances(fixtureValues()), nil, selfLogin) {
		for _, resource := range r.Resources {
			if strings.Contains(resource, "acme-challenge") || strings.Contains(resource, ".well-known") {
				t.Fatalf("rule %q still exempts the ACME challenge from the login", resource)
			}
		}
	}
}

// The bypass comes from the list, not from a name in this package: qBittorrent is
// neither own sign-in app, so its bypass can only be what this test passed.
func TestRulesBypassAnAppThatSignsItsUsersIn(t *testing.T) {
	host := "box.example.com"
	pathRule := Rule{Domain: host, Policy: PolicyBypass, Resources: []string{`^/qbittorrent([/?].*)?$`}}
	if got := Rules(host, "alice", seedbox.Instances(fixtureValues()), nil, []string{"qbittorrent"}); !hasRule(got, pathRule) {
		t.Fatalf("rules = %#v, want a bypass for the app under its path: %#v", got, pathRule)
	}

	hostRule := Rule{Domain: "qbittorrent." + host, Policy: PolicyBypass}
	got := Rules(host, "alice", seedbox.Instances(fixtureValues()), []string{"qbittorrent.box.example.com"}, []string{"qbittorrent"})
	if !hasRule(got, hostRule) {
		t.Fatalf("rules = %#v, want the whole app host to bypass: %#v", got, hostRule)
	}
}

// A name the list leaves out is behind the login: the list is the whole answer.
func TestRulesLeaveAnAppOutOfTheBypass(t *testing.T) {
	host := "box.example.com"
	got := Rules(host, "alice", seedbox.Instances(fixtureValues()), nil, []string{"plex", "seerr"})
	for _, r := range got {
		for _, resource := range r.Resources {
			if strings.HasPrefix(resource, `^/tautulli`) {
				t.Fatalf("tautulli is exempted from the login by %q although the list leaves it out", resource)
			}
		}
	}
	if !hasRule(got, Rule{Domain: host, Policy: PolicyOneFactor, Subject: "alice"}) {
		t.Fatalf("rules = %#v, want the host's one-factor rule to stand in front of tautulli", got)
	}
}

// hasRule reports whether one rendered rule is in the list, field for field.
func hasRule(rules []Rule, want Rule) bool {
	for _, r := range rules {
		if r.Domain != want.Domain || r.Policy != want.Policy || r.Subject != want.Subject {
			continue
		}
		if len(r.Resources) != len(want.Resources) {
			continue
		}
		ok := true
		for i := range r.Resources {
			if r.Resources[i] != want.Resources[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// A second instance of an app that signs its users in has a name of its own, and its
// rule is that name: the catalog decides the posture per app, so both instances of
// Seerr bypass the login and neither is left on a path rule.
func TestRulesFollowASecondInstanceToItsOwnName(t *testing.T) {
	values := map[string]any{"apps": map[string]any{
		"seerr": map[string]any{"enabled": true, "instances": []any{
			map[string]any{"suffix": "", "enabled": true},
			map[string]any{"suffix": "jellyfin", "enabled": true},
		}},
	}}
	host := "box.example.com"
	hosted := []string{"seerr." + host, "seerr-jellyfin." + host}
	got := Rules(host, "alice", seedbox.Instances(values), hosted, []string{"seerr"})
	for _, name := range hosted {
		if !hasRule(got, Rule{Domain: name, Policy: PolicyBypass}) {
			t.Fatalf("%s has no bypass rule:\n%+v", name, got)
		}
	}
	// One login rule per name that keeps the portal, not one per app.
	if !hasRule(got, Rule{Domain: host, Policy: PolicyOneFactor, Subject: "alice"}) {
		t.Fatalf("the box's own name lost its login rule:\n%+v", got)
	}
}
