package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

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
// release lives"). box pulls a chart by version from here; it never reads
// charts/ from its own tree.
const DefaultChartRepo = "oci://ghcr.io/swizzin/charts/s5n"

// MaxChartArchive bounds the packaged chart box will accept from a registry.
// The real chart is well under a megabyte; the cap is here because the artifact
// is untrusted input to a process that may run as root. Helm's own loader caps
// what an archive may decompress to (100 MiB); this caps what box downloads at
// all.
const MaxChartArchive = 8 << 20

// OCIChart pulls a published chart release with Helm's registry client, which
// is the only registry code box has: the same code that pushes a chart reads
// it, so the tag-to-version mapping (an OCI tag, plus Helm's `_` for `+` rule)
// is not reimplemented here.
//
// The pulled artifact is untrusted: the archive is capped before it is
// unpacked, the files box reads are capped individually, and the schema is
// compiled with external $refs refused (internal/seedbox.Validate). Nothing
// here executes anything the chart ships.
type OCIChart struct {
	// BoxVersion is this box's version. "dev" (a build without -X
	// main.version) skips the min-box check and says so once.
	BoxVersion string
	// Say receives the one line about a release box skipped. Nil is silent.
	Say say.Reporter
	// Retry is the policy a failed registry request is retried under: the
	// same rule bootstrap's downloads use (internal/fetch), so an upstream
	// 5xx during apply's chart pull is retried instead of aborting the
	// operator's apply. The zero value is box's policy: GET/HEAD only, 5xx
	// and 429 and a refused, reset or timed-out connection, four attempts.
	Retry fetch.Policy

	devOnce sync.Once
}

func (o *OCIChart) logf(format string, args ...any) {
	if o.Say == nil {
		return
	}
	o.Say.Line(fmt.Sprintf(format, args...))
}

// Load pulls one chart release, or the newest this box can install when
// ref.Version is empty. The min-box refusal happens here, before Applier has a
// cluster client, so a chart box is too old for cannot have written anything.
//
// The context is the pull's: Helm's registry client builds its requests with
// context.Background() and takes none, so the transport installs it on every
// attempt (see transport). A cancelled apply therefore stops a pull in flight,
// and stops the wait between a retried attempt.
func (o *OCIChart) Load(ctx context.Context, ref ChartRef) (Chart, error) {
	if err := ctx.Err(); err != nil {
		return Chart{}, err
	}
	repo, base, err := chartRepo(ref.Repo)
	if err != nil {
		return Chart{}, err
	}
	plain, err := plainHTTPFor(base)
	if err != nil {
		return Chart{}, err
	}
	// One private Helm home for this pull: its credentials path is inside the
	// directory box creates, so Helm reads nothing of the operator's, and the
	// directory is removed before Load returns.
	env, err := newHelmEnv()
	if err != nil {
		return Chart{}, fmt.Errorf("chart registry for %s: %w", repo, err)
	}
	defer env.Close()
	client, err := o.client(ctx, env, plain)
	if err != nil {
		return Chart{}, fmt.Errorf("chart registry for %s: %w", repo, err)
	}
	if IsDevVersion(o.BoxVersion) {
		o.devOnce.Do(func() {
			o.logf("box was built without a version (%s): skipping the chart's %s check", DevVersion, MinBoxAnnotation)
		})
	}

	// One pull per version: picking a version reads each candidate's min-box,
	// and the release it settles on must not be downloaded a second time.
	loaded := make(map[string]Chart)
	once := func(version string) (Chart, error) {
		if ch, ok := loaded[version]; ok {
			return ch, nil
		}
		ch, err := o.pull(client, repo, base, version)
		if err != nil {
			return Chart{}, err
		}
		loaded[version] = ch
		return ch, nil
	}

	version := strings.TrimSpace(ref.Version)
	if version != "" {
		if _, err := parseVersion(version); err != nil {
			return Chart{}, fmt.Errorf("--chart-version %q is not a version: %v", version, err)
		}
		chart, err := once(version)
		if err != nil {
			return Chart{}, err
		}
		if err := CheckMinBox(o.BoxVersion, chart); err != nil {
			return Chart{}, err
		}
		return chart, nil
	}

	tags, err := client.Tags(base)
	if err != nil {
		return Chart{}, o.pullError(repo, "", err)
	}
	chosen, skipped, err := selectVersion(tags, o.BoxVersion, func(version string) (string, error) {
		chart, err := once(version)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(chart.Annotations[MinBoxAnnotation]), nil
	})
	if err != nil {
		return Chart{}, o.noVersionError(repo, err)
	}
	for _, skip := range skipped {
		o.logf("%s %s is newer but needs box %s; this is box %s, so box installs %s",
			chartName(repo), skip.Version, skip.MinBox, o.BoxVersion, chosen)
	}
	return once(chosen)
}

// client builds the registry client. Helm writes "Pulled: ... Digest: ..." to
// its own writer, which would be box's voice saying someone else's lines, so it
// is discarded; the HTTP client caps what one response may be and refuses to
// wait on a registry forever.
//
// env is the private Helm home the client's credentials file points into, and
// the authorizer carries no credential, so no credentials store -- Helm's
// registry/config.json or docker's config.json -- is ever queried: every pull
// is anonymous.
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

// transport is what every request of one pull goes through, outermost first:
//
//	capped  refuses a response over MaxChartArchive
//	pull    gives the attempt the pull's context
//	retry   fetch.Transport's policy, the one bootstrap's downloads use
//	net     base, or http.DefaultTransport
//
// The cap is outside the retry on purpose: the body that survives the retries
// is the one capped, and a 5xx body is never read. base is the network; nil
// means http.DefaultTransport, and a test replaces it.
func (o *OCIChart) transport(ctx context.Context, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return cappedTransport{max: MaxChartArchive, base: pullContext{ctx: ctx, base: fetch.Transport{
		Policy: o.Retry,
		Base:   base,
	}}}
}

// pullContext installs the pull's context on a request Helm's registry client
// built with context.Background() (v4.3.1, registry/generic.go: every request
// of a pull). Without it neither a cancelled apply nor the retry policy's wait
// could see the context, because the policy reads the request's own.
//
// The request's context stays the parent: http.Client.Timeout puts its
// deadline there, and replacing it would turn a stalled registry into an
// unbounded wait. The two are joined instead, so a cancelled apply and a
// timed-out request both stop it.
type pullContext struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t pullContext) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ctx.Done() == nil {
		// Nothing can cancel this pull (context.Background): the request's
		// own context already carries the client's deadline, and there is
		// no registration worth making.
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	// The pull ending cancels this request. Nothing stops the registration
	// here: when the client is done with the response it stops its timer and
	// cancels the request's context, which releases this child with it.
	_ = context.AfterFunc(t.ctx, cancel)
	return t.base.RoundTrip(req.WithContext(ctx))
}

// pull downloads one version and reads it the way Helm will install it.
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

// pullError is box's voice for a registry that answered nothing usable. A
// missing or private release is the common case (a fresh project, a typo'd
// --chart-version), and Helm's own text for it is a chain of ORAS errors that
// names no release. 403 is what ghcr answers for a package that is not there
// or not public, so it means the same thing to box as 404.
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

// noVersionError says why the default version is not there: either the
// repository publishes nothing box recognises as a release, or every release
// needs a newer box. selectVersion already names the skips, so this keeps the
// reason and adds the ref.
func (o *OCIChart) noVersionError(repo string, err error) error {
	return fmt.Errorf("%s: %v", repo, err)
}

// chartRepo validates an --chart-repo value and returns it in both forms box
// needs: the oci:// reference it prints, and the scheme-stripped reference
// Helm's client takes.
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

// plainHTTPFor says whether a repository may be spoken to over plain HTTP.
// Only a loopback registry may: it is a test's local registry, reached over a
// socket on this host. A registry anywhere else gets TLS, because a chart is
// what box installs as root.
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

// chartName is the repository's last path element: the chart's name, for the
// one line box prints about a release it skipped.
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

// cappedTransport refuses a response larger than max. It is what keeps a
// hostile registry from making box allocate an unbounded download before any
// size check could see it: the check has to be on the way in, not after.
//
// It is the outer layer of transport: base is the retrying fetch under it, so
// what the cap sees is the response that survived the retries, and a retried
// body is still capped.
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
