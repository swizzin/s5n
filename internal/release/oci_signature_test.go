package release

// A chart is installed as root, so box checks the signature a chart's release publishes
// over its manifest digest before it pulls anything, and then pulls that digest rather
// than whatever the tag points at by then. The tests here are the two servers a pull
// reads: oci_test.go's registry, and the release page that signs what it holds.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
)

// signKey is the key a test's release page signs with, and the verifier a source gets.
// cmd/box fills that seam from internal/selfupdate, which release cannot import (it
// imports release), so the wire format is written out a second time here: a change to it
// on either side fails these tests.
type signKey struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func newSignKey(t *testing.T) signKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return signKey{private: private, public: public}
}

// message is what a chart's signature covers: its release's line, then the digest.
func chartSigned(version, digest string) []byte {
	return []byte("s5n chart s5n-v" + version + "\n" + digest + "\n")
}

// asset is what a release publishes for one digest: base64, one line.
func (k signKey) asset(version, digest string) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(k.private, chartSigned(version, digest))) + "\n")
}

// verify is release's verification seam, as cmd/box fills it.
func (k signKey) verify(version, digest string, signature []byte) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || !ed25519.Verify(k.public, chartSigned(version, digest), raw) {
		return fmt.Errorf("the chart's signature does not verify against the signing key compiled into this box (it covers s5n-v%s and the chart's digest): refusing to install a chart this box cannot trust", version)
	}
	return nil
}

// releasePage serves a chart's GitHub release assets, at the path box fetches them
// from: /releases/download/s5n-v<version>/<asset>.
type releasePage struct {
	srv  *httptest.Server
	mu   sync.Mutex
	sigs map[string][]byte
}

func newReleasePage(t *testing.T) *releasePage {
	t.Helper()
	p := &releasePage{sigs: make(map[string][]byte)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel, ok := strings.CutPrefix(r.URL.Path, "/releases/download/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		tag, file, ok := strings.Cut(rel, "/")
		version, ok2 := strings.CutPrefix(tag, "s5n-v")
		if !ok || !ok2 || file != ChartSignatureAsset {
			http.NotFound(w, r)
			return
		}
		p.mu.Lock()
		signature, published := p.sigs[version]
		p.mu.Unlock()
		if !published {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(signature)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *releasePage) publish(version string, signature []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sigs[version] = signature
}

// unpublish removes an asset, as a release published before box checked signatures has
// none.
func (p *releasePage) unpublish(version string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sigs, version)
}

// charts is one registry and the release page that signs what is pushed into it.
type charts struct {
	host string
	page *releasePage
	key  signKey
}

func newCharts(t *testing.T) *charts {
	t.Helper()
	return chartsAt(t, registry(t))
}

// chartsAt is newCharts over a registry a test already started, such as one that fails
// requests on purpose.
func chartsAt(t *testing.T, host string) *charts {
	t.Helper()
	return &charts{host: host, page: newReleasePage(t), key: newSignKey(t)}
}

// source is an OCIChart for the registry, verified against the page.
func (c *charts) source(box string) *OCIChart {
	return &OCIChart{BoxVersion: box, ReleasesURL: c.page.srv.URL, VerifyChart: c.key.verify}
}

// chart packages this checkout's chart, pushes it and signs it, as a release does.
func (c *charts) chart(t *testing.T) []byte {
	t.Helper()
	data := pushChart(t, c.host)
	c.sign(t, chartVersion(t, data))
	return data
}

// push publishes data under an OCI tag and signs the version that tag names.
func (c *charts) push(t *testing.T, tag string, data []byte) {
	t.Helper()
	if err := pushData(t, c.host+"/charts/s5n:"+tag, data); err != nil {
		t.Fatalf("push %s: %v", tag, err)
	}
	c.sign(t, tag)
}

// sign publishes the signature over the digest the tag for version points at now.
func (c *charts) sign(t *testing.T, version string) {
	t.Helper()
	c.page.publish(version, c.key.asset(version, c.digest(t, version)))
}

// digest is what the registry's tag for version points at now: the digest a signature
// covers.
func (c *charts) digest(t *testing.T, version string) string {
	t.Helper()
	src := c.source("0.0.0")
	p, err := src.open(context.Background(), ChartRef{Repo: "oci://" + c.host + "/charts/s5n"})
	if err != nil {
		t.Fatalf("opening the registry: %v", err)
	}
	defer p.Close()
	digest, err := src.resolve(p, version)
	if err != nil {
		t.Fatalf("resolving %s: %v", version, err)
	}
	return digest
}

func (c *charts) ref() ChartRef {
	return ChartRef{Repo: "oci://" + c.host + "/charts/s5n"}
}

func TestOCIChartPullsTheDigestItsSignatureCovers(t *testing.T) {
	c := newCharts(t)
	want := c.chart(t)
	version := chartVersion(t, want)

	pulled, err := c.source(version).Load(context.Background(), c.ref())
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if pulled.Digest != c.digest(t, version) {
		t.Fatalf("chart carries digest %q, want the signed %q", pulled.Digest, c.digest(t, version))
	}
	if !strings.HasPrefix(pulled.Digest, "sha256:") {
		t.Fatalf("the recorded digest is %q, want the registry's manifest digest", pulled.Digest)
	}
	// The bytes are the ones the signature covers, not merely the ones the tag
	// happened to carry at resolve time.
	if string(pulled.Archive) != string(want) {
		t.Fatalf("pulled %d bytes, want the %d that were pushed", len(pulled.Archive), len(want))
	}
}

func TestOCIChartRefusesAChartItsSignatureDoesNotCover(t *testing.T) {
	// The digest the signature names is not the digest the tag resolves to: a
	// release page signed one chart and the registry serves another.
	c := newCharts(t)
	version := chartVersion(t, c.chart(t))
	c.page.publish(version, c.key.asset(version, "sha256:"+strings.Repeat("d", 64)))

	src := c.source(version)
	_, err := src.Load(context.Background(), c.ref())
	if err == nil {
		t.Fatal("pulled a chart whose digest its signature does not cover")
	}
	if !strings.Contains(err.Error(), "does not verify") || !strings.Contains(err.Error(), version) {
		t.Fatalf("error = %v, want the signature's refusal naming %s", err, version)
	}
	if len(src.pulled) != 0 {
		t.Fatalf("a chart that did not verify was loaded anyway: %v", src.pulled)
	}
}

func TestOCIChartRefusesAnotherReleasesSignature(t *testing.T) {
	// The replay the version in the signed message stops: this digest, signed for the
	// release after it.
	c := newCharts(t)
	c.chart(t)
	version := "9.9.9"
	c.push(t, version, archiveChart(t, changedChart(t, version)))
	c.page.publish(version, c.key.asset("9.9.8", c.digest(t, version)))

	_, err := c.source(version).Load(context.Background(), c.ref())
	if err == nil {
		t.Fatal("pulled a chart under another release's signature")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("error = %v, want the signature's refusal", err)
	}
}

func TestOCIChartRefusesABoxReleasesSignature(t *testing.T) {
	// A box release signs a line naming box-v<version> and then a file's bytes. The
	// same digest under that message is not this chart's release, and box must not
	// read one kind of signature as the other.
	c := newCharts(t)
	version := chartVersion(t, c.chart(t))
	digest := c.digest(t, version)
	message := []byte("s5n box release box-v" + version + "\n" + digest + "\n")
	c.page.publish(version, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(c.key.private, message))+"\n"))

	_, err := c.source(version).Load(context.Background(), c.ref())
	if err == nil {
		t.Fatal("pulled a chart under a box release's signature")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("error = %v, want the signature's refusal", err)
	}
}

func TestOCIChartRefusesAReleaseWithNoSignature(t *testing.T) {
	// Every chart release is signed; one without an asset is one no box installs.
	c := newCharts(t)
	version := chartVersion(t, c.chart(t))
	c.page.unpublish(version)

	_, err := c.source(version).Load(context.Background(), c.ref())
	if err == nil {
		t.Fatal("pulled a chart nothing signed")
	}
	for _, want := range []string{ChartSignatureAsset, "s5n-v" + version, "no signature"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to name %q", err, want)
		}
	}
}

func TestOCIChartRefusesEveryChartInAKeylessBox(t *testing.T) {
	// signing.pub is committed empty until the maintainer fills it in, and a box with
	// no key cannot verify a chart: there is no flag that skips the check.
	c := newCharts(t)
	version := chartVersion(t, c.chart(t))
	src := &OCIChart{BoxVersion: version, ReleasesURL: c.page.srv.URL}
	_, err := src.Load(context.Background(), c.ref())
	if err == nil {
		t.Fatal("a box with no signing key installed a chart")
	}
	if !strings.Contains(err.Error(), "built without a signing key") {
		t.Fatalf("error = %v, want the missing-key refusal", err)
	}
}

func TestOCIPullsTheDigestItResolvedWhenTheTagMoves(t *testing.T) {
	// Resolve, then the tag moves: box installs the bytes the signature covers, not
	// what the tag points at when the pull runs.
	c := newCharts(t)
	want := c.chart(t)
	version := chartVersion(t, want)
	src := c.source(version)

	p, err := src.open(context.Background(), c.ref())
	if err != nil {
		t.Fatalf("opening the registry: %v", err)
	}
	defer p.Close()
	digest, err := src.resolve(p, version)
	if err != nil {
		t.Fatalf("resolving %s: %v", version, err)
	}

	// The same tag, now another chart. A pull by tag would install this one.
	c.push(t, version, archiveChart(t, changedChart(t, version)))
	if same := c.digest(t, version); same == digest {
		t.Fatalf("the tag still points at %s; the test moved nothing", digest)
	}

	pulled, err := src.pull(p.client, p.repo, p.base, version, digest)
	if err != nil {
		t.Fatalf("pull by digest: %v", err)
	}
	if pulled.Digest != digest {
		t.Fatalf("pulled %s, want the resolved %s", pulled.Digest, digest)
	}
	if string(pulled.Archive) != string(want) {
		t.Fatalf("pulled the moved tag's chart, not the one the signature covers")
	}
}

// changedChart is this checkout's chart at another version, so a tag can be moved onto
// bytes that differ from the ones a signature covers.
func changedChart(t *testing.T, version string) *chartv2.Chart {
	t.Helper()
	ch, err := loader.LoadArchive(bytes.NewReader(packageChart(t, filepath.Join("..", "..", "charts", "s5n"))))
	if err != nil {
		t.Fatalf("loading the packaged chart: %v", err)
	}
	setChartVersion(t, ch, version, "")
	return ch
}
