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
	"github.com/swizzin/s5n/internal/setup"
)

// sgr matches the escape sequences a renderer writes. It is not named ansi:
// the package of that name is imported here through paint.go.
var sgr = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// strip turns what the live renderer wrote into the lines a terminal would
// show, without colour or cursor movement.
func strip(s string) []string {
	s = strings.ReplaceAll(s, "\r", "\n")
	s = sgr.ReplaceAllString(s, "")
	out := strings.Split(s, "\n")
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
	if finish <= card || claim <= finish {
		t.Fatalf("pending rows are not after the card (card %d, heading %d, row %d):\n%s", card, finish, claim, strings.Join(lines, "\n"))
	}
	for _, want := range []string{"  ✓ wiring ok", "  ✓ qbittorrent/login  signed in", "  ✓ sonarr/downloadclient  created"} {
		if indexOf(lines, want) < 0 {
			t.Fatalf("receipt is missing %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// Every check mark is one colour: the accent of the theme in force, wherever it
// is drawn. A ✓ in the plan, a finished step, the chart and the card are the
// same glyph and the same escape. The escape is not written down here: the
// palette is the theme's, and this is about the reporter using one of it.
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
	// The escape accentStyle writes in front of its text, so the check
	// survives any palette: what must not vary is the reporter's choice.
	painted := accentStyle.Render("✓")
	accent := painted[:strings.Index(painted, "✓")]
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
		{
			Mark: say.Install,
			Text: "Envoy Gateway 1.9.3, the front door every app is reached through",
			Children: []say.Item{
				{Mark: say.Install, Text: "the s5n gateway, answering HTTP on 80"},
				{Mark: say.Question, Text: "Gateway s5n-gateway/s5n getting an address"},
			},
		},
		{
			Mark: say.Question,
			Text: "MetalLB 0.16.1, which gives the gateway this node's address, unless the Gateway gets an address without it",
			Children: []say.Item{
				{Mark: say.Question, Text: "an address pool holding only this node's IP, announced on the local network"},
			},
		},
		{Mark: say.Warning, Text: "Plex is not installed by this version of box; configure it by hand and run bootstrap again"},
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

// A plan's sub-steps are drawn under the item they configure, two columns past
// its text, with their own marks and their own wrapping: a child that runs past
// the terminal keeps starting at its indent, never at column 0.
func TestReporterPlanIndentsChildren(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 80, NoColor: true, Yes: true})
	r.Plan("platform plan", []say.Item{
		{
			Mark: say.Install,
			Text: "Envoy Gateway 1.9.3, the front door every app is reached through",
			Children: []say.Item{
				{Mark: say.Install, Text: "the s5n gateway, answering HTTP on 80"},
				{Mark: say.Question, Text: "an address pool holding only this node's IP, announced on the local network"},
			},
		},
	})
	r.Close()

	lines := strip(buf.String())
	if got := indexOf(lines, "  + Envoy Gateway 1.9.3, the front door every app is reached through"); got < 0 {
		t.Fatalf("the item is not at box's two columns:\n%s", strings.Join(lines, "\n"))
	}
	parent := indexOf(lines, "  + Envoy Gateway 1.9.3, the front door every app is reached through")
	if indexOf(lines, "      + the s5n gateway, answering HTTP on 80") < parent {
		t.Fatalf("the first child is not under its item:\n%s", strings.Join(lines, "\n"))
	}
	// The long child wraps, and its continuation keeps the child's indent.
	var wrapped []string
	for _, line := range lines[parent+1:] {
		if strings.HasPrefix(line, "      ") {
			wrapped = append(wrapped, line)
		}
	}
	if len(wrapped) < 3 {
		t.Fatalf("the two children and their wrapped line are not all indented under the item:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(wrapped[0], "      + ") || !strings.HasPrefix(wrapped[1], "      ? ") {
		t.Fatalf("a child lost its mark or its indent: %q", wrapped)
	}
	if !strings.HasPrefix(wrapped[2], "        ") || strings.TrimSpace(wrapped[2]) != "network" {
		t.Fatalf("the wrapped child line reads %q:\n%s", wrapped[2], strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("line is %d columns wide: %q", w, line)
		}
	}
}

// A masthead box opens with exactly one empty line, and never two: the frames a
// person saw sat directly under the line above them, and a section's own box is
// the same box the setup questions draw, so the gap is counted once for both.
func TestEveryMastheadOpensWithOneBlankLine(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 100, NoColor: true, Yes: true})
	r.Phase("cluster")
	r.Success("clean host: k0s v1.37.2+k0s.1 installed")
	r.Phase("platform")
	r.Plan("platform plan", []say.Item{{Mark: say.Install, Text: "MetalLB, which gives the gateway this node's address"}})
	// Two sections in a row: the second must not stack a second gap.
	r.Phase("apply")
	// A finished step, then init's first question: the question's masthead is
	// the same box as a section's, one line under the step until it opens with
	// its own gap.
	r.Chart("s5n", "0.5.0", "oci://ghcr.io/swizzin/charts/s5n:0.5.0")
	r.ui.accessible = true
	r.ui.in = strings.NewReader("box.example.com\n")
	r.ui.Step(1, 4, "what domain will this box answer on?")
	if _, err := r.ui.Input(setup.Input{Key: "gateway.host", Prompt: "domain"}); err != nil {
		t.Fatal(err)
	}
	r.Close()

	lines := strip(buf.String())
	var heads []int
	for i, line := range lines {
		if strings.HasPrefix(line, "╭") {
			heads = append(heads, i)
		}
	}
	if len(heads) != 4 {
		t.Fatalf("%d mastheads, want 4 (cluster, platform, apply, the setup question):\n%s", len(heads), strings.Join(lines, "\n"))
	}
	if heads[0] != 0 {
		t.Fatalf("the first masthead is printed under %q", lines[0])
	}
	for _, at := range heads[1:] {
		if lines[at-1] != "" {
			t.Fatalf("the masthead at line %d has no empty line above it: %q\n%s", at, lines[at-1], strings.Join(lines, "\n"))
		}
		if at >= 2 && lines[at-2] == "" {
			t.Fatalf("the masthead at line %d has two empty lines above it\n%s", at, strings.Join(lines, "\n"))
		}
	}
}

// The live reporter draws a table as the transcript does, plus colour: a titled
// block with the glyph, the padded name and the facts wrapped under themselves.
func TestReporterTable(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 60, NoColor: true, Yes: true})
	r.Table("2 apps", []say.TableRow{
		{Glyph: "✓", Name: "sonarr", Facts: []string{"5.2 MiB", "1,562 paths"}},
		{Glyph: "✓", Name: "qbittorrent", Facts: []string{"571.0 MiB"}},
	})
	r.Close()

	lines := strip(buf.String())
	for _, want := range []string{"2 apps", "  ✓ sonarr       5.2 MiB · 1,562 paths", "  ✓ qbittorrent  571.0 MiB"} {
		if indexOf(lines, want) < 0 {
			t.Fatalf("table is missing %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// A long fact wraps under the facts column and not under the name, so the name
// column stays a column.
func TestReporterTableWrapsUnderTheFacts(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 40, NoColor: true, Yes: true})
	r.Table("", []say.TableRow{{Glyph: "!", Name: "sonarr, radarr", Facts: []string{strings.Repeat("word ", 12)}}})
	r.Close()

	lines := strip(buf.String())
	first := indexPrefix(lines, "  ! sonarr, radarr  word")
	if first < 0 {
		t.Fatalf("the row did not start with its name and the first fact:\n%s", strings.Join(lines, "\n"))
	}
	wrapped := indexPrefix(lines, "                    word")
	if wrapped < 0 {
		t.Fatalf("the fact did not wrap under the facts column:\n%s", strings.Join(lines, "\n"))
	}
}

// A block with columns is the same table the transcript prints: the heading in
// the name column, each column heading over the facts it names, and the last
// column wrapped under itself at the terminal's width so an 80-column terminal
// still reads a row as one row.
func TestReporterGroupIsTheTranscriptsTable(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 80, NoColor: true, Yes: true})
	r.Group(say.Group{
		Title:   "apps",
		Columns: []string{"carries", "changes"},
		Rows: []say.TableRow{
			{Name: "autobrr", Facts: []string{"1.5 GiB", "2 addresses · login → s5n"}},
			{Name: "radarr", Facts: []string{"34.0 MiB", `2,930 paths · 1 address · keeps download client "qBT" · keeps its API key`}},
		},
	})
	r.Note("2 notes · 4 other workloads stay behind · 2 values left as they are · --details")
	r.Close()

	lines := strip(buf.String())
	for i, line := range lines {
		if len([]rune(line)) > 80 {
			t.Fatalf("line %d is wider than the terminal: %q", i, line)
		}
	}
	if got, want := lines[0], "  apps     carries   changes"; got != want {
		t.Fatalf("heading = %q, want %q", got, want)
	}
	if got, want := lines[1], "  · autobrr  1.5 GiB   2 addresses · login → s5n"; got != want {
		t.Fatalf("first row = %q, want %q", got, want)
	}
	// The last column wraps under the column it starts in, so the words line
	// up with the facts above them.
	if got := indexPrefix(lines, `  · radarr   34.0 MiB  2,930 paths · 1 address · keeps download client`); got < 0 {
		t.Fatalf("the long row is not in the table:\n%s", strings.Join(lines, "\n"))
	}
	if got := indexPrefix(lines, "                       keeps its API key"); got < 0 {
		t.Fatalf("the wrapped part does not hang under the facts column:\n%s", strings.Join(lines, "\n"))
	}
	if got := indexPrefix(lines, "  2 notes · 4 other workloads"); got < 0 {
		t.Fatalf("no note:\n%s", strings.Join(lines, "\n"))
	}
}

// The live table widens the same way the transcript does: a name longer than
// any other takes the block's column with it, and nothing is moved to a line of
// its own with its facts under it.
func TestReporterTableWidensForALongName(t *testing.T) {
	var buf bytes.Buffer
	r := NewReporter(ReporterOptions{Out: &buf, Width: 100, NoColor: true, Yes: true})
	r.Table("", []say.TableRow{
		{Glyph: "✓", Name: "box.example.test", Facts: []string{"https (Let's Encrypt)"}},
		{Glyph: "!", Name: "an-app-host-name-that-is-longer-than-the-column", Facts: []string{"http only"}},
	})
	r.Close()
	lines := strip(buf.String())
	if got, want := lines[0], "  ✓ box.example.test                                 https (Let's Encrypt)"; got != want {
		t.Fatalf("row 0 = %q, want %q", got, want)
	}
	if got, want := lines[1], "  ! an-app-host-name-that-is-longer-than-the-column  http only"; got != want {
		t.Fatalf("row 1 = %q, want %q", got, want)
	}
}
