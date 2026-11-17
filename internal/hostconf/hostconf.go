// Package hostconf is root's config file for the host: /etc/s5n/box.yaml,
// the one file that decides whether this box updates by itself and where it
// posts notices.
//
// It sits beside the cluster record bootstrap writes, so `--record` moves it
// too, and it is root's to edit: box writes the file once (the template below)
// and afterwards only ever writes the one line `box init` asks about
// (SetNoticeURL). Everything else box reads from it strictly -- an unknown key,
// a malformed line or a bad value names the file, the line and the key -- so a
// typo is a message rather than a silently ignored setting.
//
// The package is stdlib only. It is read by bootstrap, `box init`, `box update`
// and `box status`, none of which may grow a YAML dependency: the four keys are
// a hand-written parser in the style of internal/bootstrap's record.
package hostconf

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/atomicfile"
	"github.com/swizzin/s5n/internal/hostroot"
)

// FileName is root's config, beside the cluster record.
const FileName = hostroot.ConfigName

// MinInterval is the shortest interval the file accepts. Anything shorter is a
// busy loop against the registry: a release lands hours after upstream, and the
// timer's own jitter is ten minutes.
const MinInterval = 15 * time.Minute

// Config is what root's file says. The zero value is not the default; Defaults
// is, and Load starts there and overrides what the file sets.
type Config struct {
	// Timer says box checks for newer releases by itself. Off means no update
	// checks and no health notices.
	Timer bool
	// Interval is how often the timer checks for a newer release.
	Interval time.Duration
	// HoldBox says take no new box, the same as --hold-box on every run.
	HoldBox bool
	// NoticeURL is where box posts a JSON notice, or empty for none. It can
	// carry a token, which is why the file is 0600 and why nothing ever prints
	// it: see Redact.
	NoticeURL string
}

// Defaults is a fresh host: the timer on, hourly, no hold, no notices.
func Defaults() Config {
	return Config{Timer: true, Interval: time.Hour, HoldBox: false, NoticeURL: ""}
}

// Path is root's config for a box whose cluster record is at recordPath: the
// same directory, the same rule as update.FilePath.
func Path(recordPath string) string {
	return filepath.Join(filepath.Dir(recordPath), FileName)
}

// Template is the file bootstrap writes on a host that has none, byte for byte.
// The comments are the documentation: box never rewrites them, and an operator
// who reads the file sees what each key does.
func Template() []byte {
	return []byte(`# box's settings for this host. box never rewrites this file; edit it by hand.
update:
  timer: on        # off: no update checks and no health notices
  interval: 1h     # how often the timer checks for a newer release; 15m at least
  hold-box: false  # true: take no new box, the same as --hold-box on every run
notices:
  url: ""          # box POSTs a JSON notice here after updates and on health changes
`)
}

// Ensure writes the default file when path is missing, and never touches one
// that is there: the comments and the answers in it are root's. It reports
// whether it wrote. A file that cannot be stat'ed is an error -- "there is none"
// and "the disk did not answer" lead to opposite decisions about writing.
func Ensure(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		if err := Write(path, Template()); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, err
	}
}

// Write replaces path with data atomically, mode 0600: the notice URL can carry
// a token, so the file is root's alone.
func Write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := atomicfile.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Load reads root's config. A missing file is the defaults, not an error: a box
// bootstrap made on a newer host has one, and one upgraded by hand may not.
// exists says whether there was a file to read.
func Load(path string) (cfg Config, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	cfg, err = parse(path, data)
	if err != nil {
		return Config{}, true, err
	}
	return cfg, true, nil
}

// section is one of the file's two top-level keys.
type section string

const (
	sectionUpdate  section = "update"
	sectionNotices section = "notices"
)

// parse reads the four keys strictly. The file has two levels: update and
// notices, and the leaves under them. A key outside them, a leaf at the top
// level, an indented line before a section, a repeated key and a value that is
// not what its key takes are all errors naming file, line and key.
func parse(path string, data []byte) (Config, error) {
	cfg := Defaults()
	var current section
	seen := map[string]bool{}
	for i, raw := range strings.Split(string(data), "\n") {
		n := i + 1
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(stripComment(line))
		if trimmed == "" {
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		key, value, ok := splitKV(trimmed)
		if !ok {
			return Config{}, fmt.Errorf("%s:%d: malformed line %q", path, n, trimmed)
		}
		if !indented {
			switch section(key) {
			case sectionUpdate, sectionNotices:
				if strings.TrimSpace(value) != "" {
					return Config{}, fmt.Errorf("%s:%d: %s takes no value", path, n, key)
				}
				current = section(key)
			default:
				return Config{}, fmt.Errorf("%s:%d: unknown key %q", path, n, key)
			}
			continue
		}
		if current == "" {
			return Config{}, fmt.Errorf("%s:%d: unexpected indented line %q", path, n, trimmed)
		}
		name := string(current) + "." + key
		if seen[name] {
			return Config{}, fmt.Errorf("%s:%d: %s is set twice", path, n, name)
		}
		seen[name] = true
		v := unquote(value)
		var err error
		switch current {
		case sectionUpdate:
			switch key {
			case "timer":
				cfg.Timer, err = parseOnOff(v)
			case "interval":
				cfg.Interval, err = parseInterval(v)
			case "hold-box":
				cfg.HoldBox, err = parseOnOff(v)
			default:
				return Config{}, fmt.Errorf("%s:%d: unknown key %q", path, n, name)
			}
		case sectionNotices:
			switch key {
			case "url":
				cfg.NoticeURL, err = parseURL(v)
			default:
				return Config{}, fmt.Errorf("%s:%d: unknown key %q", path, n, name)
			}
		}
		if err != nil {
			return Config{}, fmt.Errorf("%s:%d: %s %q %w", path, n, name, v, err)
		}
	}
	return cfg, nil
}

// parseOnOff is a switch's value: on/off, and true/false for someone who writes
// the file from a shell script.
func parseOnOff(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true":
		return true, nil
	case "off", "false":
		return false, nil
	default:
		return false, errors.New("is not on or off")
	}
}

// parseInterval is a Go duration, and at least MinInterval: a shorter one would
// hammer the registry, and the timer's jitter makes it pointless anyway.
func parseInterval(v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, errors.New("is not a duration such as 1h or 30m")
	}
	if d < MinInterval {
		return 0, fmt.Errorf("is shorter than %s", MinInterval)
	}
	return d, nil
}

// parseURL is an empty string (no notices) or an absolute http(s) address.
func parseURL(v string) (string, error) {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return "", nil
	}
	if err := ValidURL(raw); err != nil {
		return "", err
	}
	return raw, nil
}

// ValidURL is the rule the notice address follows, exported because `box init`
// asks for it: an empty answer means no notices, and anything else has to be an
// absolute http or https address whose host a post can reach.
func ValidURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("is not an http or https URL")
	}
	return nil
}

// splitKV splits "key: value" on the first colon, keeping any colon after it:
// "url: https://example.test/a" is one key and one value.
func splitKV(line string) (key, value string, ok bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// unquote strips one layer of matching single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// stripComment removes a trailing comment: a `#` that starts a line or follows
// whitespace, outside quotes. A `#` inside a quoted value is the value's, which
// is what keeps a URL's fragment from cutting its line in half.
func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// SetNoticeURL rewrites the one `notices.url` line of data and keeps every other
// byte, so `box init`'s answer does not reflow root's file: comments, key order,
// blank lines and the three other keys survive. A file with no url line is an
// error -- init writes the answer into root's file, it does not make one up.
func SetNoticeURL(src []byte, raw string) ([]byte, error) {
	lines := strings.Split(string(src), "\n")
	current := ""
	written := false
	for i, line := range lines {
		stripped := strings.TrimRight(line, "\r")
		cr := line[len(stripped):]
		body := stripComment(stripped)
		trimmed := strings.TrimSpace(body)
		if trimmed == "" {
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		key, _, ok := splitKV(trimmed)
		if !ok {
			continue
		}
		if !indented {
			current = key
			continue
		}
		if current != string(sectionNotices) || key != "url" {
			continue
		}
		indent := stripped[:len(stripped)-len(strings.TrimLeft(stripped, " \t"))]
		out := indent + "url: " + strconv.Quote(strings.TrimSpace(raw))
		// The template's comment says what the line is for, and it is not
		// box's to delete: keep it where it was.
		if comment := strings.TrimSpace(stripped[len(body):]); comment != "" {
			out += "  " + comment
		}
		lines[i] = out + cr
		written = true
	}
	if !written {
		return nil, fmt.Errorf("%s: no %s.url line to write: box init sets the notice address only in the file bootstrap wrote", FileName, sectionNotices)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// Redact is a notice address as box may print it: scheme and host, never the
// path or the query, because either can be the token that makes the URL work.
// A string that is not an absolute URL redacts to nothing rather than to itself.
func Redact(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
