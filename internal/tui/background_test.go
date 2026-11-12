//go:build linux

package tui

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests need a terminal, because the query is about one: it puts the
// descriptor in raw mode, polls it and reads it. A pseudo-terminal is the only
// thing that behaves like the wire box talks to, so the tests open one and play
// the terminal on the master side.
//
// The build tag is for the pseudo-terminal, not the query: /dev/ptmx and the
// ioctls that name its slave are Linux's. The test's own reads go through the
// same poll the query uses, because a pseudo-terminal is not one of the files
// os.File can put a deadline on.

// ptyPair is a terminal: master is what the terminal emulator would be, slave is
// box's end of the wire (one descriptor, both streams, as a real run has).
func ptyPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		ptmx.Close()
		t.Fatalf("unlock the new terminal: %v", err)
	}
	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		ptmx.Close()
		t.Fatalf("name the new terminal: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		t.Fatalf("open the terminal's slave: %v", err)
	}
	t.Cleanup(func() {
		ptmx.Close()
		slave.Close()
	})
	return ptmx, slave
}

// waitReadable polls fd for at most timeout and reports whether there are bytes
// to read.
func waitReadable(fd int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return false
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			return true
		}
		return false
	}
}

// readExactly reads n bytes from fd, or fails the test.
func readExactly(t *testing.T, fd int, n int) []byte {
	t.Helper()
	buf := make([]byte, 0, n)
	deadline := time.Now().Add(5 * time.Second)
	for len(buf) < n {
		if !waitReadable(fd, time.Until(deadline)) {
			t.Fatalf("the terminal answered %d of %d bytes: %q", len(buf), n, buf)
		}
		chunk := make([]byte, n-len(buf))
		read, err := unix.Read(fd, chunk)
		if err != nil {
			t.Fatalf("read the terminal: %v", err)
		}
		buf = append(buf, chunk[:read]...)
	}
	return buf
}

// respond plays the terminal: it reads exactly the query box writes and answers
// with reply. The read is exact so a reply written by the test cannot be taken
// for the query itself.
func respond(t *testing.T, master *os.File, reply string) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		query := readExactly(t, int(master.Fd()), len(oscBackgroundQuery+da1Query))
		if string(query) != oscBackgroundQuery+da1Query {
			t.Errorf("box asked %q, want the colour request fenced by DA1", query)
			return
		}
		if _, err := master.WriteString(reply); err != nil {
			t.Errorf("answer the query: %v", err)
		}
	}()
	return done
}

// silent plays a terminal that answers neither request.
func silent(t *testing.T, master *os.File) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		readExactly(t, int(master.Fd()), len(oscBackgroundQuery+da1Query))
	}()
	return done
}

// empty asserts the terminal's input has nothing left: a reply box read and did
// not consume would be delivered to the first form as keystrokes.
func empty(t *testing.T, slave *os.File) {
	t.Helper()
	if waitReadable(int(slave.Fd()), 50*time.Millisecond) {
		buf := make([]byte, 16)
		n, _ := unix.Read(int(slave.Fd()), buf)
		t.Fatalf("%d bytes were left in the terminal's input: %q", n, buf[:n])
	}
}

// A terminal that answers OSC 11 and then the fence: the colour decides the
// palette, and both replies are consumed.
func TestQueryBackgroundUsesTheColourBeforeTheFence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply string
		dark  bool
	}{
		{"dark, ST", "\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\\x1b[?62;22c", true},
		{"light, ST", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\\x1b[?62;22c", false},
		{"light, BEL", "\x1b]11;rgb:f/f/f\x07\x1b[?62c", false},
		{"dark, short form", "\x1b]11;rgb:0000/0000/0000\x07\x1b[?1;2c", true},
		{"light, one digit", "\x1b]11;rgb:e/e/e\x1b\\\x1b[?62c", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := ptyPair(t)
			done := respond(t, master, tc.reply)
			start := time.Now()
			got := queryBackground(slave, slave)
			elapsed := time.Since(start)
			<-done
			if got != tc.dark {
				t.Fatalf("queryBackground = %v, want dark=%v for %q", got, tc.dark, tc.reply)
			}
			if elapsed > 200*time.Millisecond {
				t.Fatalf("an answering terminal took %s", elapsed)
			}
			empty(t, slave)
		})
	}
}

// A terminal that answers the fence and not OSC 11 -- PuTTY, KiTTY, GNU screen,
// mosh -- says so at once, and box does not wait for a colour it is not going to
// get.
func TestQueryBackgroundTakesDA1AsNoAnswer(t *testing.T) {
	master, slave := ptyPair(t)
	done := respond(t, master, "\x1b[?62;22c")
	start := time.Now()
	got := queryBackground(slave, slave)
	elapsed := time.Since(start)
	<-done
	if !got {
		t.Fatal("a terminal that answered only the fence was read as light")
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("DA1 alone took %s, want well under 100ms", elapsed)
	}
	empty(t, slave)
}

// A terminal that answers neither is the one case capped by the timeout, and the
// cap leaves nothing behind: the read happens on this goroutine, after a poll,
// so there is no reader holding the terminal when the query gives up.
func TestQueryBackgroundCapsATerminalThatNeverAnswers(t *testing.T) {
	master, slave := ptyPair(t)
	done := silent(t, master)
	before := runtime.NumGoroutine()
	start := time.Now()
	got := queryBackground(slave, slave)
	elapsed := time.Since(start)
	<-done
	if !got {
		t.Fatal("a silent terminal was read as light")
	}
	if elapsed < backgroundTimeout-100*time.Millisecond || elapsed > backgroundTimeout+500*time.Millisecond {
		t.Fatalf("a silent terminal took %s, want about the %s cap", elapsed, backgroundTimeout)
	}
	// The reader is the caller: when the query returns, nothing is left reading
	// the descriptor and nothing is left of the terminal's mode to restore.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("the query left %d goroutines behind", after-before)
	}
	raw, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("read the terminal's mode: %v", err)
	}
	if raw.Lflag&unix.ECHO == 0 {
		t.Fatal("the terminal was left in raw mode: echo is still off")
	}
}

// The light answer reaches the palette, which is the whole reason box asks.
func TestALightTerminalGetsTheLightVariant(t *testing.T) {
	master, slave := ptyPair(t)
	done := respond(t, master, "\x1b]11;rgb:ffff/ffff/ffff\x1b\\\x1b[?62;22c")
	u := New(Options{In: slave, Out: slave, Theme: "s5n"})
	<-done
	if got := u.Palette().Name; got != "light" {
		t.Fatalf("a light terminal got the %q variant", got)
	}
}

// A variant name is not a question: a pinned variant is the operator's answer,
// and a box run pays no wait for one it is not going to use.
func TestAPinnedVariantDoesNotAskTheTerminal(t *testing.T) {
	master, slave := ptyPair(t)
	u := New(Options{In: slave, Out: slave, Theme: "s5n-light"})
	if got := u.Palette().Name; got != "light" {
		t.Fatalf("a pinned light variant resolved as %q", got)
	}
	if waitReadable(int(master.Fd()), 100*time.Millisecond) {
		buf := make([]byte, 16)
		n, _ := unix.Read(int(master.Fd()), buf)
		t.Fatalf("a pinned variant asked the terminal: %q", buf[:n])
	}
}

// The parsers on their own: the spellings terminals actually send, and the ones
// they do not.
func TestParseBackgroundColor(t *testing.T) {
	for _, tc := range []struct {
		value   string
		r, g, b uint8
		ok      bool
	}{
		{"rgb:0000/0000/0000", 0, 0, 0, true},
		{"rgb:ffff/ffff/ffff", 255, 255, 255, true},
		{"rgb:f/f/f", 255, 255, 255, true},
		{"rgb:0/0/0", 0, 0, 0, true},
		{"rgb:1e1e/2e2e/3e3e", 0x1e, 0x2e, 0x3e, true},
		{"rgba:ffff/0000/8080/ffff", 255, 0, 0x80, true},
		{"#ffffff", 255, 255, 255, true},
		{"#000", 0, 0, 0, true},
		{"rgb:1/2", 0, 0, 0, false},
		{"rgb:zz/00/00", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"garbage", 0, 0, 0, false},
	} {
		r, g, b, ok := parseBackgroundColor(tc.value)
		if ok != tc.ok || (ok && (r != tc.r || g != tc.g || b != tc.b)) {
			t.Errorf("parseBackgroundColor(%q) = %d,%d,%d,%v; want %d,%d,%d,%v", tc.value, r, g, b, ok, tc.r, tc.g, tc.b, tc.ok)
		}
	}
}

// The split-reply case: a terminal may send its answer in two reads, and the
// fence's byte may be the first of the second.
func TestReplyScannerTakesASplitReply(t *testing.T) {
	var reply backgroundReply
	var scan replyScanner
	for _, part := range []string{"\x1b]11;rgb:ff", "ff/0000/0000\x1b\\\x1b[?", "62;22c"} {
		if done := scan.feed([]byte(part), &reply); done {
			break
		}
	}
	if !reply.colour || reply.r != 255 || reply.g != 0 || reply.b != 0 {
		t.Fatalf("split reply = %+v, want red", reply)
	}
	if reply.dark() {
		t.Fatal("a red background was read as dark")
	}
}
