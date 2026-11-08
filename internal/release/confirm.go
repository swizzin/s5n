package release

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Terminal is the prompt. `--yes` answers for you, and a prompt that cannot be
// shown is a refusal rather than an implicit yes: box writes to a cluster as
// root, so "nobody was there" must never mean "go ahead".
type Terminal struct {
	In    io.Reader
	Out   io.Writer
	Yes   bool
	IsTTY func() bool
	// ReadLine is the default reader; tests replace it.
	ReadLine func() (string, error)
}

// Ask prints the question and reads one answer. Anything but yes is no.
func (t *Terminal) Ask(question string) (bool, error) {
	if t.Yes {
		return true, nil
	}
	tty := t.IsTTY
	if tty == nil {
		tty = func() bool { return true }
	}
	if !tty() {
		return false, errors.New("not a terminal: pass --yes to apply without a prompt")
	}
	fmt.Fprintf(t.Out, "%s [y/N] ", question)
	line, err := t.readLine()
	if err != nil {
		return false, fmt.Errorf("read the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func (t *Terminal) readLine() (string, error) {
	if t.ReadLine != nil {
		return t.ReadLine()
	}
	in := t.In
	if in == nil {
		in = os.Stdin
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// TTY reports whether f is a character device, which is the cheap test for "a
// human can answer this". It is a function rather than a method so cmd/box can
// pass os.Stdin without this package depending on a terminal library.
func TTY(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
