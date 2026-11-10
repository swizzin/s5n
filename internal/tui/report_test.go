package tui

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/swizzin/s5n/internal/say"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// strip turns what the live renderer wrote into the lines a terminal would
// show, without colour or cursor movement.
func strip(s string) []string {
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ansi.ReplaceAllString(s, "")
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, line)
	}
	return out
}

// Every line the live renderer leaves behind fits the terminal. The plain
// transcript has the same promise, and the 80-column terminal is the narrowest
// box claims to support.
func TestReporterNeverExceedsWidth(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 80, NoColor: true, Yes: true})

	r.Phase("cluster")
	r.Fact("kubeconfig", "/home/example/.kube/config")
	r.List("platform", []string{"gateway-api: auto -> install (no Gateway API CRDs)"})
	r.Plan("platform plan", []say.Item{
		{Mark: say.Install, Text: "envoy-gateway 1.9.3 (Helm, envoy-gateway-system)"},
		{Mark: say.Question, Text: "IPAddressPool s5n-pool (the node's InternalIP as a /32) and L2Advertisement s5n-l2, unless the Gateway gets an address without it"},
		{Mark: say.Warning, Text: "cert-manager: box does not install it yet; install cert-manager yourself, or set platform.cert-manager.mode=external, and run bootstrap again"},
	})
	h := r.Start(say.Op{Label: "waiting for the Gateway to report an address", Budget: 15 * time.Second})
	time.Sleep(120 * time.Millisecond)
	h.Done("")
	r.Created([]string{"/srv/s5n/data", "/srv/s5n/config", "/srv/s5n/config/qbittorrent", "/srv/s5n/config/sonarr", "/srv/s5n/config/radarr-4k", "/srv/s5n/config/prowlarr", "/srv/s5n/config/plex"})
	if _, err := r.Confirm("create /srv/s5n/data on that filesystem?"); err != nil {
		t.Fatal(err)
	}
	r.Banner("seedbox ready", "box.example.com")
	r.Error(errors.New("the Gateway has no address: its Service has no LoadBalancer address, and box has nothing left to install for it"))
	r.Close()

	for _, line := range strip(buf.String()) {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("line is %d columns wide:\n%q", w, line)
		}
	}
}

// The live renderer says the same things in box's voice: the section frame,
// the ✓ record lines, and the final card with the domain.
func TestReporterLooksLikeBox(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 100, NoColor: true, Yes: true})
	r.Phase("cluster")
	r.Success("platform ready")
	if _, err := r.Confirm("install the platform layer?"); err != nil {
		t.Fatal(err)
	}
	r.Banner("seedbox ready", "box.example.com")
	r.Close()

	got := buf.String()
	for _, want := range []string{"b o x", "s5n · cluster", "✓ platform ready", "✓ install the platform layer?", "seedbox ready", "box.example.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}
