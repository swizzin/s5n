package main

// migrate_settle.go asks a guided read's defaults, one question per address,
// writing the answer into the plan. An unattended run takes them all and counts
// (askDefaults). Each answer is one row (docs/output.md, rules 10 and 1); a prior
// read's answers never reach here. A path has no question: identity is the only
// safe rewrite.

import (
	"fmt"

	"github.com/swizzin/s5n/internal/migrate/apply"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// leaveValue is "leave it"; no app name can collide with it.
const leaveValue = "leave"

// settleAsker asks one default and writes one row.
type settleAsker struct {
	out    say.Reporter
	prompt setup.Prompter
	// apps are the plan's instance names, every possible answer.
	apps []string
}

// ask returns the app to point at, or "" to leave it.
func (a *settleAsker) ask(c apply.Choice) (string, error) {
	if a.prompt == nil {
		return c.App, nil
	}
	answer, err := a.prompt.Select(setup.Select{
		Key:    "migrate.hosts",
		Prompt: a.question(c),
		// Box's own answer is focused first: the default is one key away.
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

// answered is one answer as a row.
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

// question is the address and what to do; the reason is above (rule 10).
func (a *settleAsker) question(c apply.Choice) string {
	return c.Value + ": point it at which app?"
}

// defaultValue is box's default, so an empty answer keeps it.
func (a *settleAsker) defaultValue(c apply.Choice) string {
	if c.App == "" {
		return leaveValue
	}
	return c.App
}

// options are leave and every app, box's answer first.
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
