package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testBox is the version a test archive says wrote it.
const testBox = "0.5.0-test"

// testManifest is the header of a test archive.
func testManifest(kind Kind, mode Mode) Manifest {
	return Manifest{
		Kind:    kind,
		Mode:    mode,
		Created: "2026-11-16T12:00:00Z",
		Box:     testBox,
	}
}

// writeTree builds the config tree a migration carries, with the two links a
// real qBittorrent config has: one inside the tree and one at the pod's stdout.
func writeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "qBittorrent", "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qBittorrent", "qBittorrent.conf"), []byte("[BitTorrent]\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("qBittorrent.conf", filepath.Join(dir, "qBittorrent", "relative-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/self/fd/1", filepath.Join(dir, "qBittorrent", "logs", "qbittorrent.log")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// migrationArchive writes the archive every round-trip test reads: a plan, a
// report, one config tree with the two links a real qBittorrent config has, and
// the source's manifests.
func migrationArchive(t *testing.T) (path string, plan []byte) {
	t.Helper()
	tree := writeTree(t)
	path = filepath.Join(t.TempDir(), "source.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plan = []byte("version: 2\napps:\n  qbittorrent:\n    kind: kubernetes\n    source: qbittorrent\n    config: /srv/apps/qbittorrent\n    paths: []\n")
	if err := w.AddFile(PlanName, plan, 0o600); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if err := w.AddFile(ReportName, []byte("plan: /var/lib/s5n/migrate/plan.yaml\n"), 0o600); err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	if _, err := w.AddTree("config/qbittorrent", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, plan
}

// TestWriterWritesOneFile: what the write half promises on its own -- a file
// only root can read, a name that is never taken twice, the manifest last, and
// the link it left out reported to the caller.
func TestWriterWritesOneFile(t *testing.T) {
	tree := writeTree(t)
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	m := testManifest(KindMigrate, ModeFinal)
	w, err := Create(path, m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	skipped, err := w.AddTree("config/qbittorrent", tree)
	if err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := Create(path, m); err == nil {
		t.Error("Create accepted a path that already holds an archive")
	}
	if got := lastEntry(t, path); got != ManifestName {
		t.Fatalf("the archive's last entry is %q, want %q: the manifest is what makes a stream readable", got, ManifestName)
	}
	if len(skipped) != 1 || skipped[0].Path != "config/qbittorrent/qBittorrent/logs/qbittorrent.log" {
		t.Fatalf("skipped = %+v, want the log link", skipped)
	}
}

// TestRoundTrip: what comes back out is the tree that went in, under box's own
// modes, with the links inside it and without the ones that named the pod.
func TestRoundTrip(t *testing.T) {
	path, plan := migrationArchive(t)
	dir := filepath.Join(t.TempDir(), "extracted")
	got, err := Open(path, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Format != Format || got.Kind != KindMigrate || got.Mode != ModeFinal || got.Box != testBox {
		t.Errorf("manifest = %+v, want the header this archive was written with", got)
	}
	if got.Created != testManifest(KindMigrate, ModeFinal).Created {
		t.Errorf("created = %q, want the time it was written", got.Created)
	}
	for _, name := range []string{
		PlanName,
		ReportName,
		"config/qbittorrent",
		"config/qbittorrent/qBittorrent",
		"config/qbittorrent/qBittorrent/qBittorrent.conf",
		"config/qbittorrent/qBittorrent/relative-link",
	} {
		if got.lookup(name) == nil {
			t.Errorf("the manifest does not name %s: %v", name, got.names())
		}
	}
	if planBack, err := os.ReadFile(filepath.Join(dir, PlanName)); err != nil || !bytes.Equal(planBack, plan) {
		t.Errorf("plan.yaml = %q (%v), want the plan added", planBack, err)
	}
	checkExtractedTree(t, dir)
}

// checkExtractedTree is the tree migrationArchive put in, as it comes back.
func checkExtractedTree(t *testing.T, dir string) {
	t.Helper()
	conf := filepath.Join(dir, "config", "qbittorrent", "qBittorrent", "qBittorrent.conf")
	if data, err := os.ReadFile(conf); err != nil || string(data) != "[BitTorrent]\n" {
		t.Errorf("config = %q (%v), want the source's bytes", data, err)
	}
	link := filepath.Join(dir, "config", "qbittorrent", "qBittorrent", "relative-link")
	if target, err := os.Readlink(link); err != nil || target != "qBittorrent.conf" {
		t.Errorf("relative-link = %q (%v), want it carried", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "config", "qbittorrent", "qBittorrent", "logs", "qbittorrent.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the log link was extracted (%v): a link to the kernel's filesystems is left out", err)
	}
	// What box wrote for itself is box's: the extraction's own directory and
	// the one above the config trees 0700, its plan 0600. The trees inside are
	// the source's modes (mode_test.go).
	for _, d := range []string{"", PlanName, "config"} {
		info, err := os.Lstat(filepath.Join(dir, d))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s is %o, want %o", d, info.Mode().Perm(), want)
		}
	}
}

// TestReadAgreesWithOpen: the manifest a caller reads to choose an archive is
// the one Open verified.
func TestReadAgreesWithOpen(t *testing.T) {
	path, _ := migrationArchive(t)
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, err := Open(path, filepath.Join(t.TempDir(), "extracted"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(m.Entries) != len(got.Entries) {
		t.Errorf("Read and Open disagree: %d and %d entries", len(m.Entries), len(got.Entries))
	}
}

// lastEntry is the name of the archive's final tar entry.
func lastEntry(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	last := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return last
		}
		if err != nil {
			t.Fatal(err)
		}
		last = hdr.Name
	}
}

func TestWriterRefusesALinkThatLeavesTheTree(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "etc", "passwd"), filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = w.AddTree("config/thing", tree)
	if err == nil {
		t.Fatal("AddTree carried a link that leaves the tree")
	}
	if !strings.Contains(err.Error(), "outside the config directory") {
		t.Fatalf("the refusal does not say where the link goes:\n%s", err)
	}
	w.Discard()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an abandoned archive left a file behind (%v)", err)
	}
}

// TestWriterLeavesOutASpecialFile: a running app keeps its own IPC in its
// config directory -- qBittorrent's ipc-socket is one -- and an archive cannot
// carry a socket or a named pipe. The app makes it again when it starts, so the
// tree is carried without it and the reason is reported, rather than the whole
// snapshot being refused while the app is up.
func TestWriterLeavesOutASpecialFile(t *testing.T) {
	tree := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(tree, "pipe"), 0o600); err != nil {
		t.Skipf("no fifo on this system: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tree, "qBittorrent.conf"), []byte("[BitTorrent]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	w, err := Create(path, testManifest(KindBackup, ModeBackup))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	skipped, err := w.AddTree("config/thing", tree)
	if err != nil {
		t.Fatalf("AddTree with a fifo: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(skipped) != 1 || skipped[0].Path != "config/thing/pipe" || !strings.Contains(skipped[0].Why, "named pipe") {
		t.Fatalf("the fifo was not reported as left out: %+v", skipped)
	}
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, ok := m.Entries["config/thing/pipe"]; ok {
		t.Error("the archive carries the named pipe it said it left out")
	}
	if _, ok := m.Entries["config/thing/qBittorrent.conf"]; !ok {
		t.Error("the archive lost the file beside the pipe")
	}
}

func TestCreateRefusesAManifestItCannotVouchFor(t *testing.T) {
	at := func(m Manifest) error {
		_, err := Create(filepath.Join(t.TempDir(), "a.tar.gz"), m)
		return err
	}
	tests := []struct {
		name string
		m    Manifest
		want string
	}{
		{"a kind box does not write", Manifest{Kind: "restore", Mode: ModeFinal, Created: "2026-11-16T12:00:00Z"}, "kind"},
		{"a mode box does not write", testManifest(KindMigrate, "live"), "mode"},
		{"a backup in a migration's mode", testManifest(KindBackup, ModeFinal), "mode"},
		{"a migration in a backup's mode", testManifest(KindMigrate, ModeBackup), "mode"},
		{"a creation that is not a time", Manifest{Kind: KindMigrate, Mode: ModeFinal, Created: "yesterday"}, "created"},
		{"a kind and a mode left empty", Manifest{}, "kind"},
		{"entries handed to the writer", Manifest{Kind: KindMigrate, Mode: ModeFinal, Created: "2026-11-16T12:00:00Z", Entries: map[string]Entry{"a": {}}}, "entries are added"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := at(tt.m)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Create error = %v, want it to name %q", err, tt.want)
			}
		})
	}
}

// TestASurveyHoldsNoConfigs: a survey is the read `box migrate discover` writes
// when it is not rehearsing -- the plan, the report and the source's manifests.
// It is a mode a migration archive may have, and the one thing it may never
// carry is a config tree, because apply would then have configs a survey is
// defined not to have.
func TestASurveyHoldsNoConfigs(t *testing.T) {
	t.Run("a survey of a plan and a report is an archive", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "survey.tar.gz")
		w, err := Create(path, testManifest(KindMigrate, ModeSurvey))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := w.AddFile(PlanName, []byte("version: 2\napps: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.AddFile(ReportName, []byte("plan: a survey\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.AddFile(SourcePrefix+"/deployment-media-sonarr.json", []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		m, err := Read(path)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if m.Mode != ModeSurvey {
			t.Errorf("mode = %q, want %q", m.Mode, ModeSurvey)
		}
	})

	t.Run("a survey naming a config is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "survey.tar.gz")
		w, err := Create(path, testManifest(KindMigrate, ModeSurvey))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := w.AddFile(ConfigPrefix+"/sonarr/config.xml", []byte("<Config/>"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "a survey holds no configs") {
			t.Fatalf("Read error = %v, want it to refuse a survey with a config tree in it", err)
		}
	})

	t.Run("a backup cannot be a survey", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "backup.tar.gz")
		if _, err := Create(path, testManifest(KindBackup, ModeSurvey)); err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("Create error = %v, want it to refuse a backup in a survey's mode", err)
		}
	})
}

func TestCreateDefaultsTheHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	w, err := Create(path, Manifest{Kind: KindMigrate, Mode: ModeRehearsal, Box: testBox})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.AddFile(PlanName, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if m.Format != Format {
		t.Errorf("format = %d, want %d", m.Format, Format)
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		t.Errorf("created = %q, which is not a time", m.Created)
	}
}

// TestOpenRefusesWhatIsNotBoxs: one crafted archive per way a copy can be
// wrong. Each case starts from a valid manifest and changes one thing.
func TestOpenRefusesWhatIsNotBoxs(t *testing.T) {
	file := craftEntry{name: "config/x", typeflag: tar.TypeReg, mode: 0o600, data: []byte("hello")}
	link := craftEntry{name: "config/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "../../outside"}
	inside := craftEntry{name: "config/inside", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "x"}
	dir := craftEntry{name: "config", typeflag: tar.TypeDir, mode: 0o755}

	tests := []struct {
		name string
		// build returns the archive and a destination.
		build func(t *testing.T) (string, string)
		want  string
	}{
		{
			"an archive with no manifest",
			func(t *testing.T) (string, string) {
				return craft(t, []craftEntry{dir, file}, nil, false), t.TempDir() + "/x"
			},
			"has no manifest.json",
		},
		{
			"a manifest that is not last",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, file)
				return craft(t, []craftEntry{dir, file}, &m, true), t.TempDir() + "/x"
			},
			"after its manifest",
		},
		{
			"an entry the manifest does not name",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir)
				return craft(t, []craftEntry{dir, file}, &m, false), t.TempDir() + "/x"
			},
			"which its manifest does not name",
		},
		{
			"a manifest entry the archive does not hold",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir)
				m.Entries["config/gone"] = Entry{Size: 0, SHA256: digest(nil), Mode: "0600"}
				return craft(t, []craftEntry{dir}, &m, false), t.TempDir() + "/x"
			},
			"does not hold config/gone",
		},
		{
			"a size that does not match",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, file)
				e := m.Entries["config/x"]
				e.Size = 4
				m.Entries["config/x"] = e
				return craft(t, []craftEntry{dir, file}, &m, false), t.TempDir() + "/x"
			},
			"is 5 bytes and its manifest says 4",
		},
		{
			"a sha256 that does not match",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, file)
				m.Entries["config/x"] = Entry{Size: 5, SHA256: digest([]byte("other")), Mode: "0600"}
				return craft(t, []craftEntry{dir, file}, &m, false), t.TempDir() + "/x"
			},
			"does not match its manifest",
		},
		{
			"a mode that does not match",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, file)
				m.Entries["config/x"] = Entry{Size: 5, SHA256: digest([]byte("hello")), Mode: "0644"}
				return craft(t, []craftEntry{dir, file}, &m, false), t.TempDir() + "/x"
			},
			"its manifest says 0644",
		},
		{
			"an absolute path",
			func(t *testing.T) (string, string) {
				abs := craftEntry{name: "/etc/passwd", typeflag: tar.TypeReg, mode: 0o600, data: []byte("x")}
				m := manifestOf(dir, abs)
				return craft(t, []craftEntry{dir, abs}, &m, false), t.TempDir() + "/x"
			},
			"not a path inside an archive",
		},
		{
			"a path that climbs out",
			func(t *testing.T) (string, string) {
				out := craftEntry{name: "../escape", typeflag: tar.TypeReg, mode: 0o600, data: []byte("x")}
				m := manifestOf(dir, out)
				return craft(t, []craftEntry{dir, out}, &m, false), t.TempDir() + "/x"
			},
			"not a path inside an archive",
		},
		{
			"an entry twice",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, file)
				return craft(t, []craftEntry{dir, file, file}, &m, false), t.TempDir() + "/x"
			},
			"twice",
		},
		{
			"a link that leaves the tree",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, link)
				return craft(t, []craftEntry{dir, link}, &m, false), t.TempDir() + "/x"
			},
			"outside the config directory",
		},
		{
			"a link to the kernel's filesystems",
			func(t *testing.T) (string, string) {
				proc := craftEntry{name: "config/log", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "/proc/self/fd/1"}
				m := manifestOf(dir, proc)
				return craft(t, []craftEntry{dir, proc}, &m, false), t.TempDir() + "/x"
			},
			"kernel's own filesystems",
		},
		{
			"a link whose target the manifest does not match",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, inside)
				m.Entries["config/inside"] = Entry{Size: 3, SHA256: digest([]byte("x")), Mode: "0777"}
				return craft(t, []craftEntry{dir, inside}, &m, false), t.TempDir() + "/x"
			},
			"does not match its manifest",
		},
		{
			"a file written through a link",
			func(t *testing.T) (string, string) {
				through := craftEntry{name: "config/inside/x", typeflag: tar.TypeReg, mode: 0o600, data: []byte("x")}
				m := manifestOf(dir, inside, through)
				return craft(t, []craftEntry{dir, inside, through}, &m, false), t.TempDir() + "/x"
			},
			"is not a directory",
		},
		{
			"a character device",
			func(t *testing.T) (string, string) {
				dev := craftEntry{name: "config/null", typeflag: tar.TypeChar, mode: 0o600}
				m := manifestOf(dir, dev)
				return craft(t, []craftEntry{dir, dev}, &m, false), t.TempDir() + "/x"
			},
			"character device",
		},
		{
			"a named pipe",
			func(t *testing.T) (string, string) {
				fifo := craftEntry{name: "config/pipe", typeflag: tar.TypeFifo, mode: 0o600}
				m := manifestOf(dir, fifo)
				return craft(t, []craftEntry{dir, fifo}, &m, false), t.TempDir() + "/x"
			},
			"named pipe",
		},
		{
			"a hardlink",
			func(t *testing.T) (string, string) {
				hard := craftEntry{name: "config/hard", typeflag: tar.TypeLink, mode: 0o600, linkname: "/etc/passwd"}
				m := manifestOf(dir, hard)
				return craft(t, []craftEntry{dir, hard}, &m, false), t.TempDir() + "/x"
			},
			"hardlink",
		},
		{
			"a manifest that is not JSON",
			func(t *testing.T) (string, string) {
				return craftRaw(t, []craftEntry{dir}, []byte("{not json"), false), t.TempDir() + "/x"
			},
			"does not read",
		},
		{
			"a manifest from a newer format",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir)
				m.Format = Format + 1
				return craft(t, []craftEntry{dir}, &m, false), t.TempDir() + "/x"
			},
			"format 2",
		},
		{
			"a manifest that lists itself",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir)
				m.Entries[ManifestName] = Entry{Size: 1, SHA256: digest([]byte("x")), Mode: "0600"}
				return craft(t, []craftEntry{dir}, &m, false), t.TempDir() + "/x"
			},
			"does not list itself",
		},
		{
			"a manifest with a creation that is not a time",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir)
				m.Created = "last tuesday"
				return craft(t, []craftEntry{dir}, &m, false), t.TempDir() + "/x"
			},
			"is not a time",
		},
		{
			"a manifest with a sum that is not one",
			func(t *testing.T) (string, string) {
				m := manifestOf(dir, file)
				e := m.Entries["config/x"]
				e.SHA256 = "xyz"
				m.Entries["config/x"] = e
				return craft(t, []craftEntry{dir, file}, &m, false), t.TempDir() + "/x"
			},
			"is not a hex digest",
		},
		{
			"an archive cut short by a byte",
			func(t *testing.T) (string, string) {
				return truncateOneByte(t), t.TempDir() + "/x"
			},
			"unexpected EOF",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, dest := tt.build(t)
			_, err := Open(path, dest)
			if err == nil {
				t.Fatalf("Open accepted:\n%s", path)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to name %q", err, tt.want)
			}
			if _, statErr := os.Lstat(dest); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused extraction left %s behind (%v)", dest, statErr)
			}
		})
	}
}

func TestOpenRefusesADestinationThatExists(t *testing.T) {
	path := validArchive(t)
	if _, err := Open(path, t.TempDir()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Open into an existing directory: %v", err)
	}
}

// TestBigFileStreams: a config tree can hold a database of gigabytes. Neither
// half may hold it: the writer copies through to the archive and the reader
// through to the extracted file, and only the manifest's sum sees the bytes.
func TestBigFileStreams(t *testing.T) {
	const size = 64 << 20
	tree := t.TempDir()
	big := filepath.Join(tree, "plex.db")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// A hole, so the test spends no time writing 64 MiB of zeros.
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("the end"), size-7); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "source.tar.gz")
	dest := filepath.Join(t.TempDir(), "extracted")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.AddTree("config/plex", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open(path, dest); err != nil {
		t.Fatalf("Open: %v", err)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > size/2 {
		t.Fatalf("writing and reading the archive allocated %d bytes for a %d byte file: it is not streaming", grew, size)
	}
	info, err := os.Stat(filepath.Join(dest, "config", "plex", "plex.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Errorf("extracted size = %d, want %d", info.Size(), size)
	}
}

// validArchive is a small, whole migration archive.
func validArchive(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.AddFile(PlanName, []byte("version: 2\napps: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// truncateOneByte is a whole archive with its last byte gone: the copy that ran
// out of space just before it finished.
func truncateOneByte(t *testing.T) string {
	t.Helper()
	path := validArchive(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	return path
}

// craftEntry is one tar entry a crafted archive holds.
type craftEntry struct {
	name     string
	typeflag byte
	mode     int64
	data     []byte
	linkname string
}

// manifestOf is the valid manifest for these entries.
func manifestOf(entries ...craftEntry) Manifest {
	m := testManifest(KindMigrate, ModeFinal)
	m.Format = Format
	m.Entries = make(map[string]Entry, len(entries))
	for _, e := range entries {
		switch e.typeflag {
		case tar.TypeDir:
			m.Entries[e.name] = Entry{Size: 0, SHA256: digest(nil), Mode: modeText(uint32(e.mode))}
		case tar.TypeSymlink:
			m.Entries[e.name] = Entry{Size: int64(len(e.linkname)), SHA256: digest([]byte(e.linkname)), Mode: modeText(uint32(e.mode))}
		default:
			m.Entries[e.name] = Entry{Size: int64(len(e.data)), SHA256: digest(e.data), Mode: modeText(uint32(e.mode))}
		}
	}
	return m
}

// craft writes the entries, then the manifest, into a new tar + gzip.
func craft(t *testing.T, entries []craftEntry, m *Manifest, manifestFirst bool) string {
	t.Helper()
	var raw []byte
	if m != nil {
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = data
	}
	return craftRaw(t, entries, raw, manifestFirst)
}

// craftRaw is craft with the manifest's bytes given: a test that needs a
// manifest that is not JSON, or one with a field from a newer box.
func craftRaw(t *testing.T, entries []craftEntry, manifestBytes []byte, manifestFirst bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crafted.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	writeManifest := func() {
		if manifestBytes == nil {
			return
		}
		hdr := &tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(manifestBytes)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(manifestBytes); err != nil {
			t.Fatal(err)
		}
	}
	if manifestFirst {
		writeManifest()
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typeflag, Linkname: e.linkname, Size: int64(len(e.data))}
		if e.typeflag == tar.TypeSymlink || e.typeflag == tar.TypeDir || e.typeflag == tar.TypeLink {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !manifestFirst {
		writeManifest()
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEntryDigestMatchesTheBytes is the manifest's promise in one line: the sum
// of a file written by the writer is the sum of the file that comes back.
func TestEntryDigestMatchesTheBytes(t *testing.T) {
	path := validArchive(t)
	m, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	e := m.lookup(PlanName)
	if e == nil {
		t.Fatal("the plan is not in the manifest")
	}
	sum := sha256.Sum256([]byte("version: 2\napps: {}\n"))
	if e.SHA256 != hex.EncodeToString(sum[:]) || e.Size != int64(len("version: 2\napps: {}\n")) {
		t.Errorf("entry = %+v, want the plan's own size and sum", e)
	}
}

// TestWriterRefusesNamesThatCollide: a writer that added a file where a
// directory has to be would produce an archive Open refuses, so it refuses
// first and names the two entries.
func TestWriterRefusesNamesThatCollide(t *testing.T) {
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.AddTree("config/qbittorrent", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	tests := []struct{ name, want string }{
		{"config/qbittorrent/file", "twice"},
		{"config", "is a directory already"},
		{"config/qbittorrent/file/under", "is a file"},
		{ManifestName, "written by Close"},
		{"../escape", "not a path inside the archive"},
	}
	for _, tt := range tests {
		err := w.AddFile(tt.name, []byte("x"), 0o600)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("AddFile(%q) = %v, want it to name %q", tt.name, err, tt.want)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.AddFile("late", []byte("x"), 0o600); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("AddFile after Close = %v, want a refusal", err)
	}
}

// TestAddFilesCarriesOnlyTheNamedFiles: a tree an archive carries a part of
// goes in as exactly the files named and the directories above them, so the set
// is what adders read and what Open puts back. A named file the tree does not
// have is reported, not refused: a database's -wal exists only while something
// has it open.
func TestAddFilesCarriesOnlyTheNamedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Library", "Application Support", "Databases"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Library", "Application Support", "Preferences.xml"), []byte("<Preferences/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Library", "Application Support", "Databases", "library.db"), []byte("sqlite\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// The part of the tree a snapshot does not carry: covers and thumbnails the
	// app rebuilds, and a log at the pod's stdout.
	if err := os.MkdirAll(filepath.Join(dir, "Metadata", "Posters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Metadata", "Posters", "poster.jpg"), []byte("jpeg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/self/fd/1", filepath.Join(dir, "Plex Media Server.log")); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"Library/Application Support/Preferences.xml",
		"Library/Application Support/Databases/library.db",
		// Named and absent: a -wal beside a database nobody has open.
		"Library/Application Support/Databases/library.db-wal",
		// A link at the pod's stdout: inside the listed set, out of the archive.
		"Plex Media Server.log",
	}
	path := filepath.Join(t.TempDir(), "part.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeFinal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	carried, err := w.AddFiles("config/plex", dir, "", want)
	if err != nil {
		t.Fatalf("AddFiles: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := carried.Missing; len(got) != 1 || got[0] != want[2] {
		t.Errorf("Missing = %v, want just the -wal", got)
	}
	if len(carried.Skipped) != 1 || !strings.Contains(carried.Skipped[0].Path, "Plex Media Server.log") {
		t.Errorf("Skipped = %+v, want the link at the pod's stdout", carried.Skipped)
	}

	m, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for name := range m.Entries {
		got = append(got, name)
	}
	sort.Strings(got)
	wantEntries := []string{
		"config/plex",
		"config/plex/Library",
		"config/plex/Library/Application Support",
		"config/plex/Library/Application Support/Databases",
		"config/plex/Library/Application Support/Databases/library.db",
		"config/plex/Library/Application Support/Preferences.xml",
	}
	if !reflect.DeepEqual(got, wantEntries) {
		t.Errorf("the archive holds\n  %v\nwant\n  %v", got, wantEntries)
	}
	// The directories keep the modes the tree had, and a carried file keeps its
	// own: the archive's rule, on a subset as on a whole tree.
	if e := m.lookup("config/plex/Library"); e == nil || e.Mode != "0750" {
		t.Errorf("config/plex/Library = %+v, want the tree's 0750", e)
	}
	if e := m.lookup("config/plex/Library/Application Support/Databases/library.db"); e == nil || e.Mode != "0640" {
		t.Errorf("the database = %+v, want the tree's 0640", e)
	}

	// What the set names is what Open puts back: the metadata tree is not in it,
	// and the absent companion is not a hole extraction complains about.
	extracted := filepath.Join(t.TempDir(), "out")
	if _, err := Open(path, extracted); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, "config", "plex", "Metadata")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the extracted tree holds Metadata: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, "config", "plex", "Library", "Application Support", "Databases", "library.db")); err != nil {
		t.Errorf("the extracted tree has no database: %v", err)
	}
}

// TestAddFilesRefusesWhatTheSetCannotCarry: a name that is not a file inside the
// tree, and a listed directory, are refusals rather than an archive with a hole
// in it.
func TestAddFilesRefusesWhatTheSetCannotCarry(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Databases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, want string }{
		{"../escape", "relative path"},
		{"/etc/passwd", "relative path"},
		{"./file", "relative path"},
		{"Databases", "is a directory"},
		{"file/under", "not a directory"},
	}
	for _, tt := range tests {
		path := filepath.Join(t.TempDir(), "bad.tar.gz")
		w, err := Create(path, testManifest(KindMigrate, ModeFinal))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := w.AddFiles("config/app", dir, "", []string{tt.name}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("AddFiles(%q) = %v, want it to name %q", tt.name, err, tt.want)
		}
		w.Discard()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused AddFiles(%q) left %s behind", tt.name, path)
		}
	}
}

// manyFiles builds a config tree the size of a real torrent client's: one
// resume file and one torrent per hash, thousands of them, so the manifest
// holds one entry per file and runs to megabytes.
func manyFiles(t *testing.T, n int) string {
	t.Helper()
	tree := t.TempDir()
	dir := filepath.Join(tree, "qBittorrent", "BT_backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		sum := sha256.Sum256([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		hash := hex.EncodeToString(sum[:20])
		for _, ext := range []string{".fastresume", ".torrent"} {
			if err := os.WriteFile(filepath.Join(dir, hash+ext), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return tree
}

// A real config tree is thousands of files: a qBittorrent with 1500 torrents
// alone is 3000 entries, and a box must read back the snapshot it wrote of one.
func TestReadsTheManifestOfAThousandsOfFilesTree(t *testing.T) {
	tree := manyFiles(t, 4000)
	path := filepath.Join(t.TempDir(), "rehearsal.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeRehearsal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.AddTree("config/qbittorrent", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m, err := Read(path)
	if err != nil {
		t.Fatalf("Read refused the archive this box just wrote: %v", err)
	}
	if got := len(m.Entries); got < 8000 {
		t.Fatalf("manifest holds %d entries, want every file", got)
	}
	if _, err := Open(path, filepath.Join(t.TempDir(), "extracted")); err != nil {
		t.Fatalf("Open refused the archive this box just wrote: %v", err)
	}
}

// A manifest past what a box reads is refused when it is written, naming how
// many files it holds, so no archive exists that its own box cannot open.
func TestCloseRefusesAManifestNoBoxCouldRead(t *testing.T) {
	old := maxManifest
	maxManifest = 64 << 10
	t.Cleanup(func() { maxManifest = old })
	tree := manyFiles(t, 400)
	path := filepath.Join(t.TempDir(), "rehearsal.tar.gz")
	w, err := Create(path, testManifest(KindMigrate, ModeRehearsal))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.AddTree("config/qbittorrent", tree); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	err = w.Close()
	if err == nil || !strings.Contains(err.Error(), "803 entries") {
		t.Fatalf("Close = %v, want a refusal naming the 803 entries", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused archive left %s behind (%v)", path, statErr)
	}
}
