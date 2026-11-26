package apply

// settle_carry_test.go is the answers a cutover does not ask again: what an

import (
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// unsettled is a plan with one address nothing settles, as discovery writes it,
// and the called-out list Settle would hand back for it.
func unsettled() (*plan.Plan, Settled) {
	p := &plan.Plan{
		Version: plan.Version,
		Apps: map[string]plan.App{
			"maintainerr": {Kind: plan.Kubernetes, Source: "maintainerr", Config: "/srv/apps/maintainerr"},
			"plex":        {Kind: plan.Kubernetes, Source: "plex", Config: "/srv/apps/plex"},
		},
		Hosts: []plan.HostRule{{Host: "10.244.0.94", Port: 32400, Leave: true, Default: true, Why: "left as it is: box cannot tell what it is"}},
	}
	s := Settled{}
	s.add(actionLeave, SettledItem{
		Value: "10.244.0.94:32400", Why: "box cannot tell what it is",
		Host: "10.244.0.94", Port: 32400, Default: true,
		Holds: []SettledHold{{App: "maintainerr", File: "settings.json", Where: "plex"}},
	})
	return p, s
}

func TestAnAnswerFromAnEarlierReadIsCarriedNotAsked(t *testing.T) {
	now, settled := unsettled()
	prior := &plan.Plan{
		Version: plan.Version,
		Apps: map[string]plan.App{
			"maintainerr": {Kind: plan.Kubernetes, Source: "maintainerr", Config: "/srv/apps/maintainerr"},
			"plex":        {Kind: plan.Kubernetes, Source: "plex", Config: "/srv/apps/plex"},
		},
		Hosts: []plan.HostRule{{Host: "10.244.0.94", Port: 32400, App: "plex", Why: "you chose plex"}},
	}
	left, carried := CarryAnswers(now, settled, prior)
	if len(carried) != 1 {
		t.Fatalf("carried %v, want the one answer", carried)
	}
	if c := carried[0]; c.Value != "10.244.0.94:32400" || c.Answer() != "plex" {
		t.Errorf("the carried answer = %+v, want 10.244.0.94:32400 → plex", c)
	}
	if !left.Empty() {
		t.Errorf("the answered value is still called out: %v", left.Lines())
	}
	// The plan carries the answer as a rule, with no default left on it.
	var rule *plan.HostRule
	for i := range now.Hosts {
		if now.Hosts[i].Host == "10.244.0.94" {
			rule = &now.Hosts[i]
		}
	}
	if rule == nil || rule.App != "plex" || rule.Leave || rule.Default {
		t.Fatalf("the plan's rule = %+v, want it answered and not a default", rule)
	}
	// And it is no longer a default the plan would ask about.
	if got := now.Defaults(); len(got) != 0 {
		t.Errorf("the plan still carries the answer as a default: %v", got)
	}
}

func TestAnAnswerForAnAppThisPlanDoesNotHaveIsAsked(t *testing.T) {
	now, settled := unsettled()
	prior := &plan.Plan{
		Version: plan.Version,
		Apps:    map[string]plan.App{"sonarr": {Kind: plan.Kubernetes, Source: "sonarr", Config: "/srv/apps/sonarr"}},
		Hosts:   []plan.HostRule{{Host: "10.244.0.94", Port: 32400, App: "sonarr"}},
	}
	left, carried := CarryAnswers(now, settled, prior)
	if len(carried) != 0 {
		t.Errorf("carried %v, and this plan has no sonarr", carried)
	}
	if len(left) != 1 || !left[0].Items[0].Default {
		t.Errorf("the value stopped being a default: %v", left.Lines())
	}
}

func TestALeaveFromAnEarlierReadIsCarried(t *testing.T) {
	now, settled := unsettled()
	prior := &plan.Plan{
		Version: plan.Version,
		Apps:    map[string]plan.App{"maintainerr": {Kind: plan.Kubernetes, Source: "maintainerr", Config: "/srv/apps/maintainerr"}},
		Hosts:   []plan.HostRule{{Host: "10.244.0.94", Port: 32400, Leave: true, Why: "you chose to leave it as stored"}},
	}
	left, carried := CarryAnswers(now, settled, prior)
	if len(carried) != 1 {
		t.Fatalf("carried %v, want the one answer", carried)
	}
	if got := carried[0].Answer(); got != "left as it is" {
		t.Errorf("the carried answer = %q, want it left as it is", got)
	}
	if !left.Empty() {
		t.Errorf("the answered value is still called out: %v", left.Lines())
	}
	if got := now.Hosts[0]; got.Default || got.App != "" || !got.Leave {
		t.Errorf("the plan's rule = %+v, want a leave that is not a default", got)
	}
}

func TestCarriedAnswersReadAsOneLineEach(t *testing.T) {
	now, settled := unsettled()
	prior := &plan.Plan{
		Version: plan.Version,
		Apps:    map[string]plan.App{"plex": {Kind: plan.Kubernetes, Source: "plex", Config: "/srv/apps/plex"}},
		Hosts:   []plan.HostRule{{Host: "10.244.0.94", Port: 32400, App: "plex", Why: "you chose plex"}},
	}
	_, carried := CarryAnswers(now, settled, prior)
	if got := strings.Join([]string{carried[0].Value, carried[0].Answer()}, " → "); got != "10.244.0.94:32400 → plex" {
		t.Errorf("the row reads %q", got)
	}
}
