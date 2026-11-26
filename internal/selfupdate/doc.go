// Package selfupdate replaces box's own binary with a newer signed release.
//
// box runs as root, so an update anyone on the network can steer would be
// remote code execution. A release is trusted only when its sha256sums.txt
// verifies against a key compiled into the binary, over a message that includes
// the release's tag: an older release's genuine sums served under a newer tag
// would otherwise pass, and box would install the older binary as the new one.
// A box built without a key refuses to update at all, never trusting checksums
// alone; signing.go holds the wire format so signer and verifier cannot drift.
//
// The replacement is atomic and reversible once: the new binary is staged
// beside the old one, fsynced, rehashed from disk and renamed over box, which
// leaves the old one as box.previous for Rollback. An interrupted update leaves
// the old binary where it was.
package selfupdate
