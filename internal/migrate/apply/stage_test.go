package apply

// stage_test.go is the staging copy on its own: what it carries, what it
// refuses, and what it leaves exactly as it found it.
//
// The copy is deliberately generic (no per-app lists), so the properties here
// are the whole contract: a file keeps its mode and its mtime, a directory
// claiming to be read-only is still copied into, a link that stays inside the
// tree is a link in the copy, and anything that could reach outside it is
// refused by name.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCopyTreePreservesWhatAnAppReads(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "qBittorrent", "qBittorrent.conf"), "conf", 0o640)
	writeFile(t, filepath.Join(src, "logs", "app.log"), "line", 0o600)
	writeFile(t, filepath.Join(src, "readonly", "inside.txt"), "x", 0o400)
	if err := os.Chmod(filepath.Join(src, "readonly"), 0o500); err != nil {
		t.Fatal(err)
	}
	// A directory the fixture made read-only needs to be writable again before
	// the test's cleanup can remove it, in the source and in the copy.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "readonly"), 0o755) })
	if err := os.Symlink("qBittorrent.conf", filepath.Join(src, "qBittorrent", "relative-link")); err != nil {
		t.Fatal(err)
	}
	// An absolute link to a file inside the tree: copied as a relative link,
	// because the tree moves and the absolute text would name the source host.
	if err := os.Symlink(filepath.Join(src, "qBittorrent", "qBittorrent.conf"), filepath.Join(src, "absolute-link")); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "qBittorrent", "qBittorrent.conf"), stamp, stamp); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "copy")
	// The copy is as read-only as the source; the test's own cleanup needs the
	// directory writable again before it can remove it.
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, "readonly"), 0o755) })
	st, err := copyTree(dst, src, os.Geteuid(), os.Getegid())
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if st.Files != 3 {
		t.Errorf("copied %d files, want 3", st.Files)
	}
	if want := int64(len("conf") + len("line") + len("x")); st.Bytes != want {
		t.Errorf("copied %d bytes, want %d", st.Bytes, want)
	}

	for rel, want := range map[string]os.FileMode{
		"qBittorrent/qBittorrent.conf": 0o640,
		"logs/app.log":                 0o600,
		"readonly/inside.txt":          0o400,
		"readonly":                     0o500,
	} {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s is %o, want %o", rel, info.Mode().Perm(), want)
		}
	}
	info, err := os.Stat(filepath.Join(dst, "qBittorrent", "qBittorrent.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Errorf("mtime is %s, want %s", info.ModTime(), stamp)
	}
	if target, err := os.Readlink(filepath.Join(dst, "qBittorrent", "relative-link")); err != nil || target != "qBittorrent.conf" {
		t.Errorf("relative-link is %q (%v), want qBittorrent.conf", target, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "absolute-link")); err != nil || target != filepath.Join("qBittorrent", "qBittorrent.conf") {
		t.Errorf("absolute-link is %q (%v), want the relative form of the file it names", target, err)
	}
}

func TestCopyTreeRefusesWhatCanLeave(t *testing.T) {
	cases := []struct {
		name  string
		make  func(t *testing.T, src string)
		want  string
		unset bool
	}{
		{
			name: "a relative link out of the tree",
			make: func(t *testing.T, src string) {
				if err := os.MkdirAll(filepath.Join(src, "a"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../etc/passwd", filepath.Join(src, "a", "link")); err != nil {
					t.Fatal(err)
				}
			},
			want: "outside the config directory",
		},
		{
			name: "an absolute link out of the tree",
			make: func(t *testing.T, src string) {
				if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
					t.Fatal(err)
				}
			},
			want: "outside the config directory",
		},
		{
			name: "a named pipe",
			make: func(t *testing.T, src string) {
				if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
					t.Skipf("no fifo on this system: %v", err)
				}
			},
			want: "named pipe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			writeFile(t, filepath.Join(src, "keep.txt"), "keep", 0o600)
			tc.make(t, src)
			dst := filepath.Join(t.TempDir(), "copy")
			_, err := copyTree(dst, src, os.Geteuid(), os.Getegid())
			if err == nil {
				t.Fatal("the copy went ahead")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, err)
			}
		})
	}
}

// TestCopyTreeSkipsAKernelLink: the images point their log at the container's
// own stdout, so every qBittorrent config directory a real host holds carries a
// link into /proc. That is the pod's plumbing, not a file in the tree: the copy
// leaves it out and the app writes it again on start. Refusing it made a real
// qBittorrent install uncopyable, which is how this was found (hack/e2e-box.sh,
// migrate).
func TestCopyTreeSkipsAKernelLink(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "qBittorrent", "qBittorrent.conf"), "conf", 0o600)
	logs := filepath.Join(src, "qBittorrent", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/self/fd/1", filepath.Join(logs, "qbittorrent.log")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := copyTree(dst, src, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatalf("a config whose log is the container's stdout was refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "qBittorrent", "logs", "qbittorrent.log")); !os.IsNotExist(err) {
		t.Errorf("the copy carries the /proc link (err %v): the app writes it again on start", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "qBittorrent", "qBittorrent.conf")); err != nil {
		t.Errorf("the rest of the tree did not come across: %v", err)
	}
}

// TestCopyTreeChownsToTheRelease: every entry belongs to the release's uid, not
// to whoever ran box.
func TestCopyTreeChownsToTheRelease(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a", "b.txt"), "b", 0o600)
	dst := filepath.Join(t.TempDir(), "copy")
	uid, gid := os.Geteuid(), os.Getegid()
	if _, err := copyTree(dst, src, uid, gid); err != nil {
		t.Fatalf("copy: %v", err)
	}
	for _, rel := range []string{"", "a", "a/b.txt"} {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no stat_t on this system")
		}
		if int(st.Uid) != uid || int(st.Gid) != gid {
			t.Errorf("%s is owned %d:%d, want %d:%d", rel, st.Uid, st.Gid, uid, gid)
		}
	}
}
