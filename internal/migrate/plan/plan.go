// Package plan is the file `box migrate` writes while it reads a source
// install: what each app was called there, where its config came from, and how
// the paths it stored translate into the ones box's release gives it.
//
// The table is per app because every app saw its own paths. A source install
// commonly mounts the downloads into the torrent client and the library into
// the Arrs under different names, and one app's table applied to another app's
// files would move a path that app never had. So a stored path is always
// rewritten through the table of the app that stored it, which is why Rewrite
// takes the app's name.
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

// Version is the only plan version this box reads. A plan is a promise about
// what apply will do, so a box that meets a shape it does not know refuses the
// file rather than applying the half of it that it happens to parse.
//
// 2 adds the source block, each app's kind, image and variant, and the why and
// default a rule carries when discovery wrote it as a default. Version 1 plans
// existed only in test runs: apply refuses one and names the way forward.
const Version = 2

// Plan is the whole file.
type Plan struct {
	Version int `yaml:"version"`
	// Source is the install the plan was read from: the platform(s) that
	// contributed an app, when it was read, and who owned what was read.
	// Absent for a plan written by hand with no source behind it.
	Source *Source `yaml:"source,omitempty"`
	// Data is where the source kept its downloads and media, when box adopts
	// them in place. Absent, apply touches no data.
	Data *Data `yaml:"data,omitempty"`
	// Domain is the DNS name box will serve this seedbox under, when discovery
	// could read it from the source: the one name the source's Ingress hosts
	// all are, or the domain they share. It is the plan's default for the
	// run's question, never a question discovery asks, and empty when the
	// source could not say -- which is when the run asks for it.
	Domain string `yaml:"domain,omitempty"`
	// Apps is keyed by box's instance name.
	Apps map[string]App `yaml:"apps"`
	// Hosts classifies every address one source app stored for another that
	// is not a localhost address (those are resolved through Apps' ports).
	Hosts []HostRule `yaml:"hosts,omitempty"`
	// Outside are the source's workloads that are not catalog apps, as
	// <namespace>/<name>, and the containers of a migrated app's pod that the
	// chart's pod does not carry, as <namespace>/<name>/<container>. migrate
	// never touches them; they are listed so the operator sees what has to come
	// back by hand.
	Outside []string `yaml:"outside,omitempty"`
}

// App is one app as the source named it and as box will name it. The key it is
// filed under in Plan.Apps is box's instance name, which is what the chart
// calls the pod and what every caller passes to Rewrite.
type App struct {
	// Kind is the platform the app ran on. One plan can hold apps from
	// several sources, and each app says which one it came from.
	Kind string `yaml:"kind"`
	// Source is the name the app had in the install being migrated: how
	// discovery found it, and what the operator recognises in the source's own
	// interface.
	Source string `yaml:"source"`
	// Image is the image reference the source ran. It is informational: the
	// chart's variant is what runs after the migration, and the source's own
	// image is never carried, because the admission policy is the guard on
	// what a pod may run.
	Image string `yaml:"image,omitempty"`
	// Variant is the blessed variant that image already is, empty when it is
	// none of them and the chart's default is what runs. A variant is only
	// ever one of the chart's own (charts/s5n/variants.yaml).
	Variant string `yaml:"variant,omitempty"`
	// Config is the directory the app's config is copied from. migrate reads
	// it and never writes it, so the running source stays the rollback until
	// the operator tears it down.
	Config string `yaml:"config"`
	// Namespace is the source namespace the app ran in, which is what a
	// Service's short name stored in this app's config resolves against.
	// Empty for a source that is not a cluster.
	Namespace string `yaml:"namespace,omitempty"`
	// Ports are the ports this app listened on in the source, as its own
	// config set them. A localhost address another app stored is resolved to
	// this app by its port.
	Ports []int `yaml:"ports,omitempty"`
	// Category is the directory under /data/media box lays this instance's
	// library out in, as the chart's values name it: `movies` for radarr,
	// `movies-4k` for radarr-4k. Empty for an app that keeps no library, and
	// for a read that had no chart to ask.
	Category string `yaml:"category,omitempty"`
	// DownloadCategory is the qBittorrent category this instance's downloads
	// are in, as the source's own download client named it: `Movies`,
	// `4K/TV`. A migrated box keeps it, because the torrents are already in
	// that directory and box never moves them; empty means the source said
	// nothing (or said nothing useful), and box's own category is the answer.
	DownloadCategory string `yaml:"downloadCategory,omitempty"`
	// Paths are this app's path rules.
	Paths []PathRule `yaml:"paths"`
}

// PathRule maps one directory the app saw to the directory it will see.
type PathRule struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	// Why is one line saying why discovery settled this mapping for the
	// operator, written when the rule is a default rather than the operator's
	// own: the directory it could not place, and what it took instead.
	Why string `yaml:"why,omitempty"`
	// Default says discovery chose this rule, and the operator has not
	// confirmed it. A default without a Why is a decision with no reason.
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

// Parse reads and checks a plan. Unknown keys are refused rather than ignored:
// a plan is written by hand or by a discovery run, and a misspelled key
// ("path:" for "paths:") would otherwise be a rule that silently does nothing.
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
	// A second document would be ignored by the decoder above, so refuse it
	// instead of reading only the first.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &Error{Problems: []string{"has more than one YAML document"}}
	}
	if problems := p.check(); len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return &p, nil
}

// check enforces what a rule has to be for Rewrite to be total, and normalises
// every path it accepts. Problems are returned together: an operator fixes the
// file once, not once per line.
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
			// The rule is about how a path the app itself stored reads now, so
			// both sides name directories the app saw or will see, and both are
			// absolute. A relative one could mean either side of the mount.
			if !path.IsAbs(r.From) {
				problems = append(problems, fmt.Sprintf("apps.%s: from %q is not an absolute path", name, r.From))
			}
			if !path.IsAbs(r.To) {
				problems = append(problems, fmt.Sprintf("apps.%s: to %q is not an absolute path", name, r.To))
			}
			// Clean first, then compare: /downloads and /downloads/ are one
			// directory, and two rules for it would leave the winner to the
			// order of the list.
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
		// An outside entry is a workload, or one container of a workload: a
		// pod a catalog app ran beside a sidecar the chart's pod does not
		// carry. Nothing else is a thing an operator can bring back by hand.
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

// Rewrite maps one stored path through the named app's table.
//
// A path that is not absolute comes back unchanged and true: an empty value, or
// a name relative to the save path it sits under, follows whatever save path the
// app is given, so no rule can be written for it and none is needed.
//
// An absolute path no rule covers comes back unchanged and false. That is a gap
// in the plan, and the caller collects it rather than guessing at the mapping.
//
// The longest From wins and only whole path segments match, because /downloads2
// is not a subdirectory of /downloads: mapping it as one would point the app at
// a directory holding nothing it knows, which libtorrent would recheck from
// scratch and the Arrs would read as a vanished episode. The rest of the path
// is passed through exactly as it was found, including a trailing slash and any
// doubled separator, because it names a file inside the mapped directory.
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
