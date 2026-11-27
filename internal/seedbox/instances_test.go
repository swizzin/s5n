package seedbox

import (
	"reflect"
	"strings"
	"testing"
)

// fixtureValues is one box's values: a hosted pair, an app on a path, an Arr with a 4K
// instance, and one instance switched off.
func fixtureValues() map[string]any {
	app := func(extra map[string]any) map[string]any {
		out := map[string]any{"enabled": true}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return map[string]any{
		"apps": map[string]any{
			"plex":        app(nil),
			"prowlarr":    app(map[string]any{"envPrefix": "PROWLARR"}),
			"qbittorrent": app(nil),
			"radarr":      app(map[string]any{"envPrefix": "RADARR"}),
			"seerr":       app(nil),
			"sonarr": app(map[string]any{
				"envPrefix": "SONARR",
				"instances": []any{
					map[string]any{"suffix": "", "enabled": true},
					map[string]any{"suffix": "4k", "enabled": true},
				},
			}),
			"radarr-4k-off": app(nil),
			"tautulli":      app(nil),
		},
	}
}

// The chart's list, rebuilt: an app with no instances is one workload named after it, and
// an app with them is one per enabled suffix.
func TestInstances(t *testing.T) {
	values := fixtureValues()
	apps := values["apps"].(map[string]any)
	delete(apps, "radarr-4k-off")
	apps["radarr"] = map[string]any{
		"enabled":   true,
		"envPrefix": "RADARR",
		"instances": []any{
			map[string]any{"suffix": "", "enabled": true},
			map[string]any{"suffix": "4k", "enabled": false},
		},
	}
	got := Instances(values)
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

// A disabled instance is not served, so a rule for it would be a rule about a route that
// does not exist: the 4K Radarr here is off.
func TestInstancesSkipsDisabled(t *testing.T) {
	values := fixtureValues()
	apps := values["apps"].(map[string]any)
	apps["radarr"] = map[string]any{
		"enabled":   true,
		"envPrefix": "RADARR",
		"instances": []any{
			map[string]any{"suffix": "", "enabled": true},
			map[string]any{"suffix": "4k", "enabled": false},
		},
	}
	for _, inst := range Instances(values) {
		if inst.Name == "radarr-4k" {
			t.Fatal("a disabled instance is in the list")
		}
	}
	// A switched-off app is no workload at all.
	apps["seerr"] = map[string]any{"enabled": false}
	for _, inst := range Instances(values) {
		if inst.Name == "seerr" {
			t.Fatal("a disabled app is in the list")
		}
	}
}

// An instance's name is its `host` override when it has one: a label gives
// <label>.<domain>, and a name under the domain is taken as it stands. Everything a box
// serves -- the DNS check, listener, Certificate, login rule and the address it prints --
// reads this one answer.
func TestHostNameResolvesTheOverride(t *testing.T) {
	for _, tc := range []struct{ label, host, want string }{
		{"the instance's name", "", "seerr.box.example.com"},
		{"a label", "requests", "requests.box.example.com"},
		{"a name under the domain", "requests.box.example.com", "requests.box.example.com"},
		{"a name with a dash", "media-requests", "media-requests.box.example.com"},
	} {
		inst := Instance{Name: "seerr", App: "seerr", Host: tc.host}
		got, err := inst.HostName("box.example.com")
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if got != tc.want {
			t.Fatalf("%s: name = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// A name the login cannot cover is refused before a certificate is written for it: the
// session cookie is the domain's, so a hosted name is one label under it.
func TestHostNameRefusesANameTheLoginCannotCover(t *testing.T) {
	for _, tc := range []struct{ label, host, want string }{
		{"another domain", "requests.example.com", "is not under box.example.com"},
		{"the box's own name", "box.example.com", "is the box's own name"},
		{"too many labels", "a.requests.box.example.com", "more than one label"},
	} {
		inst := Instance{Name: "seerr", App: "seerr", Host: tc.host}
		_, err := inst.HostName("box.example.com")
		if err == nil {
			t.Fatalf("%s: %q was accepted", tc.label, tc.host)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to say %q", tc.label, err, tc.want)
		}
	}
}

// Two workloads on one name is one certificate and one login rule for both, so the second
// one is refused by name.
func TestCheckHostsRefusesOneNameTwice(t *testing.T) {
	instances := []Instance{
		{Name: "seerr", App: "seerr", Host: "requests"},
		{Name: "seerr-jellyfin", App: "seerr", Host: "requests"},
	}
	err := CheckHosts(instances, "box.example.com")
	if err == nil {
		t.Fatal("two instances on one name were accepted")
	}
	if !strings.Contains(err.Error(), `"requests.box.example.com"`) || !strings.Contains(err.Error(), "seerr") {
		t.Fatalf("error = %v, want it to name the name and the instance that has it", err)
	}
	// Distinct names, overrides and defaults alike, are fine.
	ok := []Instance{{Name: "seerr", App: "seerr"}, {Name: "seerr-jellyfin", App: "seerr", Host: "requests"}}
	if err := CheckHosts(ok, "box.example.com"); err != nil {
		t.Fatalf("two different names were refused: %v", err)
	}
}
