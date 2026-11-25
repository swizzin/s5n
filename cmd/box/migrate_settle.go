package main

// migrate_settle.go is what a guided read asks: the same decisions the report
// carries, one question per address, and the answer written into the plan.
//
// A default is never silent. An unattended run takes every one and says how many
// at the end, with the report naming each (askDefaults, migrate.go); this file is
// the run with somebody at the terminal. Each answer collapses to one row -- the
// apps that hold the address, the address, and what it now points at -- because a
// question already answered is a fact (docs/output.md, rules 10 and 1). An answer
// an earlier read gave never reaches this file at all: cutover carries it.
//
// A path has no question, because box has no alternative to offer: identity is
// the only rewrite that cannot make that app worse.

import (
	"fmt"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// leaveValue is the select's answer for "leave it as it is": no app name can
// collide with it, because an instance name is the chart's.
const leaveValue = "leave"

// settleAsker turns one default into one question on the terminal, and one row
// into the transcript.
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
		a.answered(c, "")
		return "", nil
	}
	for _, app := range a.apps {
		if app == answer {
			a.answered(c, app)
			return app, nil
		}
	}
	return "", fmt.Errorf("%s is not an app in this plan", answer)
}

// answered is one answer as a row: who holds the address, the address, and what
// it now means.
func (a *settleAsker) answered(c apply.Choice, app string) {
	to := leftAsItIs
	if app != "" {
		to = app
	}
	a.out.Rows([]say.Row{{
		Mark:  say.MarkDone,
		Name:  holders(c.Holds),
		Facts: []string{c.Value + " → " + to},
	}})
}

// question is the one line the form asks: the address, and what to do with it.
// The reason is above it in the transcript (the answer rows and the report), so
// the question stays short enough to read at a glance (rule 10).
func (a *settleAsker) question(c apply.Choice) string {
	return c.Value + ": point it at which app?"
}

// defaultValue is the value the select starts on: the default box took, so an
// empty answer keeps it.
func (a *settleAsker) defaultValue(c apply.Choice) string {
	if c.App == "" {
		return leaveValue
	}
	return c.App
}

// options are leave and every app the plan has, with box's own answer first. The
// labels are what the answer is, with nothing telling the reader where it goes.
func (a *settleAsker) options(c apply.Choice) []setup.Option {
	def := a.defaultValue(c)
	label := func(value string) string {
		if value == leaveValue {
			return "leave it"
		}
		return "point it at " + value
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
