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

// JobLogs is what a ReleaseClient may also offer: the wiring Job's log. A
// client that does not offer it still waits for the receipt; the wait just has
// no line to show.
//
// The Job is a Helm hook, so it is not a release resource and Helm knows
// nothing about its pod. An implementation finds the pod itself, which is why
// this is the client's job and not the wait's.
type JobLogs interface {
	// JobLog returns the wiring Job's log: followed from the beginning when
	// follow is set, so box reads each line once as the Job writes it, or as it
	// stands and then the end, which is what a copy taken afterwards reads. An
	// error means there is nothing to read (yet): the pod does not exist, or
	// has not started.
	JobLog(ctx context.Context, follow bool) (io.ReadCloser, error)
}

// maxJobLogLines bounds what one wait keeps of the Job's log for the run log. A
// wiring run prints some fifty lines, so a bound this wide is a runaway log
// rather than a wiring run.
const maxJobLogLines = 4000

// jobLog is the wiring Job's log while box waits for its receipt: the newest
// line for the live line, and every line kept for the run log.
//
// The stream is opened once, in the background, and read into memory: the
// newest line is a field, not a request, so a pod that is not there, is
// restarting, or is gone costs the screen nothing, and the wait's own budget is
// never spent on a log.
type jobLog struct {
	open   func(context.Context, bool) (io.ReadCloser, error)
	poll   time.Duration
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	lines   []string
	dropped int

	done chan struct{}
}

// startJobLog follows the wiring Job's log for one wait. A client that cannot
// follow one gets nil, which every method here tolerates: the wait is the same
// wait, and silence is what box had before.
func startJobLog(ctx context.Context, client ReleaseClient, poll time.Duration) *jobLog {
	src, ok := client.(JobLogs)
	if !ok {
		return nil
	}
	if poll <= 0 {
		poll = time.Second
	}
	runCtx, cancel := context.WithCancel(ctx)
	j := &jobLog{open: src.JobLog, poll: poll, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go j.follow(runCtx)
	return j
}

// newest is the newest line the Job has written, for the live line. It reads
// memory and never a pod.
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

// follow reads the Job's log until it ends or ctx is cancelled. A pod that is
// not there yet is not a fault: the Job may still be scheduling, and the wait's
// budget is what says how long box gives it.
func (j *jobLog) follow(ctx context.Context) {
	defer close(j.done)
	for ctx.Err() == nil {
		rc, err := j.open(ctx, true)
		if err != nil {
			if !pause(ctx, j.poll) {
				return
			}
			continue
		}
		j.read(ctx, rc)
		_ = rc.Close()
		// The pod that wrote the log box was waiting on has finished. A Job
		// that retried starts another pod, and the receipt -- not the log -- is
		// what this wait is for; following the same pod again would only add
		// the lines already read.
		return
	}
}

// read appends every line the stream carries. A line is trimmed of the padding
// the Job's own columns add on the right, because this text goes into box's
// lines, not into a table of the Job's.
func (j *jobLog) read(ctx context.Context, rc io.ReadCloser) {
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

// stop waits for the reader to end. Cancelling its context is what ends a
// stream still being read; this is the wait for it, so the run log's copy of
// the Job's log is the whole log rather than the part that happened to arrive.
func (j *jobLog) stop() {
	if j == nil {
		return
	}
	j.cancel()
	<-j.done
	// A wait can end in the same breath it began -- a receipt that was already
	// published -- and the reader may not have been scheduled at all. Reading
	// once here is what makes the run log's copy deterministic rather than a
	// race with the scheduler.
	if !j.empty() {
		return
	}
	// The wait's log context is cancelled by now, and a followed log would wait
	// for a line that may never come: this read is its own, not followed, and
	// still bounded, so a Job that stopped answering cannot hold the command.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(j.ctx), finalRead)
	defer cancel()
	rc, err := j.open(ctx, false)
	if err != nil {
		return
	}
	defer rc.Close()
	j.read(ctx, rc)
}

// finalRead bounds the one read a wait that never sampled does, so a job still
// running cannot hold the command open: the stream ends at the deadline.
const finalRead = 5 * time.Second

// empty says whether nothing has been read yet.
func (j *jobLog) empty() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.lines) == 0
}

// logTo writes the Job's log into the run log. The screen showed one line at a
// time; this is what a person reads when that line stopped moving.
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

// pause waits one poll interval, or until ctx is done. It is false when the
// context ended, which is the one thing a retry loop has to tell apart from a
// plain wait.
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
