package peerbanhelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// configFixture is config.yml shaped the way PBH's own template is at v9.5.4:
// a bilingual comment over most settings, `server:` near the top, and a block
// of the same shape for every downloader under `client:`. The downloaders are
// what the app writes when one is added through its WebUI, and the ids, hosts
// and credentials are invented.
const configFixture = `config-version: 48
# 设置程序语言
# Set the program language
language: default
# Http 服务器设置
# Http Server Settings
server:
  # WebUI 监听端口
  # WebUI listen port
  http: 8088
  # WebUI 监听地址
  # WebUI listen address
  address: "127.0.0.1"
  # PBH 需要给下载器传递地址时使用此地址
  # When PBH needs to pass the URL of blocklist to downloader, it uses this address
  prefix: "http://127.0.0.1:8088"
  # 要访问 WebUI 端点，则需要 Token
  # To access the WebUI endpoint, a token is required
  token: "source-token"
  allow-cors: false
# 日志记录器配置
# Logger configurer
logger:
  hide-finish-log: false
client:
  qbt-main:
    type: qbittorrent
    name: qBittorrent
    endpoint: http://localhost:8080
    username: admin
    password: synthetic-password
    verify-ssl: true
  remote-nas:
    type: qbittorrent
    name: NAS
    endpoint: http://192.0.2.10:8081
    username: nas
    password: synthetic
`

const planYAML = `version: 2
apps:
  pbh:
    kind: kubernetes
    source: pbh
    config: /srv/apps/pbh
    ports: [8088]
    paths: []
  qbittorrent:
    kind: kubernetes
    source: qbittorrent
    config: /srv/apps/qbittorrent
    ports: [8080]
    paths: []
`

func targets() map[string]rewrite.Endpoint {
	return map[string]rewrite.Endpoint{
		"pbh":         {Host: "pbh", Port: 9898},
		"qbittorrent": {Host: "qbittorrent", Port: 8080},
	}
}

func setup(t *testing.T) rewrite.Input {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(configRel)), []byte(configFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(planYAML))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return rewrite.Input{App: "pbh", Dir: dir, Plan: p, Targets: targets()}
}

// TestRegistered keeps apply from meeting an app with no rewriter: every app
// migrate copies is registered, and pbh's answer is the listen settings and the
// downloaders above rather than "nothing to do".
func TestRegistered(t *testing.T) {
	if _, ok := rewrite.For("pbh"); !ok {
		t.Fatal("pbh has no rewriter registered")
	}
}

// TestRewritesListenSettingsAndDownloaders is the whole answer: the app's own
// listener becomes the chart's, the qBittorrent downloader points at the box and
// is filed for adoption by its string id, and the LAN downloader is reported
// rather than guessed at.
func TestRewritesListenSettingsAndDownloaders(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	before := treeHash(t, in.Dir)

	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unclassified) != 1 {
		t.Fatalf("Unclassified = %v, want the LAN downloader", rep.Unclassified)
	}
	got := rep.Unclassified[0]
	if got.File != configRel || got.Where != "client/remote-nas/endpoint" || got.Host != "192.0.2.10" || got.Port != 8081 {
		t.Errorf("Unclassified[0] = %+v", got)
	}
	if !rep.Blocked() {
		t.Error("an unclassified downloader did not block")
	}
	if after := treeHash(t, in.Dir); after != before {
		t.Errorf("Check changed the config: %s -> %s", before, after)
	}
	if _, err := (rewriter{}).Write(ctx, in); err == nil {
		t.Error("Write ran with an unclassified address")
	}

	// The plan can keep the NAS where it is.
	in.Plan.Hosts = append(in.Plan.Hosts, plan.HostRule{Host: "192.0.2.10", Leave: true})
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() {
		t.Fatalf("a leave rule did not settle the NAS: %v", rep.Unclassified)
	}
	want := map[string]int{
		"server/http":                1,
		"server/address":             1,
		"server/prefix":              1,
		"client/qbt-main/endpoint":   1,
		"client/remote-nas/endpoint": 0,
	}
	gotChanges := map[string]int{}
	for _, c := range rep.Changes {
		if c.File != configRel {
			t.Errorf("change in %q, want %q", c.File, configRel)
		}
		gotChanges[c.Where] += c.Count
	}
	for where, n := range want {
		if gotChanges[where] != n {
			t.Errorf("%s: %d changes, want %d (all: %v)", where, gotChanges[where], n, rep.Changes)
		}
	}
	wantOwned := []rewrite.Owned{{
		Kind: downloaderKind, StringID: "qbt-main", Name: "qBittorrent", Target: "qbittorrent",
	}}
	if len(rep.Owned) != 1 || rep.Owned[0] != wantOwned[0] {
		t.Errorf("Owned = %#v, want %#v", rep.Owned, wantOwned)
	}

	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(in.Dir, filepath.FromSlash(configRel)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, kept := range []string{
		"# 设置程序语言",
		"  # WebUI 监听端口\n",
		"  # WebUI 监听地址\n",
		`  token: "source-token"` + "\n",
		"  allow-cors: false\n",
		"    endpoint: http://192.0.2.10:8081\n",
		"    password: synthetic-password\n",
		"  hide-finish-log: false\n",
	} {
		if !strings.Contains(text, kept) {
			t.Errorf("%q is not in the rewritten file:\n%s", kept, text)
		}
	}
	for _, want := range []string{
		"  http: 9898\n",
		`  address: "0.0.0.0"` + "\n",
		`  prefix: "http://pbh:9898"` + "\n",
		"    endpoint: http://qbittorrent:8080\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config.yml is missing %q:\n%s", want, text)
		}
	}

	// A second pass over the copy the first one wrote has nothing left to do,
	// and no address is unclassified: the downloader names the box, and
	// in.Resolve knows its own endpoints.
	rep2, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Changes) != 0 {
		t.Errorf("after Write, Check still reports %v", rep2.Changes)
	}
	if len(rep2.Unclassified) != 0 {
		t.Errorf("after Write, Check reports unclassified addresses: %v", rep2.Unclassified)
	}
	if len(rep2.Owned) != 1 || rep2.Owned[0] != wantOwned[0] {
		t.Errorf("after Write, Owned = %#v", rep2.Owned)
	}
}

// TestAddsListenKeysTheFileLacks: PBH's own config updater fills a missing
// template key from its defaults on start, which would leave the app on its own
// port. The key is added to the block the app keeps it in, in that block's
// indentation.
func TestAddsListenKeysTheFileLacks(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	trimmed := strings.Replace(configFixture, `  prefix: "http://127.0.0.1:8088"`+"\n", "", 1)
	if trimmed == configFixture {
		t.Fatal("fixture did not lose its prefix")
	}
	if err := os.WriteFile(filepath.Join(in.Dir, filepath.FromSlash(configRel)), []byte(trimmed), 0o600); err != nil {
		t.Fatal(err)
	}
	in.Plan.Hosts = append(in.Plan.Hosts, plan.HostRule{Host: "192.0.2.10", Leave: true})
	if _, err := (rewriter{}).Check(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := (rewriter{}).Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(in.Dir, filepath.FromSlash(configRel)))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "  prefix: http://pbh:9898\n") && !strings.Contains(text, `  prefix: "http://pbh:9898"`) {
		t.Errorf("the missing prefix was not added to server:\n%s", text)
	}
	// It went inside server:, not after client:'s downloaders.
	if strings.Index(text, "prefix:") > strings.Index(text, "client:") {
		t.Errorf("the added key landed outside server:\n%s", text)
	}
	if strings.Count(text, "prefix:") != 1 {
		t.Errorf("the key was added twice:\n%s", text)
	}
}

// TestMissingConfigIsNoted: a source that never started PBH has no config.yml,
// and there is nothing to rewrite in one.
func TestMissingConfigIsNoted(t *testing.T) {
	in := rewrite.Input{App: "pbh", Dir: t.TempDir()}
	rep, err := rewriter{}.Check(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocked() || len(rep.Changes) != 0 {
		t.Errorf("Check = %+v, want nothing", rep)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], configRel) {
		t.Errorf("Notes = %v, want the missing file named", rep.Notes)
	}
}

// TestUnsettledQbittorrentDownloaderProposesTheBoxsClient: the downloader's own
// type says which app its address can only mean. Box runs one qBittorrent, so a
// qBittorrent downloader nothing settles is proposed as that app, for Settle to
// write as a default; the rewriter still reports the address and writes nothing.
// A downloader of another type is not proposed.
func TestUnsettledQbittorrentDownloaderProposesTheBoxsClient(t *testing.T) {
	ctx := context.Background()
	in := setup(t)
	rep, err := rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := rewrite.Default{
		Host: "192.0.2.10", Port: 8081, App: "qbittorrent",
		Why: "it is a qBittorrent downloader, and this box runs one qBittorrent",
	}
	if len(rep.Defaults) != 1 || rep.Defaults[0] != want {
		t.Fatalf("Defaults = %+v, want %+v", rep.Defaults, want)
	}

	// A downloader of a type box does not run says nothing about the box.
	in = setup(t)
	raw, err := os.ReadFile(filepath.Join(in.Dir, filepath.FromSlash(configRel)))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "    type: qbittorrent\n    name: NAS", "    type: transmission\n    name: NAS", 1))
	if err := os.WriteFile(filepath.Join(in.Dir, filepath.FromSlash(configRel)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = rewriter{}.Check(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Defaults) != 0 {
		t.Errorf("Defaults = %+v, want none for a downloader box does not run", rep.Defaults)
	}
}

// treeHash is every file's name, mode and bytes, so a pass that writes anything
// at all changes it.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h.Write([]byte(rel))
		h.Write([]byte(info.Mode().String()))
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
