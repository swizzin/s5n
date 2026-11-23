package bootstrap

// This file is the host's packet filter: what bootstrap reads about it before
// it writes anything, and the one change box makes when the host is its own.
//
// firewalld is the default on the Fedora family, and its default zone ends the
// FORWARD chain with a reject. That is exactly enough for traffic kube-proxy
// DNATs -- pod to Service, and a Service's traffic arriving from outside -- so
// a host with firewalld still runs the apps, the wiring Job and the lock proof.
// What it refuses is the traffic that is not DNAT'd: on a single-node k0s box,
// kube-router routes pod-to-pod traffic through the host, so Envoy dialling an
// app's pod address is a forwarded packet from a pod source, and it is refused
// at once (on the host this was found on, `cx_connect_fail` to the login, and
// every request answered 403 ext_authz_error).
//
// k0s's own documentation says what to do about it (docs/networking.md,
// "Firewalld & k0s", in the pinned v1.37.2): let the host accept traffic from
// the cluster's pod and service networks, which is what putting them in
// firewalld's trusted zone does. The firewall-cmd calls are firewalld's, not
// k0s's: k0s names the networks and leaves the host's packet filter alone,
// which is why this is bootstrap's step and not the installer's.

import (
	"context"
	"fmt"
	"strings"
)

// FirewallState is what bootstrap found about the host's packet filter, for the
// platform plan to show before its confirm, and for the one step that changes
// it.
type FirewallState struct {
	// Active reports that firewalld is running on this host.
	Active bool
	// CIDRs are the cluster's pod and service networks, read back out of the
	// k0s config box wrote for the cluster it installed. Only a cluster box
	// installed has them: on a cluster box did not install box does not know
	// them, and does not guess at another operator's networks.
	CIDRs []string
	// Warning is what box tells the operator about a firewall it will not
	// change, with the two commands that would. It is set instead of CIDRs,
	// and only when Active.
	Warning string
}

// Firewall is the host's packet filter. Only firewalld is driven: it is the
// one that runs by default on a distribution box supports, and a host with no
// firewalld has nothing for box to change.
type Firewall interface {
	// Running reports whether firewalld is active. A host without
	// firewall-cmd, or with the daemon stopped, is not running one.
	Running(ctx context.Context) bool
	// Trust admits the CIDRs in firewalld's trusted zone, permanently and in
	// the running configuration, and changes nothing that is already there.
	Trust(ctx context.Context, cidrs []string) error
}

// Firewalld is the real Firewall: firewall-cmd, run the way bootstrap runs its
// other host commands. The zero value is usable.
type Firewalld struct {
	// Run executes one command. Nil means ExecRunner.
	Run Runner
}

// trustedZone is the firewalld zone box adds the cluster's networks to.
// firewalld trusts its sources without matching any further rule, which is
// what "accept everything from the pod and service networks" is.
const trustedZone = "trusted"

// Running asks firewalld for its own state: `firewall-cmd --state` prints
// "running" and exits 0 only while the daemon serves. Every other answer --
// "not running", a host without the command, a refused connection -- is "no
// firewalld to change", so it is not an error box reports.
func (f Firewalld) Running(ctx context.Context) bool {
	out, err := f.runner().Run(ctx, "firewall-cmd", "--state")
	return err == nil && strings.TrimSpace(string(out)) == "running"
}

// Trust puts each CIDR in the trusted zone twice, which are two different
// changes: --permanent writes it into the zone's file, so a reload, a restart
// of the daemon or a reboot keeps it, and the running configuration is what
// takes effect now. Each half is asked for first: an operator's own
// --add-source without --permanent leaves a source that is live and not
// durable, and box fills in only the half that is missing.
//
// It does not reload. A reload re-reads every zone from the permanent
// configuration and would drop runtime-only changes another tool made; box
// changes its own two sources in both halves instead, which leaves the rest of
// the host's firewall exactly as it was.
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

// querySource asks firewalld whether one source is in the zone, in the
// permanent configuration or in the running one. firewall-cmd answers "yes" or
// "no" on stdout, and a "no" also exits non-zero, so the word is the answer.
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

// firewallCmdError names the command and quotes what firewalld said, so a
// refused change reads as the host's answer rather than as box's guess.
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

// firewalldWarning is what the plan says about a host whose firewall box will
// not change: another operator's cluster, where the networks are theirs to know
// and the packet filter is theirs to configure. The two commands are k0s's own
// (docs/networking.md, "Firewalld & k0s"), with the cluster's networks in
// place, and the reload is what makes a permanent change take effect.
func firewalldWarning() string {
	return "firewalld is running on this host, and box does not change the firewall of a cluster it did not install: if traffic between pods stops at the host, add the cluster's pod and service networks to firewalld's trusted zone and reload it — " +
		"firewall-cmd --permanent --zone=trusted --add-source=<podCIDR> and firewall-cmd --permanent --zone=trusted --add-source=<serviceCIDR>, then firewall-cmd --reload"
}
