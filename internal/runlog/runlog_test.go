package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Where the log goes is one rule with three answers: root's directory, a
// user's state directory, and a directory the environment names, which wins
// because the e2e points it under its own work tree.
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

// A run log is root's or a user's and nobody else's: the file is 0600 and the
// directory box makes is 0700, because a transcript carries the box's own
// addresses.
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

// The log is the screen with timestamps on it: one line in, one line out, each
// with the same UTC stamp in front.
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

// A line that is only half written when the log closes is still a line: the
// transcript does not end mid-word.
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

// A registered secret never reaches the file, whatever line it turns up on --
// including the longest one, so a password that contains another secret is
// taken out whole.
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

// Colour is the screen's, not the log's: the log is plain text a person reads
// with less.
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

// One run's log stops at its cap, and the last line says why: a log that ends
// in the middle of a sentence is a log an operator cannot trust.
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

// The directory keeps the newest twenty and the newest 64 MiB, oldest first, so
// a box that checks every fifteen minutes does not fill /var/log.
func TestDirectoryIsPruned(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		path := filepath.Join(dir, "old-20260101T0000"+string(rune('a'+i))+"Z.log")
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
		name := "old-20260101T0000" + string(rune('a'+i)) + "Z.log"
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the prune", name)
		}
	}
}

// The size cap prunes by total, not only by count: twenty logs that are each
// 10 MiB are over the limit even though there are few of them.
func TestDirectoryIsPrunedBySize(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 10<<20)
	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, "big-0"+string(rune('a'+i))+".log")
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

// A directory that cannot be written never fails a command: the log comes back
// disabled, the reason is the caller's to say, and writes are accepted and
// dropped.
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
