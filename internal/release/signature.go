package release

// A chart is what box installs as root, so a published chart is verified the way a box
// release is: the signer publishes a signature over the chart's manifest digest on the
// chart's own GitHub release (s5n-v<version>), box resolves the tag to a digest, checks
// that signature before it reads anything, and pulls by the digest rather than by the tag.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/swizzin/s5n/internal/fetch"
)

// ChartSignatureAsset is the asset a chart's GitHub release publishes its signature as:
// the base64 ed25519 signature over the chart's manifest digest, which box fetches and
// checks before it pulls the chart (internal/selfupdate makes and verifies it).
const ChartSignatureAsset = "chart.sig"

// DefaultReleasesURL is where box's and the chart's GitHub releases live: box's are the
// latest release, the chart's are one s5n-v<version> each.
const DefaultReleasesURL = "https://github.com/swizzin/s5n"

// MaxChartSignature bounds the signature download: the asset is one base64 line.
const MaxChartSignature = 4 << 10

// chartTagPrefix is the chart's tag family, on both the registry and GitHub.
const chartTagPrefix = "s5n-v"

// errNoSignature says a chart release published nothing to verify, which is its own
// refusal: a box with a key still cannot check what nothing signed.
var errNoSignature = errors.New("no signature")

// chartSignatureURL is where one release's signature is published.
func (o *OCIChart) chartSignatureURL(version string) string {
	base := strings.TrimSuffix(o.ReleasesURL, "/")
	if base == "" {
		base = DefaultReleasesURL
	}
	return base + "/releases/download/" + chartTagPrefix + version + "/" + ChartSignatureAsset
}

// verifySignature fetches the chart's published signature and checks it over the digest
// the tag resolved to. Nothing of the archive is read before this: the signature is what
// says the bytes are the ones the release signed.
func (o *OCIChart) verifySignature(p *puller, version, digest string) error {
	url := o.chartSignatureURL(version)
	who := fmt.Sprintf("chart %s %s", chartName(p.repo), version)
	signature, err := o.fetchSignature(p, url)
	if errors.Is(err, errNoSignature) {
		return fmt.Errorf("%s: no signature at %s, and box installs only a chart whose digest it can verify", who, url)
	}
	if err != nil {
		return fmt.Errorf("%s: cannot read its signature at %s: %w", who, url, err)
	}
	if o.VerifyChart == nil {
		return fmt.Errorf("%s: this box was built without a signing key, so it cannot verify a chart: replace box by hand", who)
	}
	if err := o.VerifyChart(version, digest, signature); err != nil {
		return fmt.Errorf("%s: %w", who, err)
	}
	return nil
}

// fetchSignature reads the asset. A 404 is the page saying it has none; anything else is
// the page failing, which is not the same answer.
func (o *OCIChart) fetchSignature(p *puller, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(p.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: fetch.Transport{Policy: o.Retry}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNoSignature
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("the release page answered %s", resp.Status)
	}
	signature, err := io.ReadAll(io.LimitReader(resp.Body, MaxChartSignature+1))
	if err != nil {
		return nil, err
	}
	if len(signature) > MaxChartSignature {
		return nil, fmt.Errorf("the signature is larger than %d bytes", MaxChartSignature)
	}
	return signature, nil
}
