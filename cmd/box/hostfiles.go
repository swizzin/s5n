package main

import (
	"sync"

	"github.com/swizzin/s5n/internal/hostroot"
	"github.com/swizzin/s5n/internal/say"
)

// hostFilesOnce and hostFilesErr make the check once per process: `box
// bootstrap` runs init in this process, so a warning would otherwise appear
// twice and a refusal would be printed by whichever command got there first.
var (
	hostFilesOnce sync.Once
	hostFilesErr  error
)

// hostFiles is what every command that reads box's own files runs first: it
// refuses a host whose files are still where box kept them before 0.5, and
// names a copy that both roots have. It returns the error the command exits on.
func hostFiles(out say.Reporter, recordPath, grantPath string) error {
	hostFilesOnce.Do(func() {
		hostFilesErr = checkHostFiles(hostroot.OSStat, recordPath, grantPath, out)
	})
	return hostFilesErr
}

// checkHostFiles is hostFiles without the once, so a test drives it: the
// refusal when the old root has box's files and the new one does not, and one
// warning per file both roots have (the new one is read).
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
