package seedbox

import (
	"fmt"
	"sort"
	"strings"
)

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
	// Host is the instance's `host` override for an app served at a name of its own: a
	// label, or a name under the box's domain. Empty means <Name>.<domain>.
	Host string
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
			out = append(out, Instance{Name: name, App: app, Arr: arr, Host: suffixHost(inst)})
		}
	}
	return out
}

// suffixHost is one instance entry's `host` override, "" when it has none.
func suffixHost(inst map[string]any) string {
	host, _ := inst["host"].(string)
	return strings.TrimSpace(host)
}

// HostName is the name one instance is served at when its app has a host of its own: its
// `host` override (a label, or a name under the box's domain), else <name>.<domain>.
//
// A name outside the domain is refused because the login's session cookie is the domain's,
// and the box's own name is refused because its certificate already answers there.
func (i Instance) HostName(domain string) (string, error) {
	override := strings.TrimSpace(i.Host)
	if override == "" {
		return i.Name + "." + domain, nil
	}
	if !strings.Contains(override, ".") {
		return override + "." + domain, nil
	}
	if override == domain {
		return "", fmt.Errorf("apps.%s.host: %q is the box's own name, which its own certificate already answers; give %s a name of its own", i.App, override, i.Name)
	}
	suffix := "." + domain
	if !strings.HasSuffix(override, suffix) {
		return "", fmt.Errorf("apps.%s.host: %q is not under %s; a hosted instance is reached at a name under the box's domain, because the login's session cookie is the domain's", i.App, override, domain)
	}
	if strings.Contains(strings.TrimSuffix(override, suffix), ".") {
		return "", fmt.Errorf("apps.%s.host: %q has more than one label before %s; a hosted instance is one name under the box's domain", i.App, override, domain)
	}
	return override, nil
}

// CheckHosts refuses two hosted instances on one name. It reads no catalog: the caller
// passes the instances its app serves at a name of their own.
func CheckHosts(instances []Instance, domain string) error {
	seen := map[string]string{}
	for _, inst := range instances {
		name, err := inst.HostName(domain)
		if err != nil {
			return err
		}
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("apps.%s.host: %s names %q, which %s already serves; two workloads on one name is one certificate and one login rule for both", inst.App, inst.Name, name, prev)
		}
		seen[name] = inst.Name
	}
	return nil
}

// hasEnvPrefix is the chart's own test for "this app is an Arr": every Arr and Prowlarr
// declares the environment prefix its settings are read from.
func hasEnvPrefix(cfg map[string]any) bool {
	_, ok := cfg["envPrefix"]
	return ok
}
