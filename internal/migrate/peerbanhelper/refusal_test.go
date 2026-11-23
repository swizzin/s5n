package peerbanhelper

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// A downloader reached through a public name is stored as an https URL with no
// port. Its port is the scheme's, 443: the refusal should name that port, and a
// rule narrowed to it should settle it.
func TestHTTPSEndpointWithoutAPortIsPort443(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	path := in.Dir + "/" + configRel
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "http://192.0.2.10:8081", "https://qbt.example.org/qbittorrent", 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 || rep.Unclassified[0].Port != 443 {
		t.Errorf("Unclassified = %v, want qbt.example.org at port 443", rep.Unclassified)
	}

	in.Plan.Hosts = append(in.Plan.Hosts, plan.HostRule{Host: "qbt.example.org", Port: 443, App: "qbittorrent"})
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Errorf("a rule for qbt.example.org port 443 did not settle https://qbt.example.org/qbittorrent: %v", rep.Unclassified)
	}
}
