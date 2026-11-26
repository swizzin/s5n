package apps

import (
	"crypto/sha256"
	"encoding/hex"
)

// SecretFingerprint is sha256-hex of a secret, the only form of one the Job may
// persist. qui and autobrr never return a stored secret, so the fingerprint in
// owned.json is what makes a rotation in the release Secret visible.
func SecretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
