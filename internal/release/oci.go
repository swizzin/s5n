package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	helmregistry "helm.sh/helm/v4/pkg/registry"
	"oras.land/oras-go/v2/errdef"
	orasregistry "oras.land/oras-go/v2/registry"
	orasauth "oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/swizzin/s5n/internal/fetch"
	"github.com/swizzin/s5n/internal/say"
)

// DefaultChartRepo is where s5n publishes its chart (docs/box.md, "Where a
// release lives"); box never reads charts/ from its own tree.
const DefaultChartRepo = "oci://ghcr.io/swizzin/charts/s5n"

// MaxChartArchive bounds a downloaded chart: the artifact is untrusted input to
// a root process. Helm's loader caps decompression (100 MiB); this caps what box
// downloads at all.
const MaxChartArchive = 8 << 20

// OCIChart pulls a chart with Helm's registry client, the same code that pushes
// it, so the tag-to-version mapping (`_` for `+`) is not reimplemented.
//
// The artifact is untrusted: the archive is capped before unpacking, files are
// capped individually, and schema $refs are refused (internal/seedbox.Validate).
type OCIChart struct {
	// BoxVersion is this box's version; "dev" skips the min-box check once.
	BoxVersion string
	// Say receives the one line about a release box skipped. Nil is silent.
	Say say.Reporter
	// Retry is bootstrap's download policy (internal/fetch): GET/HEAD, 5xx,
	// 429, a refused, reset or timed-out connection, four attempts.
	Retry fetch.Policy

	devOnce sync.Once

	// mu guards pulled. One pull per version: update asks for every release's
	// annotations, then loads the ones it installs. A failed pull is not cached.
	mu     sync.Mutex
	pulled map[string]Chart
}

func (o *OCIChart) logf(format string, args ...any) {
	if o.Say == nil {
		return
	}
	o.Say.Line(fmt.Sprintf(format, args...))
}

// Load pulls one release, or the newest this box can install. The min-box
// refusal is here, before Applier has a cluster client. The context reaches
// Helm's background-context requests through the transport, so a cancelled
// apply stops a pull in flight and the wait between attempts.
func (o *OCIChart) Load(ctx context.Context, ref ChartRef) (Chart, error) {
	p, err := o.open(ctx, ref)
	if err != nil {
		return Chart{}, err
	}
	defer p.Close()

	version := strings.TrimSpace(ref.Version)
	if version != "" {
		if _, err := parseVersion(version); err != nil {
			return Chart{}, fmt.Errorf("--chart-version %q is not a version: %v", version, err)
		}
		chart, err := o.chart(p, version)
		if err != nil {
			return Chart{}, err
		}
		if err := CheckMinBox(o.BoxVersion, chart); err != nil {
			return Chart{}, err
		}
		return chart, nil
	}

	tags, err := p.client.Tags(p.base)
	if err != nil {
		return Chart{}, o.pullError(p.repo, "", err)
	}
	chosen, skipped, err := SelectVersion(tags, o.BoxVersion, func(version string) (string, error) {
		chart, err := o.chart(p, version)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(chart.Annotations[MinBoxAnnotation]), nil
	}, nil)
	if err != nil {
		return Chart{}, o.noVersionError(p.repo, err)
	}
	for _, skip := range skipped {
		o.logf("%s %s is newer but needs box %s; this is box %s, so box installs %s",
			chartName(p.repo), skip.Version, skip.MinBox, o.BoxVersion, chosen)
	}
	return o.chart(p, chosen)
}

// Releases lists published releases at or above floor, oldest first. Old charts
// may not be readable by today's box (no platform pins, no schema), so starting
// at the installed release avoids failing on history. A release at or above the
// floor that cannot be read is still an error; an empty listing is current.
// Pulled charts are cached, so the settled release is not downloaded twice.
// Non-version tags and prereleases are left out: box never installs one unasked.
func (o *OCIChart) Releases(ctx context.Context, ref ChartRef, floor string) ([]Published, error) {
	p, err := o.open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer p.Close()

	tags, err := p.client.Tags(p.base)
	if err != nil {
		return nil, o.pullError(p.repo, "", err)
	}
	// An unreadable floor lists everything: never call such a box current.
	var from *semver.Version
	if f := strings.TrimSpace(floor); f != "" {
		if v, err := semver.NewVersion(f); err == nil {
			from = v
		}
	}
	var out []Published
	versions := 0
	for _, tag := range tags {
		v, err := semver.NewVersion(tag)
		if err != nil || v.Prerelease() != "" {
			continue
		}
		versions++
		if from != nil && v.LessThan(from) {
			continue
		}
		chart, err := o.chart(p, tag)
		if err != nil {
			return nil, err
		}
		out = append(out, Published{
			Version:     tag,
			MinBox:      strings.TrimSpace(chart.Annotations[MinBoxAnnotation]),
			UpgradeFrom: strings.TrimSpace(chart.Annotations[UpgradeFromAnnotation]),
		})
	}
	if versions == 0 {
		return nil, o.noVersionError(p.repo, fmt.Errorf("no published release is a version box can read"))
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := semver.NewVersion(out[i].Version)
		b, _ := semver.NewVersion(out[j].Version)
		return a.LessThan(b)
	})
	return out, nil
}

// puller is one registry session and its private Helm home; Close removes it.
type puller struct {
	repo, base string
	client     *helmregistry.Client
	env        *helmEnv
}

func (p *puller) Close() {
	if p.env != nil {
		p.env.Close()
	}
}

// open builds the client Load and Releases share. The private Helm home means
// the operator's Helm configuration cannot redirect or poison a pull.
func (o *OCIChart) open(ctx context.Context, ref ChartRef) (*puller, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repo, base, err := chartRepo(ref.Repo)
	if err != nil {
		return nil, err
	}
	plain, err := plainHTTPFor(base)
	if err != nil {
		return nil, err
	}
	env, err := newHelmEnv()
	if err != nil {
		return nil, fmt.Errorf("chart registry for %s: %w", repo, err)
	}
	client, err := o.client(ctx, env, plain)
	if err != nil {
		env.Close()
		return nil, fmt.Errorf("chart registry for %s: %w", repo, err)
	}
	if IsDevVersion(o.BoxVersion) {
		o.devOnce.Do(func() {
			o.logf("box was built without a version (%s): skipping the chart's %s check", DevVersion, MinBoxAnnotation)
		})
	}
	return &puller{repo: repo, base: base, client: client, env: env}, nil
}

// chart pulls a version at most once; a failure is not remembered.
func (o *OCIChart) chart(p *puller, version string) (Chart, error) {
	o.mu.Lock()
	chart, ok := o.pulled[version]
	o.mu.Unlock()
	if ok {
		return chart, nil
	}
	chart, err := o.pull(p.client, p.repo, p.base, version)
	if err != nil {
		return Chart{}, err
	}
	o.mu.Lock()
	if o.pulled == nil {
		o.pulled = make(map[string]Chart)
	}
	o.pulled[version] = chart
	o.mu.Unlock()
	return chart, nil
}

// client builds the registry client, discarding Helm's own output. The
// authorizer carries no credential, so neither Helm's nor docker's config.json
// is queried: every pull is anonymous.
func (o *OCIChart) client(ctx context.Context, env *helmEnv, plainHTTP bool) (*helmregistry.Client, error) {
	httpClient := &http.Client{
		Transport: o.transport(ctx, nil),
		Timeout:   2 * time.Minute,
	}
	opts := []helmregistry.ClientOption{
		helmregistry.ClientOptWriter(io.Discard),
		helmregistry.ClientOptHTTPClient(httpClient),
		helmregistry.ClientOptCredentialsFile(env.registryConfig),
		helmregistry.ClientOptAuthorizer(orasauth.Client{Client: httpClient}),
	}
	if plainHTTP {
		opts = append(opts, helmregistry.ClientOptPlainHTTP())
	}
	return helmregistry.NewClient(opts...)
}

// transport layers, outermost first: capped (refuses over MaxChartArchive),
// pull (installs the pull's context), retry (bootstrap's policy), net. The cap
// is outside the retry so the surviving body is the one capped and a 5xx body
// is never read.
func (o *OCIChart) transport(ctx context.Context, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return cappedTransport{max: MaxChartArchive, base: pullContext{ctx: ctx, base: fetch.Transport{
		Policy: o.Retry,
		Base:   base,
	}}}
}

// pullContext installs the pull's context on requests Helm built with
// context.Background() (v4.3.1, registry/generic.go). The request's own context
// stays the parent, so the client's deadline survives; a child joins the two.
type pullContext struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t pullContext) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ctx.Done() == nil {
		// Nothing can cancel this pull; the request already carries a deadline.
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	// The pull ending cancels this request; the client releases the child when
	// it cancels the request's context.
	_ = context.AfterFunc(t.ctx, cancel)
	return t.base.RoundTrip(req.WithContext(ctx))
}

// pull downloads one version and loads it as Helm will.
func (o *OCIChart) pull(client *helmregistry.Client, repo, base, version string) (Chart, error) {
	ref := base + ":" + version
	got, err := client.Pull(ref)
	if err != nil {
		return Chart{}, o.pullError(repo, version, err)
	}
	if got.Chart == nil || len(got.Chart.Data) == 0 {
		return Chart{}, fmt.Errorf("release %s: the registry returned no chart layer", repo+":"+version)
	}
	if int64(len(got.Chart.Data)) > MaxChartArchive {
		return Chart{}, fmt.Errorf("release %s: the chart archive is %d bytes; box reads at most %d",
			repo+":"+version, len(got.Chart.Data), MaxChartArchive)
	}
	ch, err := loader.LoadArchive(bytes.NewReader(got.Chart.Data))
	if err != nil {
		return Chart{}, fmt.Errorf("release %s: not a chart archive: %w", repo+":"+version, err)
	}
	if ch.Metadata != nil && ch.Metadata.Version != "" && ch.Metadata.Version != version {
		return Chart{}, fmt.Errorf("release %s: the archive's Chart.yaml says version %s; box asked for %s",
			repo+":"+version, ch.Metadata.Version, version)
	}
	chart, err := chartFrom(repo+":"+version, ch)
	if err != nil {
		return Chart{}, err
	}
	chart.Archive = got.Chart.Data
	return chart, nil
}

// pullError is box's voice for a registry that answered nothing usable.
// ghcr's 403 for a missing or private package means the same as 404.
func (o *OCIChart) pullError(repo, version string, err error) error {
	if version == "" {
		if notPublished(err) {
			return fmt.Errorf("no published releases at %s: nothing has been published there yet", repo)
		}
		return fmt.Errorf("cannot list the releases at %s: %w", repo, err)
	}
	if notPublished(err) {
		return fmt.Errorf("no published release %s at %s: nothing with that version has been published", version, repo)
	}
	if statusOf(err) == http.StatusUnauthorized {
		return fmt.Errorf("release %s at %s: the registry wants credentials, and box has none for it yet", version, repo)
	}
	return fmt.Errorf("cannot pull release %s from %s: %w", version, repo, err)
}

// noVersionError keeps selectVersion's reason and adds the ref.
func (o *OCIChart) noVersionError(repo string, err error) error {
	return fmt.Errorf("%s: %v", repo, err)
}

// chartRepo returns the oci:// reference and the scheme-stripped one Helm takes.
func chartRepo(ref string) (display, base string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = DefaultChartRepo
	}
	ref = strings.TrimSuffix(ref, "/")
	if i := strings.Index(ref, "://"); i >= 0 && !strings.HasPrefix(ref, "oci://") {
		return "", "", fmt.Errorf("chart repo %s: box pulls charts over OCI, so the reference must start with oci://, e.g. %s", ref, DefaultChartRepo)
	}
	base = strings.TrimPrefix(ref, "oci://")
	if base == "" || !strings.Contains(base, "/") {
		return "", "", fmt.Errorf("chart repo %s: want oci://<registry>/<path>, e.g. %s", ref, DefaultChartRepo)
	}
	if _, err := orasregistry.ParseReference(base); err != nil {
		return "", "", fmt.Errorf("chart repo %s: %v", ref, err)
	}
	return ref, base, nil
}

// plainHTTPFor allows plain HTTP only for a loopback registry; a chart is what
// box installs as root.
func plainHTTPFor(base string) (bool, error) {
	ref, err := orasregistry.ParseReference(base)
	if err != nil {
		return false, fmt.Errorf("chart repo %s: %v", base, err)
	}
	host := ref.Registry
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback(), nil
	}
	return false, nil
}

// chartName is the repository's last path element, for a skipped-release line.
func chartName(repo string) string {
	base := strings.TrimPrefix(strings.TrimSuffix(repo, "/"), "oci://")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		return base[i+1:]
	}
	return base
}

func statusOf(err error) int {
	var resp *errcode.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode
	}
	return 0
}

func notPublished(err error) bool {
	switch statusOf(err) {
	case http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return errors.Is(err, errdef.ErrNotFound)
}

// cappedTransport refuses a response larger than max on the way in, so a
// hostile registry cannot make box allocate before a size check. It is the
// outer layer, so the capped body is the one that survived the retries.
type cappedTransport struct {
	max  int64
	base http.RoundTripper
}

func (t cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.max {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: the response is %d bytes; box reads at most %d", req.URL, resp.ContentLength, t.max)
	}
	resp.Body = &cappedBody{body: resp.Body, remaining: t.max, max: t.max}
	return resp, nil
}

type cappedBody struct {
	body      io.ReadCloser
	remaining int64
	max       int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n, fmt.Errorf("the response is larger than box reads (%d bytes)", b.max)
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.body.Close() }
