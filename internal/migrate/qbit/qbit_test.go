package qbit

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/bencode"
	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// qbitPlan is the plan a discovery run would write for this source: qBittorrent
// saw its downloads at /downloads and its own config at /config, and one
// save path had a space in it. The identity rules are what discovery writes on
// a second run, when the tree already sits where box wants it.
const qbitPlan = `version: 1
apps:
  qbittorrent:
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths:
      - from: /downloads
        to: /data/downloads
      - from: /srv/My Downloads
        to: /data/My Downloads
      - from: /config
        to: /config
      - from: /data/downloads
        to: /data/downloads
      - from: /data/My Downloads
        to: /data/My Downloads
`

var qbitTargets = map[string]rewrite.Endpoint{
	"qbittorrent": {Host: "qbittorrent", Port: 8080},
}

func testInput(t *testing.T, dir string) rewrite.Input {
	t.Helper()
	p, err := plan.Parse([]byte(qbitPlan))
	if err != nil {
		t.Fatalf("parse the test plan: %v", err)
	}
	return rewrite.Input{App: "qbittorrent", Dir: dir, Plan: p, Targets: qbitTargets}
}

func qbitConf() string {
	return `[BitTorrent]
Session\DefaultSavePath="/srv/My Downloads"
Session\TempPath="/srv/My Downloads/temp"
Session\TorrentBackupDirectory=/downloads/.torrent-backup
Session\IPFilter=/config/qBittorrent/ipfilter.dat
Session\Port=6881
Session\ResumeDataStorageType=Legacy

[Preferences]
Downloads\ScanDirsLastPath="/srv/My Downloads"
General\CustomUIThemePath=/mnt/theme
Search\pythonExecutablePath=/config/python/python3

[WebUI]
Port=8080
RootFolder=/config/qBittorrent/webui
HTTPS\CertificatePath=/config/qBittorrent/tls/cert.pem

[MainWindow]
LastDir=@Invalid()

[AddNewTorrentDialog]
SavePathHistory="/downloads", "/srv/My Downloads"
`
}

// bdict renders a bencoded dictionary whose values are already encoded.
func bdict(pairs ...string) []byte {
	var b strings.Builder
	b.WriteString("d")
	for i := 0; i < len(pairs); i += 2 {
		fmt.Fprintf(&b, "%d:%s", len(pairs[i]), pairs[i])
		b.WriteString(pairs[i+1])
	}
	b.WriteString("e")
	return []byte(b.String())
}

func bstr(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }

func resumeBlob(savePath string) []byte {
	// libtorrent sorts its keys, and qBt-category sorts before save_path.
	return bdict("qBt-category", bstr("TV"), "save_path", bstr(savePath))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func newDB(t *testing.T, path string, statements ...string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedb.OpenReadWrite(context.Background(), path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer db.Close()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// source builds a config directory the way a stopped qBittorrent 5.3.0 leaves
// one: the conf, the JSON files, a Legacy resume store and an SQLite one (a
// source that converted and kept both).
func source(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, dirRel)
	writeFile(t, filepath.Join(root, "qBittorrent.conf"), qbitConf())
	writeFile(t, filepath.Join(root, "categories.json"),
		`{"Movies":{"save_path":"/downloads/Movies","download_path":"","ratio_limit":-2},"TV":{"download_path":false,"save_path":"/downloads/tv","share_limits_mode":"Default"}}`)
	writeFile(t, filepath.Join(root, "watched_folders.json"),
		`{"/downloads/in":{"add_torrent_params":{"category":"TV","download_path":"/downloads/in/.tmp","save_path":"/downloads/in"},"recursive":true}}`)
	writeFile(t, filepath.Join(root, "rss", "download_rules.json"),
		`{"TV":{"enabled":true,"mustContain":"*","savePath":"/downloads/tv","torrentParams":{"category":"TV","save_path":"/downloads/tv"}}}`)

	writeFile(t, filepath.Join(root, "BT_backup", "h1.fastresume"), string(resumeBlob("/downloads/tv")))
	writeFile(t, filepath.Join(root, "BT_backup", "h1.torrent"), "d4:infodee")
	writeFile(t, filepath.Join(root, "BT_backup", "queue"), "l20:0123456789abcdefghije")

	blob := resumeBlob("/downloads/tv")
	newDB(t, filepath.Join(root, "torrents.db"),
		`CREATE TABLE torrents (id INTEGER PRIMARY KEY, target_save_path TEXT, download_path TEXT, libtorrent_resume_data BLOB NOT NULL)`,
		fmt.Sprintf(`INSERT INTO torrents (id, target_save_path, download_path, libtorrent_resume_data)
			VALUES (1, '/downloads/tv', '/downloads/tv/.tmp', X'%x')`, blob),
		// A torrent in portable mode stores a relative path, which follows the
		// app and needs no rule.
		fmt.Sprintf(`INSERT INTO torrents (id, target_save_path, download_path, libtorrent_resume_data)
			VALUES (2, 'downloads/Movies', '', X'%x')`, resumeBlob("downloads/Movies")),
	)
	return dir
}

// snapshot is every file under dir, so a test can prove a pass wrote nothing.
func snapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func jsonValue(t *testing.T, path string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(readFile(t, path)), into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestRewritesEveryPath(t *testing.T) {
	dir := source(t)
	in := testInput(t, dir)
	before := snapshot(t, dir)

	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Fatal("Check changed the config directory, which it only reads")
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	wantNotes := 1 // both resume stores are present
	if len(rep.Notes) != wantNotes {
		t.Errorf("notes = %v, want one about the two resume stores", rep.Notes)
	}

	got, err := (rewriter{}).Write(context.Background(), in)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !reflect.DeepEqual(got, rep) {
		t.Errorf("Write reported %+v, Check reported %+v", got, rep)
	}

	assertQbitFiles(t, dir)

	// The next run has nothing to do: every store, file and column already
	// holds what the plan says.
	rep2, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("second Check still reports changes: %+v", rep2.Changes)
	}
}

// readRows reads the three resume columns, keeping the blob as bytes.
func readRows(t *testing.T, path string) [][3]any {
	t.Helper()
	db, err := sqlitedb.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT target_save_path, download_path, libtorrent_resume_data FROM torrents ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][3]any
	for rows.Next() {
		var a, b string
		var blob []byte
		if err := rows.Scan(&a, &b, &blob); err != nil {
			t.Fatal(err)
		}
		out = append(out, [3]any{a, b, blob})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertQbitFiles checks the conf, the three JSON files and both resume
// stores after a write.
func assertQbitFiles(t *testing.T, dir string) {
	t.Helper()
	conf := readFile(t, filepath.Join(dir, confRel))
	for _, want := range []string{
		`Session\DefaultSavePath="/data/My Downloads"`,
		`Session\TempPath="/data/My Downloads/temp"`,
		`Session\TorrentBackupDirectory=/data/downloads/.torrent-backup`,
		`Session\IPFilter=/config/qBittorrent/ipfilter.dat`,
		`Session\Port=6881`,
		`Session\ResumeDataStorageType=Legacy`,
		`Search\pythonExecutablePath=/config/python/python3`,
		`RootFolder=/config/qBittorrent/webui`,
		`Port=8080`,
		`SavePathHistory=/data/downloads, "/data/My Downloads"`,
		`LastDir=@Invalid()`,
		// A key with no rule behind it and no effect on a download stays.
		`General\CustomUIThemePath=/mnt/theme`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf does not contain %q:\n%s", want, conf)
		}
	}
	if !strings.HasSuffix(conf, "\n") {
		t.Error("conf lost its trailing newline")
	}
	if strings.Contains(conf, "Session\\DefaultSavePath=\"/srv/My Downloads\"") {
		t.Error("conf still holds the source's save path")
	}

	var categories map[string]map[string]any
	jsonValue(t, filepath.Join(dir, categoriesRel), &categories)
	if _, ok := categories["Movies"]; !ok {
		t.Errorf("category name changed: %v", categories)
	}
	if categories["Movies"]["save_path"] != "/data/downloads/Movies" || categories["Movies"]["ratio_limit"] != float64(-2) {
		t.Errorf("Movies = %v", categories["Movies"])
	}
	if categories["TV"]["save_path"] != "/data/downloads/tv" || categories["TV"]["download_path"] != false {
		t.Errorf("TV = %v, want the flag kept as a flag", categories["TV"])
	}

	var watched map[string]map[string]any
	jsonValue(t, filepath.Join(dir, watchedRel), &watched)
	params, ok := watched["/data/downloads/in"]
	if !ok {
		t.Fatalf("watched folder key was not rewritten: %v", watched)
	}
	add, ok := params["add_torrent_params"].(map[string]any)
	if !ok || add["save_path"] != "/data/downloads/in" || add["download_path"] != "/data/downloads/in/.tmp" || add["category"] != "TV" {
		t.Errorf("watched folder params = %v", params["add_torrent_params"])
	}
	if params["recursive"] != true {
		t.Errorf("recursive = %v", params["recursive"])
	}

	var rules map[string]map[string]any
	jsonValue(t, filepath.Join(dir, rulesRel), &rules)
	if rules["TV"]["savePath"] != "/data/downloads/tv" || rules["TV"]["mustContain"] != "*" {
		t.Errorf("rule = %v", rules["TV"])
	}
	ruleParams, _ := rules["TV"]["torrentParams"].(map[string]any)
	if ruleParams["save_path"] != "/data/downloads/tv" {
		t.Errorf("rule torrentParams = %v", ruleParams)
	}

	legacy := readFile(t, filepath.Join(dir, legacyRel, "h1.fastresume"))
	if !strings.Contains(legacy, "/data/downloads/tv") || strings.Contains(legacy, "13:/downloads/tv") {
		t.Errorf("legacy resume file = %q", legacy)
	}
	if got := readFile(t, filepath.Join(dir, legacyRel, "h1.torrent")); got != "d4:infodee" {
		t.Errorf("torrent metadata was rewritten: %q", got)
	}

	rows := readRows(t, filepath.Join(dir, resumeRel))
	if rows[0][0] != "/data/downloads/tv" || rows[0][1] != "/data/downloads/tv/.tmp" {
		t.Errorf("torrents.db row 1 = %v", rows[0])
	}
	if rows[1][0] != "downloads/Movies" {
		t.Errorf("a relative portable path was rewritten: %v", rows[1])
	}
	decoded, err := bencode.Decode(rows[0][2].([]byte))
	if err != nil {
		t.Fatalf("rewritten blob is not bencode: %v", err)
	}
	if got := decoded.Lookup("save_path"); got == nil || string(got.Bytes) != "/data/downloads/tv" {
		t.Errorf("blob save_path = %v, want the box's path", got)
	}

}

func TestUncoveredLiveKeyBlocks(t *testing.T) {
	dir := source(t)
	conf := filepath.Join(dir, confRel)
	writeFile(t, conf, strings.Replace(qbitConf(), `Session\TempPath="/srv/My Downloads/temp"`,
		`Session\TempPath=/mnt/elsewhere/temp`, 1))
	in := testInput(t, dir)
	before := snapshot(t, dir)

	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !rep.Blocked() {
		t.Fatal("a live path no rule covers did not block the run")
	}
	if len(rep.Uncovered) != 1 || rep.Uncovered[0].Path != "/mnt/elsewhere/temp" ||
		rep.Uncovered[0].Key != `BitTorrent Session\TempPath` {
		t.Errorf("uncovered = %v", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err == nil {
		t.Fatal("Write ran over a gap Check found")
	}
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Error("a refused Write changed the config directory")
	}
}

func TestUncoveredCosmeticKeyIsLeftAlone(t *testing.T) {
	dir := source(t)
	writeFile(t, filepath.Join(dir, confRel), strings.Replace(qbitConf(), `General\CustomUIThemePath=/mnt/theme`,
		`General\CustomUIThemePath=/mnt/another/theme`, 1))
	in := testInput(t, dir)

	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("a GUI-history path blocked the run: %v", rep.Uncovered)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if conf := readFile(t, filepath.Join(dir, confRel)); !strings.Contains(conf, `General\CustomUIThemePath=/mnt/another/theme`) {
		t.Errorf("a cosmetic path was changed:\n%s", conf)
	}
}

func TestOlderLayoutRefused(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config", "qBittorrent.conf"), qbitConf())
	writeFile(t, filepath.Join(dir, "data", "BT_backup", "h1.fastresume"), string(resumeBlob("/downloads/tv")))
	in := testInput(t, dir)

	_, err := (rewriter{}).Check(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "config/qBittorrent.conf") {
		t.Fatalf("Check error = %v, want it to name the older layout", err)
	}
}

func TestMissingConfRefused(t *testing.T) {
	dir := t.TempDir()
	in := testInput(t, dir)
	_, err := (rewriter{}).Check(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), confRel) {
		t.Fatalf("Check error = %v, want it to name the file", err)
	}
}

func TestLegacyResumeStoreAlone(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, dirRel)
	writeFile(t, filepath.Join(root, "qBittorrent.conf"), "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	writeFile(t, filepath.Join(root, "BT_backup", "h1.fastresume"), string(resumeBlob("/downloads/tv")))
	in := testInput(t, dir)

	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	if len(rep.Notes) != 0 {
		t.Errorf("notes = %v, want none: one store is not worth a note", rep.Notes)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "BT_backup", "h1.fastresume")); !strings.Contains(got, "/data/downloads/tv") {
		t.Errorf("resume file = %q", got)
	}
}

func TestNoResumeStoreNoted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, dirRel, "qBittorrent.conf"), "[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")
	in := testInput(t, dir)

	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "no resume data") {
		t.Errorf("notes = %v, want one saying there is no resume data", rep.Notes)
	}
}

func TestLegacyMapWithoutItsJSONFileRefused(t *testing.T) {
	dir := t.TempDir()
	conf := "[BitTorrent]\nSession\\Categories=@Variant(\\0\\0\\0\\v\\0\\0\\0\\x)\nSession\\DefaultSavePath=/downloads\n"
	writeFile(t, filepath.Join(dir, dirRel, "qBittorrent.conf"), conf)
	in := testInput(t, dir)

	_, err := (rewriter{}).Check(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "categories.json") {
		t.Fatalf("Check error = %v, want it to name the file that settles the map", err)
	}
}

func TestConfRoundTripsWhatItDoesNotChange(t *testing.T) {
	// A conf with a comment, an unusual blank line and an empty value: none of
	// them may move.
	raw := "[BitTorrent]\nSession\\DefaultSavePath=/downloads/tv\n\n; a comment\n\n[WebUI]\nPort=8080\n\n"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, dirRel, "qBittorrent.conf"), raw)
	in := testInput(t, dir)

	if _, err := (rewriter{}).Check(context.Background(), in); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := strings.Replace(raw, "/downloads/tv", "/data/downloads/tv", 1)
	if got := readFile(t, filepath.Join(dir, dirRel, "qBittorrent.conf")); got != want {
		t.Errorf("conf =\n%q\nwant\n%q", got, want)
	}
}

func TestSplitAndJoinList(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{`"/a b", /c`, []string{"/a b", "/c"}},
		{`/a`, []string{"/a"}},
		{``, nil},
		{`"a,b", c`, []string{"a,b", "c"}},
	}
	for _, tt := range tests {
		if got := splitList(tt.raw); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitList(%q) = %v, want %v", tt.raw, got, tt.want)
		}
		if got := joinList(tt.want); got != tt.raw {
			t.Errorf("joinList(%v) = %q, want %q", tt.want, got, tt.raw)
		}
	}
}

// TestEveryPathKeyIsRewritten builds a conf holding every key the table names
// and proves each one is rewritten: the table is what decides what a migrated
// conf still points at the source's filesystem.
func TestEveryPathKeyIsRewritten(t *testing.T) {
	doc := &conf{}
	var wanted []confKey
	for _, k := range confKeys {
		if k.kind == legacyMap {
			continue
		}
		doc.set(k.section, k.name, `/downloads/x`)
		wanted = append(wanted, k)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, dirRel, "qBittorrent.conf"), string(doc.bytes()))
	in := testInput(t, dir)

	rep, err := (rewriter{}).Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.Blocked() {
		t.Fatalf("Check blocked: %v", rep.Uncovered)
	}
	count := 0
	for _, c := range rep.Changes {
		if c.File == confRel && c.What == "path" {
			count += c.Count
		}
	}
	if count != len(wanted) {
		t.Errorf("conf changes = %d, want one per key (%d): %+v", count, len(wanted), rep.Changes)
	}
	if _, err := (rewriter{}).Write(context.Background(), in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := parseConf([]byte(readFile(t, filepath.Join(dir, dirRel, "qBittorrent.conf"))))
	for _, k := range wanted {
		value, ok := after.get(k.section, k.name)
		if !ok {
			t.Errorf("%s %s vanished", k.section, k.name)
			continue
		}
		if k.kind == pathList {
			if got := splitList(value); len(got) != 1 || got[0] != "/data/downloads/x" {
				t.Errorf("%s %s = %v", k.section, k.name, got)
			}
			continue
		}
		if got := unquote(value); got != "/data/downloads/x" {
			t.Errorf("%s %s = %q, want the box's path", k.section, k.name, got)
		}
	}
}
