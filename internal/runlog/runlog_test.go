package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirChoice(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	home := func() (string, error) { return "/home/example", nil }
	cases := []struct {
		name string
		root bool
		env  map[string]string
		want string
	}{
		{"root's own directory", true, nil, RootDir},
		{"a user's state directory", false, nil, "/home/example/.local/state/box"},
		{"XDG_STATE_HOME when it is set", false, map[string]string{"XDG_STATE_HOME": "/state"}, "/state/box"},
		{"the environment's directory wins, root or not", true, map[string]string{DirEnv: "/tmp/work/logs"}, "/tmp/work/logs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dirFor(tc.root, env(tc.env), home)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("dir = %q, want %q", got, tc.want)
			}
		})
	}
}

// The transcript carries box's own addresses, so the file is 0600 and the
// directory 0700.
func TestLogIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "box")
	l, err := OpenAt(dir, "update")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Write([]byte("a line\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 700", got)
	}
	fi, err = os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %o, want 600", got)
	}
	if base := filepath.Base(l.Path()); !strings.HasPrefix(base, "update-") || !strings.HasSuffix(base, "Z.log") {
		t.Fatalf("log name = %q, want update-<UTC>.log", base)
	}
}

func TestLinesAreTimestamped(t *testing.T) {
	l, err := OpenAt(t.TempDir(), "apply")
	if err != nil {
		t.Fatal(err)
	}
	l.now = func() time.Time { return time.Date(2026, 11, 23, 7, 55, 0, 0, time.UTC) }
	if _, err := l.Write([]byte("wiring ok\n  sonarr: ok (created)\n")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	body := read(t, l.Path())
	want := "2026-11-23T07:55:00Z  wiring ok\n" +
		"2026-11-23T07:55:00Z    sonarr: ok (created)\n"
	if body != want {
		t.Fatalf("log:\n%q\nwant:\n%q", body, want)
	}
}

func TestPartialLineIsFinished(t *testing.T) {
	l, err := OpenAt(t.TempDir(), "status")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("one line, no newline")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if body := read(t, l.Path()); !strings.HasSuffix(body, "  one line, no newline\n") {
		t.Fatalf("log = %q", body)
	}
}

// The longest registered secret is replaced first, so a password containing
// another secret is taken out whole.
func TestSecretsNeverReachTheFile(t *testing.T) {
	l, err := OpenAt(t.TempDir(), "update")
	if err != nil {
		t.Fatal(err)
	}
	l.Register("hunter2-hunter2")
	l.Register("hunter2")
	l.Register("s3cret-token")
	if _, err := l.Write([]byte("password hunter2-hunter2 and token s3cret-token and hunter2\n")); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	body := read(t, l.Path())
	for _, secret := range []string{"hunter2", "s3cret-token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the log carries the secret %q:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, Redacted) {
		t.Fatalf("nothing was redacted:\n%s", body)
	}
}

func TestColourIsStripped(t *testing.T) {
	l, err := OpenAt(t.TempDir(), "apply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("\x1b[1;32m✓ applied\x1b[0m\n")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	body := read(t, l.Path())
	if !strings.HasSuffix(body, "  ✓ applied\n") || strings.Contains(body, "\x1b") {
		t.Fatalf("log = %q", body)
	}
}

func TestLogStopsAtItsCap(t *testing.T) {
	l, err := OpenAt(t.TempDir(), "apply")
	if err != nil {
		t.Fatal(err)
	}
	l.max = 200
	for i := 0; i < 20; i++ {
		if _, err := l.Write([]byte("a line of the transcript that takes up room\n")); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	body := read(t, l.Path())
	if !strings.Contains(body, "truncated") {
		t.Fatalf("the capped log does not say it stopped:\n%s", body)
	}
	if len(body) > 400 {
		t.Fatalf("the capped log is %d bytes, want about the cap", len(body))
	}
}

func TestDirectoryIsPruned(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		path := filepath.Join(dir, fmt.Sprintf("apply-20260101T0000%02dZ.log", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenAt(dir, "update"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != KeepFiles {
		t.Fatalf("the directory holds %d logs, want %d", len(entries), KeepFiles)
	}
	// The oldest six are the ones that went.
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("apply-20260101T0000%02dZ.log", i)
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the prune", name)
		}
	}
}

// The size cap prunes by total, not only by count.
func TestDirectoryIsPrunedBySize(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 10<<20)
	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, fmt.Sprintf("init-20260101T0000%02dZ.log", i))
		if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenAt(dir, "update"); err != nil {
		t.Fatal(err)
	}
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > KeepBytes {
		t.Fatalf("the directory holds %d bytes, want at most %d", total, KeepBytes)
	}
	if len(entries) > KeepFiles {
		t.Fatalf("the directory holds %d logs", len(entries))
	}
}

func TestAnUnwritableDirectoryIsOneLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	l, err := OpenAt(dir, "update")
	if err != nil && l == nil {
		t.Fatal("OpenAt returned no log at all")
	}
	if _, werr := l.Write([]byte("still runs\n")); werr != nil {
		t.Fatalf("a write to a log with nowhere to go failed the command: %v", werr)
	}
	if cerr := l.Close(); cerr != nil {
		t.Fatalf("Close failed the command: %v", cerr)
	}
	if os.Geteuid() == 0 {
		// Root can write a 0500 directory, so there is nothing to assert.
		return
	}
	if err == nil {
		t.Fatal("a directory with no write permission opened a log")
	}
	if l.Path() != "" {
		t.Fatalf("a log that could not be opened has a path: %q", l.Path())
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTwoRunsInOneSecondBothKeepTheirLog(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	name1, f1, err := create(dir, "apply", at)
	if err != nil {
		t.Fatal(err)
	}
	defer f1.Close()
	name2, f2, err := create(dir, "apply", at)
	if err != nil {
		t.Fatalf("the second run of the same command in the same second lost its log: %v", err)
	}
	defer f2.Close()
	if name1 == name2 {
		t.Fatalf("both runs are in %q: one overwrote the other", name1)
	}
	for _, name := range []string{name1, name2} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s is not on disk: %v", name, err)
		}
	}
}

func TestThePruneLeavesWhatBoxDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	foreign := []string{"syslog.log", "pacman.log", "apply.log", "apply-yesterday.log", "Xorg.0.log"}
	for _, name := range foreign {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 1<<20)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		path := filepath.Join(dir, fmt.Sprintf("apply-20260101T0000%02dZ.log", i))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenAt(dir, "update"); err != nil {
		t.Fatal(err)
	}
	for _, name := range foreign {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("the prune removed %s, which box did not write: %v", name, err)
		}
	}
	var mine int
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "apply-2026") || strings.HasPrefix(e.Name(), "update-") {
			mine++
		}
	}
	if mine != KeepFiles {
		t.Fatalf("box's own logs number %d after the prune, want %d", mine, KeepFiles)
	}
}

func TestTimerWakesNeverPushOutARealRun(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	apply := filepath.Join(dir, "apply-20260101T000000Z.log")
	if err := os.WriteFile(apply, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(apply, base, base); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 40; i++ {
		at := base.Add(time.Duration(i) * 15 * time.Minute)
		path := filepath.Join(dir, TimerCommand+"-"+at.Format(fileStamp)+".log")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenAt(dir, TimerCommand); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(apply); err != nil {
		t.Fatalf("forty timer wakes pushed out the apply's log: %v", err)
	}
	var wakes int
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), TimerCommand+"-") {
			wakes++
		}
	}
	if wakes != KeepTimer {
		t.Fatalf("the directory keeps %d timer wakes, want %d (this one included)", wakes, KeepTimer)
	}
}
