package say

import (
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
		want bool
	}{
		{"yes", "y\n", true},
		{"yes spelled out", "yes\n", true},
		{"yes in capitals", "Yes\n", true},
		{"surrounding whitespace", "  y  \n", true},
		{"no", "n\n", false},
		{"no spelled out", "no\n", false},
		{"an empty line takes the default, which is no", "\n", false},
		{"anything else is no", "sure, why not\n", false},
		{"a whole line without its newline still answers", "y", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plain{In: strings.NewReader(tc.in)}
			got, err := p.Confirm("apply s5n in the seedbox namespace?")
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
	got, err := p.Confirm("install the platform layer?")
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
	if _, err := p.Confirm("apply s5n?"); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// With no reader there is no terminal to ask on, and that is the documented
// refusal rather than an implicit yes: box writes to a cluster as root.
func TestPlainConfirmRefusesWithoutATerminal(t *testing.T) {
	p := &Plain{}
	_, err := p.Confirm("apply s5n?")
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
	ok, err := p.Ask("apply s5n?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ok {
		t.Fatal("Ask answered no to a y")
	}
	if _, err := (&Plain{}).Ask("apply s5n?"); err == nil {
		t.Fatal("Ask without a terminal answered")
	}
}

// failingReader is stdin when nobody is typing: reading it fails loudly, so a
// prompt that reads anyway cannot pass silently.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read from a stdin nobody typed into")
}
