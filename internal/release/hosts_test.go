package release

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/seedbox"
)

// fakeHosts records where the per-host step landed relative to the release.
type fakeHosts struct {
	// missing is what Missing answers.
	missing []string
	missErr error
	// ensureErr fails the step, as a DNS mismatch does.
	ensureErr error

	asks    []HostPlan
	ensured []HostPlan
	events  *[]string

	// mayWrite is true for root, false for a user who cannot write the login.
	mayWrite bool
	// registered is the client Secret each app holds.
	registered map[string]bool
	// clients records every ClientRegistered question.
	clients []string

	// loginPending is what Login.Pending answers; false is already right.
	loginPending bool
	pendingErr   error
	reconErr     error

	prunes     []HostPlan
	pruneErr   error
	pendings   []HostPlan
	reconciled []HostPlan
	stales     []HostPlan
	// stale is what Stale answers; staleErr fails it.
	stale    []string
	staleErr error
	// staleFor reads the plan Stale was handed; it wins over stale.
	staleFor func(HostPlan) []string
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

// MayWrite is LoginAccess' first answer.
func (f *fakeHosts) MayWrite(context.Context) (bool, error) { return f.mayWrite, nil }

// ClientRegistered reports the app's Secret and records the question.
func (f *fakeHosts) ClientRegistered(_ context.Context, _, app string) (bool, error) {
	f.clients = append(f.clients, app)
	return f.registered[app], nil
}

// Root's step answers both of the release's questions, and prunes what this run leaves.
var _ LoginAccess = (*fakeHosts)(nil)
var _ HostPruner = (*fakeHosts)(nil)

// hostsForUser serves its names and has no login half: a user cannot write it.
type hostsForUser struct{ missing []string }

func (h *hostsForUser) Missing(context.Context, HostPlan) ([]string, error) {
	return h.missing, nil
}

func (h *hostsForUser) Ensure(context.Context, HostPlan) error { return nil }

// The user's step is a Hosts and not a Login.
var _ Hosts = (*hostsForUser)(nil)

func (f *fakeHosts) Missing(_ context.Context, plan HostPlan) ([]string, error) {
	f.asks = append(f.asks, plan)
	if f.missErr != nil {
		return nil, f.missErr
	}
	return f.missing, nil
}

// Stale is init's optional half: answering hosts with the wrong Certificate.
func (f *fakeHosts) Stale(_ context.Context, plan HostPlan) ([]string, error) {
	f.stales = append(f.stales, plan)
	if f.staleErr != nil {
		return nil, f.staleErr
	}
	if f.staleFor != nil {
		return f.staleFor(plan), nil
	}
	return f.stale, nil
}

// Prune records what a run asks to remove, with the plan it was given.
func (f *fakeHosts) Prune(_ context.Context, plan HostPlan) error {
	f.prunes = append(f.prunes, plan)
	if f.pruneErr != nil {
		return f.pruneErr
	}
	if f.events != nil {
		*f.events = append(*f.events, "prune")
	}
	return nil
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

func TestTheBoxsOwnHostIsServedBeforeTheRelease(t *testing.T) {
	f := newFakes(t)
	f.hosts = &fakeHosts{
		missing: []string{"box.example.com", "radarr.box.example.com"},
		events:  &f.events,
	}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.asks) != 1 {
		t.Fatalf("the per-host step was asked %d times, want once", len(f.hosts.asks))
	}
	got := f.hosts.asks[0].Hosts
	if len(got) == 0 || got[0] != "box.example.com" {
		t.Fatalf("the hosts apply planned = %v, want the box's own name first: the login is served on it", got)
	}
	if len(f.hosts.ensured) != 1 {
		t.Fatalf("the per-host step ran %d times, want once", len(f.hosts.ensured))
	}
	if len(res.Hosts) != 2 {
		t.Fatalf("the result reports %v as the hosts to serve", res.Hosts)
	}
	if a, b := eventIndex(f.events, "hosts"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want every host served before the release moves: Helm's wait cannot end before its app can dial the issuer", f.events)
	}
}

func TestInitPlansAroundTheLoginItInstalls(t *testing.T) {
	f := newFakes(t)
	// Pending says yes for a cluster with no login; this run installs one.
	f.hosts = &fakeHosts{
		missing:      []string{"box.example.com", "radarr.box.example.com"},
		loginPending: true,
		events:       &f.events,
	}
	// The marker stands in for init's login install after the confirm.
	f.events = append(f.events, "login-install")

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.Login {
		t.Fatal("the plan did not show the login: this run installs it, and a plan that hides it says the box is ready without one")
	}
	if len(f.hosts.pendings) != 1 {
		t.Fatalf("the login was read %d times, want once: the plan has to ask before it can promise the page", len(f.hosts.pendings))
	}
	if len(f.hosts.ensured) != 1 {
		t.Fatalf("the per-host step ran %d times, want once", len(f.hosts.ensured))
	}
	for _, want := range [][2]string{
		{"login-install", "hosts"},
		{"hosts", "helm"},
	} {
		a, b := eventIndex(f.events, want[0]), eventIndex(f.events, want[1])
		if a < 0 || b < 0 || a > b {
			t.Fatalf("events = %v, want %s before %s", f.events, want[0], want[1])
		}
	}
}

func TestInitServesEveryHostItDecided(t *testing.T) {
	f := newFakes(t)
	enableRadarrInGrant(t, f.grantPath)
	f.hostsAll = true
	f.hosts = &fakeHosts{events: &f.events} // nothing is missing

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.ensured) != 1 {
		t.Fatalf("the per-host step ran %d times, want once", len(f.hosts.ensured))
	}
	plan := f.hosts.ensured[0]
	if len(plan.Hosts) != 0 {
		t.Fatalf("the plan shows %v as work: nothing was missing", plan.Hosts)
	}
	if len(plan.All) != 2 || plan.All[0] != "box.example.com" || plan.All[1] != "radarr.box.example.com" {
		t.Fatalf("the step was handed %v, want every name init decided", plan.All)
	}
	if a, b := eventIndex(f.events, "hosts"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want the hosts before the release", f.events)
	}
}

func TestInitServesAHostWhoseCertificateIsStale(t *testing.T) {
	f := newFakes(t)
	settled(t, f)
	f.hostsAll = true
	// Nothing is missing: both hosts answer. One of them is stale.
	f.hosts = &fakeHosts{stale: []string{"box.example.com"}, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.stales) != 1 {
		t.Fatalf("the step was asked about stale certificates %d times, want once", len(f.hosts.stales))
	}
	if len(f.hosts.ensured) != 1 {
		t.Fatalf("the per-host step ran %d times, want once", len(f.hosts.ensured))
	}
	if plan := f.hosts.ensured[0]; len(plan.Hosts) != 1 || plan.Hosts[0] != "box.example.com" {
		t.Fatalf("the step was shown %v, want the host whose certificate is stale", plan.Hosts)
	}
	if len(f.hosts.ensured[0].All) != 1 || f.hosts.ensured[0].All[0] != "box.example.com" {
		t.Fatalf("the step was handed %v as every host, want the one this fixture serves", f.hosts.ensured[0].All)
	}
	if res.Applied || f.rel.upgrades != 0 {
		t.Fatalf("re-issuing a certificate bumped the release (%d upgrades, applied %v)", f.rel.upgrades, res.Applied)
	}
	if !strings.Contains(f.out.String(), "box.example.com") {
		t.Fatalf("the plan did not show the host it is about to re-issue:\n%s", f.out)
	}
}

func TestInitReissuesAHostTheDNSDecisionMoved(t *testing.T) {
	f := newFakes(t)
	settled(t, f)
	f.hostsAll = true
	f.hostIssuers = map[string]string{"box.example.com": "letsencrypt"}
	f.hosts = &fakeHosts{
		events: &f.events,
		staleFor: func(plan HostPlan) []string {
			if plan.Issuers["box.example.com"] != "letsencrypt" {
				t.Errorf("the stale read was given issuers %v, want this run's own decision", plan.Issuers)
			}
			return []string{"box.example.com"}
		},
	}
	f.anchor = &fakeAnchor{line: "remove the host CA ConfigMap testbox/s5n-host-ca", events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.ensured) != 1 {
		t.Fatalf("the per-host step ran %d times, want once", len(f.hosts.ensured))
	}
	plan := f.hosts.ensured[0]
	if len(plan.Hosts) != 1 || plan.Hosts[0] != "box.example.com" {
		t.Fatalf("the step was shown %v, want the host whose issuer moved", plan.Hosts)
	}
	if plan.Issuers["box.example.com"] != "letsencrypt" {
		t.Fatalf("the step was handed issuers %v, want the decision the plan used", plan.Issuers)
	}
	if len(res.Hosts) != 1 || res.Hosts[0] != "box.example.com" {
		t.Fatalf("result hosts = %v, want the moved host", res.Hosts)
	}
	out := f.out.String()
	for _, want := range []string{"Certificate, listener and login rule for box.example.com", "remove the host CA ConfigMap testbox/s5n-host-ca"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the plan did not show %q:\n%s", want, out)
		}
	}
	if res.Applied || f.rel.upgrades != 0 {
		t.Fatalf("moving a certificate bumped the release (%d upgrades, applied %v)", f.rel.upgrades, res.Applied)
	}
}

func TestAnInitWithEveryHostRightChangesNothing(t *testing.T) {
	f := newFakes(t)
	settled(t, f)
	f.hostsAll = true
	f.hostIssuers = map[string]string{"box.example.com": "selfsigned"}
	f.anchorIssuer = "selfsigned"
	f.hosts = &fakeHosts{events: &f.events}
	f.anchor = &fakeAnchor{events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.ensured) != 0 {
		t.Fatalf("the per-host step wrote %v, want nothing", f.hosts.ensured)
	}
	if len(f.anchor.ensured) != 0 {
		t.Fatalf("the anchor step wrote %v, want nothing", f.anchor.ensured)
	}
	if len(f.anchor.plans) != 1 {
		t.Fatalf("the anchor read ran %d times, want once: it is how the plan knows there is nothing to do", len(f.anchor.plans))
	}
	if f.anchor.plans[0].Issuer != "selfsigned" {
		t.Fatalf("the anchor was planned for issuer %q, want this run's decision", f.anchor.plans[0].Issuer)
	}
	if len(f.hosts.stales) != 1 {
		t.Fatalf("the stale read ran %d times, want once: it is how the plan knows there is nothing to do", len(f.hosts.stales))
	}
	if res.Applied || f.rel.upgrades != 0 {
		t.Fatalf("a run with nothing to change made a revision (%d upgrades)", f.rel.upgrades)
	}
	if !strings.Contains(f.out.String(), "nothing to change") {
		t.Fatalf("init did not say there was nothing to change:\n%s", f.out)
	}
}

// enableRadarrInGrant is root editing the grant to switch on a hosted app.
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

// eventIndex is one event's position, or -1.
func eventIndex(events []string, want string) int {
	for i, got := range events {
		if got == want {
			return i
		}
	}
	return -1
}

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
	// The box's own name is planned first: its listener answers 443.
	if len(ask.Hosts) != 2 || ask.Hosts[0] != "box.example.com" || ask.Hosts[1] != "radarr.box.example.com" {
		t.Fatalf("the step was asked about %v, want the box's own name and the one hosted app's", ask.Hosts)
	}
	if len(f.hosts.ensured) != 1 || len(f.hosts.ensured[0].Hosts) != 1 || f.hosts.ensured[0].Hosts[0] != "radarr.box.example.com" {
		t.Fatalf("the step wrote %v, want radarr's host", f.hosts.ensured)
	}
	// The step carries the login's rule inputs, as init writes them.
	rule := f.hosts.ensured[0].Rule
	if rule.Login != "alice" {
		t.Fatalf("the rule names %q, want the grant's login", rule.Login)
	}
	// The list is the names themselves: the rule is the whole <instance>.<domain>, and
	// the default instance's name is the app's.
	if len(rule.Hosted) != 1 || rule.Hosted[0] != "radarr.box.example.com" {
		t.Fatalf("hosted names = %v, want radarr's host", rule.Hosted)
	}
	// The catalog's own-login list reaches the rule write.
	if len(rule.OwnLogin) != 1 || rule.OwnLogin[0] != "radarr" {
		t.Fatalf("own-login apps = %v, want the catalog's radarr", rule.OwnLogin)
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

func TestApplyAsksAboutNoHostWhenTheAppsStayOnPaths(t *testing.T) {
	f := newFakes(t)
	f.hosts = &fakeHosts{events: &f.events}

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.asks) != 1 || len(f.hosts.asks[0].Hosts) != 1 || f.hosts.asks[0].Hosts[0] != "box.example.com" {
		t.Fatalf("the step was asked about %v, want only the box's own name", f.hosts.asks)
	}
	if len(f.hosts.ensured) != 0 || eventIndex(f.events, "hosts") >= 0 {
		t.Fatalf("the step wrote %v, want nothing: the box's own name is served", f.hosts.ensured)
	}
}

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
	if f.chart != nil {
		chart = *f.chart
	}
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

// A workload's host is the instance's name, not the app's: a second instance of an app
// that lives at a name of its own is reached at <instance>.<domain>, and the first
// instance is named after the app, which is every name a one-instance box has.
func TestAHostIsTheInstancesName(t *testing.T) {
	chart := testChart(t)
	values := map[string]any{"apps": map[string]any{
		"radarr": map[string]any{"enabled": true, "instances": []any{
			map[string]any{"suffix": "", "enabled": true},
			map[string]any{"suffix": "4k", "enabled": true},
			map[string]any{"suffix": "off", "enabled": false},
		}},
		// Path-served: no host of its own, however many instances it has.
		"sonarr": map[string]any{"enabled": true},
	}}
	hosts, err := hostedHosts(chart, values, "box.example.com")
	if err != nil {
		t.Fatalf("hostedHosts: %v", err)
	}
	want := []string{"radarr.box.example.com", "radarr-4k.box.example.com"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}

}

// A run with nothing to add still removes what a previous one wrote and this one does not
// serve: the plan carries every name this run serves, so the pruning half knows what to
// keep.
func TestAPrunerIsToldWhatThisRunServes(t *testing.T) {
	f := newFakes(t)
	// The fixture runs radarr, the one hosted app: its name is what this run serves beside
	// the box's own.
	enableRadarrInGrant(t, f.grantPath)
	f.hosts = &fakeHosts{} // nothing missing, nothing stale
	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if len(f.hosts.prunes) != 1 {
		t.Fatalf("the pruning step ran %d times, want once", len(f.hosts.prunes))
	}
	got := f.hosts.prunes[0].Served
	if len(got) == 0 || got[0] != "box.example.com" {
		t.Fatalf("served = %v, want the box's own name first", got)
	}
	if len(got) < 2 || got[1] != "radarr.box.example.com" {
		t.Fatalf("served = %v, want every name this run serves", got)
	}
	// It runs after the write, so a name this run adds is never removed in the same run.
	if f.events != nil {
		if a, b := eventIndex(f.events, "prune"), eventIndex(f.events, "helm"); a > -1 && b > -1 && a > b {
			t.Fatalf("events = %v, want pruning before the release", f.events)
		}
	}
}

// A step that cannot read or drop a name stops the run: a half-pruned box serves a name
// nobody asked for.
func TestAFailedPruneStopsTheRun(t *testing.T) {
	f := newFakes(t)
	f.hosts = &fakeHosts{pruneErr: errors.New("the API server refused the write")}
	if _, err := f.apply(t); err == nil {
		t.Fatalf("apply succeeded after a failed prune\n%s", f.out)
	}
}
