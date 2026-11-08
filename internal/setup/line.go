package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// LinePrompt is the plain implementation of Prompter: one line per answer, on
// stdin and stdout, with no cursor movement and no colour. It is what a server
// with a serial console gets, and it is the reference for the TUI lane.
//
// Cancel is Ctrl-D (or a lone "q"), reflected as ErrCancelled. Ctrl-C is the
// terminal's own signal, and it leaves nothing written for the same reason: init
// writes the two files only after every question is answered.
type LinePrompt struct {
	In  io.Reader
	Out io.Writer
	// Mask reads a secret without echoing it, when In is a terminal. cmd/box
	// supplies the terminal implementation; nil reads a plain line, which is
	// what a pipe and the tests get.
	Mask func() (string, error)

	reader *bufio.Reader
}

func (p *LinePrompt) printf(format string, args ...any) {
	fmt.Fprintf(p.Out, format, args...)
}

func (p *LinePrompt) Input(f Input) (string, error) {
	label := f.Prompt
	if label == "" {
		label = f.Key
	}
	if f.Help != "" {
		p.printf("  %s\n", f.Help)
	}
	for {
		if f.Default != "" {
			p.printf("%s [%s]: ", label, f.Default)
		} else {
			p.printf("%s: ", label)
		}
		line, err := p.readLine()
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = f.Default
		}
		if value == "" && f.Validate == nil {
			p.printf("  a value is needed\n")
			continue
		}
		if f.Validate != nil {
			if err := f.Validate(value); err != nil {
				p.printf("  %v\n", err)
				continue
			}
		}
		return value, nil
	}
}

func (p *LinePrompt) Secret(f Secret) (string, error) {
	label := f.Prompt
	if label == "" {
		label = f.Key
	}
	if f.Help != "" {
		p.printf("  %s\n", f.Help)
	}
	for {
		p.printf("%s: ", label)
		var (
			line string
			err  error
		)
		if p.Mask != nil {
			line, err = p.Mask()
			// A masked read leaves the newline on the terminal.
			p.printf("\n")
		} else {
			line, err = p.readLine()
		}
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" && !f.Optional {
			p.printf("  a value is needed\n")
			continue
		}
		return value, nil
	}
}

func (p *LinePrompt) Confirm(f Confirm) (bool, error) {
	hint := "y/N"
	if f.Default {
		hint = "Y/n"
	}
	for {
		p.printf("%s [%s] ", f.Prompt, hint)
		line, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return f.Default, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.printf("  y or n\n")
	}
}

func (p *LinePrompt) Select(f Select) (string, error) {
	for i, o := range f.Options {
		p.printf("  %2d) %s%s\n", i+1, o.Label, helpSuffix(o.Help))
	}
	for {
		def := f.Default
		if def == "" {
			def = f.Options[0].Value
		}
		p.printf("%s [%s]: ", f.Prompt, def)
		line, err := p.readLine()
		if err != nil {
			return "", err
		}
		answer := strings.TrimSpace(line)
		if answer == "" {
			answer = def
		}
		if v, ok := pickOption(f.Options, answer); ok {
			return v, nil
		}
		p.printf("  %s is not one of the choices\n", answer)
	}
}

func (p *LinePrompt) MultiSelect(f MultiSelect) ([]string, error) {
	selected := make(map[string]bool, len(f.Selected))
	for _, s := range f.Selected {
		selected[s] = true
	}
	for i, o := range f.Options {
		mark := " "
		if selected[o.Value] {
			mark = "x"
		}
		p.printf("  %2d) [%s] %s%s\n", i+1, mark, o.Label, helpSuffix(o.Help))
	}
	for {
		p.printf("%s (numbers, comma separated; enter keeps them): ", f.Prompt)
		line, err := p.readLine()
		if err != nil {
			return nil, err
		}
		answer := strings.TrimSpace(line)
		var picked []string
		if answer == "" {
			picked = append(picked, f.Selected...)
		} else {
			seen := map[string]bool{}
			bad := ""
			for _, part := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				v, ok := pickOption(f.Options, part)
				if !ok {
					bad = part
					break
				}
				if !seen[v] {
					seen[v] = true
					picked = append(picked, v)
				}
			}
			if bad != "" {
				p.printf("  %s is not one of the choices\n", bad)
				continue
			}
		}
		if len(picked) < f.Min {
			p.printf("  at least %d %s needed\n", f.Min, plural(f.Min, "app", "apps"))
			continue
		}
		return picked, nil
	}
}

func (p *LinePrompt) Note(text string) error {
	p.printf("%s\n", text)
	return nil
}

// readLine reads one answer. End of input is a cancel, and so is a lone "q":
// both are how somebody says "not now" without a TUI's escape key.
func (p *LinePrompt) readLine() (string, error) {
	if p.reader == nil {
		if p.In == nil {
			return "", ErrCancelled
		}
		p.reader = bufio.NewReader(p.In)
	}
	line, err := p.reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if strings.TrimSpace(line) == "" {
				return "", ErrCancelled
			}
		} else {
			return "", err
		}
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "q" {
		return "", ErrCancelled
	}
	return line, nil
}

// pickOption matches a number (1-based, as printed) or a value or label,
// case-insensitively, so "2", "sonarr" and "Sonarr" all work.
func pickOption(options []Option, answer string) (string, bool) {
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(options) {
		return options[n-1].Value, true
	}
	for _, o := range options {
		if strings.EqualFold(answer, o.Value) || strings.EqualFold(answer, o.Label) {
			return o.Value, true
		}
	}
	return "", false
}

func helpSuffix(help string) string {
	if help == "" {
		return ""
	}
	return "  — " + help
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

var _ Prompter = (*LinePrompt)(nil)
