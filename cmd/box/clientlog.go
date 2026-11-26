package main

import (
	"bytes"
	"flag"
	"io"

	"k8s.io/klog/v2"

	"github.com/swizzin/s5n/internal/runlog"
)

// clientPrefix marks a line client-go wrote: the transcript is box's, and a
// line klog wrote is not.
const clientPrefix = "client-go: "

// pointClientLogs sends everything klog writes to l, never to a terminal:
// klog's default is stderr alone, for every severity (klog/v2 klog.go:441 at
// v2.140.0).
//
// LogToStderr(false) with SetOutput (klog.go:877, 857) writes to l instead and
// keeps klog's header and caller, but Error is still copied to stderr by the
// severity threshold (klog.go:933) and, without one_output, each line is
// written once per lower severity (klog.go:957). Both are flags only (klog.go:428).
// SetLogger is no use: it takes InfoS and ErrorS straight to the logger
// (klog.go:809) and drops the header.
func pointClientLogs(l *runlog.Log) {
	if l == nil {
		l = &runlog.Log{}
	}
	klog.LogToStderr(false)
	klog.SetOutput(&clientLines{w: l})
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("stderrthreshold", "FATAL")
	_ = fs.Set("one_output", "true")
}

// clientLines writes klog's bytes to the run log, clientPrefix in front of each
// line so a client-go line cannot read as box's.
type clientLines struct {
	w io.Writer
	// open is true while a line's prefix is written and its newline is not: the
	// log path may stop mid-line (klog/v2 klog.go:797).
	open bool
}

func (c *clientLines) Write(p []byte) (int, error) { //nolint:unparam // io.Writer's contract; klog reads neither result.
	n := len(p)
	var b bytes.Buffer
	for len(p) > 0 {
		if !c.open {
			b.WriteString(clientPrefix)
			c.open = true
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			b.Write(p)
			break
		}
		b.Write(p[:i+1])
		c.open = false
		p = p[i+1:]
	}
	// The run log caps and discards; it never fails a command.
	_, _ = c.w.Write(b.Bytes())
	return n, nil
}
