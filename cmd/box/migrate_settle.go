package main

// migrate_settle.go is what a guided read asks: the same decisions report.txt
// carries, one question per address, and the answer written into the plan.
//
// A default is never silent. An unattended run takes every one and says so
// (askDefaults, migrate.go); this file is the run with somebody at the terminal,
// which asks in the order the called-out list is grouped and offers, for each
// address, exactly the answers that exist: leave it as it is, or point it at one
// of the plan's apps. A path has no question, because box has no alternative to
// offer: identity is the only rewrite that cannot make that app worse.

import (
	"fmt"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// leaveValue is the select's answer for "leave it as it is": no app name can
// collide with it, because an instance name is the chart's.
const leaveValue = "leave"

// settleAsker turns one default into one question on the terminal.
type settleAsker struct {
	out    say.Reporter
	prompt setup.Prompter
	// apps are the plan's instance names: every answer a rewrite can take.
	apps []string
}

// ask is the callback apply.Decide takes. It returns the app to point the
// address at, or "" to leave it as it is.
func (a *settleAsker) ask(c apply.Choice) (string, error) {
	if a.prompt == nil {
		return c.App, nil
	}
	answer, err := a.prompt.Select(setup.Select{
		Key:    "migrate.hosts",
		Prompt: a.question(c),
		// The options are what the operator can do about this one address, and
		// box's own answer is the one focused first: taking the default has to
		// be one key away.
		Options: a.options(c),
		Default: a.defaultValue(c),
	})
	if err != nil {
		return "", err
	}
	if answer == leaveValue {
		return "", nil
	}
	for _, app := range a.apps {
		if app == answer {
			return app, nil
		}
	}
	return "", fmt.Errorf("%s is not an app in this plan", answer)
}

// question is one address, why box took the decision it did, and where the
// value is stored: the line the report carries, asked rather than shown.
func (a *settleAsker) question(c apply.Choice) string {
	var b strings.Builder
	b.WriteString(c.Value)
	if c.Why != "" {
		b.WriteString(" (" + c.Why + ")")
	}
	b.WriteString("\nwhat should box do with it?")
	for _, h := range c.Holds {
		b.WriteString("\n  " + h.String())
	}
	return b.String()
}

// defaultValue is the value the select starts on: the default box took, so an
// empty answer keeps it.
func (a *settleAsker) defaultValue(c apply.Choice) string {
	if c.App == "" {
		return leaveValue
	}
	return c.App
}

// options are leave and every app the plan has, with box's own answer first and
// labelled as the default.
func (a *settleAsker) options(c apply.Choice) []setup.Option {
	def := a.defaultValue(c)
	label := func(value string) string {
		text := "leave it as it is"
		if value != leaveValue {
			text = "point it at " + value
		}
		if value == def {
			text += " (box's default)"
		}
		return text
	}
	out := []setup.Option{{Value: def, Label: label(def)}}
	if def != leaveValue {
		out = append(out, setup.Option{Value: leaveValue, Label: label(leaveValue)})
	}
	for _, app := range a.apps {
		if app == def {
			continue
		}
		out = append(out, setup.Option{Value: app, Label: label(app)})
	}
	return out
}
