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
	want := `platform plan:
  + cert-manager ` + pins.CertManager + ` (certificates)
      + ClusterIssuers: Let's Encrypt, plus self-signed
  + Envoy Gateway ` + pins.EnvoyGateway + ` (the front door)
      + the s5n Gateway, HTTP on :80
  + MetalLB ` + pins.MetalLB + ` (the gateway's address)
      + IPAddressPool: this node's IP as a /32, over L2
  + ValidatingAdmissionPolicies enforcing the grant
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
	want := `platform plan:
  = cert-manager (certificates): already on this cluster, left alone
      + ClusterIssuers: Let's Encrypt, plus self-signed
  = Envoy Gateway (the front door): already on this cluster, left alone
      + the s5n Gateway, HTTP on :80
      ? Gateway s5n-gateway/s5n getting an address
  ? MetalLB ` + pins.MetalLB + ` (the gateway's address), unless the Gateway gets an address without it
      ? IPAddressPool: this node's IP as a /32, over L2
  + ValidatingAdmissionPolicies enforcing the grant
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
	want := map[string]string{
		"cert-manager":  "  + cert-manager " + pins.CertManager + " (certificates)",
		"Envoy Gateway": "  = Envoy Gateway " + pins.EnvoyGateway + " (the front door): installed, verifying",
		"MetalLB":       "  ~ MetalLB " + pins.MetalLB + " (the gateway's address); upgrading from 0.0.1",
	}
	text := plainPlan(t, plan)
	for _, line := range want {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("the plan is missing %q:\n%s", line, text)
		}
	}
	// A component at its pin is verified, and its sub-steps still run: the
	// issuers and the pool are applied on every run, because re-applying them
	// is what repairs a cluster where they were removed.
	if !strings.Contains(text, "      + ClusterIssuers: Let's Encrypt") {
		t.Fatalf("a verified cert-manager lost its issuers:\n%s", text)
	}
	if !strings.Contains(text, "      + IPAddressPool: this node's IP") {
		t.Fatalf("an upgraded MetalLB lost its pool:\n%s", text)
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
