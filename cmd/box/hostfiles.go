package main

import (
	"sync"

	"github.com/swizzin/s5n/internal/hostroot"
	"github.com/swizzin/s5n/internal/say"
)

// The check runs once per process: bootstrap runs init in-process.
var (
	hostFilesOnce sync.Once
	hostFilesErr  error
)

// hostFiles refuses files only at the old root and names one both roots have.
func hostFiles(out say.Reporter, recordPath, grantPath string) error {
	hostFilesOnce.Do(func() {
		hostFilesErr = checkHostFiles(hostroot.OSStat, recordPath, grantPath, out)
	})
	return hostFilesErr
}

// checkHostFiles is hostFiles without the once, for a test.
func checkHostFiles(stat hostroot.Stat, recordPath, grantPath string, out say.Reporter) error {
	stale, err := hostroot.Look(recordPath, grantPath, stat)
	if err != nil {
		return err
	}
	for _, warning := range stale.Warnings() {
		out.Warn(warning)
	}
	return stale.Err()
}
