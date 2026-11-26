package fastresume

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/bencode"
	"github.com/swizzin/s5n/internal/migrate/plan"
)

// infoHash is 20 invented bytes, as libtorrent stores a v1 hash.
const infoHash = "0123456789abcdefghij"

// qbtPlan moves the download root and its Series subdirectory under box's data
// contract. Paths are invented.
const qbtPlan = `version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /downloads
        to: /data/downloads
      - from: /downloads/Series
        to: /data/media/Series
`

func testTable(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(qbtPlan))
	if err != nil {
		t.Fatalf("parse the test plan: %v", err)
	}
	return p
}

// bstr renders a bencode string as libtorrent's writer does.
func bstr(s string) string { return strconv.Itoa(len(s)) + ":" + s }

// dict joins pairs in the order given, which for these fixtures is libtorrent's:
// keys sorted.
func dict(pairs ...string) string {
	var b strings.Builder
	b.WriteString("d")
	for i := 0; i < len(pairs); i += 2 {
		b.WriteString(bstr(pairs[i]))
		b.WriteString(pairs[i+1])
	}
	b.WriteString("e")
	return b.String()
}

// list renders a list of encoded values.
func list(values ...string) string { return "l" + strings.Join(values, "") + "e" }

// lt12Resume is shaped like libtorrent 1.2's file for qBittorrent 5, with enough
// keys that a reordering or a dropped one would show.
func lt12Resume(savePath, qbtSavePath, qbtDownloadPath string, mapped ...string) []byte {
	names := make([]string, 0, len(mapped))
	for _, m := range mapped {
		names = append(names, bstr(m))
	}
	return []byte(dict(
		"active_time", "i60e",
		"added_time", "i1700000000e",
		"completed_time", "i1700003600e",
		"file-format", bstr("libtorrent resume file"),
		"file-version", "i1e",
		"info-hash", bstr(infoHash),
		"libtorrent-version", bstr("1.2.20.0"),
		"mapped_files", list(names...),
		"name", bstr("Some.Film.2024.1080p"),
		"qBt-category", bstr("Movies"),
		"qBt-downloadPath", bstr(qbtDownloadPath),
		"qBt-savePath", bstr(qbtSavePath),
		"save_path", bstr(savePath),
		"seeding_time", "i30e",
		"total_downloaded", "i12345e",
	))
}

// lt2Resume is libtorrent 2's: two keys 1.2 lacks, the path keys unchanged, which
// is why one rewriter serves both.
func lt2Resume(savePath, qbtSavePath, qbtDownloadPath string, mapped ...string) []byte {
	names := make([]string, 0, len(mapped))
	for _, m := range mapped {
		names = append(names, bstr(m))
	}
	return []byte(dict(
		"active_time", "i60e",
		"added_time", "i1700000000e",
		"completed_time", "i1700003600e",
		"file-format", bstr("libtorrent resume file"),
		"file-version", "i1e",
		"info-hash", bstr(infoHash),
		"info-hash2", bstr(strings.Repeat("f0", 16)),
		"libtorrent-version", bstr("2.0.11.0"),
		"mapped_files", list(names...),
		"name", bstr("Some.Film.2024.1080p"),
		"paused", "i0e",
		"piece layers", dict(infoHash+"f", bstr(strings.Repeat("f", 8))),
		"qBt-category", bstr("Series"),
		"qBt-downloadPath", bstr(qbtDownloadPath),
		"qBt-savePath", bstr(qbtSavePath),
		"save_path", bstr(savePath),
		"seeding_time", "i30e",
		"total_downloaded", "i12345e",
	))
}

func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot reads a directory so a test can prove nothing in it moved.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = data
	}
	return out
}

func TestRewriteDirRewritesBothShapes(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	writeFiles(t, src, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "/downloads/Movies", "/downloads/Movies",
			"a/relative.name.mkv", "/downloads/Movies/absolute.name.mkv"),
		"h2.fastresume": lt12Resume("/downloads/Series", "", ""),
		"h3.fastresume": lt2Resume("/downloads/Series/Show", "/downloads/Series/Show", "/downloads/Series/Show",
			"", "/downloads/Series/Show/e01.mkv"),
		"h1.torrent": []byte(dict("info", dict("name", bstr("Some.Film.2024.1080p")))),
		"queue":      []byte(list(bstr(infoHash))),
	})
	before := snapshot(t, src)

	rep, err := RewriteDir(src, dst, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("RewriteDir: %v", err)
	}

	want := map[string][]byte{
		"h1.fastresume": lt12Resume("/data/downloads/Movies", "/data/downloads/Movies", "/data/downloads/Movies",
			"a/relative.name.mkv", "/data/downloads/Movies/absolute.name.mkv"),
		"h2.fastresume": lt12Resume("/data/media/Series", "", ""),
		"h3.fastresume": lt2Resume("/data/media/Series/Show", "/data/media/Series/Show", "/data/media/Series/Show",
			"", "/data/media/Series/Show/e01.mkv"),
		"h1.torrent": []byte(dict("info", dict("name", bstr("Some.Film.2024.1080p")))),
		"queue":      []byte(list(bstr(infoHash))),
	}
	got := snapshot(t, dst)
	if !reflect.DeepEqual(got, want) {
		for name := range want {
			if !reflect.DeepEqual(got[name], want[name]) {
				t.Errorf("%s:\n got %q\nwant %q", name, got[name], want[name])
			}
		}
		t.Fatalf("dst names = %v, want %v", namesOf(got), namesOf(want))
	}

	wantReport := Report{
		Files: 5, Resume: 3, Copied: 2, Rewritten: 3, Unchanged: 0,
		Rewrites: map[string]int{
			"save_path":        3,
			"qBt-savePath":     2, // the second file leaves it empty under automatic management
			"qBt-downloadPath": 2, // and empty stays empty, so it is not a rewrite
			"mapped_files":     2,
		},
	}
	if !reflect.DeepEqual(rep, wantReport) {
		t.Errorf("report = %+v, want %+v", rep, wantReport)
	}

	if !reflect.DeepEqual(snapshot(t, src), before) {
		t.Error("RewriteDir changed the source directory, which it only reads")
	}
}

func TestRewriteDirDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	writeFiles(t, src, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "/downloads/Movies", "/downloads/Movies"),
		"h1.torrent":    []byte("d4:infodee"),
	})

	dry, err := RewriteDir(src, dst, "qbittorrent", testTable(t), DryRun())
	if err != nil {
		t.Fatalf("RewriteDir: %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dst exists after a dry run: %v", err)
	}

	real, err := RewriteDir(src, dst, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("RewriteDir: %v", err)
	}
	if !reflect.DeepEqual(dry, real) {
		t.Errorf("dry run reported %+v, the real run %+v", dry, real)
	}
}

func TestRewriteDirRefusesWithEveryGapListed(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	writeFiles(t, src, map[string][]byte{
		"a.fastresume": lt12Resume("/mnt/outside/Movies", "/mnt/outside/Movies", "/mnt/outside/Movies"),
		"b.fastresume": lt12Resume("/downloads/Movies", "", "",
			"/mnt/elsewhere/x.mkv", "rel/ok.mkv", "/mnt/outside/y.mkv"),
	})
	before := snapshot(t, src)

	_, err := RewriteDir(src, dst, "qbittorrent", testTable(t))
	if err == nil {
		t.Fatal("RewriteDir accepted a directory with paths no rule covers")
	}
	var uncovered *plan.UncoveredError
	if !errors.As(err, &uncovered) {
		t.Fatalf("error is %T, want *plan.UncoveredError: %v", err, err)
	}
	for _, want := range []string{"/mnt/outside/Movies", "/mnt/elsewhere/x.mkv", "/mnt/outside/y.mkv", "mapped_files[2]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "5 paths no rule covers") {
		t.Errorf("error = %q, want all five gaps in one refusal", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dst exists after a refusal: %v", statErr)
	}
	if !reflect.DeepEqual(snapshot(t, src), before) {
		t.Error("a refused run changed the source directory")
	}
}

func TestRewriteDirRefusals(t *testing.T) {
	resume := lt12Resume("/downloads/Movies", "/downloads/Movies", "/downloads/Movies")
	tests := []struct {
		name string
		dst  func(t *testing.T, root string) string
		want string
	}{
		{
			name: "a destination that already holds something",
			dst: func(t *testing.T, root string) string {
				dst := filepath.Join(root, "copy")
				writeFiles(t, dst, map[string][]byte{"old.fastresume": []byte("de")})
				return dst
			},
			want: "it is not empty",
		},
		{
			name: "a destination that is a file",
			dst: func(t *testing.T, root string) string {
				dst := filepath.Join(root, "copy")
				if err := os.WriteFile(dst, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return dst
			},
			want: "it exists and is not a directory",
		},
		{
			name: "a destination inside the source",
			dst:  func(t *testing.T, root string) string { return filepath.Join(root, "resume", "copy") },
			want: "it is inside the source",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "resume")
			writeFiles(t, src, map[string][]byte{"h1.fastresume": resume})
			before := snapshot(t, src)
			dst := tt.dst(t, root)

			if _, err := RewriteDir(src, dst, "qbittorrent", testTable(t)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to say %q", err, tt.want)
			}
			if !reflect.DeepEqual(snapshot(t, src), before) {
				t.Error("a refused run changed the source directory")
			}
		})
	}
}

func TestRewriteDirLeavesAFileThatIsAlreadyRight(t *testing.T) {
	// An identity rule is a plan's only way to say "this tree stays": an absolute
	// path no rule mentions is a gap.
	identity, err := plan.Parse([]byte(`version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /data/downloads
        to: /data/downloads
`))
	if err != nil {
		t.Fatalf("parse the identity plan: %v", err)
	}

	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	// Under the contract with automatic torrent management: no qBt-savePath, and
	// the mapped name is relative to save_path.
	writeFiles(t, src, map[string][]byte{
		"h1.fastresume": lt12Resume("/data/downloads/Movies", "", "", "a/relative.name.mkv"),
	})

	rep, err := RewriteDir(src, dst, "qbittorrent", identity)
	if err != nil {
		t.Fatalf("RewriteDir: %v", err)
	}
	if rep.Rewritten != 0 || rep.Unchanged != 1 {
		t.Errorf("report = %+v, want the one resume file unchanged", rep)
	}
	if len(rep.Rewrites) != 0 {
		t.Errorf("rewrites = %v, want none", rep.Rewrites)
	}
	if got, want := snapshot(t, dst), snapshot(t, src); !reflect.DeepEqual(got, want) {
		t.Errorf("copied %q, want the source bytes %q", got, want)
	}
}

func TestRewriteDirRefusesAFileItCannotRead(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	writeFiles(t, src, map[string][]byte{
		"h1.fastresume": []byte("d8:save_path"),
		// The .torrent is copied untouched, so a broken resume file alone stops the
		// run.
		"h1.torrent": []byte("d4:infodee"),
	})

	_, err := RewriteDir(src, dst, "qbittorrent", testTable(t))
	if err == nil {
		t.Fatal("RewriteDir accepted a truncated resume file")
	}
	for _, want := range []string{"h1.fastresume", "bencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dst exists after a refusal: %v", statErr)
	}
}

func TestRewriteDirRefusesASubdirectory(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	writeFiles(t, src, map[string][]byte{"h1.fastresume": lt12Resume("/downloads", "", "")})
	if err := os.Mkdir(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := RewriteDir(src, filepath.Join(root, "copy"), "qbittorrent", testTable(t))
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %v, want it to refuse the subdirectory", err)
	}
}

func TestRewriteDirKeepsModesAndMakesTheDestination(t *testing.T) {
	// The umask decides what survives of dirPerm, so pin the one a
	// group-writable config root assumes.
	defer syscall.Umask(syscall.Umask(0o002))

	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "deep", "copy")
	writeFiles(t, src, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "", ""),
		"h1.torrent":    []byte("d4:infodee"),
	})
	if err := os.Chmod(filepath.Join(src, "h1.torrent"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := RewriteDir(src, dst, "qbittorrent", testTable(t)); err != nil {
		t.Fatalf("RewriteDir: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != dirPerm {
		t.Errorf("destination mode = %o, want %o", got, dirPerm)
	}
	for name, want := range map[string]os.FileMode{"h1.fastresume": 0o640, "h1.torrent": 0o600} {
		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
	}
}

func TestRewriteDirEmptySource(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := RewriteDir(src, dst, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("RewriteDir: %v", err)
	}
	if rep.Files != 0 || rep.Resume != 0 {
		t.Errorf("report = %+v, want nothing counted", rep)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dst holds %v, want an empty directory", namesOf(snapshot(t, dst)))
	}
}

func TestRewriteDirMissingSource(t *testing.T) {
	root := t.TempDir()
	if _, err := RewriteDir(filepath.Join(root, "absent"), filepath.Join(root, "copy"), "qbittorrent", testTable(t)); err == nil {
		t.Fatal("RewriteDir accepted a missing source directory")
	}
}

func TestRewriteDirWritesIntoAnEmptyDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "resume")
	dst := filepath.Join(root, "copy")
	writeFiles(t, src, map[string][]byte{"h1.fastresume": lt12Resume("/downloads/Movies", "", "")})
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := RewriteDir(src, dst, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("RewriteDir into an empty destination: %v", err)
	}
	if rep.Rewritten != 1 {
		t.Errorf("report = %+v, want the one resume file rewritten", rep)
	}
	if _, err := os.Stat(filepath.Join(dst, "h1.fastresume")); err != nil {
		t.Errorf("dst is missing the copied resume file: %v", err)
	}
}

func TestUndoRemovesOnlyWhatTheRunWrote(t *testing.T) {
	root := t.TempDir()

	// A destination this run created goes away entirely.
	mine := filepath.Join(root, "mine")
	writeFiles(t, mine, map[string][]byte{"a.fastresume": []byte("one")})
	undo(mine, []string{"a.fastresume"}, true)
	if _, err := os.Stat(mine); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("undo kept a destination this run created: %v", err)
	}

	// One that was already there stays, with the files undo was not told about.
	theirs := filepath.Join(root, "theirs")
	writeFiles(t, theirs, map[string][]byte{"a.fastresume": []byte("one"), "keep": []byte("x")})
	undo(theirs, []string{"a.fastresume"}, false)
	if _, err := os.Stat(filepath.Join(theirs, "a.fastresume")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("undo ignored the file it was told it wrote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(theirs, "keep")); err != nil {
		t.Errorf("undo removed a file it did not write: %v", err)
	}
}

func namesOf(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}

// TestScanWritesNothing reads every resume file and leaves every byte.
func TestScanWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resume")
	writeFiles(t, dir, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "/downloads/Movies", "/downloads/Movies"),
		"h1.torrent":    []byte(dict("info", dict("name", bstr("Some.Film.2024.1080p")))),
	})
	before := snapshot(t, dir)

	rep, gaps, err := Scan(dir, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	if rep.Files != 2 || rep.Resume != 1 || rep.Rewritten != 1 {
		t.Errorf("report = %+v, want one of two files rewritten", rep)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Error("Scan wrote into the directory it only reads")
	}
}

// TestRewriteInPlace rewrites what needs it and leaves the rest, including the
// file whose paths are already right.
func TestRewriteInPlace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resume")
	writeFiles(t, dir, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "/downloads/Movies", "/downloads/Movies"),
		"h2.fastresume": lt12Resume("/data/media/Series", "", "", "rel/name.mkv"),
		"h1.torrent":    []byte(dict("info", dict("name", bstr("Some.Film.2024.1080p")))),
	})
	untouched := snapshot(t, dir)["h2.fastresume"]

	// An identity rule is how a plan says "this tree stays"; without it the
	// second file is a gap.
	identity, err := plan.Parse([]byte(`version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /downloads
        to: /data/downloads
      - from: /data/media/Series
        to: /data/media/Series
`))
	if err != nil {
		t.Fatalf("parse the identity plan: %v", err)
	}

	rep, gaps, err := Rewrite(dir, "qbittorrent", identity)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	if rep.Rewritten != 1 || rep.Unchanged != 1 {
		t.Errorf("report = %+v, want one rewritten and one unchanged", rep)
	}
	got := snapshot(t, dir)
	if !reflect.DeepEqual(got["h2.fastresume"], untouched) {
		t.Error("a resume file whose paths were already right was rewritten")
	}
	want := lt12Resume("/data/downloads/Movies", "/data/downloads/Movies", "/data/downloads/Movies")
	if !reflect.DeepEqual(got["h1.fastresume"], want) {
		t.Errorf("h1.fastresume = %q, want %q", got["h1.fastresume"], want)
	}
}

// TestRewriteInPlaceRefusesBeforeWriting: one unmapped path leaves the directory
// whole, even the files it could have rewritten.
func TestRewriteInPlaceRefusesBeforeWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resume")
	writeFiles(t, dir, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "", ""),
		"h2.fastresume": lt12Resume("/mnt/outside/Series", "", ""),
	})
	before := snapshot(t, dir)

	_, gaps, err := Rewrite(dir, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if len(gaps) != 1 || gaps[0].Path != "/mnt/outside/Series" {
		t.Fatalf("gaps = %v, want the one outside path", gaps)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Error("a refused rewrite changed the directory")
	}
}

// TestBlob is the SQLite store's half: the same data, rewritten in memory.
func TestBlob(t *testing.T) {
	raw := []byte(dict(
		"file-format", bstr("libtorrent resume file"),
		"file-version", "i1e",
		"info-hash", bstr(infoHash),
		"mapped_files", list(bstr("a/relative.name.mkv")),
		"part_file_dir", bstr("/downloads/.parts"),
		"qBt-category", bstr("Movies"),
		"save_path", bstr("/downloads/Movies"),
	))
	out, rewrites, gaps, _, err := Blob("torrents.db", raw, "qbittorrent", testTable(t))
	if err != nil {
		t.Fatalf("Blob: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	if rewrites["save_path"] != 1 || rewrites["part_file_dir"] != 1 || rewrites["mapped_files"] != 0 {
		t.Errorf("rewrites = %v, want the two absolute keys and nothing relative", rewrites)
	}
	v, err := bencode.Decode(out)
	if err != nil {
		t.Fatalf("decoded %q: %v", out, err)
	}
	for key, want := range map[string]string{
		"save_path":     "/data/downloads/Movies",
		"part_file_dir": "/data/downloads/.parts",
		"qBt-category":  "Movies",
		"file-format":   "libtorrent resume file",
	} {
		field := v.Lookup(key)
		if field == nil || string(field.Bytes) != want {
			t.Errorf("%s = %v, want %q", key, field, want)
		}
	}
	mapped := v.Lookup("mapped_files")
	if mapped == nil || len(mapped.List) != 1 || string(mapped.List[0].Bytes) != "a/relative.name.mkv" {
		t.Errorf("mapped_files = %v, want the relative name untouched", mapped)
	}
}

// TestStopMarksEveryTorrentStopped: a file whose paths are already right is still
// written -- an unstoppped torrent seeds and announces -- with both keys inserted
// in libtorrent's sorted order and nothing else changed.
func TestStopMarksEveryTorrentStopped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resume")
	raw := lt12Resume("/data/downloads/Movies", "", "")
	writeFiles(t, dir, map[string][]byte{"h1.fastresume": raw})
	// Only the torrent's state changes; any other byte would be unreported.
	identity, err := plan.Parse([]byte(`version: 2
apps:
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    paths:
      - from: /data/downloads
        to: /data/downloads
`))
	if err != nil {
		t.Fatalf("parse the identity plan: %v", err)
	}

	rep, gaps, err := Rewrite(dir, "qbittorrent", identity, Stop())
	if err != nil {
		t.Fatalf("Rewrite with Stop: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	if rep.Stopped != 1 {
		t.Errorf("report.Stopped = %d, want the one torrent", rep.Stopped)
	}
	got, err := os.ReadFile(filepath.Join(dir, "h1.fastresume"))
	if err != nil {
		t.Fatal(err)
	}

	before, err := bencode.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	after, err := bencode.Decode(got)
	if err != nil {
		t.Fatalf("the stopped file does not decode: %v", err)
	}
	for _, key := range []string{keyPaused, keyAutoManaged} {
		f := after.Lookup(key)
		want := int64(1)
		if key == keyAutoManaged {
			want = 0
		}
		if f == nil || f.Kind != bencode.Int || f.Int != want {
			t.Errorf("%s = %v, want i%de", key, f, want)
		}
	}
	// Every other key is the source's, byte for byte.
	for _, f := range before.Dict {
		if string(f.Key) == keyPaused || string(f.Key) == keyAutoManaged {
			continue
		}
		got := after.Lookup(string(f.Key))
		if got == nil || !reflect.DeepEqual(*got, f.Value) {
			t.Errorf("%s changed: %v, want %v", f.Key, got, f.Value)
		}
	}

	// A second pass writes nothing, which makes the state file's "done" believable.
	rep, _, err = Rewrite(dir, "qbittorrent", identity, Stop())
	if err != nil {
		t.Fatalf("a second Rewrite with Stop: %v", err)
	}
	if rep.Stopped != 0 {
		t.Errorf("a second pass stopped %d torrents, want 0", rep.Stopped)
	}
}

// TestScanWithStopCountsAndWritesNothing: the summary's count comes from Check,
// and must be what Write then changes without Check touching the staged copy.
func TestScanWithStopCountsAndWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resume")
	stopped, changed, err := stopBytes("h2.fastresume", lt12Resume("/downloads/Series", "", ""))
	if err != nil || !changed {
		t.Fatalf("stopBytes: changed=%v, err=%v", changed, err)
	}
	writeFiles(t, dir, map[string][]byte{
		"h1.fastresume": lt12Resume("/downloads/Movies", "", ""),
		"h2.fastresume": stopped,
	})
	before := snapshot(t, dir)

	rep, gaps, err := Scan(dir, "qbittorrent", testTable(t), Stop())
	if err != nil {
		t.Fatalf("Scan with Stop: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	if rep.Stopped != 1 {
		t.Errorf("report.Stopped = %d, want the one torrent not already stopped", rep.Stopped)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Error("Scan wrote into the directory it only reads")
	}
}

// TestStopBlobMarksTheSQLiteResumeDataStopped: a torrent loaded from
// libtorrent_resume_data comes up stopped too.
func TestStopBlobMarksTheSQLiteResumeDataStopped(t *testing.T) {
	raw := lt2Resume("/downloads/Movies", "", "")
	out, _, gaps, stopped, err := Blob("torrents.db", raw, "qbittorrent", testTable(t), Stop())
	if err != nil {
		t.Fatalf("Blob with Stop: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
	if !stopped {
		t.Fatal("Blob reported nothing stopped, and the blob carries no paused=1/auto_managed=0")
	}
	v, err := bencode.Decode(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f := v.Lookup("paused"); f == nil || f.Int != 1 {
		t.Errorf("paused = %v, want 1", f)
	}
	if f := v.Lookup("auto_managed"); f == nil || f.Int != 0 {
		t.Errorf("auto_managed = %v, want 0", f)
	}
	if f := v.Lookup("save_path"); f == nil || string(f.Bytes) != "/data/downloads/Movies" {
		t.Errorf("save_path = %v, want the rewritten path", f)
	}
}
