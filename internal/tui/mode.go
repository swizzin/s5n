// Package tui is `box init`'s terminal UI: box's masthead and theme, drawn with huh
// forms behind setup.Prompter (docs/box.md, "Principles").
//
// The flow above decides every question, default and validation; this package
// only renders, which makes setup.LinePrompt a cheap second implementation. It
// runs only with a terminal on both ends: huh's accessible mode cannot read a
// secret (it silently returns an empty string), so a pipe gets LinePrompt. See
// Decide.
package tui

// Mode is which implementation of setup.Prompter `box init` runs.
type Mode int

const (
	// ModePlain is setup.LinePrompt: one line per answer, no cursor movement, no
	// colour; it works on a serial console, through a pipe, and with a screen
	// reader that only understands the transcript.
	ModePlain Mode = iota
	// ModeHuh is this package.
	ModeHuh
)

// Terminal is what cmd/box can see about the terminal box was started from: a
// plain struct, so the choice below is a pure function a test can pin.
type Terminal struct {
	// Stdin and Stdout say whether each is a terminal, not merely a character
	// device: /dev/null is the latter and cannot answer a question.
	Stdin, Stdout bool
	// TERM is the terminal type; "dumb" means no cursor addressing, which is what
	// huh's renderer needs.
	TERM string
	// NoColor says NO_COLOR was set. It does not select the plain prompter: huh
	// still draws the layout, without colour.
	NoColor bool
	// Plain says --plain was given: line prompts even though the terminal could
	// carry a form.
	Plain bool
}

// Plan is the UI that fits one terminal.
type Plan struct {
	Mode Mode
	// NoColor asks the huh UI for its unstyled theme; only meaningful for ModeHuh.
	NoColor bool
}

// Decide picks the prompt implementation from the terminal alone. huh needs a
// terminal on stdin (keys) and stdout (cursor), so anything else gets the line
// prompt. Falling back rather than using huh's accessible mode is deliberate: that
// mode returns an empty secret, and box would write an empty claim token silently.
func Decide(t Terminal) Plan {
	if t.Plain || !t.Stdin || !t.Stdout || t.TERM == "dumb" {
		return Plan{Mode: ModePlain}
	}
	return Plan{Mode: ModeHuh, NoColor: t.NoColor}
}
