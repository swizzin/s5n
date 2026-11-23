package tui

import (
	"strings"
	"sync"
)

// space is the one empty line between the output above a masthead box and the
// box itself.
//
// A masthead opens with it -- the previous step's last line hugging "b o x s5n
// · platform" reads as crowded -- except when there is nothing above it to be
// separated from (the first thing a run prints), and except when the output
// above already ended with a blank line, because two empty lines in a row read
// as a break in the run rather than as air.
//
// One space is shared by both of box's writers: the reporter's phases and the
// setup questions' mastheads are the same box in the same run, so the gap
// between them is counted once. It tracks what the terminal was last told, so
// callers say what they just printed rather than asking who printed it.
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

// wrote records a block of output: box's blocks end with a newline, so a block
// that ends with two of them has already left its blank line behind.
func (s *space) wrote(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.blank = strings.HasSuffix(text, "\n\n")
}

// separator records the blank line a finished field leaves above the next
// question.
func (s *space) separator() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.blank = true
}

// cleared records an erased spinner: the cursor sits on an empty line, so the
// masthead still needs its own line above to keep that gap.
func (s *space) cleared() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.blank = false
}
