package release

import (
	"encoding/json"
	"strings"
	"testing"
)

// chartWithOIDC declares gatewayIP, so the grant's record reaches the values.
func chartWithOIDC(t *testing.T) Chart {
	t.Helper()
	c := testChart(t)
	var schema map[string]any
	if err := json.Unmarshal(c.Schema, &schema); err != nil {
		t.Fatalf("the fixture chart's schema: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	auth, _ := props["auth"].(map[string]any)
	inner, _ := auth["properties"].(map[string]any)
	inner["oidc"] = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"gatewayIP": map[string]any{"type": "string"},
		},
	}
	out, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("the fixture chart's schema: %v", err)
	}
	c.Schema = out
	return c
}

// grantWithOIDC adds the Gateway's proxy address: what reaches the issuer.
func grantWithOIDC(t *testing.T, f *fakes) {
	t.Helper()
	rewrite(t, f.grantPath, map[string]string{
		"gateway:\n  host: box.example.com": "auth:\n  oidc:\n    gatewayIP: 192.0.2.10\n\ngateway:\n  host: box.example.com",
	})
}

// sonarrNewlyEnabled is deployed with sonarr off; the grant turns it on.
func sonarrNewlyEnabled(t *testing.T, f *fakes) {
	t.Helper()
	deployed := deployedValues(t, f)
	apps, _ := deployed["apps"].(map[string]any)
	entry, _ := apps["sonarr"].(map[string]any)
	entry["enabled"] = false
	f.rel.values, f.rel.exists = deployed, true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true
}

func TestANewClientIsRegisteredBeforeTheRelease(t *testing.T) {
	f := newFakes(t)
	sonarrNewlyEnabled(t, f)
	f.hosts = &fakeHosts{loginPending: true, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.Login {
		t.Fatal("the plan counted no login work for an app that has no client yet")
	}
	if len(f.hosts.reconciled) != 1 {
		t.Fatalf("the login was reconciled %d times, want once: the client Secret it writes is what the app's pod mounts", len(f.hosts.reconciled))
	}
	if a, b := eventIndex(f.events, "login"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want the login's reconcile before the release moves: Helm's wait cannot end before the app has its client", f.events)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the newly enabled app to be released", f.rel.upgrades)
	}
}

func TestFirstApplyWritesTheClientBeforeTheRelease(t *testing.T) {
	f := newFakes(t) // no namespace and no release: the fixture's fresh cluster
	f.hosts = &fakeHosts{loginPending: true, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, f.out)
	}
	if !res.NamespaceCreated {
		t.Fatal("the fixture did not create the namespace, so it is not the first-install case")
	}
	if len(f.hosts.reconciled) != 1 {
		t.Fatalf("the login was reconciled %d times on a first install, want once", len(f.hosts.reconciled))
	}
	ns, login, helm := eventIndex(f.events, "namespace"), eventIndex(f.events, "login"), eventIndex(f.events, "helm")
	if ns < 0 || login < 0 || helm < 0 || ns >= login || login >= helm {
		t.Fatalf("events = %v, want namespace, then the login's client, then the release", f.events)
	}
}

func TestAUserApplyRefusesAClientItCannotRegister(t *testing.T) {
	f := newFakes(t)
	chart := chartWithOIDC(t)
	f.chart = &chart
	grantWithOIDC(t, f)
	sonarrNewlyEnabled(t, f)
	f.hosts = &fakeHosts{loginPending: true}

	asked := false
	f.app.Confirm = confirmFunc(func(string) (bool, error) {
		asked = true
		return true, nil
	})

	res, err := f.apply(t)
	if err == nil {
		t.Fatalf("apply installed an app whose client only root can register\n%s", f.out)
	}
	want := "sonarr signs in through the login and has no client yet, and only root can register one: run `box init` as root for this seedbox, then apply again"
	if err.Error() != want {
		t.Fatalf("refusal = %q,\nwant %q", err, want)
	}
	if asked {
		t.Fatal("the refusal came after the confirm: a user was asked to approve something that cannot work")
	}
	if res.Applied || f.rel.upgrades != 0 {
		t.Fatal("the refusal wrote the release")
	}
	if len(f.store.applied) != 0 || f.store.nsCreates != 0 {
		t.Fatalf("the refusal wrote %d storage objects and created the namespace %d times", len(f.store.applied), f.store.nsCreates)
	}
	if strings.Contains(f.out.String(), "apply s5n in testbox?") {
		t.Fatalf("the refusal printed the confirm:\n%s", f.out)
	}
	if len(f.hosts.clients) != 1 || f.hosts.clients[0] != "sonarr" {
		t.Fatalf("the refusal read the cluster for %v, want sonarr's client Secret once", f.hosts.clients)
	}
}

func TestAUserApplyAllowsAReEnableWhoseClientIsThere(t *testing.T) {
	f := newFakes(t)
	chart := chartWithOIDC(t)
	f.chart = &chart
	grantWithOIDC(t, f)
	sonarrNewlyEnabled(t, f)
	f.hosts = &fakeHosts{loginPending: true, registered: map[string]bool{"sonarr": true}}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("apply refused a re-enable whose client exists: %v\n%s", err, f.out)
	}
	if !res.Applied || f.rel.upgrades != 1 {
		t.Fatalf("result = %+v, want the re-enable released", res)
	}
}

func TestNoRefusalWithoutTheGatewayAddress(t *testing.T) {
	f := newFakes(t)
	chart := chartWithOIDC(t)
	f.chart = &chart
	// The fixture grant carries no auth.oidc.
	sonarrNewlyEnabled(t, f)
	f.hosts = &fakeHosts{loginPending: true}

	if _, err := f.apply(t); err != nil {
		t.Fatalf("apply refused although the chart renders no OIDC: %v\n%s", err, f.out)
	}
	if len(f.hosts.clients) != 0 {
		t.Fatalf("the refusal read the cluster for %v without a gateway address", f.hosts.clients)
	}
	if f.rel.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the app released", f.rel.upgrades)
	}
}

func TestNoRefusalForRoot(t *testing.T) {
	f := newFakes(t)
	chart := chartWithOIDC(t)
	f.chart = &chart
	grantWithOIDC(t, f)
	sonarrNewlyEnabled(t, f)
	f.hosts = &fakeHosts{loginPending: true, mayWrite: true, events: &f.events}

	res, err := f.apply(t)
	if err != nil {
		t.Fatalf("root's apply was refused: %v\n%s", err, f.out)
	}
	if !res.Applied || f.rel.upgrades != 1 {
		t.Fatalf("result = %+v, want the release applied", res)
	}
	if len(f.hosts.clients) != 0 {
		t.Fatalf("root's apply read the cluster for a client it is about to write: %v", f.hosts.clients)
	}
	if a, b := eventIndex(f.events, "login"), eventIndex(f.events, "helm"); a < 0 || b < 0 || a > b {
		t.Fatalf("events = %v, want the client registered before the release", f.events)
	}
}
