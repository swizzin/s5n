package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/runlog"
)

// runLog is this command's transcript, opened once as its output is built. It
// is nil for the commands that print one line and leave (`box version`).
var runLog *runlog.Log

// openRunLog starts the run's log. A log that cannot be written is one muted
// line and nothing else: the command's work is not the log's, and a box on a
// host with a read-only /var/log still has to update.
func openRunLog(command string) *runlog.Log {
	lg, err := runlog.Open(commandName(command))
	if err != nil {
		fmt.Fprintf(os.Stderr, "· %v\n", err)
	}
	return lg
}

// logPath is where this run's transcript is, or "" when there is none.
func logPath(l *runlog.Log) string {
	if l == nil {
		return ""
	}
	return l.Path()
}

// redact names a value the run log must never carry. Every secret box handles --
// a generated password, a notice address with its token -- is registered before
// the line that might print it.
func redact(secret string) {
	if runLog == nil || strings.TrimSpace(secret) == "" {
		return
	}
	runLog.Register(secret)
}

// commandName is the command as the log names it: `box update`, `box migrate
// discover`, and the command alone for anything box does not know. Only letters,
// digits, dots, dashes and underscores survive: the name becomes a file name,
// and a name that walks up the directory is not one box wrote.
func commandName(command string) string {
	var b strings.Builder
	for _, r := range command {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "box"
	}
	return b.String()
}

// logName is the command this process is running, for the log's file name.
func logName() string {
	if len(os.Args) < 2 {
		return "box"
	}
	name := os.Args[1]
	if name == "migrate" && len(os.Args) > 2 && !strings.HasPrefix(os.Args[2], "-") {
		return "migrate-" + os.Args[2]
	}
	return name
}
