package wiring

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func timeoutDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

// HardlinkCheck records the /data proof: one file, two names, one device.
type HardlinkCheck struct {
	OK         bool   `json:"ok"`
	Nlink      int    `json:"nlink"`
	SameDevice bool   `json:"sameDevice"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Error      string `json:"error,omitempty"`
}

const linkCheckName = ".s5n-linkcheck"

// CheckHardlink proves the data contract: a file written under the download tree and
// hardlinked into the media tree must show nlink 2 on the same device. Otherwise
// imports copy and the box fills twice as fast, so it is a hard failure.
func CheckHardlink(savePath, rootFolder string) *HardlinkCheck {
	out := &HardlinkCheck{
		Source: filepath.Join(savePath, linkCheckName),
		Target: filepath.Join(rootFolder, linkCheckName),
	}
	fail := func(format string, args ...any) *HardlinkCheck {
		out.Error = fmt.Sprintf(format, args...)
		return out
	}
	// Both directories are EnsureDataDirs'. The proof deliberately does not create
	// them: a check that prepares what it proves would hide the empty-path first
	// install this file exists to catch.
	_ = os.Remove(out.Source)
	_ = os.Remove(out.Target)
	defer func() {
		_ = os.Remove(out.Source)
		_ = os.Remove(out.Target)
	}()

	if err := os.WriteFile(out.Source, []byte("s5n\n"), 0o644); err != nil {
		return fail("write %s: %v", out.Source, err)
	}
	if err := os.Link(out.Source, out.Target); err != nil {
		return fail("link %s -> %s: %v", out.Source, out.Target, err)
	}
	src, err := os.Stat(out.Source)
	if err != nil {
		return fail("stat %s: %v", out.Source, err)
	}
	dst, err := os.Stat(out.Target)
	if err != nil {
		return fail("stat %s: %v", out.Target, err)
	}
	out.Nlink = int(dst.Sys().(*syscall.Stat_t).Nlink)
	out.SameDevice = src.Sys().(*syscall.Stat_t).Dev == dst.Sys().(*syscall.Stat_t).Dev
	out.OK = out.Nlink == 2 && out.SameDevice
	if !out.OK {
		return fail("nlink=%d sameDevice=%v", out.Nlink, out.SameDevice)
	}
	return out
}
