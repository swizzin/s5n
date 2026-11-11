package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/kube"
)

// clusterIdentity is how a kubeconfig's current context names the cluster behind
// it: the API server URL, and the sha256 (hex) of the CA bundle client-go would
// trust. A kubeconfig path is not a cluster -- the file can be edited to point
// at another cluster between runs -- so the record carries this identity and a
// re-run compares it before it probes. Without the comparison a re-run would
// apply a record's decisions (who owns the lifecycle, the platform modes, the
// GatewayClass) to whatever cluster the file happens to reach now.
type clusterIdentity struct {
	Server string
	CAHash string
}

// noCAHash is the sha256 of the empty CA bundle: the hash every cluster trusted
// without one shares.
const noCAHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// identityOf resolves the identity of the cluster a kubeconfig's current context
// reaches, through the loader the prober and box's writers use. The CA bundle is
// hashed as client-go resolves it: the inline data when the kubeconfig carries
// it, else the file it names. A cluster trusted without a CA bundle hashes the
// empty bundle, so a new record always carries a digest.
//
// A kubeconfig this cannot resolve yields the zero identity rather than an
// error: the prober refuses a kubeconfig it cannot use with its own message
// (including the no-current-context one), and this check must never replace it.
// The zero identity is also what a record from before box recorded the identity
// carries, and the comparison skips it.
func identityOf(kubeconfig []byte) clusterIdentity {
	cfg, err := kube.ConfigFromBytes(kubeconfig)
	if err != nil {
		return clusterIdentity{}
	}
	ca := cfg.CAData
	if len(ca) == 0 && cfg.CAFile != "" {
		ca, err = os.ReadFile(cfg.CAFile)
		if err != nil {
			return clusterIdentity{}
		}
	}
	sum := sha256.Sum256(ca)
	return clusterIdentity{
		Server: strings.TrimRight(cfg.Host, "/"),
		CAHash: hex.EncodeToString(sum[:]),
	}
}

// identityConflict is the refusal for a recorded cluster whose kubeconfig now
// reaches a different one. It names both server URLs and the record file an
// operator removes deliberately, so the message says what changed and what the
// way out is.
//
// The CA bundle is the cluster's identity; the server URL is only how this
// kubeconfig reaches it. One cluster is often reached at several addresses (a
// local one, a tailnet name, a load balancer), and an operator who switches
// between them has not moved box to another cluster, so a changed URL with the
// same CA is accepted. A cluster trusted with no CA bundle at all has no
// identity of its own (every such cluster hashes the empty bundle), so there the
// URL is all box can compare.
//
// A record with either field absent is a record written before box recorded the
// identity (box writes both together, always). It is accepted: box must not
// refuse the cluster it has managed all along over fields the box that wrote the
// record did not keep, and the next record box writes carries them. A kubeconfig
// box cannot resolve has no identity either; the prober refuses it next with its
// own message, which says more than a comparison against nothing would.
func identityConflict(rec *Record, cur clusterIdentity, recordPath string) string {
	if rec.Server == "" || rec.CAHash == "" || cur.CAHash == "" {
		return ""
	}
	changed := "the cluster CA bundle"
	if rec.CAHash == cur.CAHash {
		if rec.CAHash != noCAHash || rec.Server == cur.Server {
			return ""
		}
		changed = "the API server URL of a cluster trusted without a CA bundle"
	}
	return fmt.Sprintf(
		"recorded %s cluster at %s: the kubeconfig now reaches %s, but the cluster record at %s was written for %s (%s changed). box will not treat a different cluster as the recorded one; remove %s deliberately to move box to another cluster.",
		rec.Source, rec.Kubeconfig, cur.Server, recordPath, rec.Server, changed, recordPath)
}
