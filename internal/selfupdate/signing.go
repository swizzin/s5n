// The release's signature: the one decoder and verifier. See doc.go for why
// box will not update itself without one.

package selfupdate

import (
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// signing.pub is the base64 of the release key's 32 bytes, committed empty for
// the maintainer to fill: a key generated in the repository is a key in the
// repository. An empty file is never a licence to skip verification; Update
// refuses with it.
//
//go:embed signing.pub
var embeddedKey string

// signingKey is the key a release is verified against, a variable only this
// package's tests may replace; it is not an Options field.
var signingKey = embeddedKey

// errNoKey is what a box built without a signing key says: updates are unsafe,
// so it must be replaced by hand once.
var errNoKey = errors.New("this box was built without a signing key, so it cannot verify a box release and will not update itself: replace it by hand")

// ParsePublicKey reads signing.pub's form: base64 of the 32 key bytes, as
// `hack/sign keygen` prints it.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	raw, err := decodeKey(text, ed25519.PublicKeySize)
	if err != nil {
		if errors.Is(err, errNoKey) {
			return nil, err
		}
		return nil, fmt.Errorf("the signing key is not a key (%v), so box cannot verify a release and will not update itself: replace it by hand", err)
	}
	return ed25519.PublicKey(raw), nil
}

// ParseSeed reads the release's signing secret: base64 of the 32 seed bytes.
func ParseSeed(text string) (ed25519.PrivateKey, error) {
	raw, err := decodeKey(text, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("the signing seed: %w", err)
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// decodeKey is the one decoder both key forms use, so an empty value means the
// same thing everywhere.
func decodeKey(text string, size int) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("not base64: %v", err)
	}
	if len(raw) != size {
		return nil, fmt.Errorf("%d bytes, want %d", len(raw), size)
	}
	return raw, nil
}

// signingLine separates a box release's signature from any other use of this
// key, and names the tag family.
const signingLine = "s5n box release box-v"

// signedMessage is what a release's signature covers: the fixed line naming its
// tag, then its exact sha256sums.txt.
//
// The version is in the message because checksums are not unique to a release:
// an older release's signed sums and binary, served under a newer tag by a
// mirror, proxy or stale cache, would otherwise verify and install as a
// downgrade to a box with a known hole.
func signedMessage(version string, sums []byte) []byte {
	message := make([]byte, 0, len(signingLine)+len(version)+1+len(sums))
	message = append(message, signingLine...)
	message = append(message, version...)
	message = append(message, '\n')
	return append(message, sums...)
}

// SignSums returns a release's sha256sums.txt.sig: base64 of the 64 signature
// bytes on one line. The signer and verifier share it. The tag is not in the
// file, which stays plain `sha256sum -c` input; the message carries it.
func SignSums(seed ed25519.PrivateKey, version string, sums []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(seed, signedMessage(version, sums))) + "\n")
}

// VerifySums checks a release's sha256sums.txt against its .sig. A signature
// that does not verify is not a warning: box is about to install a binary that
// runs as root. Another version's signature is another release's, and refused.
func VerifySums(key ed25519.PublicKey, version string, sums, signature []byte) error {
	if len(key) != ed25519.PublicKeySize {
		return errNoKey
	}
	sig, err := signatureBytes("sha256sums.txt.sig", signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, signedMessage(version, sums), sig) {
		return fmt.Errorf("sha256sums.txt for %s%s does not verify against the signing key compiled into this box (the signature covers the release's tag as well as its checksums): refusing to install a release this box cannot trust", tagPrefix, version)
	}
	return nil
}

// VerifyChartSignature checks a chart's signature against the key box was built with.
// cmd/box fills release's verification seam with it, so every pulled chart is checked the
// way a box release is, before anything of it is read. A box built without a key refuses
// in its own words: there is no path that installs an unverified chart.
func VerifyChartSignature(version, digest string, signature []byte) error {
	key, err := ParsePublicKey(signingKey)
	if err != nil {
		if errors.Is(err, errNoKey) {
			return errors.New("this box was built without a signing key, so it cannot verify a chart and will not install one: replace box by hand")
		}
		return fmt.Errorf("the signing key is not a key (%v), so box cannot verify a chart: replace box by hand", err)
	}
	return VerifyChart(key, version, digest, signature)
}

// signatureBytes decodes one .sig asset: base64 of the 64 signature bytes, on one line.
// A stray CR from a proxy's rewrite is not a different signature.
func signatureBytes(name string, signature []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil {
		return nil, fmt.Errorf("%s is not base64 (%v): refusing to install a release whose signature cannot be read", name, err)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%s is %d bytes, want %d: refusing to install a release whose signature cannot be read", name, len(raw), ed25519.SignatureSize)
	}
	return raw, nil
}

// chartLine separates a chart's signature from a box release's and names its tag
// family, so no signature of one kind can be read as the other.
const chartLine = "s5n chart s5n-v"

// ChartMessage is what a chart's signature covers: the line naming its release,
// then the chart's manifest digest. The version is in the message because a tag
// can be moved: a signature over a digest alone would still be this chart's, and
// one over a version alone would sign whatever the tag pointed at.
func ChartMessage(version, digest string) []byte {
	message := make([]byte, 0, len(chartLine)+len(version)+len(digest)+2)
	message = append(message, chartLine...)
	message = append(message, version...)
	message = append(message, '\n')
	message = append(message, digest...)
	return append(message, '\n')
}

// SignChart returns the chart release's signature, the asset published on its
// s5n-v<version> GitHub release. It covers the digest, so box installs the bytes
// the signer saw and not whatever a tag points at later.
func SignChart(seed ed25519.PrivateKey, version, digest string) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(seed, ChartMessage(version, digest))) + "\n")
}

// VerifyChart checks a chart's manifest digest against the signature its release
// publishes. A chart installs as root, so a missing or unverifiable signature is
// a refusal; box has no flag that skips it.
func VerifyChart(key ed25519.PublicKey, version, digest string, signature []byte) error {
	if len(key) != ed25519.PublicKeySize {
		return errNoKey
	}
	sig, err := signatureBytes("chart.sig", signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, ChartMessage(version, digest), sig) {
		return fmt.Errorf("the chart's signature does not verify against the signing key compiled into this box (it covers s5n-v%s and the chart's digest): refusing to install a chart this box cannot trust", version)
	}
	return nil
}
