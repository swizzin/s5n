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
const Version = 1

// Plan is the whole file.
type Plan struct {
	Version int            `yaml:"version"`
	Apps    map[string]App `yaml:"apps"`
}

// App is one app as the source named it and as box will name it. The key it is
// filed under in Plan.Apps is box's instance name, which is what the chart
// calls the pod and what every caller passes to Rewrite.
type App struct {
	// Source is the name the app had in the install being migrated: how
	// discovery found it, and what the operator recognises in the source's own
	// interface.
	Source string `yaml:"source"`
	// Config is the directory the app's config is copied from. migrate reads
	// it and never writes it, so the running source stays the rollback until
	// the operator tears it down.
	Config string `yaml:"config"`
	// Paths are this app's path rules.
	Paths []PathRule `yaml:"paths"`
}

// PathRule maps one directory the app saw to the directory it will see.
type PathRule struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
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
		problems = append(problems, fmt.Sprintf("version %d: this box reads version %d", p.Version, Version))
	}
	for _, name := range p.appNames() {
		app := p.Apps[name]
		seen := make(map[string]bool, len(app.Paths))
		for i := range app.Paths {
			r := &app.Paths[i]
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
		}
		p.Apps[name] = app
	}
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
