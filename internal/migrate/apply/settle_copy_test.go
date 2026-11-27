package apply

// settle_copy_test.go proves what the read-only pass copies and where: the state a
// check reads, out of an app whose config is a library as well, and into the
// run's own working directory -- never the system's temporary directory, which on
// a Fedora or an Arch host is a tmpfs.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheCopyACheckReadsIsTheStateAndNotItsLibrary(t *testing.T) {
	src := t.TempDir()
	home := filepath.Join(src, "Library", "Application Support", "Plex Media Server")
	for rel, body := range map[string]string{
		"Preferences.xml": "<Preferences/>\n",
		"Plug-in Support/Databases/com.plexapp.plugins.library.db": "sqlite\n",
		"Metadata/Posters/poster.jpg":                              strings.Repeat("j", 4096),
		"Cache/PhotoTranscoder/thumb.jpg":                          strings.Repeat("c", 4096),
	} {
		at := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file beside Plex's home, which is the source's mount and not Plex's: a
	// check reads nothing of it, and a read that copied it would copy the mount.
	if err := os.WriteFile(filepath.Join(src, "not-plex.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	s := &settler{in: SettleInput{Catalog: rehearsalCatalog}, dir: work, root: anchorFor(t, work)}
	dst := filepath.Join(s.dir, "plex")
	if err := s.stageForCheck(appPlex, "plex", src); err != nil {
		t.Fatalf("the copy a check reads: %v", err)
	}
	const where = "Library/Application Support/Plex Media Server"
	for _, kept := range []string{"Preferences.xml", "Plug-in Support/Databases/com.plexapp.plugins.library.db"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(where), filepath.FromSlash(kept))); err != nil {
			t.Errorf("the copy holds no %s: %v", kept, err)
		}
	}
	// The library and the mount beside Plex's home are what the copy does not
	// hold: the state is where the app reads it, and nothing else travels.
	for _, left := range []string{"Metadata", "Cache"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(where), left)); !os.IsNotExist(err) {
			t.Errorf("the copy holds %s, which no check reads: %v", left, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "not-plex.txt")); !os.IsNotExist(err) {
		t.Errorf("the copy holds a file beside Plex's home: %v", err)
	}

	// Every other app's config is its state: its whole directory is the copy.
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "config.xml"), []byte("<Config/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(s.dir, "sonarr")
	if err := s.stageForCheck("sonarr", "sonarr", plain); err != nil {
		t.Fatalf("the copy a check reads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "config.xml")); err != nil {
		t.Errorf("an app whose config is its state was not copied whole: %v", err)
	}
}

func TestSettleRefusesNoWorkingDirectory(t *testing.T) {
	r := newRehearsal(t, "", true)
	in := r.settleInput()
	in.Work = ""
	_, _, err := Settle(context.Background(), r.plan(), in)
	if err == nil || !strings.Contains(err.Error(), "working copy") {
		t.Errorf("Settle with no working directory said %v, want a refusal naming it", err)
	}
}

func TestSettleCopiesBesideTheSnapshotAndNotInTmp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory it cannot write, so the read-only $TMPDIR would prove nothing")
	}
	r := newRehearsal(t, "", true)
	// Every directory this test needs is made before $TMPDIR is taken away:
	// testing.TempDir is a temporary directory as well.
	in := r.settleInput()
	readOnly := filepath.Join(t.TempDir(), "no-tmp")
	if err := os.Mkdir(readOnly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", readOnly)
	if _, _, err := Settle(context.Background(), r.plan(), in); err != nil {
		t.Fatalf("Settle with a read-only $TMPDIR: %v", err)
	}
	entries, err := os.ReadDir(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the pass left %d entries in the system's temporary directory, want none", len(entries))
	}
}
