package apps

import (
	"crypto/sha256"
	"encoding/hex"
)

// SecretFingerprint is sha256-hex of a secret, the only form of one the wiring
// Job may persist. qui and autobrr never return a stored password or API key
// (qui omits it, autobrr masks it as "<redacted>"), so the fingerprint recorded
// in owned.json is what makes a rotated secret in the release Secret visible:
// it is compared with the fingerprint of the value about to be written, and a
// difference means the app's copy is stale.
func SecretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
