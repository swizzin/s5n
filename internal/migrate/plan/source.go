package plan

import (
	"fmt"
	"strings"
	"time"
)

// The platforms a plan can name, for the whole plan and for each app. One plan
// can hold apps from several of them, so the source block says which ones
// contributed and every app says which one it came from.
const (
	Kubernetes = "kubernetes"
	Docker     = "docker"
	Swizzin    = "swizzin"
)

// Source is the install the plan was read from: for the operator, and for what
// apply checks about the host it is about to write on. Nothing here is a rule;
// the apps and their path tables are what apply acts on.
type Source struct {
	// Host is the source host's name as discovery saw it. Empty when it could
	// not say (a plan rebuilt from saved manifests), which is not the same as
	// a host named nothing.
	Host string `yaml:"host,omitempty"`
	// Taken is when the source was read, RFC3339 in UTC. A snapshot taken
	// before a cutover is only the source's last state if it was taken after
	// the apps stopped, so the time is what a reader checks it against.
	Taken string `yaml:"taken"`
	// Owner is the uid:gid that owned the apps' config directories and their
	// data. Absent when discovery could not read it; root is uid 0, so absent
	// is not the default.
	Owner *Owner `yaml:"owner,omitempty"`
	// Kinds are the platforms that contributed an app to the plan, in the
	// order they were read: Kubernetes, then swizzin, then docker.
	Kinds []string `yaml:"kinds"`
}

// Owner is who a source's files belong to. The data root after a migration is
// owned by this account, and apply refuses a data root another user owns rather
// than chowning a media tree.
type Owner struct {
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}

// kindList is the platforms a plan can name, as a refusal lists them.
func kindList() string { return Kubernetes + ", " + Swizzin + " or " + Docker }

// knownKind reports whether k names a platform a plan can hold apps from.
func knownKind(k string) bool {
	switch k {
	case Kubernetes, Docker, Swizzin:
		return true
	}
	return false
}

// checkSource enforces that the source block says what it claims: a time that
// is a time, and the platforms that were actually read.
func (p *Plan) checkSource() []string {
	s := p.Source
	if s == nil {
		return nil
	}
	var problems []string
	if strings.ContainsAny(s.Host, "\r\n") {
		problems = append(problems, fmt.Sprintf("source: host %q must be one line", s.Host))
	}
	if s.Taken == "" {
		problems = append(problems, "source: taken is missing; it is when the source was read, as RFC3339")
	} else if _, err := time.Parse(time.RFC3339, s.Taken); err != nil {
		problems = append(problems, fmt.Sprintf("source: taken %q is not a time (RFC3339, e.g. 2026-11-16T12:00:00Z)", s.Taken))
	}
	if len(s.Kinds) == 0 {
		problems = append(problems, "source: kinds is empty; name every platform that contributed an app")
	}
	seen := make(map[string]bool, len(s.Kinds))
	for _, k := range s.Kinds {
		if !knownKind(k) {
			problems = append(problems, fmt.Sprintf("source: kind %q is not one of %s", k, kindList()))
			continue
		}
		if seen[k] {
			problems = append(problems, fmt.Sprintf("source: kind %q appears twice", k))
		}
		seen[k] = true
	}
	if s.Owner != nil {
		if s.Owner.UID < 0 || s.Owner.GID < 0 {
			problems = append(problems, fmt.Sprintf("source: owner %d:%d is not a uid:gid", s.Owner.UID, s.Owner.GID))
		}
	}
	return problems
}

// checkApp enforces what an app has to say about itself for the rest of the
// file to mean anything: which platform it ran on, and that a variant names the
// image it was mapped from.
func checkApp(name string, app App) []string {
	var problems []string
	switch {
	case app.Kind == "":
		problems = append(problems, fmt.Sprintf("apps.%s: kind is missing; it is one of %s", name, kindList()))
	case !knownKind(app.Kind):
		problems = append(problems, fmt.Sprintf("apps.%s: kind %q is not one of %s", name, app.Kind, kindList()))
	}
	if app.Variant != "" && app.Image == "" {
		problems = append(problems, fmt.Sprintf("apps.%s: variant %q is a mapping from an image, and image is empty", name, app.Variant))
	}
	return problems
}

// whyProblems enforces what a rule's reason is: one line, and present whenever
// the rule is a default discovery chose rather than the operator's own.
func whyProblems(at, why string, def bool) []string {
	var problems []string
	if strings.ContainsAny(why, "\r\n") {
		problems = append(problems, fmt.Sprintf("%s: why %q must be one line", at, why))
	}
	if def && strings.TrimSpace(why) == "" {
		problems = append(problems, fmt.Sprintf("%s: default: true needs a why saying what discovery could not settle", at))
	}
	return problems
}
