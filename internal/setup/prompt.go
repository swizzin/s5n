package setup

import (
	"errors"
	"fmt"
	"io/fs"
)

// ErrCancelled says the operator stopped: Ctrl-D, "q", or the prompter's own
// gesture. Nothing has been written, and box prints "oh heck, nvm", not an error.
var ErrCancelled = errors.New("cancelled")

// Prompter is `box init`'s whole UI and the seam a terminal UI replaces without
// touching the flow: the order, defaults, validation and writes all live above it,
// so a TUI only renders. LinePrompt is the reference; another implementation must
// keep the same contract, returning ErrCancelled for its cancel gesture and never
// echoing a Secret.
type Prompter interface {
	// Input asks for one line of text. Validate is the flow's rule: a prompter
	// shows its error and asks again rather than deciding.
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

// Stepper is an optional extension: a prompter that shows progress hears which
// piece of the flow the next answer belongs to, before it asks. Nothing is decided
// by it.
type Stepper interface {
	// Step is the step the next field belongs to, out of this run's total, and its
	// question as a line of chrome. The field still carries its own label.
	Step(n, total int, question string)
}

// PathChecker is an optional extension for the path report, the one thing init
// shows that is more than a line of text: a prompter that can style it gets the
// structure instead of Note(check.Text()).
type PathChecker interface {
	ShowPathCheck(check PathCheck) error
}

// Input is one line of free text. Key is the values path it answers, so an error
// names which answer is wrong.
type Input struct {
	Key      string
	Prompt   string
	Help     string
	Default  string
	Validate func(string) error
	// Record is a note a prompter with a transcript shows beside the answer, in
	// parentheses ("web login"), so the line says which of two accounts it is.
	// LinePrompt has no record to annotate and ignores it.
	Record string
}

// Secret is one line that must not be echoed. Optional allows an empty answer;
// without it, an empty line is asked again.
type Secret struct {
	Key      string
	Prompt   string
	Help     string
	Optional bool
}

// Confirm is one yes/no question. Default is what an empty line takes, and the
// button a terminal UI focuses first.
type Confirm struct {
	Prompt  string
	Default bool
	// Affirmative and Negative label the two buttons; empty is the prompter's own
	// "yes" and "no". The line prompt always prints y/n.
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

// MultiSelect is any number of choices: Selected is what a re-run shows preselected,
// Min the smallest acceptable answer (0 means any).
type MultiSelect struct {
	Key      string
	Prompt   string
	Options  []Option
	Selected []string
	Min      int
}

// PathHost is the host filesystem as init sees it: the probes the path report is
// built from and the two writes it may make. An interface, so the flow runs against
// a fixture and no test touches a real host path.
type PathHost interface {
	// Stat reports one path. A missing path is fs.ErrNotExist, not a failure: init's
	// job is to notice it.
	Stat(path string) (FileInfo, error)
	// Lstat reports one path without following a symlink: box never reads through a
	// link when it walks a seedbox's files (docs/box.md), so the target's owner is
	// never taken for the box's.
	Lstat(path string) (FileInfo, error)
	// StatFS reports the free space of the filesystem holding path.
	StatFS(path string) (FSInfo, error)
	// MountInfo lists the host's mounts, the way /proc/self/mountinfo does.
	MountInfo() ([]Mount, error)
	// ReadDir lists at most max entries, plus whether there are more: the cap keeps
	// the "what is already there" summary cheap on a tree with a million files.
	ReadDir(path string, max int) ([]DirEntry, bool, error)
	// MkdirAll creates a directory and its parents.
	MkdirAll(path string, perm fs.FileMode) error
	// Lchown sets a path's owner without following a symlink: a link is owned
	// itself, so root's chown never lands on what a link points to.
	Lchown(path string, uid, gid int) error
}

// FileInfo is what init needs from a stat: directory, link, owner.
type FileInfo struct {
	IsDir bool
	// Symlink is set by Lstat for a path that is itself a link; Stat never sets it.
	Symlink bool
	Mode    fs.FileMode
	UID     int
	GID     int
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

// MajorMinor is the device in the one column a path row has for it: the kernel's
// own major:minor, since a device node's name is not in the table.
func (m Mount) MajorMinor() string {
	if m.Point == "" {
		return "-"
	}
	return fmt.Sprintf("%d:%d", m.Major, m.Minor)
}

// DirEntry is one name in a directory, with the size a file adds to the summary.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}
