package release

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

// fakeHosts is the per-host TLS step: the read and the write apply makes around
// its own, recorded in the shared event log so a test can prove where the step
// landed relative to the release.
type fakeHosts struct {
	// missing is what Missing answers: the names the cluster has no listener
	// for.
	missing []string
	missErr error
	// ensureErr fails the step, the way a DNS mismatch or a user's kubeconfig
	// does.
	ensureErr error

	asks    []HostPlan
	ensured []HostPlan
	events  *[]string
}

func (f *fakeHosts) Missing(_ context.Context, plan HostPlan) ([]string, error) {
	f.asks = append(f.asks, plan)
	if f.missErr != nil {
		return nil, f.missErr
	}
	return f.missing, nil
}

func (f *fakeHosts) Ensure(_ context.Context, plan HostPlan) error {
	if f.ensureErr != nil {
		return f.ensureErr
	}
	f.ensured = append(f.ensured, plan)
	if f.events != nil {
		*f.events = append(*f.events, "hosts")
	}
	return nil
}

// enableRadarrInGrant is "root edited the grant": the one way an app is
// switched on for a seedbox, appended to the fixture grant's apps section.
// Radarr is the fixture chart's one app served at a host of its own.
func enableRadarrInGrant(t *testing.T, grantPath string) {
	t.Helper()
	entry := "  radarr:\n    enabled: true\n"
	f, err := os.OpenFile(grantPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		t.Fatal(err)
	}
}

// eventIndex is the position of one event in the shared log, or -1.
func eventIndex(events []string, want string) int {
	for i, got := range events {
		if got == want {
			return i
		}
	}
	return -1
}

// An apply that turns on an app served at a host of its own runs the per-host
// step before the release moves, so the route the release creates always has a
// listener under it.
func TestApplyServesANewAppHostBeforeTheRelease(t *testing.T) {
	f := newFakes(t)
	enableRadarrInGrant(t, f.grantPath)
	f.hosts = &fakeHosts{missing: []string{"radarr.box.example.com"}, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.asks) != 1 {
		t.Fatalf("the step was asked %d times, want once", len(f.hosts.asks))
	}
	ask := f.hosts.asks[0]
	if ask.Domain != "box.example.com" {
		t.Fatalf("the step was told the domain %q, want the grant's box.example.com", ask.Domain)
	}
	if len(ask.Hosts) != 1 || ask.Hosts[0] != "radarr.box.example.com" {
		t.Fatalf("the step was asked about %v, want the one hosted app's name", ask.Hosts)
	}
	if len(f.hosts.ensured) != 1 || len(f.hosts.ensured[0].Hosts) != 1 || f.hosts.ensured[0].Hosts[0] != "radarr.box.example.com" {
		t.Fatalf("the step wrote %v, want radarr's host", f.hosts.ensured)
	}
	// The step carries the login's inputs too: a host is served when its rule
	// exists, and the rule is the one init writes -- the grant's account, the
	// document the release renders, and the release's own login image.
	rule := f.hosts.ensured[0].Rule
	if rule.Login != "alice" {
		t.Fatalf("the rule names %q, want the grant's login", rule.Login)
	}
	if len(rule.Hosted) != 1 || rule.Hosted[0] != "radarr" {
		t.Fatalf("hosted apps = %v, want radarr", rule.Hosted)
	}
	if on, _ := rule.Values["apps"].(map[string]any)["radarr"].(map[string]any)["enabled"].(bool); !on {
		t.Fatalf("the rule's values do not have radarr enabled: %v", rule.Values["apps"])
	}
	if rule.Image != "docker.io/example/authelia:1.0.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("the rule write names image %q, want the chart's pin", rule.Image)
	}
	if got := res.Hosts; len(got) != 1 || got[0] != "radarr.box.example.com" {
		t.Fatalf("result hosts = %v, want the name this run served", got)
	}
	// The order is the promise: no route before its listener.
	if a, b := eventIndex(f.events, "hosts"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want the per-host step before the release", f.events)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the release moved once", f.rel.upgrades)
	}
	if !strings.Contains(f.out.String(), "Certificate, listener and login rule for radarr.box.example.com") {
		t.Fatalf("the plan did not show the certificate it would write:\n%s", f.out)
	}
}

// A box whose hosted app already has its listener has nothing for the step to
// do, and apply does not call it.
func TestApplyLeavesAnAppHostThatIsAlreadyServed(t *testing.T) {
	f := newFakes(t)
	enableRadarrInGrant(t, f.grantPath)
	f.hosts = &fakeHosts{events: &f.events}

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.ensured) != 0 {
		t.Fatalf("the step wrote %v, want nothing", f.hosts.ensured)
	}
	if eventIndex(f.events, "hosts") >= 0 {
		t.Fatalf("events = %v, want no per-host step", f.events)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the first install", f.rel.upgrades)
	}
}

// No app is served at a host of its own: apply asks the cluster nothing about
// one, so a chart without a hosted app costs this path nothing.
func TestApplyAsksAboutNoHostWhenTheAppsStayOnPaths(t *testing.T) {
	f := newFakes(t)
	f.hosts = &fakeHosts{events: &f.events}

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.asks) != 0 {
		t.Fatalf("the step was asked about %v, want nothing", f.hosts.asks)
	}
}

// A stop in the per-host step is a stop before the release: nothing was
// installed, and no storage or namespace was written either.
func TestAFailedHostStepLeavesTheReleaseAlone(t *testing.T) {
	f := newFakes(t)
	enableRadarrInGrant(t, f.grantPath)
	f.hosts = &fakeHosts{
		missing:   []string{"radarr.box.example.com"},
		ensureErr: errors.New("app hosts: radarr.box.example.com resolves, but box.example.com does not"),
		events:    &f.events,
	}

	_, err := f.apply(t)
	if err == nil {
		t.Fatalf("apply installed a release whose host never got a listener\n%s", f.out)
	}
	if !strings.Contains(err.Error(), "app hosts:") {
		t.Fatalf("error = %v, want the step's own refusal", err)
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none", f.rel.upgrades)
	}
	if f.store.nsCreates != 0 {
		t.Fatalf("the namespace was created before the host step stopped the run")
	}
	if len(f.store.applied) != 0 {
		t.Fatalf("storage was written before the host step stopped the run: %v", f.store.applied)
	}
}

// A certificate that is the only work left does not churn the release: the
// revision users would roll back to stays where it is.
func TestServingAHostDoesNotMakeAReleaseRevision(t *testing.T) {
	f := newFakes(t)
	enableRadarrInGrant(t, f.grantPath)
	f.rel.values, f.rel.exists = deployedValues(t, f), true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true
	f.hosts = &fakeHosts{missing: []string{"radarr.box.example.com"}, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.ensured) != 1 {
		t.Fatalf("the step wrote %v, want radarr's host", f.hosts.ensured)
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none: only the certificate was missing", f.rel.upgrades)
	}
	if res.Applied {
		t.Fatalf("result says the release was applied")
	}
	if !strings.Contains(f.out.String(), "the release is unchanged") {
		t.Fatalf("apply did not say the release itself was unchanged:\n%s", f.out)
	}
}

// deployedValues is what the deployed release would hold for this run's files,
// so a test can start from "the release is already right".
func deployedValues(t *testing.T, f *fakes) map[string]any {
	t.Helper()
	sb, err := seedbox.Load(f.grantPath, f.values)
	if err != nil {
		t.Fatal(err)
	}
	chart := testChart(t)
	values, err := sb.Effective(seedbox.Chart{Values: chart.Values, Schema: chart.Schema})
	if err != nil {
		t.Fatal(err)
	}
	return values
}
