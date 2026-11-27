package seedbox

import "sort"

// Instance is one workload the chart serves on this host: an app, or one of an app's
// instances, under the name it answers to (angles's own `s5n.instances`). It is one
// statement for the login's prefixes, the hosts an app is served at and the rules
// access_control talks about, so no step can disagree about a name.
type Instance struct {
	// Name is the workload name -- an app's, or the app's with its instance suffix --
	// and therefore the URL prefix, or the host's first label.
	Name string
	// App is the values key it came from.
	App string
	// Arr is whether it is one of the Arrs (or Prowlarr): their /api and /ping carry
	// their own key, their UI does not.
	Arr bool
}

// Instances is the chart's `s5n.instances` list, rebuilt from the values document box
// already has: an enabled instance of an Arr with suffix "4k" is `sonarr-4k`, which is
// also its URL prefix.
//
// It reads the merged values (defaults, the user's file, the grant), the same document
// Helm renders, so a second instance that exists has its own rules and host and one that
// does not is never mentioned.
func Instances(values map[string]any) []Instance {
	apps, _ := values["apps"].(map[string]any)
	out := make([]Instance, 0, len(apps))
	names := make([]string, 0, len(apps))
	for name := range apps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, app := range names {
		cfg, ok := apps[app].(map[string]any)
		if !ok {
			continue
		}
		if on, _ := cfg["enabled"].(bool); !on {
			continue
		}
		arr := hasEnvPrefix(cfg)
		instances, _ := cfg["instances"].([]any)
		if len(instances) == 0 {
			out = append(out, Instance{Name: app, App: app, Arr: arr})
			continue
		}
		for _, raw := range instances {
			inst, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if on, _ := inst["enabled"].(bool); !on {
				continue
			}
			suffix, _ := inst["suffix"].(string)
			name := app
			if suffix != "" {
				name = app + "-" + suffix
			}
			out = append(out, Instance{Name: name, App: app, Arr: arr})
		}
	}
	return out
}

// hasEnvPrefix is the chart's own test for "this app is an Arr": every Arr and Prowlarr
// declares the environment prefix its settings are read from.
func hasEnvPrefix(cfg map[string]any) bool {
	_, ok := cfg["envPrefix"]
	return ok
}
