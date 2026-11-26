package bootstrap

// This file is the host's packet filter: what bootstrap reads before it writes, and
// the one change box makes when the host is its own.
//
// firewalld is the Fedora family's default, and its default zone ends the FORWARD
// chain with a reject. DNAT'd traffic survives -- pod to Service, and a Service's from
// outside -- which is why the apps, the wiring Job and the lock proof still pass. What
// it refuses is traffic that is not DNAT'd: on a single-node k0s box kube-router
// routes pod-to-pod through the host, so Envoy dialling an app's pod is a forwarded
// packet from a pod source, refused at once (`cx_connect_fail` to the login, 403
// ext_authz_error on every request).
//
// k0s's docs say what to do (docs/networking.md, "Firewalld & k0s", at the pinned
// v1.37.2): let the host accept traffic from the pod and service networks, which is
// firewalld's trusted zone. Those calls are firewalld's, since k0s names the networks
// and leaves the packet filter alone -- which is why this is bootstrap's step.

import (
	"context"
	"fmt"
	"strings"
)

// FirewallState is what bootstrap found about the packet filter, for the plan to show
// before its confirm and for the one step that changes it.
type FirewallState struct {
	// Active reports that firewalld is running on this host.
	Active bool
	// CIDRs are the cluster's pod and service networks, read back from the config box
	// wrote for a cluster it installed. It does not guess at another operator's.
	CIDRs []string
	// Warning is what box says about a firewall it will not change, with the two
	// commands that would. Set instead of CIDRs, and only when Active.
	Warning string
}

// Firewall is the host's packet filter. Only firewalld is driven: it runs by default
// on a distribution box supports, and a host without it has nothing to change.
type Firewall interface {
	// Running reports whether firewalld is active; no firewall-cmd or a stopped daemon
	// is not.
	Running(ctx context.Context) bool
	// Trust admits the CIDRs in the trusted zone, permanently and now, changing nothing
	// already there.
	Trust(ctx context.Context, cidrs []string) error
}

// Firewalld is the real Firewall: firewall-cmd, as bootstrap runs its other host
// commands. The zero value is usable.
type Firewalld struct {
	// Run executes one command. Nil means ExecRunner.
	Run Runner
}

// trustedZone is the zone box adds the networks to: firewalld trusts a zone's sources
// without further rules, which is "accept everything from them".
const trustedZone = "trusted"

// Running asks firewalld's own state: `--state` prints "running" and exits 0 only
// while the daemon serves. Every other answer is "no firewalld to change", not an
// error box reports.
func (f Firewalld) Running(ctx context.Context) bool {
	out, err := f.runner().Run(ctx, "firewall-cmd", "--state")
	return err == nil && strings.TrimSpace(string(out)) == "running"
}

// Trust puts each CIDR in the trusted zone twice, which are two changes: --permanent
// writes the zone's file, so a reload, restart or reboot keeps it, and the running
// configuration takes effect now. Each half is asked for first, so an operator's own
// --add-source is only completed where it is missing.
//
// It does not reload: a reload re-reads every zone from the permanent configuration
// and would drop another tool's runtime-only changes. Box changes its own sources in
// both halves, leaving the rest of the firewall as it was.
func (f Firewalld) Trust(ctx context.Context, cidrs []string) error {
	run := f.runner()
	for _, permanent := range []bool{true, false} {
		for _, cidr := range cidrs {
			present, err := querySource(ctx, run, cidr, permanent)
			if err != nil {
				return err
			}
			if present {
				continue
			}
			args := []string{}
			if permanent {
				args = append(args, "--permanent")
			}
			args = append(args, "--zone="+trustedZone, "--add-source="+cidr)
			if out, err := run.Run(ctx, "firewall-cmd", args...); err != nil {
				return firewallCmdError(args, out, err)
			}
		}
	}
	return nil
}

// querySource asks whether one source is in the zone, permanent or running. --query
// answers "yes" or "no" and "no" also exits non-zero, so the word is the answer.
func querySource(ctx context.Context, run Runner, cidr string, permanent bool) (bool, error) {
	args := []string{}
	if permanent {
		args = append(args, "--permanent")
	}
	args = append(args, "--zone="+trustedZone, "--query-source="+cidr)
	out, err := run.Run(ctx, "firewall-cmd", args...)
	switch strings.TrimSpace(string(out)) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	if err != nil {
		return false, firewallCmdError(args, out, err)
	}
	return false, fmt.Errorf("firewall-cmd %s answered %q, which is neither yes nor no", strings.Join(args, " "), strings.TrimSpace(string(out)))
}

// firewallCmdError names the command and quotes firewalld, so a refusal reads as the
// host's answer.
func firewallCmdError(args []string, out []byte, err error) error {
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return fmt.Errorf("firewall-cmd %s: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("firewall-cmd %s: %v: %s", strings.Join(args, " "), err, detail)
}

func (f Firewalld) runner() Runner {
	if f.Run != nil {
		return f.Run
	}
	return ExecRunner{}
}

// firewalldWarning is what the plan says about a firewall box will not change: another
// operator's cluster, whose networks and filter are theirs. The commands are k0s's
// own (docs/networking.md), and the reload makes a permanent change take effect.
func firewalldWarning() string {
	return "firewalld is running on this host, and box does not change the firewall of a cluster it did not install: if traffic between pods stops at the host, add the cluster's pod and service networks to firewalld's trusted zone and reload it — " +
		"firewall-cmd --permanent --zone=trusted --add-source=<podCIDR> and firewall-cmd --permanent --zone=trusted --add-source=<serviceCIDR>, then firewall-cmd --reload"
}
