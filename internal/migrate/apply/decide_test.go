package apply

// decide_test.go proves the guided half of Settle: an operator's answer becomes
// the plan's rule, and the list says the decision is theirs. What is left
// unanswered stays a default, which is what the end of a run names.

import (
	"context"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// settleFixture settles the shared rehearsal and hands back the plan Settle
// wrote and its list.
func settleFixture(t *testing.T) (*plan.Plan, Settled) {
	t.Helper()
	r := newRehearsal(t, rehearsalHosts(true), true)
	p, called, err := Settle(context.Background(), r.plan(), r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	return p, called
}

func TestDecideAsksAboutThePlansOwnDefaults(t *testing.T) {
	r := newRehearsal(t, rehearsalHosts(false)+`  - host: 10.244.0.7
    port: 8081
    app: qbittorrent
    default: true
    why: "rewrite to qbittorrent: 10.244.0.7:8081 was its pod's address, which does not survive the move"
`, false)
	p, called, err := Settle(context.Background(), r.plan(), r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// Called out with the action in the header and the reason on the line, not
	// the action twice.
	var found *SettledItem
	for _, g := range called {
		for i, it := range g.Items {
			if it.Value == "10.244.0.7:8081" {
				found = &g.Items[i]
				if g.Action != actionRewrite+"qbittorrent" {
					t.Errorf("the plan's default is listed under %q", g.Action)
				}
			}
		}
	}
	if found == nil {
		t.Fatalf("the plan's own default is not called out:\n%s", strings.Join(called.Lines(), "\n"))
	}
	if strings.HasPrefix(found.Why, "rewrite to ") {
		t.Errorf("the reason repeats the action: %q", found.Why)
	}
	// And it is a question like any other.
	var asked []Choice
	decided, err := Decide(p, called, func(c Choice) (string, error) {
		asked = append(asked, c)
		if c.Host == "10.244.0.7" {
			return "sonarr", nil
		}
		return c.App, nil
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var got *plan.HostRule
	for i := range p.Hosts {
		if p.Hosts[i].Host == "10.244.0.7" {
			got = &p.Hosts[i]
		}
	}
	if got == nil || got.App != "sonarr" || got.Default {
		t.Errorf("the answered rule = %+v, want sonarr and the operator's own decision", got)
	}
	for _, line := range decided.Defaults().Lines() {
		if strings.Contains(line, "10.244.0.7") {
			t.Errorf("the summary still names an answered default: %q", line)
		}
	}
}

func TestDecideWritesTheAnswerIntoThePlan(t *testing.T) {
	p, called := settleFixture(t)
	var asked []Choice
	decided, err := Decide(p, called, func(c Choice) (string, error) {
		asked = append(asked, c)
		if c.Host == "10.244.0.56" {
			return "radarr", nil
		}
		return c.App, nil
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(asked) != 2 {
		t.Fatalf("asked %d questions, want one per address: %+v", len(asked), asked)
	}
	// The rule the answer replaced is the operator's now: no longer a default,
	// pointing where they said, with the answer as its reason.
	var found *plan.HostRule
	for i := range p.Hosts {
		if p.Hosts[i].Host == "10.244.0.56" {
			found = &p.Hosts[i]
		}
	}
	if found == nil {
		t.Fatal("the plan has no rule for 10.244.0.56")
	}
	if found.App != "radarr" || found.Leave || found.Default {
		t.Errorf("the answered rule = %+v, want radarr, not a default", *found)
	}
	if found.Why != "you chose radarr" {
		t.Errorf("the answered rule's reason = %q", found.Why)
	}
	// The list agrees, and the value is no longer one of the defaults taken.
	var group string
	for _, g := range decided {
		for _, it := range g.Items {
			if it.Value == "10.244.0.56:32400" {
				group = g.Action
				if it.Default {
					t.Error("the answered value is still counted as a default")
				}
			}
		}
	}
	if group != actionRewrite+"radarr" {
		t.Errorf("the answered value is listed under %q", group)
	}
	for _, line := range decided.Defaults().Lines() {
		if strings.Contains(line, "10.244.0.56") {
			t.Errorf("the summary of defaults taken names an answered value: %q", line)
		}
	}
}

func TestDecideLeavesWhatTheOperatorLeaves(t *testing.T) {
	p, called := settleFixture(t)
	decided, err := Decide(p, called, func(c Choice) (string, error) {
		if c.Host == "example.com" {
			return "", nil
		}
		return c.App, nil
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var found *plan.HostRule
	for i := range p.Hosts {
		if p.Hosts[i].Host == "example.com" {
			found = &p.Hosts[i]
		}
	}
	if found == nil {
		t.Fatal("the plan has no rule for example.com")
	}
	if !found.Leave || found.App != "" || found.Default {
		t.Errorf("the answered rule = %+v, want a leave that is not a default", *found)
	}
	for _, g := range decided {
		for _, it := range g.Items {
			if it.Value == "example.com:443" && g.Action != actionLeave {
				t.Errorf("an address the operator left is listed under %q", g.Action)
			}
		}
	}
}

func TestDecideAsksAboutAddressesOnly(t *testing.T) {
	r := newRehearsal(t, rehearsalHosts(false), false)
	exec(t, r.path("sonarr")+"/sonarr.db",
		`INSERT INTO Series (Id, Title, Path) VALUES (2, 'Elsewhere', '/mnt/other/shows')`,
		`INSERT INTO RootFolders (Id, Path) VALUES (2, '/mnt/other/shows')`)
	p, called, err := Settle(context.Background(), r.plan(), r.settleInput())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	var values []string
	decided, err := Decide(p, called, func(c Choice) (string, error) {
		values = append(values, c.Value)
		return c.App, nil
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for _, v := range values {
		if strings.HasPrefix(v, "/") {
			t.Errorf("Decide asked about the path %q", v)
		}
	}
	var paths int
	for _, line := range decided.Defaults().Lines() {
		if strings.Contains(line, "/mnt/other/shows") {
			paths++
		}
	}
	if paths != 1 {
		t.Errorf("the path default appears %d times in the defaults taken, want once", paths)
	}
}

func TestDecideRefusesAnAppThePlanDoesNotHave(t *testing.T) {
	p, called := settleFixture(t)
	_, err := Decide(p, called, func(Choice) (string, error) { return "emby", nil })
	if err == nil || !strings.Contains(err.Error(), "emby") {
		t.Fatalf("Decide accepted an app the plan does not have: %v", err)
	}
}

func TestDecideAcceptingADefaultMakesItAnAnswer(t *testing.T) {
	p, called := settleFixture(t)
	before := append([]plan.HostRule(nil), p.Hosts...)
	decided, err := Decide(p, called, func(c Choice) (string, error) { return c.App, nil })
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	for i, r := range p.Hosts {
		if r.App != before[i].App || r.Leave != before[i].Leave {
			t.Errorf("accepting a default moved the address: %+v want %+v", r, before[i])
		}
		if r.Default {
			t.Errorf("%s is still counted as a default nobody looked at", r.Host)
		}
	}
	// What is still listed as taken is what was never asked: the paths, whose
	// only rewrite is identity.
	wantUnasked := 0
	for _, g := range called {
		for _, it := range g.Items {
			if it.Host == "" {
				wantUnasked++
			}
		}
	}
	got := 0
	for _, g := range decided.Defaults() {
		got += len(g.Items)
	}
	if got != wantUnasked {
		t.Errorf("the defaults taken name %d values, want the %d nothing asked about", got, wantUnasked)
	}
}
