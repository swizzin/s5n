// probe.go is `s5n probe`: the one thing box runs inside a seedbox namespace to
// find out whether the CNI enforces the release's NetworkPolicy. The probe is the
// s5n image itself, so no new image is pulled.
//
// Exit codes are the answer: 0 as --want asked, 3 the other way, 4 could not run.
// --settle holds the wanted answer several times in a row, because a CNI
// programs a new pod's policy shortly after it starts. A probe that cannot run is
// a failed proof, not a lock that does not hold.
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

// The exit codes above. They are part of the contract with internal/netproof.
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
