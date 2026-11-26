package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/runlog"
)

// runLog is this command's transcript, nil for one-line commands.
var runLog *runlog.Log

// openRunLog: a log that cannot be written is one muted line, never a failure.
func openRunLog(command string) *runlog.Log {
	lg, err := runlog.Open(commandName(command))
	if err != nil {
		fmt.Fprintf(os.Stderr, "· %v\n", err)
	}
	return lg
}

// logPath is the transcript's path, or "".
func logPath(l *runlog.Log) string {
	if l == nil {
		return ""
	}
	return l.Path()
}

// redact registers a secret before any line that might print it.
func redact(secret string) {
	if runLog == nil || strings.TrimSpace(secret) == "" {
		return
	}
	runLog.Register(secret)
}

// boxName is the fallback log name.
const boxName = "box"

// commandName becomes a file name: only letters, digits, dots, dashes and
// underscores survive, so a name cannot walk up the directory.
func commandName(command string) string {
	var b strings.Builder
	for _, r := range command {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return boxName
	}
	return b.String()
}

// logName is the command this process is running, for the log's file name.
func logName() string { return logNameFor(os.Args) }

// logNameFor: a timer wake logs as runlog.TimerCommand, so the prune keeps only
// a few and never pushes out a person's run.
func logNameFor(args []string) string {
	if len(args) < 2 {
		return boxName
	}
	name := args[1]
	if name == "migrate" && len(args) > 2 && !strings.HasPrefix(args[2], "-") {
		return "migrate-" + args[2]
	}
	if name == "update" {
		for _, a := range args[2:] {
			if a == "--" {
				break
			}
			if a == "--timer" || a == "-timer" || a == "--timer=true" || a == "-timer=true" {
				return runlog.TimerCommand
			}
		}
	}
	return name
}
