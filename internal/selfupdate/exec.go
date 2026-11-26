package selfupdate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Rollback puts the binary box replaced back, once: the update keeps exactly one
// previous binary, and putting it back consumes it.
func Rollback(path string) error {
	previous := previousPath(path)
	if _, err := os.Stat(previous); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("there is no %s beside %s, so there is no previous box to go back to: box has not replaced itself here", filepath.Base(previous), path)
		}
		return fmt.Errorf("looking for the previous box at %s: %w", previous, err)
	}
	if err := os.Rename(previous, path); err != nil {
		return fmt.Errorf("putting %s back over %s: %w", previous, path, err)
	}
	_ = syncDir(filepath.Dir(path))
	return nil
}

// Reexec replaces this process with the box at path, so the update finishes
// under the version it installed. args is os.Args[1:]. version is a parameter
// because a process cannot read the version it is about to exec; it marks the
// new process (ReexecEnv) as already updated, which stops a loop.
func Reexec(path string, args, env []string, version string) error {
	if strings.TrimSpace(version) == "" {
		return errors.New("re-exec: no version to mark the new process with")
	}
	return execProcess(path, append([]string{path}, args...), withEnv(env, ReexecEnv, version))
}

// execProcess is syscall.Exec, a variable so a test can watch the arguments
// instead of becoming another process.
var execProcess = func(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}

// withEnv sets one variable, replacing any existing value: two entries for one
// name leave the winner to libc.
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && name == key {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}

// previousPath is where the binary an update replaced is kept.
func previousPath(path string) string {
	return path + ".previous"
}
