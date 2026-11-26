package main

// bootstrap_one.go is the one question a guided fresh install asks: the platform
// layer and the seedbox are one decision (docs/output.md). It comes inside
// init's flow between the last question and the first write, so the platform
// plan is shown with every answer settled and nothing written; the platform is
// installed there, before init reads the Gateway. An existing platform asks per
// half; --yes or no terminal asks nothing.

import (
	"context"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// guidedFresh is a person and a missing platform; an existing one asks per half.
func guidedFresh(yes, terminal bool, components []bootstrap.ComponentResult) bool {
	return !yes && terminal && platformAbsent(components)
}

// platformAbsent: every component is box's, so the plan installs all of it. The
// LoadBalancer "probe" is that component's one action.
func platformAbsent(components []bootstrap.ComponentResult) bool {
	writes := 0
	for _, c := range components {
		switch c.Action {
		case "install":
			writes++
		case "probe":
		default:
			return false
		}
	}
	return writes > 0
}

// oneQuestion is a guided fresh install's single answer.
type oneQuestion struct {
	// ask is the flow's own confirmer.
	ask asker
	// platform installs the shown layer; a field so a test watches the order.
	platform func(ctx context.Context) error
	out      say.Reporter
}

// beforeWrite asks, then installs. A no leaves the host as it was. Yes is the
// default: the operator just answered the whole run, and the plan above is the
// review.
func (q oneQuestion) beforeWrite(ctx context.Context, a setup.Answers) error {
	ok, err := q.ask.Ask(questionFor(a), true)
	if err != nil {
		return err
	}
	if !ok {
		q.out.Line("oh heck, nvm")
		return setup.ErrCancelled
	}
	return q.platform(ctx)
}

// questionFor is the one question, named for the box it makes: the login that
// will sign in to it, when init decided one.
func questionFor(a setup.Answers) string {
	if login := a.Login; login != "" {
		return "install the platform and " + login + "'s seedbox?"
	}
	return "install the platform and the seedbox?"
}
