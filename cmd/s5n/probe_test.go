package main

// probe_test.go drives the exit codes `s5n probe` reports: they are the whole
// contract internal/netproof reads, so "as asked" and "the other way" must
// be distinguishable from "the probe could not run".

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestProbeExitCodes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	open := ln.Addr().String()
	defer ln.Close()

	// A port nothing listens on: the address is taken from a listener that is
	// then closed, which the kernel does not hand out again in this test.
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closed := gone.Addr().String()
	gone.Close()

	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		output string
	}{
		{"open as asked", []string{"tcp", "--address", open, "--want", "open"}, exitAsAsked, "opened, as asked"},
		{"open and closed was asked", []string{"tcp", "--address", open, "--want", "closed"}, exitOpposite, "opened, and --want closed"},
		{"closed as asked", []string{"tcp", "--address", closed, "--want", "closed"}, exitAsAsked, "did not open, as asked"},
		{"closed and open was asked", []string{"tcp", "--address", closed, "--want", "open"}, exitOpposite, "did not open, and --want open"},
		{"no address", []string{"tcp", "--want", "open"}, exitBroken, "no --address"},
		{"bad want", []string{"tcp", "--address", open, "--want", "maybe"}, exitBroken, "name open or closed"},
		{"not tcp", []string{"udp"}, exitBroken, "usage: s5n probe tcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := probeArgs(tc.args, &out, &errOut); got != tc.code {
				t.Errorf("exit = %d, want %d (stdout %q, stderr %q)", got, tc.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String()+errOut.String(), tc.output) {
				t.Errorf("output = %q / %q, want it to contain %q", out.String(), errOut.String(), tc.output)
			}
		})
	}
}

func TestProbeTreatsAnUnresolvableNameAsClosed(t *testing.T) {
	var out, errOut bytes.Buffer
	code := probeArgs([]string{"tcp", "--address", "does-not-exist.invalid:53", "--want", "closed", "--timeout", "1s"}, &out, &errOut)
	if code != exitAsAsked {
		t.Errorf("exit = %d, want %d: a name that does not resolve did not open a connection", code, exitAsAsked)
	}
}

// TestProbeListenIsTheTargetHalfOfANetworkProof: the listener answers a dial and
// keeps answering, because a proof's target outlives the client that dials it.
func TestProbeListenIsTheTargetHalfOfANetworkProof(t *testing.T) {
	port := freePort(t)
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- probeArgs([]string{"listen", "--port", port}, &out, &errOut) }()

	var conn net.Conn
	var err error
	for i := 0; i < 200; i++ {
		if conn, err = net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial the probe's listener: %v (stdout %q, stderr %q)", err, out.String(), errOut.String())
	}
	conn.Close()
	// Still listening: a second dial opens too, which is what a --settle run needs.
	again, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	again.Close()
	select {
	case code := <-done:
		t.Fatalf("the listener exited %d, want it to keep listening (stdout %q, stderr %q)", code, out.String(), errOut.String())
	case <-time.After(50 * time.Millisecond):
	}
}

// TestProbeListenRefusesWhatItCannotDo: no port is a refusal, and so is a port
// something else holds.
func TestProbeListenRefusesWhatItCannotDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		output string
	}{
		{"no port", []string{"listen"}, "no --port"},
		{"bad port", []string{"listen", "--port", "0"}, "no --port"},
		{"no such subcommand", []string{"listen-to"}, "usage: s5n probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := probeArgs(tc.args, &out, &errOut); code != exitBroken {
				t.Errorf("exit = %d, want %d (stdout %q, stderr %q)", code, exitBroken, out.String(), errOut.String())
			}
			if !strings.Contains(out.String()+errOut.String(), tc.output) {
				t.Errorf("output = %q / %q, want it to contain %q", out.String(), errOut.String(), tc.output)
			}
		})
	}

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()
	_, port, _ := net.SplitHostPort(held.Addr().String())
	var out, errOut bytes.Buffer
	if code := probeArgs([]string{"listen", "--port", port}, &out, &errOut); code != exitBroken {
		t.Errorf("exit = %d, want %d: the port was taken (stdout %q, stderr %q)", code, exitBroken, out.String(), errOut.String())
	}
}

// freePort is a port nothing holds, as a string: a listener is named then closed,
// which the kernel does not hand out again inside the test.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	return port
}

func TestProbeSettleWaitsForThePolicyToLand(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	// The "policy" lands after a second: the address stops answering.
	time.AfterFunc(time.Second, func() { ln.Close() })

	var out, errOut bytes.Buffer
	if code := probeArgs([]string{"tcp", "--address", addr, "--want", "closed", "--timeout", "300ms", "--settle", "10s"}, &out, &errOut); code != exitAsAsked {
		t.Fatalf("exit = %d, want %d: the address closed after a second and --settle should have waited for it (stdout %q, stderr %q)", code, exitAsAsked, out.String(), errOut.String())
	}

	// An address that never closes is the other answer once the deadline passes.
	still, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer still.Close()
	go func() {
		for {
			c, err := still.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	out.Reset()
	start := time.Now()
	if code := probeArgs([]string{"tcp", "--address", still.Addr().String(), "--want", "closed", "--timeout", "300ms", "--settle", "1500ms"}, &out, &errOut); code != exitOpposite {
		t.Fatalf("exit = %d, want %d: the address never closed", code, exitOpposite)
	}
	if time.Since(start) < 1400*time.Millisecond {
		t.Errorf("gave up after %s, before the 1.5s --settle deadline", time.Since(start))
	}
	if !strings.Contains(out.String(), "opened") {
		t.Errorf("output %q does not say the address opened", out.String())
	}
}
