package apply

// chart.go is what migrate knows about the chart it adopts into: which instance
// names the values switch on, the endpoint each one answers on inside the
// release, and the image an app runs.
//
// None of it comes from the plan. The plan describes the install being left
// behind; the release is described by the chart release this box is about to
// apply, and a rewriter hands every rewritten address to an endpoint from here
// (internal/migrate/rewrite, Endpoint). The templates are the one authority:
// charts/s5n/templates/workloads.yaml names each Service after its instance and
// gives it the app's port, so both are read statically here and pinned by a
// test that loads the chart's own files.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// The catalog app names this package names at all. They are constants because
// two of the three tables here key on them (urlBase and apiKeys), and a typo in
// one of those would silently stop a rewrite or a kept key rather than fail:
// the strings come from the chart's catalog and are checked against it by
// TestTargetsCoverEveryCatalogApp.
const (
	appSonarr      = "sonarr"
	appRadarr      = "radarr"
	appProwlarr    = "prowlarr"
	appQBittorrent = "qbittorrent"
	appPlex        = "plex"
	appTautulli    = "tautulli"
	appSeerr       = "seerr"
	appQui         = "qui"
	appAutobrr     = "autobrr"
	appPBH         = "pbh"
	appMaintainerr = "maintainerr"
)

// CatalogApp is the catalog app an instance name belongs to: "sonarr-4k" is
// sonarr, "prowlarr" is prowlarr.
//
// The same rule discovery names instances by (internal/migrate/discover,
// instanceName), read backwards: an app that takes instances is named
// `<app>` or `<app>-<suffix>`, and no other app's name can start the same way
// because a suffix is lower-case letters and digits. The catalog is the chart's
// list, so a chart that adds an app is a box that can migrate it without a
// table here.
func CatalogApp(instance string, catalog []string) string {
	best := ""
	for _, app := range catalog {
		if instance == app {
			return app
		}
		if !strings.HasPrefix(instance, app+"-") {
			continue
		}
		// The longest app name wins: an app called "sonarr" and one called
		// "sonarr-4k" would both prefix an instance called "sonarr-4k-hd".
		if len(app) > len(best) {
			best = app
		}
	}
	return best
}

// Suffix is the instance's own part of its name: "4k" for sonarr-4k, empty for
// the app's first instance. It is what the chart's instances list keys an entry
// by (charts/s5n/templates/_helpers.tpl, s5n.instances).
func Suffix(instance, app string) string {
	if app == "" || instance == app {
		return ""
	}
	return strings.TrimPrefix(instance, app+"-")
}

// Instances is every instance the values switch on, sorted, named the way the
// chart names it: the app itself, or `<app>-<suffix>` for an entry under an
// app that takes instances.
//
// It is s5n.instances read a second time, and deliberately so: migrate hands a
// rewriter the endpoint of every running app, which is exactly the Service list
// the chart renders, and deriving it from anything else (the plan alone, or the
// values' app names) would miss the instance a run before this one switched on.
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
// `empty $i.suffix`).
func joinInstance(app, suffix string) string {
	if suffix == "" {
		return app
	}
	return app + "-" + suffix
}

// EnabledIn reports whether the values switch one instance on. Every instance
// of an app has to exist as an entry *and* be enabled: an app that is on with
// its 4K entry off runs one Sonarr, not two.
func EnabledIn(values map[string]any, instance string) bool {
	for _, name := range Instances(values) {
		if name == instance {
			return true
		}
	}
	return false
}

// urlBase is the URL base the chart gives one app's pod, per instance:
//
//	/<instance>   the Arrs (workloads.yaml:248, <PREFIX>__SERVER__URLBASE), qui
//	              (:287, QUI__BASE_URL) and Maintainerr (:391, BASE_PATH)
//	/<instance>/  autobrr (:330), whose router mounts at base URL *plus* the
//	              rest of the path, so the trailing slash is what puts its API
//	              at /autobrr/api
//	none          qBittorrent, Plex, Tautulli and Seerr, whose route strips the
//	              prefix instead (values.yaml, rewritePath), and PeerBanHelper,
//	              which is served at a host of its own with no base path
//
// ok is false for an app this box knows nothing about: guessing "" would hand a
// rewriter an address that answers at the wrong path, so Targets refuses
// instead.
func urlBase(app, instance string) (string, bool) {
	switch app {
	case appSonarr, appRadarr, appProwlarr, appQui, appMaintainerr:
		return "/" + instance, true
	case appAutobrr:
		return "/" + instance + "/", true
	case appQBittorrent, appPlex, appTautulli, appSeerr, appPBH:
		return "", true
	}
	return "", false
}

// Targets is where every instance answers inside the release, by instance name:
// the Service the chart renders for it, the app's port, and its URL base.
//
// names is every instance a rewriter may meet an address for: the plan's own,
// plus every one the values already switch on. The port comes from the values
// rather than a constant here, because it is a chart value an operator may have
// changed.
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

// Image is the image a release renders for one app, resolved the way
// charts/s5n/templates/_helpers.tpl resolves it: the variant the values select,
// a pinned digest replacing the variant's, a pinned tag replacing its tag and
// dropping its digest. It is the reference a Job has to run, so it is the same
// string the pod gets, tag and digest together.
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

// intValue reads an integer out of a values document. The documents box holds
// carry Go ints (both the chart's values.yaml and a seedbox's files are parsed
// by the YAML library), and a float is what a chart value that came through
// JSON looks like, so both are read.
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

// sortStrings sorts in place. It exists so this file needs no import for one
// call in Instances and stays readable beside the tables above.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
