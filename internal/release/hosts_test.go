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

	// The login's half of the same step, which root's value also implements:
	// loginPending is what Pending answers, so a fake left at false models a
	// login that is already the one the values name.
	loginPending bool
	pendingErr   error
	reconErr     error

	pendings   []HostPlan
	reconciled []HostPlan
}

func (f *fakeHosts) Pending(_ context.Context, plan HostPlan) (bool, error) {
	f.pendings = append(f.pendings, plan)
	if f.pendingErr != nil {
		return false, f.pendingErr
	}
	return f.loginPending, nil
}

func (f *fakeHosts) Reconcile(_ context.Context, plan HostPlan) error {
	if f.reconErr != nil {
		return f.reconErr
	}
	f.reconciled = append(f.reconciled, plan)
	if f.events != nil {
		*f.events = append(*f.events, "login")
	}
	return nil
}

// hostsForUser is the step a user's apply gets: it serves the names it is
// allowed to, and it has no login half at all -- a seedbox user's kubeconfig
// cannot write the login, so the page everyone signs in on is never theirs.
type hostsForUser struct{ missing []string }

func (h *hostsForUser) Missing(context.Context, HostPlan) ([]string, error) {
	return h.missing, nil
}

func (h *hostsForUser) Ensure(context.Context, HostPlan) error { return nil }

// The compile-time half of the claim above: it is a Hosts and it is not a
// Login, so an apply handed this value has no page to reconcile.
var _ Hosts = (*hostsForUser)(nil)

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

// settled starts an apply from the release and the storage being as they
// already are, so the only work left is whatever the test turns on.
func settled(t *testing.T, f *fakes) {
	t.Helper()
	f.rel.values, f.rel.exists = deployedValues(t, f), true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true
}

// A theme change is the login's work and nobody else's. The values file names
// the palette, so the page everyone signs in on is re-rendered in it -- and no
// name is DNS-checked, given a listener or re-certified for that, because a
// palette is not a host.
func TestAThemeChangeReconcilesTheLoginAlone(t *testing.T) {
	f := newFakes(t)
	settled(t, f)
	f.writeValues(t, "theme: s5n-light\n")
	f.hosts = &fakeHosts{loginPending: true, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.pendings) != 1 {
		t.Fatalf("the login was read %d times, want once", len(f.hosts.pendings))
	}
	if len(f.hosts.reconciled) != 1 {
		t.Fatalf("the login was re-rendered %d times, want once", len(f.hosts.reconciled))
	}
	if !res.Login {
		t.Fatal("the result does not say the login was re-rendered")
	}
	if len(f.hosts.ensured) != 0 || eventIndex(f.events, "hosts") >= 0 {
		t.Fatalf("a theme change ran the per-host step: %v", f.events)
	}
	// The plan said so before it wrote: the operator confirmed a run that
	// re-renders the sign-in page.
	if !strings.Contains(f.out.String(), "the sign-in page's configuration, mark and favicon") {
		t.Fatalf("the plan did not show the login it would re-render:\n%s", f.out)
	}
	// The login is rendered from the release's own document and image, so a
	// theme change cannot reach it by a second path.
	rule := f.hosts.reconciled[0].Rule
	if rule.Login != "alice" {
		t.Fatalf("the login is drawn for %q, want the grant's account", rule.Login)
	}
	if rule.Image == "" || rule.Image != testChart(t).Pins.Authelia {
		t.Fatalf("the login names image %q, want the chart's pin", rule.Image)
	}
	if got, _ := rule.Values["theme"].(string); got != "s5n-light" {
		t.Fatalf("the login was rendered from theme %q, want the file's s5n-light", got)
	}
	if f.hosts.pendings[0].Domain != "box.example.com" {
		t.Fatalf("the login was told the domain %q, want the grant's box.example.com", f.hosts.pendings[0].Domain)
	}
	// The values really changed, so the release moves once -- that is Helm
	// recording the theme, not the login's write.
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want one for a changed values file", f.rel.upgrades)
	}
}

// A login alone is not a release revision. The page the values already name was
// hand-edited or drawn by an older box: the re-render is work, and the revision
// users would roll back to stays where it is, exactly as a certificate that is
// the only work does not move it.
func TestReconcilingTheLoginDoesNotMakeAReleaseRevision(t *testing.T) {
	f := newFakes(t)
	settled(t, f)
	f.hosts = &fakeHosts{loginPending: true, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.reconciled) != 1 {
		t.Fatalf("the login was re-rendered %d times, want once", len(f.hosts.reconciled))
	}
	if f.rel.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none: only the login page differed", f.rel.upgrades)
	}
	if res.Applied {
		t.Fatal("the result says the release was applied")
	}
	if !strings.Contains(f.out.String(), "the release is unchanged") {
		t.Fatalf("apply did not say the release itself was unchanged:\n%s", f.out)
	}
}

// An apply with nothing to change leaves the login alone: it asks, is told the
// page in the cluster is the one the values name, and stops before it asks the
// operator anything.
func TestAnApplyWithNothingToChangeLeavesTheLoginAlone(t *testing.T) {
	f := newFakes(t)
	settled(t, f)
	f.hosts = &fakeHosts{events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.pendings) != 1 {
		t.Fatalf("the login was read %d times, want once", len(f.hosts.pendings))
	}
	if len(f.hosts.reconciled) != 0 {
		t.Fatalf("the login was written %v, want nothing", f.hosts.reconciled)
	}
	if res.Login || f.rel.upgrades != 0 || res.Applied {
		t.Fatalf("a run with nothing to change did work: login=%v upgrades=%d applied=%v", res.Login, f.rel.upgrades, res.Applied)
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("apply did not say there was nothing to change:\n%s", f.out)
	}
}

// A user's apply never touches the login: their kubeconfig cannot write it, so
// the step they are given has no login half and apply reconciles nothing. The
// assertion below is the whole guard -- a value that implemented Login would be
// one a user's run could write the page everyone signs in on with.
func TestAnApplyWithoutALoginHalfNeverTouchesThePage(t *testing.T) {
	if _, ok := any(&hostsForUser{}).(Login); ok {
		t.Fatal("the user's step implements Login: their apply would write root's page")
	}
	f := newFakes(t)
	settled(t, f)
	f.writeValues(t, "theme: s5n-light\n")
	f.otherHosts = &hostsForUser{}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if res.Login {
		t.Fatal("an apply with no login half reconciled a login")
	}
	if strings.Contains(f.out.String(), "the sign-in page's configuration") {
		t.Fatalf("the plan offered a login a user cannot write:\n%s", f.out)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the values change to land", f.rel.upgrades)
	}
}
