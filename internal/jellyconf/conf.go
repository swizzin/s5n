// Package jellyconf writes the two settings Jellyfin has to be told before it
// starts: the interfaces it binds, and the peers whose X-Forwarded-For it
// honours.
//
// Both have to be before the start because Jellyfin reads them once, at startup:
// Jellyfin.Server/Extensions/WebHostBuilderExtensions.cs builds Kestrel's listen
// addresses from NetworkManager.GetAllBindInterfaces, and
// Jellyfin.Server/Extensions/ApiServiceCollectionExtensions.cs configures
// ASP.NET's ForwardedHeadersOptions from NetworkConfiguration inside
// AddJellyfinApi. A value written through the API afterwards has no effect until
// the process restarts (checked against a 12.4 server: the API accepts the write,
// and the recorded client address stays the proxy's until a restart). The chart
// therefore hands this to an init container, the way qBittorrent's and
// Tautulli's settings are seeded.
//
// # Why the bind address matters
//
// LocalNetworkAddresses is the list of interfaces Jellyfin binds Kestrel to, and
// empty means every interface it has (MediaBrowser.Common/Net/NetworkConfiguration.cs
// at v12.4: "the interface addresses which Jellyfin will bind to. If empty, all
// interfaces will be used"; NetworkManager.GetAllBindInterfaces is the code that
// does it). In a pod that is the only value that resolves to something reachable
// from outside the pod, and the NetworkPolicy -- not the bind address -- is the
// gate. This package always writes it empty, because a file left saying
// 127.0.0.1 is a server nothing outside its own pod can reach, and this container
// runs on every start so no such file survives one. The chart's startup-wizard
// is what sets the loopback value, for a server only it may reach; it runs first,
// so this container's answer is the last word.
//
// # Why the proxy list matters
//
// With KnownProxies empty, ConfigureForwardHeaders sets ForwardedHeaders.None:
// the app ignores the header completely. Every request through the Gateway then
// carries the Gateway proxy pod's address as the client, which is a private
// address on the pod network -- "my network" to Jellyfin's own IsInLocalNetwork
// (src/Jellyfin.Networking/Manager/NetworkManager.cs: private address space is
// the LAN by default). That is not a cosmetic difference: the endpoints marked
// LocalAccessOrRequiresElevation answer a local caller with no credential at
// all, so from the internet they would be open.
//
// The value an app can be given is a subnet (AddProxyAddresses tries
// IPAddress.TryParse and then TryParseToSubnet, and a subnet becomes a
// KnownIPNetworks entry), which is what makes this writable at all: the proxy
// is a pod whose address changes on every restart, and the range it lives in is
// the only stable name for it. The list is written even when the box names none,
// so a list this container wrote earlier cannot outlive the values that justify
// it.
//
// # The file
//
// network.xml is Jellyfin's own XmlSerializer output: a NetworkConfiguration
// root with one element per property, and a string array as repeated <string>
// children (the shape a live 12.4 server writes). This package edits the bytes
// of those two elements and nothing else, so every setting the app wrote --
// including what its own startup wizard saved -- survives, and a second start
// writes nothing at all.
package jellyconf

import (
	"fmt"
	"strings"
)

// indent is the file's own indentation, so an inserted block reads like the app
// wrote it, and itemIndent is one nesting step in from it.
const (
	indent     = "  "
	itemIndent = indent + indent
	// rootClose and rootOpen are where a block is inserted when the file has no
	// element yet.
	rootClose = "</NetworkConfiguration>"
	rootOpen  = "<NetworkConfiguration"
)

// element is one array-valued setting this package owns: the three spellings the
// app's serializer can leave in the file, and the element's name for messages.
type element struct {
	name string
	open string
	// empty is how the XmlSerializer writes an array with no items.
	empty string
	end   string
}

var (
	// bindAddresses is LocalNetworkAddresses, always written empty: every
	// interface, which inside a pod is the only reachable answer.
	bindAddresses = element{
		name:  "LocalNetworkAddresses",
		open:  "<LocalNetworkAddresses>",
		empty: "<LocalNetworkAddresses />",
		end:   "</LocalNetworkAddresses>",
	}
	// knownProxies is KnownProxies, written from the chart's list.
	knownProxies = element{
		name:  "KnownProxies",
		open:  "<KnownProxies>",
		empty: "<KnownProxies />",
		end:   "</KnownProxies>",
	}
)

// block renders the element with values, in the app's own shape: the empty
// element for no values, otherwise one <string> per line.
func (e element) block(values []string) string {
	if len(values) == 0 {
		return e.empty
	}
	var b strings.Builder
	b.WriteString(e.open)
	for _, v := range values {
		b.WriteString("\n" + itemIndent + "<string>" + escape(v) + "</string>")
	}
	b.WriteString("\n" + indent + e.end)
	return b.String()
}

// escape is the XML text escape the app's own serializer uses for the characters
// a subnet or address can contain (none of them, in practice, but a hand-edited
// list is not this package's to trust).
func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// ProxiesBlock renders the element for a list of addresses or subnets.
func ProxiesBlock(proxies []string) string {
	return knownProxies.block(proxies)
}

// set returns text with this element set to block, and whether that changed it.
// A file with no such element gets one beside the other settings, which the app
// deserialises as a fresh NetworkConfiguration with that one value stated.
func (e element) set(text, block string) (string, bool, error) {
	if i := strings.Index(text, e.open); i >= 0 {
		j := strings.Index(text[i:], e.end)
		if j < 0 {
			return "", false, fmt.Errorf("network.xml has %s with no %s", e.open, e.end)
		}
		end := i + j + len(e.end)
		if text[i:end] == block {
			return text, false, nil
		}
		return text[:i] + block + text[end:], true, nil
	}
	if i := strings.Index(text, e.empty); i >= 0 {
		if block == e.empty {
			return text, false, nil
		}
		return text[:i] + block + text[i+len(e.empty):], true, nil
	}
	at := strings.LastIndex(text, rootClose)
	if at < 0 || !strings.Contains(text, rootOpen) {
		return "", false, fmt.Errorf("network.xml is not a NetworkConfiguration document")
	}
	return text[:at] + indent + block + "\n" + text[at:], true, nil
}

// Ensure returns the file's new contents with LocalNetworkAddresses empty and
// KnownProxies set to proxies, and whether anything changed.
//
// An absent file (a first start) becomes a document with just these two
// elements: Jellyfin deserialises the whole document into a fresh
// configuration, so an element it does not find keeps its default, and the
// document is the one the app itself writes on its first save of these settings.
func Ensure(existing []byte, proxies []string) ([]byte, bool, error) {
	if len(existing) == 0 {
		doc := "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n" +
			"<NetworkConfiguration>\n" +
			indent + bindAddresses.block(nil) + "\n" +
			indent + knownProxies.block(proxies) + "\n" +
			"</NetworkConfiguration>\n"
		return []byte(doc), true, nil
	}
	text := string(existing)
	changed := false
	for _, step := range []struct {
		e     element
		block string
	}{
		{bindAddresses, bindAddresses.block(nil)},
		{knownProxies, knownProxies.block(proxies)},
	} {
		out, c, err := step.e.set(text, step.block)
		if err != nil {
			return nil, false, err
		}
		text, changed = out, changed || c
	}
	if !changed {
		return existing, false, nil
	}
	return []byte(text), true, nil
}
