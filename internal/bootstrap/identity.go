package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/swizzin/s5n/internal/kube"
)

// clusterIdentity is how a kubeconfig names the cluster behind it: the API server URL
// and the sha256 of the CA bundle client-go would trust. A path is not a cluster, so
// the record carries this identity and a re-run compares it before probing; without
// it, a repointed file would get a record's decisions applied to another cluster.
type clusterIdentity struct {
	Server string
	CAHash string
}

// noCAHash is the sha256 of the empty bundle: what every cluster trusted without a CA
// shares.
const noCAHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// identityOf resolves the identity a kubeconfig's current context reaches, through
// the loader the prober and the writers use: the CA bundle as client-go resolves it
// (inline data, else the file), the empty bundle when there is none, so a new record
// always carries a digest.
//
// A kubeconfig it cannot resolve yields the zero identity rather than an error: the
// prober refuses that file with its own better message, and the comparison skips the
// zero identity an older record carries.
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

// identityConflict is the refusal for a recorded cluster whose kubeconfig now reaches
// a different one, naming both servers and the record file an operator removes.
//
// The CA bundle is the identity; the URL is only how this kubeconfig reaches it, and
// one cluster is often reached at several addresses, so a changed URL with the same CA
// is accepted. A cluster trusted with no CA has no identity of its own, so there the
// URL is all box compares. A record missing either field was written before box kept
// them and is accepted, and a kubeconfig it cannot resolve is left to the prober's own
// message.
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
