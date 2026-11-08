package seedbox

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Volume is one of a seedbox's two volumes: the host directory the grant
// records, and the claim box creates for it. Kind is the chart's name for the
// volume, which is also the values key under `storage`.
type Volume struct {
	Kind  string
	Path  string
	Claim string
}

// VolumeKinds is the chart's two storage keys, in the order box creates them.
// Data first, because a box whose data claim is missing is the one with
// something to lose.
var VolumeKinds = []string{"data", "config"}

// ClaimName is the claim box creates for one volume of one seedbox. It is named
// per namespace, not per release: a PersistentVolume is cluster-scoped, so a
// fixed name would collide between two boxes on one host, and the namespace is
// the one cluster object a box owns alone.
func ClaimName(namespace, kind string) string {
	return namespace + "-s5n-" + kind
}

// Namespace is the grant's `namespace` key, which is box's own record and never
// a chart value. It names the release namespace and every object box creates
// beside the release, so it is read before anything is sent to Helm.
func (s *Seedbox) Namespace() (string, error) {
	ns, _ := s.Grant.data["namespace"].(string)
	if strings.TrimSpace(ns) == "" {
		return "", fmt.Errorf("%s: namespace is missing or is not a name; the grant is what names the release namespace", s.Grant.Path)
	}
	return ns, nil
}

// Storage reads the grant's two host paths and derives the claims box creates
// for them. It is the one place that names a volume, so the PersistentVolume box
// writes, the claim it writes and the claim it hands the chart cannot disagree.
//
// The path is required and absolute: a relative one would be resolved against
// whatever directory box happened to run in, which is how a box ends up
// mounting an empty volume beside the data it meant to use.
func (s *Seedbox) Storage() ([]Volume, error) {
	ns, err := s.Namespace()
	if err != nil {
		return nil, err
	}
	out := make([]Volume, 0, len(VolumeKinds))
	for _, kind := range VolumeKinds {
		path, err := s.hostPath(kind)
		if err != nil {
			return nil, err
		}
		out = append(out, Volume{Kind: kind, Path: path, Claim: ClaimName(ns, kind)})
	}
	return out, nil
}

// hostPath is one storage.<kind>.path, with the line the operator can fix.
func (s *Seedbox) hostPath(kind string) (string, error) {
	key := "storage." + kind + ".path"
	n, ok, err := s.Grant.lookup(key)
	if err != nil {
		return "", err
	}
	missing := fmt.Errorf("%s: %s is missing; box needs the host directory that backs this box's %s volume", s.Grant.Path, key, kind)
	if !ok || n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "") {
		return "", missing
	}
	path := strings.TrimSpace(n.Value)
	if path == "" {
		return "", missing
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("%s:%d: %s = %q: a host path must be absolute", s.Grant.Path, n.Line, key, path)
	}
	return path, nil
}

// claims is the storage block box writes into the effective values: the claim
// names it creates. No file carries them, so this is applied last, after both
// files and the chart's own defaults.
func claims(vols []Volume) map[string]any {
	storage := make(map[string]any, len(vols))
	for _, v := range vols {
		storage[v.Kind] = map[string]any{"existingClaim": v.Claim}
	}
	return map[string]any{"storage": storage}
}
