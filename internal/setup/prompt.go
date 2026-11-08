package setup

import (
	"errors"
	"io/fs"
)

// ErrCancelled says the operator stopped: Ctrl-D, "q", or whatever cancel
// gesture the prompter offers. Nothing has been written when it is returned, and
// box prints the voice -- "oh heck, nvm" -- rather than an error.
var ErrCancelled = errors.New("cancelled")

// Prompter is `box init`'s whole UI, and the seam a terminal UI can replace
// without touching the flow.
//
// Everything init asks goes through these six methods. The order of the
// questions, their defaults and validation, and what is written where all live
// above this interface, so a TUI only has to render: Input, Secret, Confirm,
// Select and MultiSelect collect one answer each, and Note shows a block of text
// (the path report) without asking anything. The line prompt in this package is
// the reference implementation; a TUI must keep the same contract, including
// returning ErrCancelled for its cancel gesture and never echoing a Secret.
type Prompter interface {
	// Input asks for one line of text. Validate is the flow's rule, so a
	// prompter shows its error and asks again rather than deciding.
	Input(f Input) (string, error)
	// Secret asks for a value that must not appear on screen. Optional means an
	// empty answer is fine.
	Secret(f Secret) (string, error)
	// Confirm asks yes or no; Default is the answer to an empty line.
	Confirm(f Confirm) (bool, error)
	// Select picks exactly one option, by value.
	Select(f Select) (string, error)
	// MultiSelect picks any number of options, starting from Selected.
	MultiSelect(f MultiSelect) ([]string, error)
	// Note shows a block of text: what init found, not a question.
	Note(text string) error
}

// Stepper is an optional extension of Prompter. A prompter that shows progress
// implements it and init tells it which piece of the flow the next answer
// belongs to, before asking. Nothing is decided by it: a prompter that does not
// implement it (LinePrompt does not) simply never hears the steps.
type Stepper interface {
	// Step is the step the next field belongs to, out of however many this run
	// will show, and the step's own question as a line of chrome. The field
	// still carries its own label.
	Step(n, total int, question string)
}

// PathChecker is an optional extension of Prompter. The path report is the one
// thing init shows that is more than a line of text, and a prompter that can
// style it is handed the structure instead: a row per fact, and a warning the
// prompter can colour. LinePrompt does not implement it and keeps receiving
// Note(check.Text()), which is exactly the text it printed before the seam
// existed.
type PathChecker interface {
	ShowPathCheck(check PathCheck) error
}

// Input is one line of free text. Key is the values path it answers: it is what
// an error names, so the operator knows which answer is wrong.
type Input struct {
	Key      string
	Prompt   string
	Help     string
	Default  string
	Validate func(string) error
}

// Secret is one line that must not be echoed. Optional allows an empty answer;
// without it, an empty line is asked again.
type Secret struct {
	Key      string
	Prompt   string
	Help     string
	Optional bool
}

// Confirm is one yes/no question. Default is the answer an empty line takes,
// and the button a terminal UI focuses first.
type Confirm struct {
	Prompt  string
	Default bool
	// Affirmative and Negative label the two buttons for a prompter that draws
	// them. Empty means the prompter's own "yes" and "no"; the line prompt
	// always prints y/n.
	Affirmative string
	Negative    string
}

// Option is one entry of a Select or MultiSelect.
type Option struct {
	Value string
	Label string
	Help  string
}

// Select is one choice out of Options; Default is the value an empty answer
// takes.
type Select struct {
	Key     string
	Prompt  string
	Options []Option
	Default string
}

// MultiSelect is any number of choices, with Selected preselected (which is
// what a re-run of init shows) and Min the smallest acceptable answer (0 means
// any).
type MultiSelect struct {
	Key      string
	Prompt   string
	Options  []Option
	Selected []string
	Min      int
}

// PathHost is the host filesystem as init sees it: the probes the path report
// is built from, and the two writes it may make. It is an interface so the whole
// flow runs against a fixture, and so no test can touch a real host path.
type PathHost interface {
	// Stat reports one path. A missing path is fs.ErrNotExist, not an error
	// worth failing on: init's job is to notice it.
	Stat(path string) (FileInfo, error)
	// StatFS reports the free space of the filesystem holding path.
	StatFS(path string) (FSInfo, error)
	// MountInfo lists the host's mounts, the way /proc/self/mountinfo does.
	MountInfo() ([]Mount, error)
	// ReadDir lists at most max entries of a directory, plus whether there are
	// more. The cap is what keeps the "what is already there" summary cheap on
	// a tree with a million files.
	ReadDir(path string, max int) ([]DirEntry, bool, error)
	// MkdirAll creates a directory and its parents.
	MkdirAll(path string, perm fs.FileMode) error
	// Chown sets a directory's owner.
	Chown(path string, uid, gid int) error
}

// FileInfo is what init needs from a stat: enough to say whether the path is a
// directory and who owns it.
type FileInfo struct {
	IsDir bool
	Mode  fs.FileMode
	UID   int
	GID   int
}

// FSInfo is the free space of one filesystem.
type FSInfo struct {
	Free  int64
	Total int64
}

// Mount is one entry of /proc/self/mountinfo, reduced to what the report says.
type Mount struct {
	Major, Minor int
	Point        string
	FSType       string
	Source       string
}

// DirEntry is one name in a directory, with the size a regular file contributes
// to the summary.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}
