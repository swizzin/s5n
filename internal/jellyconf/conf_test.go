package jellyconf

import (
	"strings"
	"testing"
)

// live is the network.xml a 12.4 server writes (the file a box's app has after
// its first save), trimmed to the elements this package must leave alone. The
// two this package owns are in their empty form, in the order the app's own
// serializer writes them (MediaBrowser.Common/Net/NetworkConfiguration.cs at
// v12.4 declares LocalNetworkAddresses before KnownProxies).
const live = `<?xml version="1.0" encoding="utf-8"?>
<NetworkConfiguration xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <BaseUrl />
  <EnableHttps>false</EnableHttps>
  <InternalHttpPort>8096</InternalHttpPort>
  <EnableRemoteAccess>true</EnableRemoteAccess>
  <LocalNetworkSubnets />
  <LocalNetworkAddresses />
  <KnownProxies />
  <IgnoreVirtualInterfaces>true</IgnoreVirtualInterfaces>
  <VirtualInterfaceNames>
    <string>veth</string>
  </VirtualInterfaceNames>
</NetworkConfiguration>
`

// bound is the network.xml a pod leaves behind when its startup-wizard is killed
// with SIGKILL after the wizard, but before its EXIT trap runs: the wizard had
// written its own loopback bind address into the app's file. A trap does not run
// on SIGKILL (an OOM kill, an eviction, a reboot, a pod deleted past its grace
// period), so this is the state the next start has to repair -- with the app's
// own writes, which the wizard's RemoteAccess call saved, still in the file.
const bound = `<?xml version="1.0" encoding="utf-8"?>
<NetworkConfiguration xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:xsd="http://www.w3.org/2001/XMLSchema">
  <BaseUrl />
  <EnableHttps>false</EnableHttps>
  <InternalHttpPort>8096</InternalHttpPort>
  <EnableRemoteAccess>true</EnableRemoteAccess>
  <LocalNetworkSubnets />
  <LocalNetworkAddresses>
    <string>127.0.0.1</string>
  </LocalNetworkAddresses>
  <KnownProxies />
  <IgnoreVirtualInterfaces>true</IgnoreVirtualInterfaces>
  <VirtualInterfaceNames>
    <string>veth</string>
  </VirtualInterfaceNames>
</NetworkConfiguration>
`

// TestALoopbackBindAddressIsCleared: LocalNetworkAddresses is the list of
// interfaces Jellyfin binds Kestrel to, and empty means every one of them
// (MediaBrowser.Common/Net/NetworkConfiguration.cs:136-139 at v12.4: "the
// interface addresses which Jellyfin will bind to. If empty, all interfaces
// will be used"). A pod's app bound to the loopback is one nothing outside the
// pod can reach, so a file a killed wizard left that way is put back on the next
// start -- and nothing else in it is touched, the app's own writes included.
func TestALoopbackBindAddressIsCleared(t *testing.T) {
	out, changed, err := Ensure([]byte(bound), nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !changed {
		t.Fatal("a file bound to the loopback was reported unchanged")
	}
	got := string(out)
	if strings.Contains(got, "127.0.0.1") {
		t.Fatalf("the rewrite kept the loopback bind address:\n%s", got)
	}
	if !strings.Contains(got, "<LocalNetworkAddresses />") {
		t.Fatalf("the bind address is not the app's own empty element:\n%s", got)
	}
	for _, keep := range []string{
		"<EnableRemoteAccess>true</EnableRemoteAccess>",
		"<InternalHttpPort>8096</InternalHttpPort>",
		"<string>veth</string>",
	} {
		if !strings.Contains(got, keep) {
			t.Fatalf("the rewrite lost %s:\n%s", keep, got)
		}
	}
	// Every start runs this, so the second one writes nothing.
	if _, changed, err := Ensure(out, nil); err != nil || changed {
		t.Fatalf("second pass: changed=%v err=%v", changed, err)
	}
}

// TestAnAbsentBindAddressIsWrittenEmpty: a document that predates the setting
// (or a hand-written one) gains the empty element, which is the app's "all
// interfaces", rather than being left to a default nobody stated.
func TestAnAbsentBindAddressIsWrittenEmpty(t *testing.T) {
	old := strings.Replace(live, "  <LocalNetworkAddresses />\n", "", 1)
	out, changed, err := Ensure([]byte(old), nil)
	if err != nil || !changed {
		t.Fatalf("ensure: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "<LocalNetworkAddresses />") {
		t.Fatalf("the bind address was not added:\n%s", out)
	}
	if !strings.Contains(string(out), "<LocalNetworkSubnets />") {
		t.Fatalf("the rewrite lost a setting beside the one it added:\n%s", out)
	}
}

// TestBothElementsAreTheBoxsOneAnswer: the wizard's loopback value and a proxy
// list the box no longer serves are both replaced in one pass, and the app's own
// settings stay.
func TestBothElementsAreTheBoxsOneAnswer(t *testing.T) {
	stale := strings.Replace(bound, "  <KnownProxies />",
		"  <KnownProxies>\n    <string>10.244.9.9/32</string>\n  </KnownProxies>", 1)
	out, changed, err := Ensure([]byte(stale), []string{"10.244.0.0/16"})
	if err != nil || !changed {
		t.Fatalf("ensure: changed=%v err=%v", changed, err)
	}
	got := string(out)
	if strings.Contains(got, "127.0.0.1") || strings.Contains(got, "10.244.9.9/32") {
		t.Fatalf("a value the box does not serve survived:\n%s", got)
	}
	if !strings.Contains(got, "<LocalNetworkAddresses />") ||
		!strings.Contains(got, "<KnownProxies>\n    <string>10.244.0.0/16</string>\n  </KnownProxies>") {
		t.Fatalf("the two elements are not the box's answer:\n%s", got)
	}
}

// TestTheProxiesLandInTheAppsOwnFile: the list is what the app reads at startup,
// so it is written into the app's own file, and every other setting in it is
// left byte for byte.
func TestTheProxiesLandInTheAppsOwnFile(t *testing.T) {
	out, changed, err := Ensure([]byte(live), []string{"10.244.0.0/16", "10.96.0.5/32"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !changed {
		t.Fatal("a file with no element was reported unchanged")
	}
	got := string(out)
	if !strings.Contains(got, "<KnownProxies>\n    <string>10.244.0.0/16</string>\n    <string>10.96.0.5/32</string>\n  </KnownProxies>") {
		t.Fatalf("the element is not the app's own shape:\n%s", got)
	}
	for _, keep := range []string{"<InternalHttpPort>8096</InternalHttpPort>", "<string>veth</string>", "<EnableRemoteAccess>true</EnableRemoteAccess>"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("the rewrite lost %s:\n%s", keep, got)
		}
	}
	// A second start writes nothing: the element already says it.
	if _, changed, err := Ensure(out, []string{"10.244.0.0/16", "10.96.0.5/32"}); err != nil || changed {
		t.Fatalf("second pass: changed=%v err=%v", changed, err)
	}
}

// TestAnEmptyListIsTheAppsOwnEmptyElement: a box that names no proxy gets the
// empty element the app writes itself, not a list nobody meant.
func TestAnEmptyListIsTheAppsOwnEmptyElement(t *testing.T) {
	out, changed, err := Ensure([]byte(live), nil)
	if err != nil || changed {
		t.Fatalf("an empty list over an empty element: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "<KnownProxies />") ||
		!strings.Contains(string(out), "<LocalNetworkAddresses />") {
		t.Fatalf("an element the box owns but did not change was rewritten:\n%s", out)
	}
}

// TestAnExistingListIsReplaced: the value a previous run wrote is replaced, and
// the list the app saved back (it writes the file whole) is too.
func TestAnExistingListIsReplaced(t *testing.T) {
	written, _, err := Ensure([]byte(live), []string{"10.244.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := Ensure(written, []string{"10.244.1.0/24"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !changed {
		t.Fatal("a different list was reported unchanged")
	}
	if strings.Contains(string(out), "10.244.0.0/16") || !strings.Contains(string(out), "10.244.1.0/24") {
		t.Fatalf("the list was not replaced:\n%s", out)
	}
}

// TestTheFirstStartMakesADocument: before the app has ever run there is no
// network.xml, and the init container is what writes the first one. Jellyfin
// deserialises what it finds over its defaults, so an element-only document is
// the app's own configuration with the two values this box owns set: all
// interfaces for the bind, and the proxy list it was given.
func TestTheFirstStartMakesADocument(t *testing.T) {
	out, changed, err := Ensure(nil, []string{"10.244.0.0/16"})
	if err != nil || !changed {
		t.Fatalf("a first start: changed=%v err=%v", changed, err)
	}
	got := string(out)
	if !strings.HasPrefix(got, `<?xml`) || !strings.Contains(got, "<NetworkConfiguration>") ||
		!strings.Contains(got, "<string>10.244.0.0/16</string>") {
		t.Fatalf("the first document is not one the app can read:\n%s", got)
	}
	if !strings.Contains(got, "<LocalNetworkAddresses />") {
		t.Fatalf("the first document binds an interface the app never wrote:\n%s", got)
	}
}

// TestAFileWithoutTheElementGetsItWithTheOthers: a document that predates this
// setting keeps its settings and gains the element beside them.
func TestAFileWithoutTheElementGetsItWithTheOthers(t *testing.T) {
	old := strings.Replace(live, "  <KnownProxies />\n", "", 1)
	out, changed, err := Ensure([]byte(old), []string{"10.244.0.0/16"})
	if err != nil || !changed {
		t.Fatalf("ensure: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(string(out), "<string>10.244.0.0/16</string>") ||
		!strings.Contains(string(out), "<InternalHttpPort>8096</InternalHttpPort>") {
		t.Fatalf("the element was not added:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(out)), "</NetworkConfiguration>") {
		t.Fatalf("the document no longer ends at its root:\n%s", out)
	}
}

// TestSomethingElseIsRefused: a file that is not this document is a wrong path,
// not something to overwrite.
func TestSomethingElseIsRefused(t *testing.T) {
	if _, _, err := Ensure([]byte("<html>hello</html>"), []string{"10.0.0.1"}); err == nil {
		t.Fatal("a document that is not NetworkConfiguration was accepted")
	}
	if _, _, err := Ensure([]byte("<NetworkConfiguration><KnownProxies>"), []string{"10.0.0.1"}); err == nil {
		t.Fatal("an unclosed element was accepted")
	}
}
