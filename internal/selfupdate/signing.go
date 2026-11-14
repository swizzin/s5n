// The release's signature: what a signed sha256sums.txt looks like on disk,
// and the one decoder and verifier for it. See doc.go for why box will not
// update itself without one.

package selfupdate

import (
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// signing.pub is the public half of the key box releases are signed with: the
// standard base64 of the 32 key bytes, on one line. It is committed empty and
// the maintainer fills it in, because a key generated in the repository is a
// key in the repository. An empty file is not a licence to skip verification:
// ParsePublicKey refuses it and Update refuses with it, which is the one state
// a box can be in where self-update cannot be made safe.
//
//go:embed signing.pub
var embeddedKey string

// signingKey is the key a release is verified against. It is a variable so this
// package's tests can put a pair they generated in its place; nothing outside
// the package can substitute a key, which is why it is not an Options field.
var signingKey = embeddedKey

// errNoKey is what a box built without a signing key says. The release has an
// asset box could check itself against, so an empty key is not "nothing to
// verify with": the box is unusable for updates and has to be replaced by hand,
// once, from a release the maintainer checked.
var errNoKey = errors.New("this box was built without a signing key, so it cannot verify a box release and will not update itself: replace it by hand")

// ParsePublicKey reads a key in the form box ships it: standard base64 of the
// 32 key bytes, on one line. That is signing.pub, and it is what `hack/sign
// keygen` prints.
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

// ParseSeed reads a signing key's seed, which is what the release's signing
// secret carries: standard base64 of the 32 seed bytes, on one line.
func ParseSeed(text string) (ed25519.PrivateKey, error) {
	raw, err := decodeKey(text, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("the signing seed: %w", err)
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

// decodeKey is the one decoder both key forms use, so an empty value says the
// same thing wherever it is found and a length is checked the same way.
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

// signingLine is the fixed start of the message a release signs: the words
// that separate a box release's signature from any other use of this key, and
// the tag family the version after it belongs to.
const signingLine = "s5n box release box-v"

// signedMessage is what one release's signature covers: the fixed line naming
// the release's tag, then the exact bytes of its sha256sums.txt.
//
// The version is in the message because the checksums are not unique to a
// release. Two releases that publish the same binaries have byte-identical
// sha256sums.txt, and an older release's sums, signature and binary are all
// validly signed: served under a newer tag -- by a mirror, a proxy, a stale
// cache, anything that can answer the download -- they verify, and box would
// install the older binary as if it were the new one. That is a downgrade to a
// box with a known hole, which is the one thing the signature exists to stop.
func signedMessage(version string, sums []byte) []byte {
	message := make([]byte, 0, len(signingLine)+len(version)+1+len(sums))
	message = append(message, signingLine...)
	message = append(message, version...)
	message = append(message, '\n')
	return append(message, sums...)
}

// SignSums signs one release's sha256sums.txt and returns the contents of the
// release's sha256sums.txt.sig: the standard base64 of the 64 signature bytes
// on one line. The release workflow's signing step and this package's verifier
// share it, so what a release publishes is exactly what box reads.
//
// The tag stays out of the file itself: sha256sums.txt is plain sha256sum
// output that `sha256sum -c` reads, and the version is carried by the signed
// message instead.
func SignSums(seed ed25519.PrivateKey, version string, sums []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(seed, signedMessage(version, sums))) + "\n")
}

// VerifySums checks the two assets box fetched, exactly as the release serves
// them: the bytes of sha256sums.txt and the bytes of sha256sums.txt.sig, for
// the release the caller decided on. A signature that does not verify is not a
// warning: box is about to install a binary that will run as root, so this is
// the only thing standing between a substituted release and the host. A
// signature over another version's message is exactly that: another release's,
// and refused.
func VerifySums(key ed25519.PublicKey, version string, sums, signature []byte) error {
	if len(key) != ed25519.PublicKeySize {
		return errNoKey
	}
	// The asset is one line with a trailing newline; a stray CR from a proxy
	// that rewrote it is not a different signature.
	text := strings.TrimSpace(string(signature))
	sig, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return fmt.Errorf("sha256sums.txt.sig is not base64 (%v): refusing to install a release whose signature cannot be read", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("sha256sums.txt.sig is %d bytes, want %d: refusing to install a release whose signature cannot be read", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(key, signedMessage(version, sums), sig) {
		return fmt.Errorf("sha256sums.txt for %s%s does not verify against the signing key compiled into this box (the signature covers the release's tag as well as its checksums): refusing to install a release this box cannot trust", tagPrefix, version)
	}
	return nil
}
