// Package plan is the file `box migrate` writes: what each app was called, where
// its config came from, and how the paths it stored translate. The table is per
// app because each app saw the same files under its own names.
package plan

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Version is the only plan version this box reads: a plan is a promise about
// what apply will do, so an unknown shape is refused, not half-applied.
const Version = 2

// Plan is the whole file.
type Plan struct {
	Version int `yaml:"version"`
	// Source is the install the plan was read from; absent for a hand-written
	// plan.
	Source *Source `yaml:"source,omitempty"`
	// Data is where the source kept downloads and media, when box adopts them
	// in place. Absent, apply touches no data.
	Data *Data `yaml:"data,omitempty"`
	// Domain is the DNS name discovery read off the source, the run's default
	// answer; empty when the source could not say and the run asks.
	Domain string `yaml:"domain,omitempty"`
	// Apps is keyed by box's instance name.
	Apps map[string]App `yaml:"apps"`
	// Hosts classifies every non-localhost address one app stored for another;
	// localhost addresses are resolved through Apps' ports.
	Hosts []HostRule `yaml:"hosts,omitempty"`
	// Outside are the source's workloads that are not catalog apps and the
	// containers of a migrated app's pod the chart's pod does not carry, as
	// <namespace>/<name>[/<container>]. migrate never touches them; they are
	// listed so the operator knows what has to come back by hand.
	Outside []string `yaml:"outside,omitempty"`
}

// App is one app as the source named it and as box will name it. The key it is
// filed under in Plan.Apps is box's instance name, which is what the chart
// calls the pod and what every caller passes to Rewrite.
type App struct {
	// Kind is the platform the app ran on; one plan can hold apps from
	// several.
	Kind string `yaml:"kind"`
	// Source is the name the app had in the install being migrated.
	Source string `yaml:"source"`
	// Image is informational: the chart's variant is what runs, since admission
	// is the guard on what a pod may run.
	Image string `yaml:"image,omitempty"`
	// Variant is the blessed variant that image already is, empty when it is
	// none and the chart's default runs.
	Variant string `yaml:"variant,omitempty"`
	// Config is the directory the app's config is copied from; migrate never
	// writes it.
	Config string `yaml:"config"`
	// Namespace is the source namespace, which a Service's short name in this
	// app's config resolves against.
	Namespace string `yaml:"namespace,omitempty"`
	// Ports are the ports this app listened on in the source; a localhost
	// address another app stored resolves to this app by its port.
	Ports []int `yaml:"ports,omitempty"`
	// Category is the /data/media directory this instance's library lives in,
	// as the chart's values name it; empty for an app with no library.
	Category string `yaml:"category,omitempty"`
	// DownloadCategory is the qBittorrent category the source's client named
	// (`4K/TV`), kept because the torrents are already there and box never
	// moves them. Empty means box's own category.
	DownloadCategory string `yaml:"downloadCategory,omitempty"`
	// MediaServer is the one server a Seerr was set up against in the source,
	// plex or jellyfin, read from its settings: the config it carries signs in
	// to that server, so the instance is that server's. Empty for every other
	// app, and for a Seerr nobody set up.
	MediaServer string `yaml:"mediaServer,omitempty"`
	// Carry is how much of the config directory the snapshot took: CarryFull,
	// or CarryMinimal for the state the app cannot rebuild (Plex only).
	Carry string `yaml:"carry,omitempty"`
	// Paths are this app's path rules.
	Paths []PathRule `yaml:"paths"`
}

// The two ways a snapshot can carry an app whose config is more than its state,
// for App.Carry.
const (
	// CarryFull is all of it, less what the app rebuilds.
	CarryFull = "full"
	// CarryMinimal is the state the app cannot rebuild: settings and databases,
	// not the library it built from the media.
	CarryMinimal = "minimal"
)

// PathRule maps one directory the app saw to the directory it will see.
type PathRule struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	// Why is one line saying why discovery settled this mapping, required when
	// the rule is a default.
	Why string `yaml:"why,omitempty"`
	// Default says discovery chose this rule and the operator has not confirmed
	// it.
	Default bool `yaml:"default,omitempty"`
}

// Load reads and checks a plan file.
func Load(name string) (*Plan, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	p, err := Parse(data)
	if err != nil {
		var perr *Error
		if errors.As(err, &perr) {
			perr.Path = name
		}
		return nil, err
	}
	return p, nil
}

// Parse reads and checks a plan. Unknown keys are refused: a misspelled key
// would otherwise be a rule that silently does nothing.
func Parse(data []byte) (*Plan, error) {
	var p Plan
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &Error{Problems: []string{"is empty; a plan names the apps to migrate"}}
		}
		return nil, &Error{Problems: []string{err.Error()}}
	}
	// A second document would be silently ignored, so refuse it.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &Error{Problems: []string{"has more than one YAML document"}}
	}
	if problems := p.check(); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return &p, nil
}

// check enforces what a rule has to be for Rewrite to be total, normalising
// every path it accepts. Problems come back together: an operator fixes the file
// once.
func (p *Plan) check() []string {
	var problems []string
	if p.Version != Version {
		problem := fmt.Sprintf("version %d: this box reads version %d", p.Version, Version)
		if p.Version < Version {
			problem += "; an older version is from an older box, so re-run `box migrate discover`"
		}
		problems = append(problems, problem)
	}
	problems = append(problems, p.checkSource()...)
	for _, name := range p.appNames() {
		app := p.Apps[name]
		problems = append(problems, checkApp(name, app)...)
		seen := make(map[string]bool, len(app.Paths))
		for i := range app.Paths {
			r := &app.Paths[i]
			at := fmt.Sprintf("apps.%s paths[%d]", name, i)
			// Both sides are absolute: a relative one could mean either side of
			// the mount.
			if !path.IsAbs(r.From) {
				problems = append(problems, fmt.Sprintf("apps.%s: from %q is not an absolute path", name, r.From))
			}
			if !path.IsAbs(r.To) {
				problems = append(problems, fmt.Sprintf("apps.%s: to %q is not an absolute path", name, r.To))
			}
			// Clean first: two rules for /downloads/ would leave the winner to
			// the order of the list.
			r.From, r.To = path.Clean(r.From), path.Clean(r.To)
			if path.IsAbs(r.From) {
				if seen[r.From] {
					problems = append(problems, fmt.Sprintf("apps.%s: from %q appears twice", name, r.From))
				}
				seen[r.From] = true
			}
			problems = append(problems, whyProblems(at, r.Why, r.Default)...)
		}
		for _, port := range app.Ports {
			if port < 1 || port > 65535 {
				problems = append(problems, fmt.Sprintf("apps.%s: port %d is not a port", name, port))
			}
		}
		p.Apps[name] = app
	}
	for _, o := range p.Outside {
		parts := strings.Split(o, "/")
		// A workload, or one container of one: a sidecar the chart's pod does
		// not carry.
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
			problems = append(problems, fmt.Sprintf("outside: %q is not <namespace>/<name> or <namespace>/<name>/<container>", o))
		}
	}
	problems = append(problems, p.checkData()...)
	problems = append(problems, p.checkHosts()...)
	problems = append(problems, p.checkDomain()...)
	return problems
}

// appNames is every app in the plan, sorted, so a refusal reads the same way
// twice.
func (p *Plan) appNames() []string {
	names := make([]string, 0, len(p.Apps))
	for name := range p.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Rewrite maps one stored path through the named app's table. The longest From
// wins and only whole segments match (/downloads2 is not under /downloads). A
// relative path comes back unchanged and true; an absolute one no rule covers,
// unchanged and false -- a gap the caller collects.
func (p *Plan) Rewrite(appName, stored string) (string, bool) {
	if !path.IsAbs(stored) {
		return stored, true
	}
	best := -1
	var to, rest string
	for _, r := range p.Apps[appName].Paths {
		if tail, ok := trimDir(stored, r.From); ok && len(r.From) > best {
			best, to, rest = len(r.From), r.To, tail
		}
	}
	if best < 0 {
		return stored, false
	}
	return join(to, rest), true
}

// trimDir returns what stored has after dir, when dir covers it whole: either
// the same path, or a prefix that ends at a segment boundary.
func trimDir(stored, dir string) (string, bool) {
	if dir == "/" {
		if stored == "/" {
			return "", true
		}
		return stored, true
	}
	if stored == dir {
		return "", true
	}
	if strings.HasPrefix(stored, dir+"/") {
		return stored[len(dir):], true
	}
	return "", false
}

// join appends what is left of a path to the directory it now sits under,
// without doubling the separator when that directory is the filesystem root.
func join(to, rest string) string {
	if to == "/" {
		return "/" + strings.TrimPrefix(rest, "/")
	}
	return to + rest
}
