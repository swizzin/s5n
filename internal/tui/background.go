package tui

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// box's own terminal-background query.
//
// box needs one bit from the terminal at startup: is the ground it is drawing
// on dark or light. lipgloss answers it by asking for OSC 11 and waiting two
// seconds per stream -- and trying both streams, so four seconds on a terminal
// that never answers -- with a goroutine that cancels the read. PuTTY, KiTTY,
// GNU screen and mosh never answer OSC 11 at all, so on those every run paid the
// whole wait before it drew anything.
//
// This is the same question, fenced the way terminal-colorsaurus asks it: the
// colour request is written immediately followed by a primary device attributes
// request (DA1), which every terminal answers. An OSC 11 reply that arrives
// before the DA1 reply is the colour; a DA1 reply first means "this terminal
// does not answer OSC 11", and the answer is dark at once. The fence is what
// turns "no answer" from a timeout into a reply.
//
// A terminal that answers neither is the one case left, and it is capped at one
// second for the whole query -- not per stream, and not per read. The cap is a
// poll on the descriptor with the time left: the read is only ever issued when
// the poll says there are bytes, so no read outlives the cap, and the query runs
// on the calling goroutine, so there is no reader left behind either. The
// terminal's mode and the descriptor's flags are restored on every path.
const (
	// oscBackgroundQuery is OSC 11 with an ST terminator: "what is your background
	// colour?".
	oscBackgroundQuery = "\x1b]11;?\x1b\\"
	// da1Query is the fence. It is written in the same write as the colour
	// request, so the two replies are ordered by the terminal's own queue and a
	// DA1 that arrives first cannot be a reply to something box sent later.
	da1Query = "\x1b[c"
	// backgroundTimeout is the whole query's budget: one second for a terminal
	// that answers neither request.
	backgroundTimeout = time.Second
)

// The bytes the reply scanner looks for.
const (
	esc = 0x1b
	bel = 0x07
)

// backgroundReply is what the terminal said before the fence closed.
type backgroundReply struct {
	// colour says an OSC 11 reply arrived, and r, g and b are that colour.
	colour  bool
	r, g, b uint8
}

// dark is the one bit box wanted. A terminal that did not answer is dark: light
// text on a light ground is unreadable, while a dark palette on a light terminal
// is only not what was asked for.
//
// The threshold is HSL's lightness, (max+min)/2, below half -- the same rule
// lipgloss applied to the same colour, so a terminal that answers gets the
// variant it always did.
func (b backgroundReply) dark() bool {
	if !b.colour {
		return true
	}
	hi, lo := max(b.r, b.g, b.b), min(b.r, b.g, b.b)
	return int(hi)+int(lo) < 255
}

// queryBackground writes the fenced query and reads the answer. It reports dark
// for everything it cannot learn: not a terminal, raw mode refused, a write that
// failed, a reply that does not parse.
func queryBackground(in, out *os.File) bool {
	fd := int(in.Fd())
	// A pipe or a file has no mode to change and nothing that answers: the
	// question is only worth asking of a terminal.
	if !term.IsTerminal(fd) || !term.IsTerminal(int(out.Fd())) {
		return true
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return true
	}
	defer func() { _ = term.Restore(fd, state) }()

	// Non-blocking reads, restored afterwards: with them a read cannot block
	// even if the terminal hangs up between the poll and the read, which is
	// what makes the cap one second rather than "one second, usually".
	flags, err := unix.FcntlInt(in.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return true
	}
	if _, err := unix.FcntlInt(in.Fd(), unix.F_SETFL, flags|unix.O_NONBLOCK); err != nil {
		return true
	}
	defer func() { _, _ = unix.FcntlInt(in.Fd(), unix.F_SETFL, flags) }()

	if _, err := io.WriteString(out, oscBackgroundQuery+da1Query); err != nil {
		return true
	}
	return readBackground(fd).dark()
}

// readBackground reads until the DA1 fence closes or the cap fires. A colour
// that arrived stays in the answer when a terminal sends the colour but never
// the fence.
func readBackground(fd int) backgroundReply {
	var (
		reply backgroundReply
		scan  replyScanner
		buf   [64]byte
	)
	deadline := time.Now().Add(backgroundTimeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return reply
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return reply
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			// HUP, ERR or NVAL: nothing more is coming from this terminal.
			return reply
		}
		read, err := unix.Read(fd, buf[:])
		switch {
		case read > 0:
			if scan.feed(buf[:read], &reply) {
				return reply
			}
		case read == 0 && err == nil:
			return reply
		}
		if err != nil && err != unix.EAGAIN && err != unix.EINTR {
			return reply
		}
	}
}

// The reply scanner's states. It reads byte by byte, so a reply split across
// reads is still one reply.
const (
	stGround = iota
	stEsc
	stOsc
	stOscEsc
	stCSI
	stCSIQ
)

// replyScanner finds the two replies box asked for in whatever the terminal
// sends, ignoring anything else (a keystroke typed during startup).
type replyScanner struct {
	state int
	osc   []byte
}

// feed consumes one read's bytes and reports whether the fence closed.
func (s *replyScanner) feed(p []byte, reply *backgroundReply) bool {
	for _, c := range p {
		switch s.state {
		case stGround:
			if c == esc {
				s.state = stEsc
			}
		case stEsc:
			switch c {
			case ']':
				s.state, s.osc = stOsc, s.osc[:0]
			case '[':
				s.state = stCSI
			case esc:
				// A run of escapes: stay ready for the one that starts a
				// sequence box understands.
			default:
				s.state = stGround
			}
		case stOsc:
			switch c {
			case bel:
				s.finishOSC(reply)
				s.state = stGround
			case esc:
				s.state = stOscEsc
			default:
				s.osc = append(s.osc, c)
			}
		case stOscEsc:
			if c == '\\' {
				// ST: the other terminator Authelia's terminals use for OSC.
				s.finishOSC(reply)
				s.state = stGround
				continue
			}
			// Not a string terminator: drop the sequence and start over.
			s.osc = s.osc[:0]
			s.state = stGround
		case stCSI:
			switch {
			case c == '?':
				s.state = stCSIQ
			case c >= 0x40 && c <= 0x7e:
				s.state = stGround // some other CSI's final byte
			}
		case stCSIQ:
			switch {
			case c == 'c':
				return true // DA1: the fence, and the answer if none came first
			case c >= 0x40 && c <= 0x7e:
				s.state = stGround
			}
		}
	}
	return false
}

// finishOSC reads a completed OSC payload: "11;rgb:..." is a background reply,
// and anything else is a sequence box did not ask for.
func (s *replyScanner) finishOSC(reply *backgroundReply) {
	payload := string(s.osc)
	s.osc = s.osc[:0]
	value, ok := strings.CutPrefix(payload, "11;")
	if !ok {
		return
	}
	r, g, b, ok := parseBackgroundColor(value)
	if !ok {
		return
	}
	reply.colour, reply.r, reply.g, reply.b = true, r, g, b
}

// parseBackgroundColor reads one OSC 11 value. It takes rgb:RRRR/GGGG/BBBB, the
// short forms (rgb:f/f/f), rgba: with an alpha box ignores, and #rrggbb.
//
// X11 lets each component be one to four hex digits and scales it to the full
// range, so "f" and "ffff" are both white. Shifting instead of scaling would
// read xterm's own short form "rgb:f/f/f" as near-black, which is the difference
// between the light variant and the unreadable one.
func parseBackgroundColor(v string) (r, g, b uint8, ok bool) {
	v = strings.TrimSpace(v)
	switch {
	case strings.HasPrefix(v, "rgb:"):
		return parseChannels(v[len("rgb:"):], 3)
	case strings.HasPrefix(v, "rgba:"):
		return parseChannels(v[len("rgba:"):], 4)
	case strings.HasPrefix(v, "#"):
		switch len(v) - 1 {
		case 3: // #rgb, one digit per channel
			channels := make([]uint8, 3)
			for i := 0; i < 3; i++ {
				c, ok := hexChannel(string(v[1+i]))
				if !ok {
					return 0, 0, 0, false
				}
				channels[i] = c
			}
			return channels[0], channels[1], channels[2], true
		case 6: // #rrggbb
			channels := make([]uint8, 3)
			for i := 0; i < 3; i++ {
				c, ok := hexChannel(v[1+2*i : 3+2*i])
				if !ok {
					return 0, 0, 0, false
				}
				channels[i] = c
			}
			return channels[0], channels[1], channels[2], true
		}
	}
	return 0, 0, 0, false
}

// parseChannels splits "/"-separated components, needing at least want of them.
func parseChannels(v string, want int) (r, g, b uint8, ok bool) {
	parts := strings.Split(v, "/")
	if len(parts) < want {
		return 0, 0, 0, false
	}
	values := make([]uint8, 3)
	for i := 0; i < 3; i++ {
		c, ok := hexChannel(parts[i])
		if !ok {
			return 0, 0, 0, false
		}
		values[i] = c
	}
	return values[0], values[1], values[2], true
}

// hexChannel scales one to four hex digits to a byte.
func hexChannel(s string) (uint8, bool) {
	if len(s) == 0 || len(s) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	max := uint64(1)<<(4*uint(len(s))) - 1
	return uint8(v * 0xff / max), true
}
