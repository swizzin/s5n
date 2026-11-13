package release

import "testing"

// TestANewClientIsRegisteredBeforeTheRelease: an apply that turns on an app
// signing in through the login is a run with login work even when every object
// the login renders is already right -- the client it needs is not there yet.
// The reconcile has to come before Helm, because the app's pod mounts the
// Secret that reconcile writes and Helm waits on that pod.
func TestANewClientIsRegisteredBeforeTheRelease(t *testing.T) {
	f := newFakes(t)
	// The release as it is deployed, with the app off: the grant enables it,
	// so this run turns on an app whose catalog entry says it is a client.
	deployed := deployedValues(t, f)
	apps, _ := deployed["apps"].(map[string]any)
	entry, _ := apps["sonarr"].(map[string]any)
	entry["enabled"] = false
	f.rel.values, f.rel.exists = deployed, true
	f.store.nsExists = true
	f.store.existing["testbox-s5n-data"] = true
	f.store.existing["testbox-s5n-config"] = true
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

// TestFirstApplyWritesTheClientBeforeTheRelease: a first install has no
// namespace until apply creates one, so the login's plan says "a client is
// missing" before there is anywhere to write it. The write still has to happen
// after CreateNamespace and before Helm -- that is the order a fresh box needs,
// and it is why a missing namespace is not a reason to plan nothing.
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
	if ns < 0 || login < 0 || helm < 0 || !(ns < login && login < helm) {
		t.Fatalf("events = %v, want namespace, then the login's client, then the release", f.events)
	}
}
