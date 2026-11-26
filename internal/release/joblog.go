package release

import (
	"bufio"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/swizzin/s5n/internal/say"
)

// JobLogs is what a ReleaseClient may also offer: the wiring Job's log, so the
// wait has a live line. The Job is a hook, not a release resource, so the client
// finds its pod itself.
type JobLogs interface {
	// JobLog follows from the beginning when follow is set, else reads what
	// stands. An error means nothing to read yet.
	JobLog(ctx context.Context, follow bool) (io.ReadCloser, error)
}

// maxJobLogLines bounds what a wait keeps; a wiring run prints about fifty.
const maxJobLogLines = 4000

// stopGrace bounds the log reader's stop, so a silent pod cannot hold box open.
const stopGrace = 5 * time.Second

// jobLog is the wiring Job's log while box waits: the newest line live, every
// line for the run log, read once in the background into memory.
//
// Nothing here cancels a request it made: client-go logs a cancelled body read
// through klog, and the run log is a transcript of what box did, not of what
// client-go retried. A wait that ends closes the stream instead.
type jobLog struct {
	open func(context.Context, bool) (io.ReadCloser, error)
	poll time.Duration
	ctx  context.Context
	end  chan struct{}

	mu      sync.Mutex
	lines   []string
	dropped int
	stream  io.ReadCloser

	done chan struct{}
	once sync.Once
}

// startJobLog follows the Job's log for one wait; a client that cannot gets
// nil, which every method tolerates.
func startJobLog(ctx context.Context, client ReleaseClient, poll time.Duration) *jobLog {
	src, ok := client.(JobLogs)
	if !ok {
		return nil
	}
	if poll <= 0 {
		poll = time.Second
	}
	j := &jobLog{
		open: src.JobLog,
		poll: poll,
		ctx:  ctx,
		end:  make(chan struct{}),
		done: make(chan struct{}),
	}
	go j.follow()
	return j
}

// newest is the newest line, read from memory, never a pod.
func (j *jobLog) newest() string {
	if j == nil {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.lines) == 0 {
		return ""
	}
	return j.lines[len(j.lines)-1]
}

// follow reads until the log ends or the wait stops it; no pod yet is not a
// fault, only the budget decides.
func (j *jobLog) follow() {
	defer close(j.done)
	for {
		if !j.running() {
			return
		}
		rc, err := j.open(j.ctx, true)
		if err != nil {
			if !j.pause() {
				return
			}
			continue
		}
		j.setStream(rc)
		// A wait that ended while opening closes the stream instead of a
		// context.
		if !j.running() {
			j.clearStream(rc)
			_ = rc.Close()
			return
		}
		j.read(rc)
		j.clearStream(rc)
		_ = rc.Close()
		// The pod finished. A retried Job starts another; the receipt is what the
		// wait is for, and following again would repeat lines.
		return
	}
}

// running is false once the wait has stopped or its context ended.
func (j *jobLog) running() bool {
	select {
	case <-j.end:
		return false
	case <-j.ctx.Done():
		return false
	default:
		return true
	}
}

// pause waits one poll interval. It is false when the wait ended.
func (j *jobLog) pause() bool {
	t := time.NewTimer(j.poll)
	defer t.Stop()
	select {
	case <-j.end:
		return false
	case <-j.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// setStream remembers the stream being read, so stopping can end it.
func (j *jobLog) setStream(rc io.ReadCloser) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stream = rc
}

func (j *jobLog) clearStream(rc io.ReadCloser) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stream == rc {
		j.stream = nil
	}
}

// read appends every line, trimmed of the Job's right padding.
func (j *jobLog) read(rc io.ReadCloser) {
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		j.mu.Lock()
		if len(j.lines) < maxJobLogLines {
			j.lines = append(j.lines, line)
		} else {
			j.dropped++
		}
		j.mu.Unlock()
	}
}

// stop ends the reader and waits for it, so the run log gets the whole log. A
// wait can end at once, before the reader ran, so an unread log is read once
// here.
func (j *jobLog) stop() {
	if j == nil {
		return
	}
	j.once.Do(func() { close(j.end) })
	j.mu.Lock()
	stream := j.stream
	j.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
	select {
	case <-j.done:
	case <-time.After(stopGrace):
	}
	if !j.empty() {
		return
	}
	// The wait's context is over: read once, not followed, and still bounded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(j.ctx), stopGrace)
	defer cancel()
	rc, err := j.open(ctx, false)
	if err != nil {
		return
	}
	defer rc.Close()
	j.read(rc)
}

// empty says whether nothing has been read yet.
func (j *jobLog) empty() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.lines) == 0
}

// logTo writes the Job's log into the run log, where a person reads it after
// the live line stopped moving.
func (j *jobLog) logTo(r say.Reporter) {
	if j == nil || r == nil {
		return
	}
	j.mu.Lock()
	lines := append([]string(nil), j.lines...)
	dropped := j.dropped
	j.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	r.Detail("wiring Job log:")
	for _, line := range lines {
		r.Detail("  " + line)
	}
	if dropped > 0 {
		r.Detail("  " + plural(dropped, "line", "lines") + " more, not kept")
	}
}
