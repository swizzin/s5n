package peerbanhelper

import "testing"

// TestParseYAMLFollowsBlocks: keys are found by their block, not their name, so
// another block's sequence cannot shadow `client:`'s endpoint.
func TestParseYAMLFollowsBlocks(t *testing.T) {
	const doc = `language: default
nat64:
  enabled: true
  prefix:
    - "64:ff9b::/96" # well-known
    - "2001:db8::/32"
server:
  http: 8088
  address: 0.0.0.0 # bind every interface
  prefix: "http://127.0.0.1:8088"
client:
  main:
    type: qbittorrent
    name: "qBittorrent #1"
    endpoint: http://localhost:8080
`
	f, err := parseYAML([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		path  []string
		value string
	}{
		{[]string{"server", "http"}, "8088"},
		{[]string{"server", "address"}, "0.0.0.0"},
		{[]string{"server", "prefix"}, "http://127.0.0.1:8088"},
		{[]string{"client", "main", "type"}, "qbittorrent"},
		{[]string{"client", "main", "name"}, "qBittorrent #1"},
		{[]string{"client", "main", "endpoint"}, "http://localhost:8080"},
	} {
		if got, ok := f.get(want.path...); !ok || got.str != want.value {
			t.Errorf("get(%v) = %q, %v; want %q", want.path, got.str, ok, want.value)
		}
	}
	if got := f.str("nat64", "prefix"); got != "" {
		t.Errorf("a sequence's items were read as a value: %q", got)
	}
	if got := f.str("nat64", "enabled"); got != "true" {
		t.Errorf("nat64.enabled = %q", got)
	}
}
