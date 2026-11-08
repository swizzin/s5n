// Package tui is `box init`'s terminal UI: box's masthead and theme, drawn
// with huh forms, behind setup.Prompter.
//
// The flow above the interface decides every question, its default and its
// validation; this package only renders. That is what makes a second
// implementation cheap: setup.LinePrompt is the reference, this is the one a
// person on a normal terminal sees. docs/box.md, "Principles": dialogs are
// huh, behind box's own prompt interface.
//
// Nothing here runs when init was asked to be unattended, and nothing here
// runs without a terminal on both ends: huh needs one, and its accessible mode
// cannot read a secret (it silently returns an empty string), so a pipe gets
// setup.LinePrompt instead. See Decide.
package tui

// Mode is which implementation of setup.Prompter `box init` runs.
type Mode int

const (
	// ModePlain is setup.LinePrompt: one line per answer, no cursor movement,
	// no colour. It works on a serial console, through a pipe and with a
	// screen reader that only understands the transcript.
	ModePlain Mode = iota
	// ModeHuh is this package.
	ModeHuh
)

// Terminal is what cmd/box can see about the terminal box was started from.
// It is a plain struct so the choice below is a pure function a test can pin.
type Terminal struct {
	// Stdin and Stdout say whether each is a terminal, not merely a character
	// device: /dev/null is the latter and cannot answer a question.
	Stdin, Stdout bool
	// TERM is the terminal type. "dumb" means no cursor addressing, which is
	// exactly what huh's renderer needs.
	TERM string
	// NoColor says NO_COLOR was set to something. It does not select the
	// plain prompter: huh still draws the layout, without colour.
	NoColor bool
	// Plain says --plain was given: the operator asked for line prompts even
	// though the terminal could carry a form.
	Plain bool
}

// Plan is the UI that fits one terminal.
type Plan struct {
	Mode Mode
	// NoColor asks the huh UI for its unstyled theme. Only meaningful for
	// ModeHuh; setup.LinePrompt never emits colour anyway.
	NoColor bool
}

// Decide picks the prompt implementation from the terminal alone.
//
// huh needs a terminal on both stdin (to read keys) and stdout (to address the
// cursor), so anything else gets the line prompt. The one thing huh cannot do
// is read a secret without a terminal, so falling back rather than using
// huh's own accessible mode is deliberate: that mode returns an empty secret
// and box would write an empty claim token without ever saying so.
func Decide(t Terminal) Plan {
	if t.Plain || !t.Stdin || !t.Stdout || t.TERM == "dumb" {
		return Plan{Mode: ModePlain}
	}
	return Plan{Mode: ModeHuh, NoColor: t.NoColor}
}
