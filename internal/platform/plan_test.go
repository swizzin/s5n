package platform

import (
	"bytes"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
)

// planText is what box prints for a plan, through the plain transcript.
func plainPlan(t *testing.T, plan Plan) string {
	t.Helper()
	var buf bytes.Buffer
	d := &Decision{Say: &say.Plain{W: &buf}}
	d.printPlan(d.Say, plan)
	return buf.String()
}

// A plan reads as the components of the platform layer, each with the steps
// that configure it under it -- what issues the certificates, what every app is
// reached through, what gives it an address -- and the policy that holds the
// box to its grant standing on its own.
func TestPlanGroupsEachStepUnderWhatItConfigures(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	want := `  + cert-manager ` + pins.CertManager + ` · Envoy Gateway ` + pins.EnvoyGateway +
		` · MetalLB ` + pins.MetalLB + ` · admission policies
`
	if got := plainPlan(t, plan); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}

	// Every step is printed, and the Gateway and its HTTP check -- the same
	// object, written and then proven -- are one line.
	items := planItems(plan)
	lines := 0
	for _, it := range items {
		lines += 1 + len(it.Children)
	}
	if want, got := len(plan.Steps)-1, lines; want != got {
		t.Fatalf("%d steps printed as %d lines", len(plan.Steps), got)
	}
}

// The plan says what box does, not which objects it writes: the exact names
// belong to the progress and done lines, which print them as it happens.
func TestPlanLeavesObjectNamesToTheProgressLines(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	text := plainPlan(t, plan)
	for _, name := range []string{
		MetalLBPoolName, MetalLBL2Name, LetsEncryptIssuer, SelfSignedIssuer,
		EnvoyGatewayNamespace, MetalLBNamespace,
		"Helm,", "(Helm",
	} {
		if strings.Contains(text, name) {
			t.Fatalf("the plan names %q:\n%s", name, text)
		}
	}
}

// A gateway box found and leaves alone is still the component the gateway and
// its HTTP check hang under: the plan says which implementation the steps go
// through, and that box does not touch it.
func TestPlanGroupsUnderAComponentBoxUses(t *testing.T) {
	plan, err := BuildPlan(pins, components(map[bootstrap.PlatformComponent]string{
		bootstrap.ComponentGatewayAPI:   "use",
		bootstrap.ComponentLoadBalancer: "probe",
		bootstrap.ComponentGateway:      "use",
		bootstrap.ComponentCertManager:  "use",
	}), "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	// The question the plan cannot answer yet stays an item of its own: it is
	// about the address layer, which may or may not be box's to install.
	want := `  + admission policies
  = cert-manager (already here, left alone) · Envoy Gateway (already here, left alone)
  ? MetalLB ` + pins.MetalLB + `, unless the Gateway gets an address without it
`
	if got := plainPlan(t, plan); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
}

// A re-run that upgrades says so, and one that finds a release at its pin
// verifies instead: the version and the reason are the words box has always
// used, on the component's own line.
func TestPlanMarksWhatARunDoesToEachComponent(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan.Steps {
		s := &plan.Steps[i]
		if s.Kind != StepRelease {
			continue
		}
		switch s.Release.Name {
		case EnvoyGatewayRelease:
			s.InstalledVersion = s.Release.Version
		case MetalLBRelease:
			s.InstalledVersion = "0.0.1"
		}
	}
	want := "  + cert-manager " + pins.CertManager + " · admission policies\n" +
		"  ~ MetalLB " + pins.MetalLB + "; upgrading from 0.0.1\n" +
		"  = Envoy Gateway " + pins.EnvoyGateway + "\n"
	text := plainPlan(t, plan)
	if text != want {
		t.Fatalf("the plan is:\n%s\nwant:\n%s", text, want)
	}
	// A component at its pin is verified, and its sub-steps still run: the
	// issuers and the pool are applied on every run, because re-applying them
	// is what repairs a cluster where they were removed. They are the log's
	// (rule 5), so the screen above folds them and the transcript keeps them.
	details := strings.Join(detailLines(plan), "\n")
	for _, line := range []string{"plan: + ClusterIssuers: Let's Encrypt, plus self-signed", "plan: + IPAddressPool: this node's IP as a /32, over L2"} {
		if !strings.Contains(details, line+"\n") {
			t.Fatalf("the log is missing %q:\n%s", line, details)
		}
	}
}

// What box will not handle is the last thing in the plan: it is what a person
// has to act on, and it never hides between two steps.
func TestPlanPrintsWarningsLast(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	plan.NotHandled = []string{"Plex is not installed by this version of box"}
	text := plainPlan(t, plan)
	last := strings.LastIndex(text, "  ! ")
	if last < 0 || !strings.HasSuffix(text, "Plex is not installed by this version of box\n") {
		t.Fatalf("the warning is not the last line:\n%s", text)
	}
	if strings.Count(text, "! ") != 1 {
		t.Fatalf("the warning is printed more than once:\n%s", text)
	}
}

// The host's packet filter comes first: it is the host, not the cluster, and
// every app behind the platform is reached through it. The two networks are the
// ones bootstrap read out of the k0s config box wrote, and the plan prints
// them because they are a change to the host rather than to the cluster.
func TestPlanTrustsTheClusterNetworksFirst(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	plan = withFirewall(plan, bootstrap.FirewallState{Active: true, CIDRs: []string{"10.244.0.0/16", "10.96.0.0/12"}})
	if len(plan.Steps) == 0 || plan.Steps[0].Kind != StepFirewall {
		t.Fatalf("the firewall is not the plan's first step: %+v", plan.Steps[0])
	}
	if got := plan.Steps[0].CIDRs; len(got) != 2 || got[0] != "10.244.0.0/16" || got[1] != "10.96.0.0/12" {
		t.Fatalf("the step trusts %v", got)
	}
	text := plainPlan(t, plan)
	line := "firewalld's trusted zone"
	if !strings.Contains(text, line) {
		t.Fatalf("the plan does not say what it does to the host:\n%s", text)
	}
	// It is one of box's own lines rather than a component's, so it is printed
	// with the admission policy and applied before either: the narrative order
	// is the components', and the plan is not a running order.
	if strings.Index(text, line) > strings.Index(text, "admission policies") {
		t.Fatalf("the host's firewall is printed after the policy:\n%s", text)
	}
}

// A host whose firewall box will not change gets the two commands as a
// warning, printed with the plan and before the confirm: box does not know a
// cluster it did not install's networks, and the operator is the one who can
// run them.
func TestPlanWarnsAboutAFirewallBoxDoesNotChange(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	plan = withFirewall(plan, bootstrap.FirewallState{Active: true, Warning: "firewalld is running: firewall-cmd --permanent --zone=trusted --add-source=<podCIDR>"})
	for _, s := range plan.Steps {
		if s.Kind == StepFirewall {
			t.Fatal("a plan with no networks to trust still writes the host's firewall")
		}
	}
	text := plainPlan(t, plan)
	if !strings.Contains(text, "  ! firewalld is running") {
		t.Fatalf("the plan does not carry the warning:\n%s", text)
	}
	if !strings.HasSuffix(text, "<podCIDR>\n") {
		t.Fatalf("the warning is not printed where a person acts on it:\n%s", text)
	}
}

// A host with no firewalld plans nothing about it, and a plan built without a
// firewall state is the plan box has always printed.
func TestPlanSaysNothingAboutAFirewallThatIsNotRunning(t *testing.T) {
	plan, err := BuildPlan(pins, allInstall, "eg", "")
	if err != nil {
		t.Fatal(err)
	}
	with := withFirewall(plan, bootstrap.FirewallState{})
	if len(with.Steps) != len(plan.Steps) || len(with.Warnings) != 0 {
		t.Fatalf("an inactive firewall changed the plan: %d steps, %d warnings", len(with.Steps), len(with.Warnings))
	}
	if strings.Contains(plainPlan(t, with), "firewalld") {
		t.Fatalf("the plan mentions a firewall that is not running:\n%s", plainPlan(t, with))
	}
}
