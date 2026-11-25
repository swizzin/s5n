package say

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// Confirm is what release and platform ask their one question with on the plain
// path, so its four answers are the contract: yes, no, an answer that never
// came, and nobody to ask at all.
func TestPlainConfirmAnswers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		def  bool
		want bool
	}{
		{"yes", "y\n", false, true},
		{"yes spelled out", "yes\n", false, true},
		{"yes in capitals", "Yes\n", false, true},
		{"surrounding whitespace", "  y  \n", false, true},
		{"no", "n\n", false, false},
		{"no spelled out", "no\n", false, false},
		{"an empty line takes a no default", "\n", false, false},
		{"an empty line takes a yes default", "\n", true, true},
		{"an explicit no beats a yes default", "n\n", true, false},
		{"anything else is no", "sure, why not\n", false, false},
		{"a whole line without its newline still answers", "y", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plain{In: strings.NewReader(tc.in)}
			got, err := p.Confirm("apply s5n in the seedbox namespace?", tc.def)
			if err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if got != tc.want {
				t.Fatalf("answer = %t, want %t", got, tc.want)
			}
		})
	}
}

// --yes answers without prompting: an unattended run still records a yes, and
// nothing is read from a stdin nobody is typing into.
func TestPlainConfirmYesNeverReads(t *testing.T) {
	p := &Plain{Yes: true, In: failingReader{}}
	got, err := p.Confirm("install the platform layer?", false)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Fatal("--yes answered no")
	}
}

// An EOF with no answer is an error, not a no: box was waiting for a person who
// was not there, and saying so is different from being told no.
func TestPlainConfirmReportsEOF(t *testing.T) {
	p := &Plain{In: strings.NewReader("")}
	if _, err := p.Confirm("apply s5n?", false); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// With no reader there is no terminal to ask on, and that is the documented
// refusal rather than an implicit yes: box writes to a cluster as root.
func TestPlainConfirmRefusesWithoutATerminal(t *testing.T) {
	p := &Plain{}
	_, err := p.Confirm("apply s5n?", false)
	if err == nil {
		t.Fatal("a prompt with nobody to ask answered yes")
	}
	if got, want := err.Error(), "not a terminal: pass --yes to apply without a prompt"; got != want {
		t.Fatalf("err = %q, want %q", got, want)
	}
}

// Ask is the name release and platform call: the same question, the same
// answer.
func TestPlainAskIsConfirm(t *testing.T) {
	p := &Plain{In: strings.NewReader("y\n")}
	ok, err := p.Ask("apply s5n?", false)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ok {
		t.Fatal("Ask answered no to a y")
	}
	if _, err := (&Plain{}).Ask("apply s5n?", false); err == nil {
		t.Fatal("Ask without a terminal answered")
	}
}

// failingReader is stdin when nobody is typing: reading it fails loudly, so a
// prompt that reads anyway cannot pass silently.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read from a stdin nobody typed into")
}

// A plan item's children are printed under it, two columns past where its own
// text starts, with their own marks -- and with spaces only: the transcript is
// what a screen reader and a logged run read.
func TestPlainPlanIndentsChildren(t *testing.T) {
	var buf strings.Builder
	p := &Plain{W: &buf}
	p.Plan("platform plan", []Item{
		{
			Mark: Install,
			Text: "cert-manager v1.21.3, which issues the certificates",
			Children: []Item{
				{Mark: Install, Text: "a Let's Encrypt issuer, and a self-signed one"},
			},
		},
		{Mark: Warning, Text: "Plex is not installed by this version of box"},
	})
	want := "platform plan:\n" +
		"  + cert-manager v1.21.3, which issues the certificates\n" +
		"      + a Let's Encrypt issuer, and a self-signed one\n" +
		"  ! Plex is not installed by this version of box\n"
	if got := buf.String(); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
	for _, r := range buf.String() {
		if r != '\n' && (r == '│' || r == '├' || r == '└' || r == '─') {
			t.Fatalf("the plain plan drew a box character: %q", buf.String())
		}
	}
}

// Table is a block of rows in two columns: the glyph, the name, and the facts
// joined with a middle dot. The transcript is a golden test's and a log's, so
// nothing in it depends on a terminal's width.
func TestPlainTable(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Table("2 apps", []TableRow{
		{Glyph: "✓", Name: "sonarr", Facts: []string{"5.2 MiB", "1,562 paths"}},
		{Glyph: "✓", Name: "qbittorrent", Facts: []string{"571.0 MiB"}},
		{Glyph: "·", Name: "", Facts: []string{"a line with no name"}},
	})
	want := "2 apps\n" +
		"  ✓ sonarr       5.2 MiB · 1,562 paths\n" +
		"  ✓ qbittorrent  571.0 MiB\n" +
		"  · a line with no name\n"
	if got := buf.String(); got != want {
		t.Fatalf("table is:\n%s\nwant:\n%s", got, want)
	}
}

// A row with no glyph is a note, and a row with no facts is a name: neither is
// an empty line a reader has to guess at.
func TestPlainTableDefaultsTheGlyph(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Table("", []TableRow{{Name: "a note"}, {Name: "another", Facts: []string{"with a fact"}}})
	want := "  · a note\n  · another  with a fact\n"
	if got := buf.String(); got != want {
		t.Fatalf("table is:\n%s\nwant:\n%s", got, want)
	}
}

// An empty block prints nothing, not a heading over no rows.
func TestPlainTableWithNoRows(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Table("needs you", nil)
	if buf.Len() != 0 {
		t.Fatalf("an empty table printed %q", buf.String())
	}
}

// The gutter carries one mark per line, and a warning is `!`: it is something
// that needs a person, which is the rulebook's word for the mark. `⚠` is not
// one of the seven marks, so neither renderer prints it.
func TestPlainWarnNeedsThePerson(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Warn("namespace seedbox does not enforce Pod Security restricted")
	want := "! namespace seedbox does not enforce Pod Security restricted\n"
	if got := buf.String(); got != want {
		t.Fatalf("warn = %q, want %q", got, want)
	}
}

// A success line is a done line: the ✓ in the gutter, the same mark a named
// step ends with.
func TestPlainSuccessIsADoneRow(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Success("created namespace s5n-alice (Pod Security restricted)")
	want := "✓ created namespace s5n-alice (Pod Security restricted)\n"
	if got := buf.String(); got != want {
		t.Fatalf("success = %q, want %q", got, want)
	}
}

// A phase is a heading: the lowercase noun on a line of its own, with no colon
// and nothing after it. The live renderer draws the same name in its masthead.
func TestPlainPhaseIsALabel(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf}
	p.Phase("platform")
	p.Phase("")
	want := "platform\n"
	if got := buf.String(); got != want {
		t.Fatalf("phase = %q, want %q", got, want)
	}
}

// A question is a mark of its own: the `?` in the gutter, then the question as
// the flow asked it, then the answer line the terminal echoes after it. The
// mark is what makes a prompt one of box's lines rather than a bare sentence
// in the middle of a run (rule 1).
func TestPlainConfirmAsksWithTheQuestionMark(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf, In: strings.NewReader("y\n")}
	yes, err := p.Confirm("install the platform layer?", false)
	if err != nil || !yes {
		t.Fatalf("answer = %v, %v", yes, err)
	}
	if got, want := buf.String(), "? install the platform layer? [y/N] "; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// The default a flow chose is on the prompt itself: [Y/n] says a bare enter
// installs, [y/N] says it does not, and the same question asked both ways shows
// both.
func TestPlainConfirmShowsTheDefaultItWasGiven(t *testing.T) {
	var buf bytes.Buffer
	p := &Plain{W: &buf, In: strings.NewReader("\ny\n")}
	if yes, err := p.Confirm("install the platform and alice's seedbox?", true); err != nil || !yes {
		t.Fatalf("an empty line over a yes default answered %v, %v", yes, err)
	}
	if yes, err := p.Confirm("apply s5n in the seedbox namespace?", false); err != nil || !yes {
		t.Fatalf("an explicit y over a no default answered %v, %v", yes, err)
	}
	want := "? install the platform and alice's seedbox? [Y/n] ? apply s5n in the seedbox namespace? [y/N] "
	if got := buf.String(); got != want {
		t.Fatalf("prompts = %q, want %q", got, want)
	}
}
