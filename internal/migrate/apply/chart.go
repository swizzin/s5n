package apply

// chart.go is what migrate knows of the chart it adopts into: instance names, each
// endpoint inside the release, and the app's image. None comes from the plan, which
// describes the install left behind; a rewriter hands rewritten addresses to these
// endpoints. The templates are the authority (workloads.yaml names each Service
// after its instance), read statically and pinned by a test.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// The catalog app names, constants because urlBase and apiKeys key on them; a typo
// would silently stop a rewrite, so TestTargetsCoverEveryCatalogApp checks them.
const (
	appSonarr      = "sonarr"
	appRadarr      = "radarr"
	appLidarr      = "lidarr"
	appBazarr      = "bazarr"
	appProwlarr    = "prowlarr"
	appQBittorrent = "qbittorrent"
	appPlex        = "plex"
	appTautulli    = "tautulli"
	appSeerr       = "seerr"
	appQui         = "qui"
	appAutobrr     = "autobrr"
	appPBH         = "pbh"
	appMaintainerr = "maintainerr"
	appSabnzbd     = "sabnzbd"
	appFilebrowser = "filebrowser"
	appJellyfin    = "jellyfin"
)

// CatalogApp is an instance's app ("sonarr-4k" is sonarr), discovery's naming read
func CatalogApp(instance string, catalog []string) string {
	best := ""
	for _, app := range catalog {
		if instance == app {
			return app
		}
		if !strings.HasPrefix(instance, app+"-") {
			continue
		}
		// The longest name wins: "sonarr" and "sonarr-4k" both prefix
		// "sonarr-4k-hd".
		if len(app) > len(best) {
			best = app
		}
	}
	return best
}

// Suffix is the instance's own part ("4k" for sonarr-4k), the chart's list key
func Suffix(instance, app string) string {
	if app == "" || instance == app {
		return ""
	}
	return strings.TrimPrefix(instance, app+"-")
}

// CatalogRuns is the plan's instances by catalog app, what a rewriter answers Sole
func CatalogRuns(p *plan.Plan, catalog []string) map[string][]string {
	out := make(map[string][]string, len(p.Apps))
	for _, name := range sortedPlanApps(p) {
		app := CatalogApp(name, catalog)
		if app == "" {
			continue
		}
		out[app] = append(out[app], name)
	}
	return out
}

// sortedPlanApps is a plan's instance names, sorted, so CatalogRuns reads the
func sortedPlanApps(p *plan.Plan) []string {
	out := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Instances is every instance the values switch on, that is the chart's Service
// list; the plan alone would miss what an earlier run switched on.
func Instances(values map[string]any) []string {
	apps, _ := values["apps"].(map[string]any)
	var out []string
	for app, raw := range apps {
		entry, _ := raw.(map[string]any)
		if on, _ := entry["enabled"].(bool); !on {
			continue
		}
		list, takesInstances := entry["instances"].([]any)
		if !takesInstances {
			out = append(out, app)
			continue
		}
		for _, raw := range list {
			inst, _ := raw.(map[string]any)
			if on, _ := inst["enabled"].(bool); !on {
				continue
			}
			suffix, _ := inst["suffix"].(string)
			out = append(out, joinInstance(app, suffix))
		}
	}
	sortStrings(out)
	return out
}

// joinInstance is the chart's name for one instance (s5n.instances: ternary
func joinInstance(app, suffix string) string {
	if suffix == "" {
		return app
	}
	return app + "-" + suffix
}

// Categories is each instance's library directory from the values, on or off: a
func Categories(values map[string]any) map[string]string {
	apps, _ := values["apps"].(map[string]any)
	out := map[string]string{}
	for app, raw := range apps {
		entry, _ := raw.(map[string]any)
		list, takesInstances := entry["instances"].([]any)
		if !takesInstances {
			continue
		}
		for _, raw := range list {
			inst, _ := raw.(map[string]any)
			suffix, _ := inst["suffix"].(string)
			category, _ := inst["category"].(string)
			if category == "" {
				continue
			}
			out[joinInstance(app, suffix)] = category
		}
	}
	return out
}

// EnabledIn reports whether an instance is both present and on; an app on with its
func EnabledIn(values map[string]any, instance string) bool {
	for _, name := range Instances(values) {
		if name == instance {
			return true
		}
	}
	return false
}

// urlBase is the chart's base per app: /<instance> for most, /<instance>/ for
// would answer at the wrong path.
func urlBase(app, instance string) (string, bool) {
	switch app {
	case appSonarr, appRadarr, appLidarr, appBazarr, appProwlarr, appQui, appMaintainerr, appTautulli, appSabnzbd, appFilebrowser:
		return "/" + instance, true
	case appAutobrr:
		return "/" + instance + "/", true
	// The apps the chart serves at a host of their own: their route matches the
	// site root, so an address that points at one has no path to it.
	case appQBittorrent, appPlex, appSeerr, appPBH, appJellyfin:
		return "", true
	}
	return "", false
}

// Targets is where every instance answers: the chart's Service, its port and base.
func Targets(values map[string]any, catalog, names []string) (map[string]rewrite.Endpoint, error) {
	apps, _ := values["apps"].(map[string]any)
	out := make(map[string]rewrite.Endpoint, len(names))
	for _, name := range names {
		app := CatalogApp(name, catalog)
		if app == "" {
			return nil, fmt.Errorf("%s: this chart's catalog names no app for it", name)
		}
		entry, _ := apps[app].(map[string]any)
		port, ok := intValue(entry["port"])
		if !ok {
			return nil, fmt.Errorf("%s: the chart's values name no port for %s", name, app)
		}
		base, ok := urlBase(app, name)
		if !ok {
			return nil, fmt.Errorf("%s: this box does not know the URL base the chart gives %s, so it cannot rewrite an address that points at it", name, app)
		}
		out[name] = rewrite.Endpoint{Host: name, Port: port, URLBase: base}
	}
	return out, nil
}

// Image is the app's rendered image, resolved as _helpers.tpl does: variant, then a
func Image(variants []byte, values map[string]any, app string) (string, error) {
	images, err := admission.Images(variants)
	if err != nil {
		return "", err
	}
	apps, _ := values["apps"].(map[string]any)
	entry, _ := apps[app].(map[string]any)
	image, _ := entry["image"].(map[string]any)
	variant, _ := image["variant"].(string)
	if variant == "" {
		variant = "default"
	}
	v, ok := images[app][variant]
	if !ok {
		return "", fmt.Errorf("%s: the chart has no %q variant for it", app, variant)
	}
	tag, digest := v.Tag, v.Digest
	if pinned, _ := image["digest"].(string); pinned != "" {
		digest = pinned
	} else if pinned, _ := image["tag"].(string); pinned != "" {
		tag, digest = pinned, ""
	}
	if v.Repository == "" || tag == "" {
		return "", fmt.Errorf("%s: variant %q names no image this box can run", app, variant)
	}
	if digest != "" {
		return v.Repository + ":" + tag + "@" + digest, nil
	}
	return v.Repository + ":" + tag, nil
}

// intValue reads an integer, which the YAML library may give as int or float.
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n > 0
	case int64:
		return int(n), n > 0
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), n > 0
	case string:
		p, err := strconv.Atoi(strings.TrimSpace(n))
		return p, err == nil && p > 0
	}
	return 0, false
}

// sortStrings sorts in place, avoiding an import beside the tables.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// WiringImage resolves the s5n image the release's Job and probe pods run, as the
// template does; a probe is a pod in a seedbox namespace, so it must be an image
func WiringImage(variants []byte, appVersion string, values map[string]any) (string, error) {
	images, err := admission.Images(variants)
	if err != nil {
		return "", err
	}
	wiring, _ := values["wiring"].(map[string]any)
	pinned, _ := wiring["image"].(map[string]any)
	variant, _ := pinned["variant"].(string)
	if variant == "" {
		variant = "default"
	}
	v, ok := images["s5n"][variant]
	if !ok {
		return "", fmt.Errorf("s5n: the chart has no %q variant for it", variant)
	}
	tag, digest := v.Tag, v.Digest
	if tag == "" {
		tag = appVersion
	}
	if digestPin, _ := pinned["digest"].(string); digestPin != "" {
		digest = digestPin
	} else if tagPin, _ := pinned["tag"].(string); tagPin != "" {
		tag, digest = tagPin, ""
	}
	if v.Repository == "" || tag == "" {
		return "", fmt.Errorf("s5n: variant %q names no image this box can run", variant)
	}
	if digest != "" {
		return v.Repository + ":" + tag + "@" + digest, nil
	}
	return v.Repository + ":" + tag, nil
}
