package apply

// chart_test.go pins what this package claims to know about the chart against
// the chart's own files.
//
// Targets is the one function every rewritten address goes through, so an
// endpoint that names the wrong Service, the wrong port or a URL base the chart
// does not give is a connection the migrated app cannot make. The chart is the
// authority, so the test reads values.yaml, catalog.yaml and workloads.yaml and
// holds the table to them -- a chart that adds an app, renames a Service or
// moves an app's base path fails here, in the commit that does it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// chartDir is the chart this package adopts into.
func chartDir() string { return filepath.Join("..", "..", "..", "charts", "s5n") }

func readChart(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir(), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// catalogApps reads the chart's catalog manifest: every app box installs, and
// whether it takes more than one instance.
func catalogApps(t *testing.T) map[string]bool {
	t.Helper()
	var cat struct {
		Apps map[string]struct {
			Instances bool   `yaml:"instances"`
			Exposure  string `yaml:"exposure"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal([]byte(readChart(t, "catalog.yaml")), &cat); err != nil {
		t.Fatalf("parse catalog.yaml: %v", err)
	}
	if len(cat.Apps) == 0 {
		t.Fatal("the catalog names no apps")
	}
	out := make(map[string]bool, len(cat.Apps))
	for name, entry := range cat.Apps {
		out[name] = entry.Instances
	}
	return out
}

// chartValues reads the chart's values.yaml as the document a chart release
// hands apply.
func chartValues(t *testing.T) map[string]any {
	t.Helper()
	var values map[string]any
	if err := yaml.Unmarshal([]byte(readChart(t, "values.yaml")), &values); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	return values
}

// TestTargetsCoverEveryCatalogApp: every app the chart installs has an endpoint,
// named after the instance the chart names its Service after, on the port the
// chart's values give it.
func TestTargetsCoverEveryCatalogApp(t *testing.T) {
	apps := catalogApps(t)
	values := chartValues(t)
	portOf := map[string]int{}
	for name := range apps {
		entry, _ := values["apps"].(map[string]any)[name].(map[string]any)
		port, ok := intValue(entry["port"])
		if !ok {
			t.Fatalf("%s: values.yaml names no port", name)
		}
		portOf[name] = port
	}
	catalog := make([]string, 0, len(apps))
	for name := range apps {
		catalog = append(catalog, name)
	}
	for name, instances := range apps {
		// A plain app is its own instance; an app that takes instances is named
		// after its suffix, so use one to prove the Service follows the
		// instance and not the app.
		instance := name
		if instances {
			instance = name + "-4k"
		}
		targets, err := Targets(values, catalog, []string{instance})
		if err != nil {
			t.Errorf("%s: %v", instance, err)
			continue
		}
		ep := targets[instance]
		// charts/s5n/templates/workloads.yaml names each Service after its
		// instance and gives it app's port; the workload list comes from
		// _helpers.tpl's s5n.instances.
		if ep.Host != instance {
			t.Errorf("%s: endpoint host %q, want the instance's Service name", instance, ep.Host)
		}
		if ep.Port != portOf[name] {
			t.Errorf("%s: endpoint port %d, values.yaml says %d", instance, ep.Port, portOf[name])
		}
	}
}

// TestURLBasesMatchTheTemplates: the URL base is the one thing about an endpoint
// the values cannot say, because the chart computes it from the instance name.
// Each app's answer is checked against the template that gives it, and an app
// with no answer of its own is refused by Targets rather than guessed at.
func TestURLBasesMatchTheTemplates(t *testing.T) {
	apps := catalogApps(t)
	workloads := readChart(t, filepath.Join("templates", "workloads.yaml"))
	values := readChart(t, "values.yaml")
	catalogRaw := readChart(t, "catalog.yaml")

	// marker is where the chart gives the app its base path: the environment
	// variable's name in workloads.yaml, or the chart file that says the app
	// has none.
	for app := range apps {
		base, ok := urlBase(app, "srv")
		if !ok {
			t.Errorf("%s: this box has no URL base for it, so migrate would refuse a plan that names it", app)
			continue
		}
		switch base {
		case "/srv":
			if !strings.Contains(workloads, "__SERVER__URLBASE") && app != "qui" && app != "maintainerr" && app != "tautulli" {
				t.Errorf("%s: the chart gives it /<instance>, and the template does not set a URL base", app)
			}
			if app == "qui" && !strings.Contains(workloads, "QUI__BASE_URL") {
				t.Error("qui: the chart gives it /<instance> and the template sets no QUI__BASE_URL")
			}
			if app == "maintainerr" && !strings.Contains(workloads, "BASE_PATH") {
				t.Error("maintainerr: the chart gives it /<instance> and the template sets no BASE_PATH")
			}
			// Tautulli's base is its own http_root, which the init container
			// seals on every start: it is a file setting, not an environment
			// variable, and the route keeps the prefix instead of stripping it.
			if app == "tautulli" && !strings.Contains(workloads, "--http-root={{ $urlBase }}") {
				t.Error("tautulli: the chart gives it /<instance>, and the init container does not seal http_root to it")
			}
		case "/srv/":
			// autobrr mounts its router at the base URL's string plus the
			// rest of the path, so the template writes it with the slash.
			if !strings.Contains(workloads, `printf "%s/" $urlBase`) {
				t.Error("autobrr: the chart's base URL carries a trailing slash and the template does not write one")
			}
		case "":
			// No base path: the route strips the prefix (rewritePath), or the
			// app is served at a host of its own and has no prefix at all.
			block := topLevelBlock(values, app)
			if !strings.Contains(block, "rewritePath: true") && !strings.Contains(topLevelBlock(catalogRaw, app), "exposure: host") {
				t.Errorf("%s: no URL base is set, and neither values.yaml's rewritePath nor catalog.yaml's exposure says why", app)
			}
		}
	}
}

// topLevelBlock is one `  <name>:` block of a two-space-indented YAML file: the
// app's own entry, without the ones around it.
func topLevelBlock(doc, name string) string {
	lines := strings.Split(doc, "\n")
	start := -1
	header := regexp.MustCompile(`^  [a-z0-9]+:$`)
	for i, line := range lines {
		if line == "  "+name+":" {
			start = i
			continue
		}
		if start >= 0 && header.MatchString(line) {
			return strings.Join(lines[start+1:i], "\n")
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start+1:], "\n")
}

// TestCatalogAppNamesInstances: the instance-to-app mapping is discovery's rule
// read backwards, so the two cannot disagree about which app a plan entry is.
func TestCatalogAppNamesInstances(t *testing.T) {
	catalog := []string{"sonarr", "radarr", "qbittorrent", "prowlarr"}
	cases := []struct{ instance, app, suffix string }{
		{"sonarr", "sonarr", ""},
		{"sonarr-4k", "sonarr", "4k"},
		{"radarr-1080", "radarr", "1080"},
		{"qbittorrent", "qbittorrent", ""},
		{"prowlarr", "prowlarr", ""},
		{"sonarr4k", "", ""},
		{"jellyfin", "", ""},
	}
	for _, tc := range cases {
		if got := CatalogApp(tc.instance, catalog); got != tc.app {
			t.Errorf("CatalogApp(%q) = %q, want %q", tc.instance, got, tc.app)
		}
		if tc.app == "" {
			continue
		}
		if got := Suffix(tc.instance, tc.app); got != tc.suffix {
			t.Errorf("Suffix(%q) = %q, want %q", tc.instance, got, tc.suffix)
		}
	}
}

// TestInstancesMatchesTheChart: the instance list is s5n.instances, read for
// the endpoints a rewriter may meet. An app switched off is not an endpoint; an
// instance entry that is off is not one either.
func TestInstancesMatchesTheChart(t *testing.T) {
	values := map[string]any{"apps": map[string]any{
		"sonarr": map[string]any{"enabled": true, "port": 8989, "instances": []any{
			map[string]any{"suffix": "", "category": "tv", "enabled": true},
			map[string]any{"suffix": "4k", "category": "tv-4k", "enabled": false},
		}},
		"radarr":      map[string]any{"enabled": false, "port": 7878},
		"qbittorrent": map[string]any{"enabled": true, "port": 8080},
	}}
	got := strings.Join(Instances(values), ",")
	if got != "qbittorrent,sonarr" {
		t.Errorf("Instances = %q, want qbittorrent,sonarr", got)
	}
	if EnabledIn(values, "sonarr-4k") {
		t.Error("EnabledIn says sonarr-4k is on, and its entry is off")
	}
	if EnabledIn(values, "radarr") {
		t.Error("EnabledIn says radarr is on, and the app is off")
	}
	if !EnabledIn(values, "sonarr") {
		t.Error("EnabledIn says sonarr is off, and it is on")
	}
}

// TestImageResolvesLikeTheTemplate: the image a Job runs is the one the chart
// renders for the app, tag and digest together, with a values pin winning the
// way _helpers.tpl lets it.
func TestImageResolvesLikeTheTemplate(t *testing.T) {
	variants := []byte(readChart(t, "variants.yaml"))
	values := chartValues(t)
	img, err := Image(variants, values, "plex")
	if err != nil {
		t.Fatalf("resolve plex: %v", err)
	}
	if !strings.HasPrefix(img, "ghcr.io/home-operations/plex:") || !strings.Contains(img, "@sha256:") {
		t.Errorf("plex image is %q, want the variant's repository, tag and digest", img)
	}

	pinned := map[string]any{"apps": map[string]any{"plex": map[string]any{
		"image": map[string]any{"variant": "default", "digest": "sha256:" + strings.Repeat("a", 64)},
	}}}
	img, err = Image(variants, pinned, "plex")
	if err != nil {
		t.Fatalf("resolve a pinned plex: %v", err)
	}
	if !strings.HasSuffix(img, "@sha256:"+strings.Repeat("a", 64)) {
		t.Errorf("a pinned digest does not win: %q", img)
	}
	// A pinned tag drops the variant's digest, which belongs to its own tag.
	tagged := map[string]any{"apps": map[string]any{"plex": map[string]any{
		"image": map[string]any{"variant": "default", "tag": "1.2.3"},
	}}}
	img, err = Image(variants, tagged, "plex")
	if err != nil {
		t.Fatalf("resolve a tagged plex: %v", err)
	}
	if img != "ghcr.io/home-operations/plex:1.2.3" {
		t.Errorf("a pinned tag renders %q, want the tag without the variant's digest", img)
	}
	if _, err := Image(variants, values, "nonexistent"); err == nil {
		t.Error("an app the chart has no variants for resolved")
	}
}

// TestIntValue: the values documents box holds carry Go ints, and one that came
// through JSON carries a float; both are the port a chart value names.
func TestIntValue(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
		ok   bool
	}{
		{8989, 8989, true},
		{int64(8989), 8989, true},
		{float64(8989), 8989, true},
		{"8989", 8989, true},
		{0, 0, false},
		{nil, 0, false},
		{1.5, 0, false},
	} {
		got, ok := intValue(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("intValue(%v) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestWiringImageIsTheOneTheReleaseRuns: the rehearsal's probe pods run the s5n
// image the release's own Job runs, resolved the way the chart's template does.
// The chart's s5n variant carries no tag of its own; the release's appVersion
// is what fills it, and a pinned tag or digest narrows it inside the repository.
func TestWiringImageIsTheOneTheReleaseRuns(t *testing.T) {
	variants := []byte(readChart(t, "variants.yaml"))
	img, err := WiringImage(variants, "0.5.0", chartValues(t))
	if err != nil {
		t.Fatalf("WiringImage: %v", err)
	}
	if img != "ghcr.io/swizzin/s5n:0.5.0" {
		t.Errorf("wiring image = %q, want the blessed repository tagged with the release", img)
	}
	pinned := map[string]any{"wiring": map[string]any{"image": map[string]any{
		"variant": "default", "digest": "sha256:" + strings.Repeat("b", 64),
	}}}
	img, err = WiringImage(variants, "0.5.0", pinned)
	if err != nil {
		t.Fatalf("WiringImage with a pinned digest: %v", err)
	}
	if !strings.HasSuffix(img, "@sha256:"+strings.Repeat("b", 64)) {
		t.Errorf("a pinned digest does not win: %q", img)
	}
	if _, err := WiringImage(variants, "", map[string]any{}); err == nil {
		t.Error("a release with no appVersion and no tag resolved to an unpullable image")
	}
}
