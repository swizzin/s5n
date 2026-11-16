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

// Rollback puts the binary box replaced back, once.
//
// One step back is the whole promise: the update keeps exactly one previous
// binary, and putting it back consumes it. A rollback that could go further
// would need a history box does not keep, and an operator who steps back twice
// has a release problem to read rather than a third binary to try.
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

// Reexec replaces this process with the box binary at path, so the update that
// installed it finishes under the version it installed.
//
// args are the process's arguments without the program name, as os.Args[1:]
// carries them: the caller passes the same arguments it was started with, so
// the command that triggered the update runs again under the new binary.
//
// version is a parameter because a process cannot read the version of the
// binary it is about to exec; the caller has it from the update it just did.
// It marks the new process (ReexecEnv) as already updated, which is what stops
// a release whose page still names something newer from looping.
func Reexec(path string, args, env []string, version string) error {
	if strings.TrimSpace(version) == "" {
		return errors.New("re-exec: no version to mark the new process with")
	}
	return execProcess(path, append([]string{path}, args...), withEnv(env, ReexecEnv, version))
}

// execProcess is syscall.Exec, which does not return on success. It is a
// variable so a test can watch the arguments instead of becoming another
// process, and so the test binary is not replaced by the box it tests.
var execProcess = func(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}

// withEnv sets one variable in an environment, replacing any value already
// there. Appending without removing would leave the process with two entries
// for one name, and which of them wins is not something to leave to libc.
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
