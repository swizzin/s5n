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

// The wiring receipt is drawn row by row, and a row that waits on a person is
// not buried in the middle of it: it comes after the card, under its own
// heading, because it is the next thing to do.
func TestReporterReceiptPutsPendingAfterTheCard(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 100, NoColor: true, Yes: true})
	r.Phase("apply")
	r.Receipt("ok", []say.ReceiptRow{
		{Name: "qbittorrent/login", Result: "ok", Detail: "signed in"},
		{Name: "sonarr/downloadclient", Result: "ok", Detail: "created"},
		{Name: "plex/claim", Result: "pending", Detail: "no claim token"},
		{Name: "tautulli/plex", Result: "pending", Detail: "plex is not configured yet"},
	})
	r.Banner("seedbox ready", "box.example.com")
	r.Close()

	lines := strip(buf.String())
	card := indexPrefix(lines, "│ ✓ seedbox ready")
	finish := indexOf(lines, "  to finish")
	claim := indexOf(lines, "  ◌ plex/claim  no claim token")
	if card < 0 || finish < 0 || claim < 0 {
		t.Fatalf("missing a receipt line, a card or the to-finish list:\n%s", strings.Join(lines, "\n"))
	}
	if !(finish > card && claim > finish) {
		t.Fatalf("pending rows are not after the card (card %d, heading %d, row %d):\n%s", card, finish, claim, strings.Join(lines, "\n"))
	}
	for _, want := range []string{"  ✓ wiring ok", "  ✓ qbittorrent/login  signed in", "  ✓ sonarr/downloadclient  created"} {
		if indexOf(lines, want) < 0 {
			t.Fatalf("receipt is missing %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// Every check mark is one colour: the accent, wherever it is drawn. A ✓ in the
// plan, a finished step, the chart and the card are the same glyph and the
// same escape.
func TestReporterUsesOneCheckColour(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 100, Yes: true})
	r.Phase("apply")
	r.Success("created namespace seedbox")
	h := r.Start(say.Op{Label: "installing s5n"})
	h.Done("applied: s5n revision 1")
	r.Receipt("ok", []say.ReceiptRow{{Name: "wiring", Result: "ok"}})
	r.Banner("seedbox ready", "box.example.com")
	r.Close()

	out := buf.String()
	accent := "38;2;242;166;90"
	total := 0
	for i := 0; i < len(out); i++ {
		if !strings.HasPrefix(out[i:], "✓") {
			continue
		}
		total++
		start := strings.LastIndex(out[:i], "\x1b[")
		if start < 0 || !strings.Contains(out[start:i], accent) {
			t.Fatalf("check mark %d is not accent-coloured (escape %q):\n%q", total, out[start:i], out)
		}
	}
	if total < 5 {
		t.Fatalf("expected check marks from every event, found %d:\n%q", total, out)
	}
}

func indexPrefix(lines []string, want string) int {
	for i, line := range lines {
		if strings.HasPrefix(line, want) {
			return i
		}
	}
	return -1
}

func indexOf(lines []string, want string) int {
	for i, line := range lines {
		if line == want {
			return i
		}
	}
	return -1
}

// One block: nothing the reporter prints sits at column 0 under a phase frame.
// Only the frames themselves (the masthead, the card, the error box) are flush
// left; every fact, plan, step, list, warning, receipt row and continuation
// starts at box's two columns, the same place huh's fields do.
func TestReporterNeverWritesAtColumnZero(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 96, NoColor: true, Yes: true})
	r.Phase("apply")
	r.Fact("kubeconfig", "/home/example/.kube/config")
	r.List("platform", []string{"gateway-api: auto -> install (no Gateway API CRDs)"})
	r.Plan("plan", []say.Item{{Mark: say.Install, Text: "Namespace seedbox (Pod Security restricted)"}})
	r.Diff([]say.Item{{Mark: say.Change, Text: "gateway.host: old.example.com -> box.example.com"}})
	r.Line("s5n in seedbox: first install")
	r.Muted("clean host: k0s v1.37.2+k0s.1 installed; using /var/lib/k0s/pki/admin.conf (k0s)")
	r.Warn("namespace seedbox does not enforce Pod Security restricted (no label): root's labels win, box leaves it alone")
	r.Success("created namespace seedbox (Pod Security restricted)")
	r.Chart("s5n", "0.2.0", "oci://ghcr.io/swizzin/charts/s5n:0.2.0")
	r.Created([]string{"/srv/s5n/data", "/srv/s5n/config", "/srv/s5n/config/radarr-4k"})
	h := r.Start(say.Op{Label: "installing s5n in seedbox"})
	h.Detail("this run created it")
	h.Done("applied: s5n revision 1 in seedbox")
	r.Receipt("ok", []say.ReceiptRow{{Name: "qbittorrent/login", Result: "ok", Detail: "signed in"}, {Name: "plex/claim", Result: "pending", Detail: "no claim token"}})
	r.Banner("seedbox ready", "box.example.com")
	r.Close()

	for _, line := range strip(buf.String()) {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "╭") || strings.HasPrefix(line, "│") || strings.HasPrefix(line, "╰") {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("line starts at column 0: %q\n%s", line, buf.String())
		}
	}
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
