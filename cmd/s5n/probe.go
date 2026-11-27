// probe.go is `s5n probe`: the one thing box runs inside a seedbox namespace to
// find out what the cluster's CNI does with a NetworkPolicy. The probe is the s5n
// image itself, so no new image is pulled.
//
// `tcp` is the client half: exit 0 when the address answered as --want asked, 3
// the other way, 4 could not run. --settle holds the wanted answer several times
// in a row, because a CNI programs a new pod's policy shortly after it starts.
//
// `listen` is the target half: it listens on --port until the pod is deleted, so
// a client that opens a connection reached it whatever the policies say.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
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
	if len(args) == 0 {
		probeUsage(stderr)
		return exitBroken
	}
	switch args[0] {
	case "tcp":
		return probeTCP(args[1:], stdout, stderr)
	case "listen":
		return probeListen(args[1:], stdout, stderr)
	}
	probeUsage(stderr)
	return exitBroken
}

func probeUsage(stderr io.Writer) {
	fmt.Fprintln(stderr, "usage: s5n probe tcp --address <host:port> --want open|closed [--timeout 5s] [--settle 30s]")
	fmt.Fprintln(stderr, "       s5n probe listen --port <port>")
}

func probeTCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe tcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("address", "", "the host:port to dial")
	want := fs.String("want", "", "open or closed")
	timeout := fs.Duration("timeout", 5*time.Second, "how long one connection attempt may take")
	settle := fs.Duration("settle", 0, "keep dialling until --want holds several times in a row, up to this long (0: one dial)")
	if err := fs.Parse(args); err != nil {
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

// probeListen accepts connections on one port until the pod is deleted, which is
// what makes an ingress proof possible: a client's dial opening when a
// deny-all policy selects this pod means the CNI ignores the policy.
func probeListen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 0, "the port to listen on")
	if err := fs.Parse(args); err != nil {
		return exitBroken
	}
	if *port <= 0 || *port > 65535 {
		fmt.Fprintln(stderr, "s5n probe: no --port: name the port to listen on")
		return exitBroken
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(*port))
	if err != nil {
		fmt.Fprintf(stderr, "s5n probe: listen on %d: %v\n", *port, err)
		return exitBroken
	}
	defer ln.Close()
	fmt.Fprintf(stdout, "listening on %d\n", *port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(stderr, "s5n probe: accept on %d: %v\n", *port, err)
			return exitBroken
		}
		conn.Close()
	}
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
