package bootstrap

// firewall_test.go is the host's packet filter: what box reads about it, and
// the two changes it makes on a host where it installed k0s. No e2e can show
// this -- kind has no firewalld -- so the command sequence is pinned here, and
// a host with firewalld is what proves it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeFirewallCmd answers firewall-cmd the way firewalld does: --state prints
// "running" or fails, --query-source answers yes/no (a "no" also fails, as the
// real command does), and --add-source writes the source into the half of the
// configuration it names. It records every call, so a test can hold the
// sequence to what it should be rather than to what box says about it.
type fakeFirewallCmd struct {
	running bool
	trusted map[string]bool // "permanent|runtime" + "|" + cidr
	fail    map[string]error
	calls   []string
}

func (f *fakeFirewallCmd) Run(_ context.Context, path string, args ...string) ([]byte, error) {
	call := path + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if err, ok := f.fail[call]; ok {
		return nil, err
	}
	if path != "firewall-cmd" {
		return nil, fmt.Errorf("box ran %s, not firewall-cmd", path)
	}
	if len(args) == 1 && args[0] == "--state" {
		if f.running {
			return []byte("running\n"), nil
		}
		return []byte("not running\n"), errors.New("exit status 252")
	}
	permanent := false
	rest := args
	if len(rest) > 0 && rest[0] == "--permanent" {
		permanent = true
		rest = rest[1:]
	}
	if len(rest) != 2 || rest[0] != "--zone=trusted" {
		return nil, fmt.Errorf("unexpected arguments %v", args)
	}
	half := "runtime"
	if permanent {
		half = "permanent"
	}
	switch {
	case strings.HasPrefix(rest[1], "--query-source="):
		cidr := strings.TrimPrefix(rest[1], "--query-source=")
		if f.trusted[half+"|"+cidr] {
			return []byte("yes\n"), nil
		}
		return []byte("no\n"), errors.New("exit status 1")
	case strings.HasPrefix(rest[1], "--add-source="):
		cidr := strings.TrimPrefix(rest[1], "--add-source=")
		if f.trusted == nil {
			f.trusted = map[string]bool{}
		}
		f.trusted[half+"|"+cidr] = true
		return []byte("success\n"), nil
	}
	return nil, fmt.Errorf("unexpected action %q", rest[1])
}

func TestFirewalldRunningIsFirewallCmdsOwnAnswer(t *testing.T) {
	cases := []struct {
		name string
		fake *fakeFirewallCmd
		run  Runner
		want bool
	}{
		{name: "running", fake: &fakeFirewallCmd{running: true}, want: true},
		{name: "not running", fake: &fakeFirewallCmd{}, want: false},
		{
			name: "no firewall-cmd at all",
			run: runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("executable file not found in $PATH")
			}),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			if run == nil {
				run = tc.fake
			}
			if got := (Firewalld{Run: run}).Running(context.Background()); got != tc.want {
				t.Errorf("Running = %v, want %v", got, tc.want)
			}
		})
	}
}

type runnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return f(ctx, path, args...)
}

// TestTrustAddsEachNetworkOnceInBothHalves is the whole rule: the cluster's two
// networks go into the trusted zone permanently (so a reload or a reboot keeps
// them) and in the running configuration (so they take effect now), each
// asked for first and added only when it is missing.
func TestTrustAddsEachNetworkOnceInBothHalves(t *testing.T) {
	pod, service := k0sPodCIDR, k0sServiceCIDR
	cases := []struct {
		name    string
		trusted []string // "half|cidr" entries already there
		want    []string
	}{
		{
			name: "nothing trusted",
			want: []string{
				"firewall-cmd --permanent --zone=trusted --query-source=" + pod,
				"firewall-cmd --permanent --zone=trusted --add-source=" + pod,
				"firewall-cmd --permanent --zone=trusted --query-source=" + service,
				"firewall-cmd --permanent --zone=trusted --add-source=" + service,
				"firewall-cmd --zone=trusted --query-source=" + pod,
				"firewall-cmd --zone=trusted --add-source=" + pod,
				"firewall-cmd --zone=trusted --query-source=" + service,
				"firewall-cmd --zone=trusted --add-source=" + service,
			},
		},
		{
			name:    "both already trusted, in both halves",
			trusted: []string{"permanent|" + pod, "permanent|" + service, "runtime|" + pod, "runtime|" + service},
			want: []string{
				"firewall-cmd --permanent --zone=trusted --query-source=" + pod,
				"firewall-cmd --permanent --zone=trusted --query-source=" + service,
				"firewall-cmd --zone=trusted --query-source=" + pod,
				"firewall-cmd --zone=trusted --query-source=" + service,
			},
		},
		{
			// An operator's own --add-source without --permanent: live, and
			// gone at the next reload. box fills in the half that is missing.
			name:    "only in the running configuration",
			trusted: []string{"runtime|" + pod, "runtime|" + service},
			want: []string{
				"firewall-cmd --permanent --zone=trusted --query-source=" + pod,
				"firewall-cmd --permanent --zone=trusted --add-source=" + pod,
				"firewall-cmd --permanent --zone=trusted --query-source=" + service,
				"firewall-cmd --permanent --zone=trusted --add-source=" + service,
				"firewall-cmd --zone=trusted --query-source=" + pod,
				"firewall-cmd --zone=trusted --query-source=" + service,
			},
		},
		{
			name:    "only in the permanent configuration",
			trusted: []string{"permanent|" + pod, "permanent|" + service},
			want: []string{
				"firewall-cmd --permanent --zone=trusted --query-source=" + pod,
				"firewall-cmd --permanent --zone=trusted --query-source=" + service,
				"firewall-cmd --zone=trusted --query-source=" + pod,
				"firewall-cmd --zone=trusted --add-source=" + pod,
				"firewall-cmd --zone=trusted --query-source=" + service,
				"firewall-cmd --zone=trusted --add-source=" + service,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeFirewallCmd{trusted: map[string]bool{}}
			for _, entry := range tc.trusted {
				fake.trusted[entry] = true
			}
			if err := (Firewalld{Run: fake}).Trust(context.Background(), []string{pod, service}); err != nil {
				t.Fatalf("Trust: %v", err)
			}
			if got, want := strings.Join(fake.calls, "\n"), strings.Join(tc.want, "\n"); got != want {
				t.Errorf("firewall-cmd calls:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// TestTrustReloadsNothing: a reload re-reads every zone from the permanent
// configuration and would drop runtime-only changes another tool made, so box
// writes its own two sources in both halves instead.
func TestTrustReloadsNothing(t *testing.T) {
	fake := &fakeFirewallCmd{}
	if err := (Firewalld{Run: fake}).Trust(context.Background(), []string{k0sPodCIDR}); err != nil {
		t.Fatalf("Trust: %v", err)
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "--reload") {
			t.Errorf("box reloaded the host's firewall: %s", call)
		}
	}
}

// TestTrustReportsARefusedAdd: a firewall-cmd that refuses is named with what
// it said, because that text is the host's answer to what box asked for.
func TestTrustReportsARefusedAdd(t *testing.T) {
	add := "firewall-cmd --permanent --zone=trusted --add-source=" + k0sPodCIDR
	fake := &fakeFirewallCmd{fail: map[string]error{
		add: errors.New("exit status 252: Authorization failed"),
	}}
	err := (Firewalld{Run: fake}).Trust(context.Background(), []string{k0sPodCIDR})
	if err == nil {
		t.Fatal("a refused add reported success")
	}
	if !strings.Contains(err.Error(), add) {
		t.Errorf("the refusal does not name the command it ran:\n%s", err)
	}
}

// TestK0sNetworksAreTheOnesBoxWrote: the networks the firewall trusts are read
// back out of the k0s config box installs with, so the cluster and the rule
// cannot disagree about them.
func TestK0sNetworksAreTheOnesBoxWrote(t *testing.T) {
	pod, service, err := k0sNetworks([]byte(k0sNodeConfig(testNodeIP)))
	if err != nil {
		t.Fatalf("read the config box writes: %v", err)
	}
	if pod != k0sPodCIDR || service != k0sServiceCIDR {
		t.Errorf("read %q and %q, want %q and %q", pod, service, k0sPodCIDR, k0sServiceCIDR)
	}

	// A k0s config without the networks is one box cannot settle: it names the
	// reason rather than trusting nothing (which would read as "no networks").
	if _, _, err := k0sNetworks([]byte("apiVersion: k0s.k0sproject.io/v1beta1\nkind: ClusterConfig\nspec:\n  api:\n    address: 192.0.2.10\n")); err == nil {
		t.Error("a config with no spec.network was accepted")
	}
	// A quoted value reads as the network, not as the quotes: k0s accepts
	// both spellings, and box's own reader must not turn one into a source
	// firewalld cannot parse.
	pod, service, err = k0sNetworks([]byte("spec:\n  network:\n    podCIDR: \"10.244.0.0/16\"\n    serviceCIDR: '10.96.0.0/12'\n"))
	if err != nil || pod != "10.244.0.0/16" || service != "10.96.0.0/12" {
		t.Errorf("quoted networks read as %q and %q (%v)", pod, service, err)
	}
}

// TestWithFirewallTellsTheOperatorAboutAHostBoxDoesNotOwn: on a cluster box did
// not install it knows neither network, so the plan carries the two commands
// and box changes nothing; on the cluster box installed, the networks come from
// the config box wrote.
func TestWithFirewallTellsTheOperatorAboutAHostBoxDoesNotOwn(t *testing.T) {
	ctx := context.Background()

	external := Result{Action: ActionUseCluster, Source: SourceExternal}
	got := (&Decision{Firewall: staticFirewall{running: true}}).withFirewall(ctx, external)
	if !got.Firewall.Active {
		t.Fatal("a host running firewalld was read as having none")
	}
	if len(got.Firewall.CIDRs) != 0 {
		t.Errorf("box trusted %v networks of a cluster it did not install", got.Firewall.CIDRs)
	}
	for _, want := range []string{
		"firewall-cmd --permanent --zone=trusted --add-source=<podCIDR>",
		"firewall-cmd --permanent --zone=trusted --add-source=<serviceCIDR>",
	} {
		if !strings.Contains(got.Firewall.Warning, want) {
			t.Errorf("the warning does not carry %q:\n%s", want, got.Firewall.Warning)
		}
	}

	// The cluster box installed: its networks are in the k0s config box wrote.
	file := t.TempDir() + "/k0s.yaml"
	if err := os.WriteFile(file, []byte(k0sNodeConfig(testNodeIP)), 0o644); err != nil {
		t.Fatal(err)
	}
	managed := Result{Action: ActionInstall, Source: SourceK0sManaged}
	got = (&Decision{Firewall: staticFirewall{running: true}, K0sConfigFile: file}).withFirewall(ctx, managed)
	if want := []string{k0sPodCIDR, k0sServiceCIDR}; fmt.Sprint(got.Firewall.CIDRs) != fmt.Sprint(want) {
		t.Errorf("the plan would trust %v, want %v", got.Firewall.CIDRs, want)
	}
	if got.Firewall.Warning != "" {
		t.Errorf("box warned about a firewall it can change: %s", got.Firewall.Warning)
	}

	// No firewalld: nothing to read and nothing to change.
	got = (&Decision{Firewall: staticFirewall{}}).withFirewall(ctx, managed)
	if got.Firewall.Active || len(got.Firewall.CIDRs) > 0 || got.Firewall.Warning != "" {
		t.Errorf("a host with no firewalld got %+v", got.Firewall)
	}

	// A config box cannot read is the operator's to fix, and the warning names
	// the file rather than trusting nothing.
	got = (&Decision{Firewall: staticFirewall{running: true}, K0sConfigFile: file + ".gone"}).withFirewall(ctx, managed)
	if !strings.Contains(got.Firewall.Warning, file+".gone") {
		t.Errorf("a missing k0s config did not name the file:\n%s", got.Firewall.Warning)
	}
	if len(got.Firewall.CIDRs) != 0 {
		t.Errorf("box trusted %v with no config to read them from", got.Firewall.CIDRs)
	}
}

// staticFirewall is a Firewall that only answers "is one running".
type staticFirewall struct{ running bool }

func (f staticFirewall) Running(context.Context) bool { return f.running }

func (f staticFirewall) Trust(context.Context, []string) error { return nil }
