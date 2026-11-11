package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	// The registry in this test stores everything in memory: no disk, no
	// daemon. Importing the driver is how distribution registers it.
	_ "github.com/distribution/distribution/v3/registry/storage/driver/inmemory"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	helmregistry "helm.sh/helm/v4/pkg/registry"

	"github.com/swizzin/s5n/internal/fetch"
	"github.com/swizzin/s5n/internal/say"
)

// registryHandler is the in-process OCI registry's handler: distribution's app
// over its in-memory driver. It is the same server Helm's own registry tests
// run against, so the round trip below exercises the real push and pull paths,
// not a fake of them.
func registryHandler(t *testing.T) http.Handler {
	t.Helper()
	cfg := &configuration.Configuration{
		Version: "0.1",
		Storage: configuration.Storage{
			"inmemory": configuration.Parameters{},
		},
		HTTP: configuration.HTTP{
			Addr:   "localhost:0",
			Secret: "box-test-registry-secret",
		},
		Log: configuration.Log{Level: "error", Formatter: "text"},
	}
	return handlers.NewApp(context.Background(), cfg)
}

// registry is that handler on a server of its own.
func registry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registryHandler(t))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushChart packages charts/s5n from this checkout and pushes it, the way the
// release workflow does with helm package and helm push.
func pushChart(t *testing.T, host string) []byte {
	t.Helper()
	data := packageChart(t, filepath.Join("..", "..", "charts", "s5n"))
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("load the packaged chart: %v", err)
	}
	client, err := helmregistry.NewClient(helmregistry.ClientOptWriter(io.Discard), helmregistry.ClientOptPlainHTTP())
	if err != nil {
		t.Fatal(err)
	}
	ref := host + "/charts/" + ch.Metadata.Name + ":" + ch.Metadata.Version
	if _, err := client.Push(data, ref); err != nil {
		t.Fatalf("push %s: %v", ref, err)
	}
	return data
}

// packageChart builds a chart archive in memory. helm package is not available
// to a Go test, and an archive is what a registry carries: the files, under a
// <name>/ prefix, gzipped and tarred.
func packageChart(t *testing.T, dir string) []byte {
	t.Helper()
	ch, err := loader.LoadDir(dir)
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     ch.Metadata.Name + "/" + filepath.ToSlash(rel),
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatalf("package %s: %v", dir, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestOCIRoundTrip: the chart this checkout ships, pushed to a registry and
// pulled by version, is the same Chart the fixture loader reads from the
// directory. box reads one chart, not two dialects of it; the directory is a
// test fixture, never a source box installs from.
func TestOCIRoundTrip(t *testing.T) {
	host := registry(t)
	want := pushChart(t, host)
	pulled, err := (&OCIChart{BoxVersion: "0.0.0"}).Load(context.Background(), ChartRef{
		Repo:    "oci://" + host + "/charts/s5n",
		Version: chartVersion(t, want),
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	fromDir, err := loadChartDir(DevVersion, filepath.Join("..", "..", "charts", "s5n"))
	if err != nil {
		t.Fatalf("load the directory: %v", err)
	}

	if pulled.Name != "s5n" || pulled.Name != fromDir.Name {
		t.Fatalf("name = %q, want s5n from both sources", pulled.Name)
	}
	if pulled.Version == "" || pulled.Version != fromDir.Version {
		t.Fatalf("version = %q, want the directory's %q", pulled.Version, fromDir.Version)
	}
	if pulled.AppVersion != fromDir.AppVersion {
		t.Fatalf("appVersion = %q, want %q", pulled.AppVersion, fromDir.AppVersion)
	}
	if pulled.Annotations[MinBoxAnnotation] != fromDir.Annotations[MinBoxAnnotation] {
		t.Fatalf("min-box = %q, want %q", pulled.Annotations[MinBoxAnnotation], fromDir.Annotations[MinBoxAnnotation])
	}
	// The pins carry the k0s checksums too, a map, so the comparison is by
	// value rather than by struct identity.
	if !reflect.DeepEqual(pulled.Pins, fromDir.Pins) {
		t.Fatalf("pins = %+v, want the directory's %+v", pulled.Pins, fromDir.Pins)
	}
	for name, pair := range map[string][2][]byte{
		"values.yaml":        {pulled.Values, fromDir.Values},
		"values.schema.json": {pulled.Schema, fromDir.Schema},
		"catalog.yaml":       {pulled.Catalog, fromDir.Catalog},
		"variants.yaml":      {pulled.Variants, fromDir.Variants},
	} {
		if len(pair[0]) == 0 || !bytes.Equal(pair[0], pair[1]) {
			t.Fatalf("%s: pulled %d bytes, directory %d bytes, equal=%v",
				name, len(pair[0]), len(pair[1]), bytes.Equal(pair[0], pair[1]))
		}
	}
	if len(pulled.Archive) == 0 || len(pulled.Archive) != len(want) {
		t.Fatalf("archive = %d bytes, want the %d that were pushed", len(pulled.Archive), len(want))
	}
	if fromDir.Archive != nil {
		t.Fatalf("a directory chart carries no archive")
	}
}

// TestOCINoPublishedRelease: a tag nobody published is box's sentence, not
// Helm's error chain.
func TestOCINoPublishedRelease(t *testing.T) {
	host := registry(t)
	pushChart(t, host)
	_, err := (&OCIChart{BoxVersion: "0.0.0"}).Load(context.Background(), ChartRef{
		Repo:    "oci://" + host + "/charts/s5n",
		Version: "9.9.9",
	})
	if err == nil {
		t.Fatal("pull of a version nobody published succeeded")
	}
	want := "no published release 9.9.9 at oci://" + host + "/charts/s5n: nothing with that version has been published"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// TestOCIEmptyRepository: a repository nothing has been published to is box's
// sentence, and it is not the same sentence as a version that is missing.
func TestOCIEmptyRepository(t *testing.T) {
	host := registry(t)
	repo := "oci://" + host + "/charts/s5n"
	_, err := (&OCIChart{BoxVersion: "0.1.0"}).Load(context.Background(), ChartRef{Repo: repo})
	if err == nil {
		t.Fatal("pull from an empty repository succeeded")
	}
	want := "no published releases at " + repo + ": nothing has been published there yet"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// TestOCIPicksTheNewestReleaseBoxCanUse: pushing two releases and adding a
// floor to the newer one selects the older, and says which one it skipped and
// why. The skip is on stdout, in box's voice.
func TestOCIPicksTheNewestReleaseBoxCanUse(t *testing.T) {
	host := registry(t)
	pushChart(t, host)

	// A second release, 0.9.0, whose floor is above this box.
	data := packageChart(t, filepath.Join("..", "..", "charts", "s5n"))
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	setChartVersion(t, ch, "0.9.0", "9.9.9")
	if err := pushData(t, host+"/charts/s5n:0.9.0", archiveChart(t, ch)); err != nil {
		t.Fatalf("push 0.9.0: %v", err)
	}

	var log bytes.Buffer
	src := &OCIChart{BoxVersion: "0.1.0", Say: &say.Plain{W: &log}}
	got, err := src.Load(context.Background(), ChartRef{Repo: "oci://" + host + "/charts/s5n"})
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, log.String())
	}
	if got.Version == "0.9.0" {
		t.Fatalf("installed 0.9.0 although it needs box 9.9.9")
	}
	if !strings.Contains(log.String(), "0.9.0 is newer but needs box 9.9.9") {
		t.Fatalf("the skipped release is not named:\n%s", log.String())
	}
}

// TestOCIRefusesAFloorAboveBox: an explicitly requested release box is too old
// for is refused, naming both versions, and the refusal happens before apply
// has a cluster client — so nothing can have been written.
func TestOCIRefusesAFloorAboveBox(t *testing.T) {
	host := registry(t)
	pushChart(t, host)
	data := packageChart(t, filepath.Join("..", "..", "charts", "s5n"))
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	setChartVersion(t, ch, "0.4.0", "0.5.0")
	if err := pushData(t, host+"/charts/s5n:0.4.0", archiveChart(t, ch)); err != nil {
		t.Fatalf("push 0.4.0: %v", err)
	}

	f := newFakes(t)
	f.app.Charts = &OCIChart{BoxVersion: "0.4.0"}
	_, err = f.app.Apply(context.Background(), Options{
		ChartRepo:    "oci://" + host + "/charts/s5n",
		ChartVersion: "0.4.0",
		GrantPath:    f.grantPath,
		ValuesPath:   f.values,
	})
	if err == nil {
		t.Fatal("apply installed a release that needs a newer box")
	}
	if !strings.Contains(err.Error(), "needs box 0.5.0") || !strings.Contains(err.Error(), "this is box 0.4.0") {
		t.Fatalf("error does not name both versions: %v", err)
	}
	if f.openCalls != 0 || f.storeOpens != 0 || f.rel.upgrades != 0 || len(f.store.applied) != 0 || f.store.nsCreates != 0 {
		t.Fatalf("the refusal touched the cluster: open=%d storageOpen=%d upgrades=%d applied=%d namespaces=%d",
			f.openCalls, f.storeOpens, f.rel.upgrades, len(f.store.applied), f.store.nsCreates)
	}
}

// TestOCIDevBoxSkipsTheFloorOnce: a box built without a version cannot be
// compared with a floor. It skips the check and says so exactly once per run.
func TestOCIDevBoxSkipsTheFloorOnce(t *testing.T) {
	host := registry(t)
	pushChart(t, host)
	var log bytes.Buffer
	src := &OCIChart{BoxVersion: DevVersion, Say: &say.Plain{W: &log}}
	for i := 0; i < 2; i++ {
		if _, err := src.Load(context.Background(), ChartRef{Repo: "oci://" + host + "/charts/s5n"}); err != nil {
			t.Fatalf("pull %d: %v", i, err)
		}
	}
	if n := strings.Count(log.String(), "built without a version"); n != 1 {
		t.Fatalf("the dev notice printed %d times, want once:\n%s", n, log.String())
	}
}

func chartVersion(t *testing.T, data []byte) string {
	t.Helper()
	ch, err := loader.LoadArchive(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return ch.Metadata.Version
}

// setChartVersion rewrites Chart.yaml's version, appVersion and min-box, so a
// test can publish a release whose floor differs from the one in this checkout.
// It edits the bytes the chart was loaded from, which is what helm package
// ships.
func setChartVersion(t *testing.T, ch *chartv2.Chart, version, minBox string) {
	t.Helper()
	for _, f := range ch.Raw {
		if f.Name != "Chart.yaml" {
			continue
		}
		text := regexp.MustCompile(`(?m)^version:.*$`).ReplaceAllString(string(f.Data), "version: "+version)
		text = regexp.MustCompile(`(?m)^appVersion:.*$`).ReplaceAllString(text, `appVersion: "`+version+`"`)
		text += "annotations:\n  " + MinBoxAnnotation + ": \"" + minBox + "\"\n"
		f.Data = []byte(text)
		ch.Metadata.Version = version
		ch.Metadata.AppVersion = version
		ch.Metadata.Annotations = map[string]string{MinBoxAnnotation: minBox}
		return
	}
	t.Fatal("the chart has no Chart.yaml")
}

// archiveChart writes a modified chart back to an archive, the shape helm
// package would produce.
func archiveChart(t *testing.T, ch *chartv2.Chart) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range ch.Raw {
		hdr := &tar.Header{
			Name:     ch.Metadata.Name + "/" + f.Name,
			Mode:     0o644,
			Size:     int64(len(f.Data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pushData(t *testing.T, ref string, data []byte) error {
	t.Helper()
	client, err := helmregistry.NewClient(helmregistry.ClientOptWriter(io.Discard), helmregistry.ClientOptPlainHTTP())
	if err != nil {
		return err
	}
	if _, err := client.Push(data, ref); err != nil {
		return err
	}
	return nil
}

// TestChartRepoRefusesAURL: --chart-repo is an OCI reference; an https URL is
// a mistake box can explain rather than a TLS failure to decode.
func TestChartRepoRefusesAURL(t *testing.T) {
	_, _, err := chartRepo("https://ghcr.io/swizzin/charts/s5n")
	if err == nil || !strings.Contains(err.Error(), "oci://") {
		t.Fatalf("error = %v, want a refusal naming oci://", err)
	}
	if _, _, err := chartRepo("ghcr.io/swizzin/charts/s5n"); err != nil {
		t.Fatalf("a bare registry reference was refused: %v", err)
	}
	if plain, err := plainHTTPFor("127.0.0.1:5000/charts/s5n"); err != nil || !plain {
		t.Fatalf("loopback registry: plain=%v err=%v, want plain HTTP", plain, err)
	}
	if plain, err := plainHTTPFor("localhost:5000/charts/s5n"); err != nil || !plain {
		t.Fatalf("localhost registry: plain=%v err=%v, want plain HTTP", plain, err)
	}
	if plain, err := plainHTTPFor("ghcr.io/swizzin/charts/s5n"); err != nil || plain {
		t.Fatalf("ghcr: plain=%v err=%v, want TLS", plain, err)
	}
}

// TestUnreadableRegistryIsNotAStackOfHelm: a registry that answers 500 is not
// "no published release"; box says what it tried and keeps the cause. Two
// attempts with no wait: the point is the sentence, and the backoff is the
// other tests'.
func TestUnreadableRegistryIsNotAStackOfHelm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	repo := "oci://" + strings.TrimPrefix(srv.URL, "http://") + "/charts/s5n"
	src := &OCIChart{BoxVersion: "0.1.0", Retry: instantRetry(2)}
	_, err := src.Load(context.Background(), ChartRef{Repo: repo, Version: "1.2.3"})
	if err == nil {
		t.Fatal("pull from a broken registry succeeded")
	}
	if strings.Contains(err.Error(), "no published release") {
		t.Fatalf("a server failure read as a missing release: %v", err)
	}
	if !strings.Contains(err.Error(), repo) || !strings.Contains(err.Error(), "1.2.3") {
		t.Fatalf("error does not name the ref and the version: %v", err)
	}
}

// poisonRegistryHome writes the operator Helm state a pull must never read and
// points the process environment at it. The registry config is not valid JSON:
// Helm's registry client parses its credentials file when it is constructed, so
// if box let that path default to helmpath.ConfigPath (HELM_REGISTRY_CONFIG,
// then ~/.config/helm) the pull would fail before it reached the registry. A
// stale repository entry and a plugin getter are there for the same reason they
// are in the platform test: none of root's Helm environment decides what box
// installs.
//
// It returns the root of the poisoned tree, for a before/after comparison.
func poisonRegistryHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "config", "registry", "config.json"), `{"auths": `)
	write(filepath.Join(root, "config", "repositories.yaml"), `apiVersion: ""
generated: "2020-01-01T00:00:00Z"
repositories:
  - name: stale
    url: https://charts.example.com
`)
	write(filepath.Join(root, "plugins", "box-getter", "plugin.yaml"), `name: box-getter
version: 0.1.0
downloaders:
  - command: "box-getter"
    protocols:
      - "https"
`)
	for key, path := range map[string]string{
		"HOME":                   root,
		"XDG_CONFIG_HOME":        filepath.Join(root, "config"),
		"XDG_CACHE_HOME":         filepath.Join(root, "cache"),
		"HELM_CONFIG_HOME":       filepath.Join(root, "config"),
		"HELM_CACHE_HOME":        filepath.Join(root, "cache"),
		"HELM_REPOSITORY_CONFIG": filepath.Join(root, "config", "repositories.yaml"),
		"HELM_REPOSITORY_CACHE":  filepath.Join(root, "cache", "repository"),
		"HELM_REGISTRY_CONFIG":   filepath.Join(root, "config", "registry", "config.json"),
		"HELM_PLUGINS":           filepath.Join(root, "plugins"),
	} {
		t.Setenv(key, path)
	}
	return root
}

// treeSnapshot renders every entry under root: path, mode, size, mtime and the
// content hash of a file. Directory mtimes are included so a write Helm cleans
// up again -- the shape of helmpath's scratch index -- is still visible.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hash := ""
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			hash = hex.EncodeToString(sum[:])
		}
		fmt.Fprintf(&b, "%s mode=%s size=%d mtime=%s sha=%s\n",
			rel, info.Mode(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), hash)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return b.String()
}

// TestOCIPullIgnoresTheOperatorsRegistryConfig: root's Helm registry config is
// unparseable and its repository state is stale, and box still pulls a public
// chart anonymously. Nothing in that tree is read for a secret or written to.
func TestOCIPullIgnoresTheOperatorsRegistryConfig(t *testing.T) {
	host := registry(t)
	want := pushChart(t, host)

	poison := poisonRegistryHome(t)
	before := treeSnapshot(t, poison)

	pulled, err := (&OCIChart{BoxVersion: "0.0.0"}).Load(context.Background(), ChartRef{
		Repo:    "oci://" + host + "/charts/s5n",
		Version: chartVersion(t, want),
	})
	if err != nil {
		t.Fatalf("the operator's Helm environment must not decide what box pulls: %v", err)
	}
	if !bytes.Equal(pulled.Archive, want) {
		t.Fatalf("pulled %d bytes, want the %d that were pushed", len(pulled.Archive), len(want))
	}
	if after := treeSnapshot(t, poison); after != before {
		t.Fatalf("box wrote into the operator's Helm home:\n--- before\n%s--- after\n%s", before, after)
	}
}

// TestOCIPullLeavesNoTemporaryHelmHome: the private home lives for one pull.
// TMPDIR is a sandbox, so a leftover directory is visible; the failure path is
// checked too, because a missing release is the common case and a defer is easy
// to miss on it.
func TestOCIPullLeavesNoTemporaryHelmHome(t *testing.T) {
	host := registry(t)
	want := pushChart(t, host)
	version := chartVersion(t, want)

	root := t.TempDir()
	sandbox := filepath.Join(root, "tmp")
	if err := os.MkdirAll(sandbox, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", sandbox)

	src := &OCIChart{BoxVersion: "0.0.0"}
	if _, err := src.Load(context.Background(), ChartRef{Repo: "oci://" + host + "/charts/s5n", Version: version}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if left := tempEntries(t, sandbox); len(left) != 0 {
		t.Fatalf("a successful pull left %v behind in its temporary directory", left)
	}

	if _, err := src.Load(context.Background(), ChartRef{Repo: "oci://" + host + "/charts/s5n", Version: "9.9.9"}); err == nil {
		t.Fatal("a version nobody published must fail the pull")
	}
	if left := tempEntries(t, sandbox); len(left) != 0 {
		t.Fatalf("a failed pull left %v behind in its temporary directory", left)
	}
}

func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// instantRetry is box's retry policy with the waits taken out, so a test that
// retries does not spend the backoff. The attempt count stays the caller's.
func instantRetry(attempts int) fetch.Policy {
	return fetch.Policy{
		Attempts: attempts,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
}

// stubTransport is the network under OCIChart.transport: reply answers the
// nth attempt, and tries counts them. It lets a test pin the retry rule and
// the response cap without a registry.
type stubTransport struct {
	tries int
	reply func(try int, req *http.Request) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.tries++
	return s.reply(s.tries, req)
}

// stubResponse is one scripted answer. A negative length leaves the size
// unknown, the way a chunked response arrives.
func stubResponse(req *http.Request, status int, body string, length int64) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: length,
		Request:       req,
	}
}

// chartRequest is one manifest GET, the request a pull makes.
func chartRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://registry.example.com/v2/charts/s5n/manifests/1.0.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestOCIPullRetriesATransientRegistryFailure: a registry that answers 503
// once, in the middle of a pull, does not abort apply's chart pull. It is the
// same rule bootstrap's downloads follow, and it is the failure that made a
// first install fail right after a bootstrap that survived it.
func TestOCIPullRetriesATransientRegistryFailure(t *testing.T) {
	host, failures, served := unhealthyRegistry(t)
	want := pushChart(t, host)
	failures.Store(1)

	pulled, err := (&OCIChart{BoxVersion: "0.0.0", Retry: instantRetry(4)}).Load(context.Background(), ChartRef{
		Repo:    "oci://" + host + "/charts/s5n",
		Version: chartVersion(t, want),
	})
	if err != nil {
		t.Fatalf("a 503 from the registry aborted the pull: %v", err)
	}
	if n := served.Load(); n != 1 {
		t.Fatalf("the registry turned away %d requests, want the 1 this pull retried", n)
	}
	if !bytes.Equal(pulled.Archive, want) {
		t.Fatalf("pulled %d bytes after the retry, want the %d that were pushed", len(pulled.Archive), len(want))
	}
}

// TestChartTransportRetriesAServerError: a 503 once then a 200 is one retried
// attempt, not a failed pull, and the body that arrives is the second one.
func TestChartTransportRetriesAServerError(t *testing.T) {
	stub := &stubTransport{reply: func(try int, req *http.Request) (*http.Response, error) {
		if try == 1 {
			return stubResponse(req, http.StatusServiceUnavailable, "upstream is unwell", -1), nil
		}
		return stubResponse(req, http.StatusOK, "chart", 5), nil
	}}
	tr := (&OCIChart{Retry: instantRetry(4)}).transport(context.Background(), stub)
	resp, err := tr.RoundTrip(chartRequest(t))
	if err != nil {
		t.Fatalf("a 503 once aborted the request: %v", err)
	}
	defer resp.Body.Close()
	if stub.tries != 2 {
		t.Fatalf("the stub saw %d attempts, want 2", stub.tries)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "chart" {
		t.Fatalf("body = %q (err %v), want the retried attempt's \"chart\"", body, err)
	}
}

// TestChartTransportDoesNotRetryANotFound: a 404 is the registry's answer. A
// second attempt would get the same one, so the retry must not make it.
func TestChartTransportDoesNotRetryANotFound(t *testing.T) {
	stub := &stubTransport{reply: func(_ int, req *http.Request) (*http.Response, error) {
		return stubResponse(req, http.StatusNotFound, `{"errors":[]}`, -1), nil
	}}
	tr := (&OCIChart{Retry: instantRetry(4)}).transport(context.Background(), stub)
	resp, err := tr.RoundTrip(chartRequest(t))
	if err != nil {
		t.Fatalf("a 404 came back as an error: %v", err)
	}
	defer resp.Body.Close()
	if stub.tries != 1 {
		t.Fatalf("the stub saw %d attempts for a 404, want exactly 1", stub.tries)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestChartTransportCapsTheBodyAfterARetry: the size cap is outside the retry,
// so the body a retry landed on is capped exactly like a first attempt's --
// both when the registry declares the length and when it streams it.
func TestChartTransportCapsTheBodyAfterARetry(t *testing.T) {
	const over = int64(MaxChartArchive) + 1
	for name, reply := range map[string]func(req *http.Request) *http.Response{
		"declared length": func(req *http.Request) *http.Response {
			return stubResponse(req, http.StatusOK, "unread", over)
		},
		"streamed length": func(req *http.Request) *http.Response {
			return stubResponse(req, http.StatusOK, strings.Repeat("x", int(over)), -1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubTransport{reply: func(try int, req *http.Request) (*http.Response, error) {
				if try == 1 {
					return stubResponse(req, http.StatusServiceUnavailable, "upstream is unwell", -1), nil
				}
				return reply(req), nil
			}}
			tr := (&OCIChart{Retry: instantRetry(4)}).transport(context.Background(), stub)
			resp, err := tr.RoundTrip(chartRequest(t))
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("a %d-byte response was accepted after a retry", over)
			}
			if !strings.Contains(err.Error(), "box reads at most") && !strings.Contains(err.Error(), "larger than box reads") {
				t.Fatalf("error = %v, want the size cap's sentence", err)
			}
			if stub.tries != 2 {
				t.Fatalf("the stub saw %d attempts, want the retry then the over-size answer", stub.tries)
			}
		})
	}
}

// TestChartTransportStopsWhenThePullIsCancelled: a cancelled apply does not sit
// out the backoff. Helm's registry client gives its requests no context, so the
// transport installs the pull's; this is the proof it reaches the wait.
func TestChartTransportStopsWhenThePullIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The pull is cancelled as the first attempt fails, which is when the
	// policy would otherwise start waiting.
	stub := &stubTransport{reply: func(_ int, req *http.Request) (*http.Response, error) {
		cancel()
		return stubResponse(req, http.StatusServiceUnavailable, "upstream is unwell", -1), nil
	}}
	// A wait long enough that a policy which ignored the context would hold
	// the request for it.
	tr := (&OCIChart{Retry: fetch.Policy{Attempts: 4, Delay: 30 * time.Second, MaxDelay: 30 * time.Second}}).
		transport(ctx, stub)

	start := time.Now()
	resp, err := tr.RoundTrip(chartRequest(t))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a cancelled pull waited %s in the backoff", took)
	}
	if resp != nil {
		resp.Body.Close()
		t.Fatalf("a cancelled pull returned a response")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if stub.tries != 1 {
		t.Fatalf("the stub saw %d attempts after cancellation, want 1", stub.tries)
	}
}

// unhealthyRegistry is the in-process registry behind a front that answers 503
// while `failures` is positive and counts what it turned away. A test pushes
// its chart, sets failures, then pulls: one 503 during a pull is an upstream
// having a bad moment, which is the case box's retry exists for.
func unhealthyRegistry(t *testing.T) (host string, failures, served *atomic.Int32) {
	t.Helper()
	failures, served = new(atomic.Int32), new(atomic.Int32)
	app := registryHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(-1) >= 0 {
			served.Add(1)
			http.Error(w, "upstream is unwell", http.StatusServiceUnavailable)
			return
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), failures, served
}
