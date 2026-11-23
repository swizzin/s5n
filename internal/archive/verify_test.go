package archive

// verify_test.go proves the pass that reads an archive whole and writes nothing:
// what it checks, what it refuses, and that an extraction can write part of an
// archive and leave the rest for a later pass without losing the promise that a
// damaged copy stops the run.

import (
	"archive/tar"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestVerifyCountsWhatItSaw: the pass reports every entry as it checks it, which
// is how a caller counts a part of a snapshot it will write later without walking
// a tree that is not on the host.
func TestVerifyCountsWhatItSaw(t *testing.T) {
	path, _ := migrationArchive(t)
	var seen []Seen
	m, err := Verify(path, func(s Seen) { seen = append(seen, s) })
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if m.Mode != ModeFinal {
		t.Errorf("the manifest Verify returned says %q", m.Mode)
	}
	kinds := map[string]EntryKind{}
	sizes := map[string]int64{}
	for _, s := range seen {
		kinds[s.Name] = s.Kind
		sizes[s.Name] = s.Size
	}
	want := map[string]EntryKind{
		PlanName:                         EntryFile,
		ReportName:                       EntryFile,
		"config/qbittorrent":             EntryDir,
		"config/qbittorrent/qBittorrent": EntryDir,
		"config/qbittorrent/qBittorrent/relative-link": EntryLink,
	}
	for name, kind := range want {
		if got, ok := kinds[name]; !ok || got != kind {
			t.Errorf("%s was seen as %v, want %v", name, got, kind)
		}
	}
	if got, want := sizes["config/qbittorrent/qBittorrent/relative-link"], int64(len("qBittorrent.conf")); got != want {
		t.Errorf("the link's size is %d, want its target's %d bytes", got, want)
	}
	// The link at the pod's stdout is not in the archive at all: box leaves it
	// out, and the pass sees what the writer wrote.
	if _, ok := kinds["config/qbittorrent/qBittorrent/logs/qbittorrent.log"]; ok {
		t.Error("the pass saw a link the writer left out")
	}
}

// TestVerifyRefusesATamperedMember: a byte of one file is not the manifest's, and
// the pass refuses it by name -- before anything is extracted.
func TestVerifyRefusesATamperedMember(t *testing.T) {
	dir := craftEntry{name: "config/plex", typeflag: tar.TypeDir, mode: 0o755}
	file := craftEntry{name: "config/plex/Preferences.xml", typeflag: tar.TypeReg, mode: 0o600, data: []byte(`<Preferences MachineIdentifier="synthetic"/>`)}
	m := manifestOf(dir, file)
	tampered := file
	// The same length, one byte different: the size check cannot catch this one,
	// and the digest is what does.
	tampered.data = []byte(`<Preferences MachineIdentifier="tampered1"/>`)
	path := craft(t, []craftEntry{dir, tampered}, &m, false)

	if _, err := Verify(path, nil); err == nil {
		t.Fatal("Verify accepted an archive whose bytes are not its manifest's")
	} else if !strings.Contains(err.Error(), "Preferences.xml") || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Errorf("the refusal is %q, want the file and its manifest", err)
	}
	// The extraction a caller would run after the pass is never reached: the
	// tree it would have written is not there at all.
	out := filepath.Join(t.TempDir(), "extract")
	if err := Extract(path, out, &m, nil, nil); err == nil {
		t.Fatal("Extract wrote a tampered archive")
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Errorf("a refused extraction left %s behind", out)
	}
}

// TestVerifyRefusesAMissingMember: the manifest names a file the copy does not
// hold, which is what a copy that stopped halfway looks like.
func TestVerifyRefusesAMissingMember(t *testing.T) {
	dir := craftEntry{name: "config/app", typeflag: tar.TypeDir, mode: 0o755}
	file := craftEntry{name: "config/app/settings.yml", typeflag: tar.TypeReg, mode: 0o600, data: []byte("a: b\n")}
	m := manifestOf(dir, file)
	path := craft(t, []craftEntry{dir}, &m, false)
	if _, err := Verify(path, nil); err == nil || !strings.Contains(err.Error(), "the copy is incomplete") {
		t.Errorf("Verify said %v, want the incomplete copy", err)
	}
}

// TestExtractWritesOnlyTheEntriesItIsTold: one part of an archive now, the rest
// later. What the filter leaves out is not on disk, and the tree it kept is --
// with its modes, and owned by the user the caller named.
func TestExtractWritesOnlyTheEntriesItIsTold(t *testing.T) {
	tree := writeTree(t)
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.AddFile(PlanName, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if _, err := w.AddTree("config/qbittorrent", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var seen []Seen
	m, err := Verify(path, func(s Seen) { seen = append(seen, s) })
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// The filter keeps the plan and the links, and leaves the trees' files for a
	// later pass: exactly what a caller counting a full carry would do.
	keep := func(name string) bool {
		return name == PlanName || !strings.HasSuffix(name, ".conf")
	}
	var kept []string
	for _, s := range seen {
		if keep(s.Name) {
			kept = append(kept, s.Name)
		}
	}
	sort.Strings(kept)
	dir := filepath.Join(t.TempDir(), "extract")
	if err := Extract(path, dir, m, keep, &Owner{UID: os.Geteuid(), GID: os.Getegid()}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var got []string
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"config/qbittorrent/qBittorrent/relative-link", PlanName}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the extraction wrote\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	info, err := os.Lstat(filepath.Join(dir, "config/qbittorrent/qBittorrent/relative-link"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the extraction wrote a link as something else")
	}
}

// TestExtractRefusesANameTheManifestDoesNotHold: a filtered pass still refuses
// what no archive box wrote, so a caller cannot use a filter to skip a check.
func TestExtractRefusesANameTheManifestDoesNotHold(t *testing.T) {
	dir := craftEntry{name: "config/app", typeflag: tar.TypeDir, mode: 0o755}
	file := craftEntry{name: "config/app/settings.yml", typeflag: tar.TypeReg, mode: 0o600, data: []byte("a: b\n")}
	m := manifestOf(dir)
	path := craft(t, []craftEntry{dir, file}, &m, false)
	out := filepath.Join(t.TempDir(), "extract")
	err := Extract(path, out, &m, func(string) bool { return true }, nil)
	if err == nil || !strings.Contains(err.Error(), "which its manifest does not name") {
		t.Errorf("Extract said %v, want the entry its manifest does not name", err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Errorf("a refused extraction left %s behind", out)
	}
}
