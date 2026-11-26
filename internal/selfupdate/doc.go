// Package selfupdate replaces box's own binary with a newer signed release.
//
// box runs as root, so a steerable update would be remote code execution. A
// release is trusted only when its sha256sums.txt verifies against a compiled-in
// key over a message including the tag: an older release's genuine sums under a
// newer tag would otherwise install as a downgrade. A box with no key refuses to
// update at all; signing.go holds the format so signer and verifier cannot drift.
//
// The swap is atomic and reversible once: staged beside box, fsynced, rehashed
// and renamed, leaving box.previous for Rollback. An interrupted update leaves
// the old binary in place.
package selfupdate
