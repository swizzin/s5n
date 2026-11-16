// Package selfupdate replaces box's own binary with a newer signed release.
//
// box runs as root and is the thing that decides what the rest of the host
// runs, so an update that anyone on the network can steer is remote code
// execution as root. Two rules follow, and the package is small because they
// are the whole design.
//
// The release has to be signed. box downloads a release's sha256sums.txt and
// sha256sums.txt.sig, verifies the ed25519 signature over the release's tag and
// the exact bytes of the checksums against a key compiled into the binary, and
// only then reads the checksum for its own architecture. The tag is in the
// signed message because the checksums alone are not a release: an older
// release's signed sums served under a newer tag would otherwise verify, and box
// would install an older binary as the new one. A box built without a key
// refuses to update itself at all; it never falls back to trusting the
// checksums alone, because a checksum served beside the binary proves only that
// both came from the same place. The wire format lives in signing.go rather
// than in the release script, so the signer (hack/sign, which the release
// workflow runs) and the verifier cannot drift.
//
// The replacement has to be atomic, and reversible once. The new binary is
// written beside the old one under a temporary name, fsynced, hashed again from
// disk, and renamed over box; the binary it replaced is kept as box.previous
// for Rollback, which is one step and no more. An interrupted update leaves the
// old binary where it was, and no stray files beside it.
//
// The entry points, in the order a `box update` meets them:
//
//   - Check answers "is there a newer box" and writes nothing. It is what a
//     user's box calls: only root may replace the binary, so a user's copy
//     reports the newer version and root's next update installs it.
//   - Update replaces the binary. It refuses, in this order, when this build
//     has no version to compare (a development build), when this process is
//     already the new version re-executed (ReexecEnv), when it is not root, and
//     when box came from a package. Then it verifies the release, installs it,
//     and leaves the previous binary beside it.
//   - Rollback puts the previous binary back.
//   - Reexec hands the process over to the binary just installed, so the update
//     that asked for the replacement finishes under the new version.
//
// Only a newer version is ever installed, in semver order, and a release the
// caller holds is named but not taken. A box built without a version (a
// development build) refuses to update itself: there is nothing to compare the
// release against.
package selfupdate
