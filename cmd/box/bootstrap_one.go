package main

// bootstrap_one.go is the one question a guided fresh install asks.
//
// A host with no platform has two halves to install -- the layer box puts under
// the seedbox and the seedbox itself -- and they are one decision: a person who
// says yes to the platform has already said yes to the box on it
// (docs/output.md, "bootstrap, init, apply"). So the platform plan is printed
// first, init asks its own questions, and the answer covers both.
//
// The question is asked inside init's flow, between its last question and its
// first write (setup.Options.BeforeWrite): by then every answer is settled, so
// the operator sees the platform plan and what the seedbox will be, and nothing
// has been written. The platform is installed from here, which is what makes
// init's writes happen on a cluster that has a Gateway, a cert-manager and a
// login -- init reads the Gateway's address for the apps that sign in through
// the login, and that read has to happen after this point.
//
// A box that already has a platform keeps asking per half: there it is the
// platform that has nothing to do (so it asks nothing), and init's release is
// the only question. And --yes, or no terminal, answers without asking at all.

import (
	"context"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// guidedFresh says this run asks one question: a person to ask, and a platform
// layer that is not there yet.
//
// A platform that is there -- a box on somebody's own Envoy Gateway, a re-run --
// is not the same decision: those runs change one half or neither, and each half
// asks about what it will change.
func guidedFresh(yes, terminal bool, components []bootstrap.ComponentResult) bool {
	return !yes && terminal && platformAbsent(components)
}

// platformAbsent reports whether every component of the layer is one box writes:
// the cluster has none of it, so the plan this run shows is an install of all of
// it.
//
// The LoadBalancer's "probe" is the plan's own question -- MetalLB unless the
// Gateway gets an address without it -- and it is the one action a fresh cluster
// has for that component.
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

// oneQuestion is a guided fresh install's single answer: the platform layer is
// installed when the answer is yes, and init writes the seedbox after it.
type oneQuestion struct {
	// ask is the one question. It is the flow's own confirmer, so a live run
	// draws the form every other question draws.
	ask asker
	// platform installs the layer the plan showed. It is a field so a test can
	// watch the order without a cluster.
	platform func(ctx context.Context) error
	out      say.Reporter
}

// beforeWrite is setup's BeforeWrite: the one question, then the layer.
//
// A "no" leaves the host as it was -- no platform, no file, no directory -- and
// says so in box's own words, the way a refused question does. The answer is yes
// by default: this question is the whole of what the run is for, an operator has
// just typed the domain, the login and the paths of a fresh host, and the cost of
// a wrong yes is in the plan above it rather than in one more keystroke.
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
