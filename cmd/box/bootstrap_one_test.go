package main

// bootstrap_one_test.go proves the one question a guided fresh install asks: a
// person is asked once, for the platform and the seedbox, and the layer is
// installed only after that answer. The two halves are fakes -- the platform here,
// init's flow in internal/setup's beforewrite_test.go -- so the order, the "no"
// and a failure in either half are read without a cluster.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/bootstrap"
	"github.com/swizzin/s5n/internal/say"
	"github.com/swizzin/s5n/internal/setup"
)

// fakeAsker is the one question: what it was asked, the default it was asked
// with, and the answer it gives.
type fakeAsker struct {
	answer   bool
	err      error
	asked    []string
	defaults []bool
	onAnswer func()
}

func (f *fakeAsker) Ask(question string, def bool) (bool, error) {
	f.asked = append(f.asked, question)
	f.defaults = append(f.defaults, def)
	if f.onAnswer != nil {
		f.onAnswer()
	}
	return f.answer, f.err
}

// oneHarness is the flow with a fake platform half and a buffer to read.
type oneHarness struct {
	ask      *fakeAsker
	out      *bytes.Buffer
	platform int
	events   []string
	perr     error
}

func newOneHarness(answer bool) *oneHarness {
	h := &oneHarness{ask: &fakeAsker{answer: answer}, out: &bytes.Buffer{}}
	h.ask.onAnswer = func() { h.events = append(h.events, "asked") }
	return h
}

func (h *oneHarness) flow() oneQuestion {
	return oneQuestion{
		ask: h.ask,
		platform: func(context.Context) error {
			h.platform++
			h.events = append(h.events, "platform")
			return h.perr
		},
		out: &say.Plain{W: h.out},
	}
}

func (h *oneHarness) answers() setup.Answers {
	return setup.Answers{Login: "alice", Apps: []string{"qbittorrent"}, Namespace: "s5n-alice"}
}

// TestOneQuestionInstallsThePlatformAfterTheAnswer: the question comes first, once,
// and the layer is installed after it. That order is the whole flow: a person who
// said no to the platform must not have one.
func TestOneQuestionInstallsThePlatformAfterTheAnswer(t *testing.T) {
	h := newOneHarness(true)
	if err := h.flow().beforeWrite(context.Background(), h.answers()); err != nil {
		t.Fatalf("the one question: %v\n%s", err, h.out.String())
	}
	if len(h.ask.asked) != 1 {
		t.Fatalf("the run asked %d questions, want one: %v", len(h.ask.asked), h.ask.asked)
	}
	if want := "install the platform and alice's seedbox?"; h.ask.asked[0] != want {
		t.Errorf("the question was %q, want %q", h.ask.asked[0], want)
	}
	// A bare enter installs: the operator has answered every question of a fresh
	// host already, and this is the decision they came for (rule 10's prompt says
	// so: [Y/n]).
	if len(h.ask.defaults) != 1 || !h.ask.defaults[0] {
		t.Errorf("the question was asked with default %v, want yes", h.ask.defaults)
	}
	if h.platform != 1 {
		t.Errorf("the platform was installed %d times, want once", h.platform)
	}
	if got := strings.Join(h.events, ","); got != "asked,platform" {
		t.Errorf("the order was %s, want asked,platform", got)
	}
}

// TestOneQuestionLeavesTheHostAloneWhenTheAnswerIsNo: nothing of the platform is
// written, and the run says so in the words a refused question uses.
func TestOneQuestionLeavesTheHostAloneWhenTheAnswerIsNo(t *testing.T) {
	h := newOneHarness(false)
	err := h.flow().beforeWrite(context.Background(), h.answers())
	if !errors.Is(err, setup.ErrCancelled) {
		t.Fatalf("a no came back as %v, want the cancellation every refused question returns", err)
	}
	if h.platform != 0 {
		t.Error("the platform was installed after a no")
	}
	if !strings.Contains(h.out.String(), "oh heck, nvm") {
		t.Errorf("a no did not say it left the host alone:\n%s", h.out.String())
	}
}

// TestOneQuestionInstallsNothingWhenTheQuestionCannotBeAsked: a terminal that went
// away (Ctrl-D) is a cancellation, not an answer, and it installs nothing either.
func TestOneQuestionInstallsNothingWhenTheQuestionCannotBeAsked(t *testing.T) {
	h := newOneHarness(true)
	h.ask.err = setup.ErrCancelled
	err := h.flow().beforeWrite(context.Background(), h.answers())
	if !errors.Is(err, setup.ErrCancelled) {
		t.Fatalf("a cancelled question came back as %v", err)
	}
	if h.platform != 0 {
		t.Error("the platform was installed although the question was never answered")
	}
}

// TestOneQuestionStopsOnThePlatformItCouldNotInstall: the answer was yes and the
// platform half failed. The failure is the run's, and the seedbox half never
// starts -- a half-installed platform is what the next run finds and finishes,
// which is why this refunds nothing.
func TestOneQuestionStopsOnThePlatformItCouldNotInstall(t *testing.T) {
	h := newOneHarness(true)
	failed := errors.New("cert-manager's webhook refused the ClusterIssuer")
	h.perr = failed
	err := h.flow().beforeWrite(context.Background(), h.answers())
	if !errors.Is(err, failed) {
		t.Fatalf("a failed platform came back as %v", err)
	}
	if got := strings.Join(h.events, ","); got != "asked,platform" {
		t.Errorf("the order was %s, want the platform tried after the answer", got)
	}
}

// TestGuidedFreshIsAFreshPlatformAndATerminal: --yes and a pipe ask nothing (they
// are unattended, and their answers are the flags), and a box that has a platform
// keeps asking per half. Only a person on a host with none is asked once.
func TestGuidedFreshIsAFreshPlatformAndATerminal(t *testing.T) {
	fresh := []bootstrap.ComponentResult{
		{Component: bootstrap.ComponentGatewayAPI, Action: "install"},
		{Component: bootstrap.ComponentLoadBalancer, Action: "probe"},
		{Component: bootstrap.ComponentGateway, Action: "install"},
		{Component: bootstrap.ComponentCertManager, Action: "install"},
	}
	there := []bootstrap.ComponentResult{
		{Component: bootstrap.ComponentGatewayAPI, Action: "use"},
		{Component: bootstrap.ComponentLoadBalancer, Action: "probe"},
		{Component: bootstrap.ComponentGateway, Action: "verify"},
		{Component: bootstrap.ComponentCertManager, Action: "install"},
	}
	for _, tc := range []struct {
		name       string
		yes        bool
		terminal   bool
		components []bootstrap.ComponentResult
		want       bool
	}{
		{"a person on a fresh host", false, true, fresh, true},
		{"--yes on a fresh host", true, true, fresh, false},
		{"no terminal on a fresh host", false, false, fresh, false},
		{"a person on a host that has a platform", false, true, there, false},
		{"a person on a host with nothing to install", false, true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := guidedFresh(tc.yes, tc.terminal, tc.components); got != tc.want {
				t.Errorf("guidedFresh = %v, want %v", got, tc.want)
			}
		})
	}
}
