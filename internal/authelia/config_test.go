package authelia

import (
	"reflect"
	"strings"
	"testing"
)

// fixtureValues is a seedbox's merged values with both flavours of instance:
// the Arrs' HD and 4K entries, Prowlarr (an Arr with no instances), the three
// apps that authenticate their own clients, and qBittorrent, which does not.
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

func TestInstancesFromValues(t *testing.T) {
	got := InstancesFromValues(fixtureValues())
	want := []Instance{
		{Name: "plex", App: "plex"},
		{Name: "prowlarr", App: "prowlarr", Arr: true},
		{Name: "qbittorrent", App: "qbittorrent"},
		{Name: "radarr", App: "radarr", Arr: true},
		{Name: "seerr", App: "seerr"},
		{Name: "sonarr", App: "sonarr", Arr: true},
		{Name: "sonarr-4k", App: "sonarr", Arr: true},
		{Name: "tautulli", App: "tautulli"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("instances = %#v\nwant %#v", got, want)
	}
}

// A disabled instance is not served, so a rule for it would be a rule about a
// route that does not exist. The 4K Radarr in this fixture is off.
func TestInstancesFromValuesSkipsDisabled(t *testing.T) {
	for _, inst := range InstancesFromValues(fixtureValues()) {
		if inst.Name == "radarr-4k" {
			t.Fatal("a disabled instance is in the list")
		}
	}
}

// TestRulesExact is the access_control contract, in order: the portal, each
// app's own credentials, then the login. The order is what Authelia evaluates,
// so a reordering here is a login that answers its own redirect, or an API
// client that is asked for a form.
func TestRulesExact(t *testing.T) {
	got := Rules("box.example.com", "alice", InstancesFromValues(fixtureValues()), nil)
	host := "box.example.com"
	want := []Rule{
		{Domain: host, Policy: "bypass", Resources: []string{`^/auth([/?].*)?$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/plex([/?].*)?$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/prowlarr/api([/?].*)?$`, `^/prowlarr/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/radarr/api([/?].*)?$`, `^/radarr/ping$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/seerr([/?].*)?$`}},
		{Domain: host, Policy: "bypass", Resources: []string{`^/sonarr/api([/?].*)?$`, `^/sonarr/ping$`}},
		// The 4K instance is its own prefix on the same host, so it needs its
		// own rules: a rule written for /sonarr does not cover /sonarr-4k.
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

// qBittorrent's UI and API are behind the login: it is the one app that
// authenticates browsers and nothing else, and the Arrs reach it over the
// cluster network, not through the Gateway.
func TestRulesLeaveQBittorrentBehindTheLogin(t *testing.T) {
	for _, r := range Rules("box.example.com", "alice", InstancesFromValues(fixtureValues()), nil) {
		for _, resource := range r.Resources {
			if strings.HasPrefix(resource, `^/qbittorrent`) {
				t.Fatalf("qBittorrent is exempted from the login by %q", resource)
			}
		}
	}
}

// A host with no apps still has a login: the portal is always there, and the
// last rule is the one that matters.
func TestRulesWithNoApps(t *testing.T) {
	got := Rules("box.example.com", "alice", nil, nil)
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

// An app served at a name of its own leaves its path behind: its rule is the
// whole <app>.<domain>, and no rule mentions /<app> any more. Authelia checks
// the request's host, so without the domain rule the name falls into
// default_policy deny, and without removing the path rule a request under the
// old path would still be served although the chart no longer routes it.
func TestRulesFollowAnAppToItsOwnHost(t *testing.T) {
	got := Rules("box.example.com", "alice", InstancesFromValues(fixtureValues()), []string{"plex", "seerr"})
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

// No rule mentions the ACME challenge: the challenge is answered on the host's
// own port-80 listener, which has no login, so a bypass here would be dead
// config pretending the challenge goes through Authelia.
func TestRulesMentionNoACMEChallenge(t *testing.T) {
	for _, r := range Rules("box.example.com", "alice", InstancesFromValues(fixtureValues()), nil) {
		for _, resource := range r.Resources {
			if strings.Contains(resource, "acme-challenge") || strings.Contains(resource, ".well-known") {
				t.Fatalf("rule %q still exempts the ACME challenge from the login", resource)
			}
		}
	}
}
