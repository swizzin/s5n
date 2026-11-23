// probe.go is `s5n probe`: the one thing box runs inside a seedbox namespace
// to find out whether the cluster enforces the release's NetworkPolicy.
//
// It exists because a NetworkPolicy is an instruction to the CNI, not something
// the API server enforces: a cluster whose CNI ignores egress rules would run a
// rehearsal as the live box, and box has to be able to tell before any app
// starts. The probe pod is the s5n image itself, so it is an image the release
// already runs and the chart's blessed-image policy already admits; nothing new
// is pulled for a rehearsal.
//
// The exit code is the answer, and it is the only thing box reads:
//
//	0  the connection behaved as --want asked
//	3  it behaved the other way
//	4  the probe could not run at all (bad address, no such host, usage)
//
// --settle is for the moment a pod starts. A CNI programs a new pod's policy
// shortly after the pod exists (kind's kindnet takes about half a second), so a
// single dial at start-up can open where every later one is denied. With
// --settle the probe dials until the answer --want names holds settleRun times
// in a row, and answers the other way only when the deadline passes first. The
// rehearsal's per-pod gate is this: an init container that holds the app back
// until its own pod's egress is closed.
//
// Splitting "the wrong answer" from "no answer" is what lets box refuse a
// rehearsal for the right reason: a control pod that cannot run means the proof
// never happened, which is not the same as a lock that does not hold.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// The exit codes above. They are part of the contract with internal/migrate/lock.
const (
	exitAsAsked  = 0
	exitOpposite = 3
	exitBroken   = 4
)

func runProbe(args []string) error {
	os.Exit(probeArgs(args, os.Stdout, os.Stderr))
	return nil
}

// probeArgs is runProbe without the exit, so a test can drive every answer.
func probeArgs(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "tcp" {
		fmt.Fprintln(stderr, "usage: s5n probe tcp --address <host:port> --want open|closed [--timeout 5s] [--settle 30s]")
		return exitBroken
	}
	fs := flag.NewFlagSet("probe tcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("address", "", "the host:port to dial")
	want := fs.String("want", "", "open or closed")
	timeout := fs.Duration("timeout", 5*time.Second, "how long one connection attempt may take")
	settle := fs.Duration("settle", 0, "keep dialling until --want holds several times in a row, up to this long (0: one dial)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitBroken
	}
	if strings.TrimSpace(*address) == "" {
		fmt.Fprintln(stderr, "s5n probe: no --address: name the host:port to dial")
		return exitBroken
	}
	wantOpen, err := parseWant(*want)
	if err != nil {
		fmt.Fprintf(stderr, "s5n probe: %v\n", err)
		return exitBroken
	}
	reachable := settled(*address, *timeout, *settle, wantOpen)
	if reachable == wantOpen {
		fmt.Fprintf(stdout, "%s: %s, as asked\n", *address, state(reachable))
		return exitAsAsked
	}
	fmt.Fprintf(stdout, "%s: %s, and --want %s\n", *address, state(reachable), *want)
	return exitOpposite
}

// settleRun is how many dials in a row must give the wanted answer before a
// settling probe believes it, and settleEvery is the pause between them.
const (
	settleRun   = 3
	settleEvery = 500 * time.Millisecond
)

// settled is the probe's answer: one dial without a settle period, and with one
// the wanted answer held settleRun times in a row -- or, when the deadline
// passes first, the other answer.
func settled(address string, timeout, settle time.Duration, wantOpen bool) bool {
	got := reachable(address, timeout)
	if settle <= 0 {
		return got
	}
	deadline := time.Now().Add(settle)
	run := 0
	for {
		if got == wantOpen {
			run++
			if run >= settleRun {
				return got
			}
		} else {
			run = 0
		}
		if !time.Now().Before(deadline) {
			// The deadline passed before the wanted answer held long enough to
			// believe, which is the other answer.
			return !wantOpen
		}
		time.Sleep(settleEvery)
		got = reachable(address, timeout)
	}
}

// reachable reports whether a TCP connection opened. Any error — refused,
// timed out, no route, a name that does not resolve — is a connection that did
// not open, which is the answer a NetworkPolicy denying egress produces.
func reachable(address string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func parseWant(want string) (bool, error) {
	switch strings.TrimSpace(want) {
	case "open":
		return true, nil
	case "closed":
		return false, nil
	}
	return false, fmt.Errorf("--want %q: name open or closed", want)
}

func state(reachable bool) string {
	if reachable {
		return "opened"
	}
	return "did not open"
}
