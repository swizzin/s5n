package tui

import (
	"strings"
	"sync"
)

// space is the one empty line between the output above a masthead box and the box.
// A masthead opens with it -- a last line hugging "b o x s5n · platform" reads as
// crowded -- except when nothing is above it, and except when the output above
// already ended blank, since two empty lines read as a break in the run.
//
// One space is shared by both of box's writers, the reporter's phases and the setup
// questions' mastheads, so the gap between them is counted once. It tracks what the
// terminal was last told, so callers say what they printed rather than asking.
type space struct {
	mu      sync.Mutex
	started bool
	// blank is true when the line above the cursor is already empty.
	blank bool
}

// outset is the empty line a masthead opens with, or "" when it needs none.
func (s *space) outset() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.blank {
		return ""
	}
	return "\n"
}

// wrote records a block of output: box's blocks end with a newline, so one ending
// with two has already left its blank line behind.
func (s *space) wrote(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.blank = strings.HasSuffix(text, "\n\n")
}

// separator records the blank line a finished field leaves above the next question.
func (s *space) separator() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.blank = true
}

// cleared records an erased spinner: the cursor sits on an empty line, so the
// masthead still needs one above it.
func (s *space) cleared() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.blank = false
}
