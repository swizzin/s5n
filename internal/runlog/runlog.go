// Package runlog is box's run log: the plain transcript of one command.
//
// Every command writes one, because the screen is a summary and the log is the
// record: object names, revisions, digests, the reasoning behind a decision, and
// every line the screen showed, with a timestamp on each. A failed run names its
// log last, so the operator has somewhere to look that is not their scrollback.
//
// It is bounded, and it has to be: a box that checks every fifteen minutes
// would otherwise fill /var/log. A run prunes the directory first -- the newest
// KeepFiles logs and at most KeepBytes in total, oldest first -- and one run's
// file stops at MaxFile with a line saying so. Secrets are registered before
// they can be written and are replaced where they would have gone.
//
// Nothing here can fail a command. A directory that cannot be written, a full
// disk, a log that does not exist: each is one line at start or one silenced
// write, never an exit code.
package runlog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// KeepFiles is how many logs the directory keeps, this run's included.
	KeepFiles = 20
	// KeepBytes is the most the directory keeps, in total.
	KeepBytes = 64 << 20
	// MaxFile is where one run's log stops.
	MaxFile = 16 << 20
	// RootDir is where root's logs go.
	RootDir = "/var/log/box"
	// DirEnv names a directory in place of the default. It is box's own (the
	// e2e points it under its work directory) and is never set by box.
	DirEnv = "BOX_LOG_DIR"
	// Redacted is what a secret becomes.
	Redacted = "<redacted>"
)

// stamp is a log line's timestamp: UTC, second precision, sortable.
const stamp = "2006-01-02T15:04:05Z"

// fileStamp is the same instant in a file name.
const fileStamp = "20060102T150405Z"

// Log is one command's transcript. The zero value is a log that was never
// opened: it discards everything and Path is empty.
type Log struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	written int64
	full    bool
	partial string
	secrets []string
	now     func() time.Time
	// max is MaxFile, or what a test sets it to.
	max      int64
	disabled bool
}

// limit is where this log stops.
func (l *Log) limit() int64 {
	if l.max > 0 {
		return l.max
	}
	return MaxFile
}

// OpenAt starts a command's log in dir, pruning it first.
//
// A log that cannot be opened is still returned, disabled, with the reason: the
// caller says one line and the command runs as it would have. It never returns a
// nil Log, so no caller has to check.
func OpenAt(dir, command string) (*Log, error) {
	l := &Log{now: time.Now}
	_, statErr := os.Stat(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		l.disabled = true
		return l, fmt.Errorf("no run log: %s: %w", dir, err)
	}
	if os.IsNotExist(statErr) {
		// Only a directory box made is box's to set the mode of: an
		// operator's own directory keeps the mode they gave it.
		_ = os.Chmod(dir, 0o700)
	}
	prune(dir)
	name, f, err := create(dir, command, time.Now())
	if err != nil {
		l.disabled = true
		return l, err
	}
	l.path = filepath.Join(dir, name)
	l.f = f
	return l, nil
}

// create makes this run's log file. Two runs of one command in the same second
// are two runs -- box applies, checks and hands over faster than that -- so a
// taken name gets a suffix rather than costing one of them its transcript.
func create(dir, command string, now time.Time) (string, *os.File, error) {
	stamp := now.UTC().Format(fileStamp)
	for i := 1; i <= 99; i++ {
		name := command + "-" + stamp + ".log"
		if i > 1 {
			name = fmt.Sprintf("%s-%s-%d.log", command, stamp, i)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return name, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, fmt.Errorf("no run log: %s: %w", path, err)
		}
	}
	return "", nil, fmt.Errorf("no run log: %s: ninety-nine runs of %s in one second", dir, command)
}

// Open starts this run's log where box keeps them.
func Open(command string) (*Log, error) {
	dir, err := Dir()
	if err != nil {
		return &Log{now: time.Now, disabled: true}, err
	}
	return OpenAt(dir, command)
}

// Dir is where this run's log goes: DirEnv when it is set, root's own directory
// for root, and the user's state directory otherwise -- root's box writes to
// /var/log, and a user's box must not need it.
func Dir() (string, error) {
	return dirFor(os.Geteuid() == 0, os.Getenv, os.UserHomeDir)
}

// dirFor is Dir with the environment it reads handed in.
func dirFor(root bool, getenv func(string) string, home func() (string, error)) (string, error) {
	if d := strings.TrimSpace(getenv(DirEnv)); d != "" {
		return d, nil
	}
	if root {
		return RootDir, nil
	}
	base := strings.TrimSpace(getenv("XDG_STATE_HOME"))
	if base == "" {
		h, err := home()
		if err != nil {
			return "", fmt.Errorf("no run log: this user has no home directory: %w", err)
		}
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, "box"), nil
}

// Path is the file this run writes, or "" when there is no log.
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// Register names a value that must never reach the file. It is called for every
// secret box handles -- a password, a token, an API key -- before the line that
// might carry it.
func (l *Log) Register(secret string) {
	if l == nil || len(secret) < 3 {
		// A one- or two-character value would match half the transcript.
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, have := range l.secrets {
		if have == secret {
			return
		}
	}
	l.secrets = append(l.secrets, secret)
	sort.Slice(l.secrets, func(i, j int) bool { return len(l.secrets[i]) > len(l.secrets[j]) })
}

// Write is the io.Writer both renderers hand their finished lines to: every
// line gets the timestamp, the secrets are taken out, and the file stops at its
// cap. A write that fails is the end of the log; it is never an error the
// command sees.
func (l *Log) Write(p []byte) (int, error) {
	if l == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil || l.disabled {
		return len(p), nil
	}
	l.partial += string(p)
	for {
		i := strings.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		line := l.partial[:i]
		l.partial = l.partial[i+1:]
		l.line(line)
	}
	return len(p), nil
}

// line is one finished line of the transcript with its timestamp in front.
func (l *Log) line(line string) {
	if l.full || l.f == nil {
		return
	}
	out := l.now().UTC().Format(stamp) + "  " + l.redact(line) + "\n"
	if l.written+int64(len(out)) > l.limit() {
		final := l.now().UTC().Format(stamp) + fmt.Sprintf("  truncated: this log stops at %d MiB\n", l.limit()>>20)
		_, _ = io.WriteString(l.f, final)
		l.full = true
		return
	}
	n, err := io.WriteString(l.f, out)
	l.written += int64(n)
	if err != nil {
		// A full disk is the log's own problem: the command carries on with
		// the screen, which is still going somewhere.
		l.full = true
	}
}

// redact takes the registered values and the colour out: the log is plain text,
// and a secret is never part of it.
func (l *Log) redact(line string) string {
	line = escape.ReplaceAllString(line, "")
	for _, secret := range l.secrets {
		line = strings.ReplaceAll(line, secret, Redacted)
	}
	return strings.TrimRight(line, " ")
}

// escape matches the colour and cursor sequences a live renderer writes.
var escape = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// Close finishes the file: the line that was still being written, then the
// file. Closing twice is safe.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	if l.partial != "" {
		l.line(l.partial)
		l.partial = ""
	}
	f := l.f
	l.f = nil
	return f.Close()
}

// prune keeps the newest KeepFiles-1 logs and at most KeepBytes of them, oldest
// first, so this run's log is the twentieth at most and the directory never
// outgrows the cap by more than one run.
func prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type logFile struct {
		path string
		mod  time.Time
		size int64
	}
	var logs []logFile
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logFile{path: filepath.Join(dir, e.Name()), mod: info.ModTime(), size: info.Size()})
		total += info.Size()
	}
	// Oldest first.
	sort.Slice(logs, func(i, j int) bool { return logs[i].mod.Before(logs[j].mod) })
	keep := KeepFiles - 1
	for len(logs) > keep || total > KeepBytes {
		oldest := logs[0]
		_ = os.Remove(oldest.path)
		total -= oldest.size
		logs = logs[1:]
		if len(logs) == 0 {
			break
		}
	}
}
