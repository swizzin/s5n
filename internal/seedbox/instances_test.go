package seedbox

import (
	"reflect"
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
