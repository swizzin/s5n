package bootstrap

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/atomicfile"
)

// Record is the persisted /etc/swizzin/cluster.yaml. It is written in a fixed,
// box-owned schema; box reads it back before any probing on a re-run so the
// decision short-circuits.
type Record struct {
	Source     Source
	Kubeconfig string
	// Server and CAHash identify the cluster the kubeconfig's current context
	// reached when this record was written: the API server URL and the sha256
	// (hex) of the cluster CA bundle. A kubeconfig path is not a cluster, so a
	// re-run compares the path's current identity before it probes and refuses
	// one that now reaches another cluster. Both are empty in a record written
	// before box recorded them; such a record is accepted, never refused for
	// the missing fields, and the next record box writes carries them.
	Server       string
	CAHash       string
	Distribution Distribution
	// Nodes is what the run that wrote the record saw. A re-run refuses the
	// recorded value cheaply before touching the network and then re-probes the
	// node list: the record is a cached decision, not evidence.
	Nodes    int
	Platform PlatformConfig
	// GatewayClass is the GatewayClass box attached its Gateway to. A re-run
	// keeps it while the cluster still serves it: the class is a decision, and
	// asking a second time would be able to silently move a box's routes. The
	// cluster still has to show it Accepted and owned by Envoy Gateway's
	// controller.
	GatewayClass string
}

// encodeRecord renders a Record as the fixed YAML schema. The kubeconfig path
// is quoted so leading/trailing spaces survive.
func encodeRecord(r *Record) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "source: %s\n", r.Source)
	fmt.Fprintf(&b, "kubeconfig: %q\n", r.Kubeconfig)
	if r.Server != "" {
		fmt.Fprintf(&b, "server: %q\n", r.Server)
	}
	if r.CAHash != "" {
		fmt.Fprintf(&b, "caHash: %q\n", r.CAHash)
	}
	fmt.Fprintf(&b, "distribution: %s\n", r.Distribution)
	fmt.Fprintf(&b, "nodes: %d\n", r.Nodes)
	fmt.Fprintf(&b, "platform:\n")
	fmt.Fprintf(&b, "  gateway-api: %s\n", r.Platform.GatewayAPI)
	fmt.Fprintf(&b, "  loadbalancer: %s\n", r.Platform.LoadBalancer)
	fmt.Fprintf(&b, "  gateway: %s\n", r.Platform.Gateway)
	fmt.Fprintf(&b, "  cert-manager: %s\n", r.Platform.CertManager)
	if r.GatewayClass != "" {
		fmt.Fprintf(&b, "gatewayClass: %q\n", r.GatewayClass)
	}
	return []byte(b.String())
}

// parseRecord decodes the fixed YAML schema. It rejects unknown keys and
// malformed lines rather than guessing.
func parseRecord(data []byte) (*Record, error) {
	rec := &Record{}
	inPlatform := false
	var plat PlatformConfig
	seenPlat := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		if !indented {
			inPlatform = false
			key, val, ok := splitKV(trimmed)
			if !ok {
				return nil, fmt.Errorf("cluster.yaml: malformed line %q", trimmed)
			}
			switch key {
			case "source":
				src := Source(unquote(val))
				if src != SourceExternal && src != SourceK0sManaged {
					return nil, fmt.Errorf("cluster.yaml: unknown source %q", val)
				}
				rec.Source = src
			case "kubeconfig":
				rec.Kubeconfig = unquote(val)
			case "server":
				rec.Server = unquote(val)
			case "caHash":
				rec.CAHash = unquote(val)
			case "distribution":
				rec.Distribution = Distribution(unquote(val))
			case "nodes":
				n, err := strconv.Atoi(strings.TrimSpace(val))
				if err != nil {
					return nil, fmt.Errorf("cluster.yaml: nodes %q is not a number", val)
				}
				rec.Nodes = n
			case "platform":
				inPlatform = true
			case "gatewayClass":
				rec.GatewayClass = unquote(val)
			case "assumeGatewayFeatures":
				// The key only existed for a class publishing no feature list,
				// and box no longer accepts any implementation other than
				// Envoy Gateway. Refuse by name rather than reporting it as an
				// unknown key: an operator upgrading a box needs to know which
				// line to remove and why.
				return nil, fmt.Errorf("cluster.yaml: assumeGatewayFeatures is no longer supported: box supports only Envoy Gateway, which publishes no feature list to override. Remove that line and run box bootstrap again")
			default:
				return nil, fmt.Errorf("cluster.yaml: unknown key %q", key)
			}
			continue
		}
		if !inPlatform {
			return nil, fmt.Errorf("cluster.yaml: unexpected indented line %q", trimmed)
		}
		key, val, ok := splitKV(trimmed)
		if !ok {
			return nil, fmt.Errorf("cluster.yaml: malformed platform line %q", trimmed)
		}
		mode, err := parseMode(val)
		if err != nil {
			return nil, fmt.Errorf("cluster.yaml: platform %s: %w", key, err)
		}
		switch key {
		case "gateway-api":
			plat.GatewayAPI = mode
		case "loadbalancer":
			plat.LoadBalancer = mode
		case "gateway":
			plat.Gateway = mode
		case "cert-manager":
			plat.CertManager = mode
		default:
			return nil, fmt.Errorf("cluster.yaml: unknown platform key %q", key)
		}
		seenPlat[key] = true
	}
	for _, want := range []string{"gateway-api", "loadbalancer", "gateway", "cert-manager"} {
		if !seenPlat[want] {
			return nil, fmt.Errorf("cluster.yaml: platform.%s is missing", want)
		}
	}
	rec.Platform = plat
	return rec, nil
}

func parseMode(val string) (PlatformMode, error) {
	m := PlatformMode(unquote(strings.TrimSpace(val)))
	switch m {
	case ModeAuto, ModeManaged, ModeExternal:
		return m, nil
	default:
		return "", fmt.Errorf("unknown mode %q", val)
	}
}

// FilesystemRecords stores the record at a path on disk.
type FilesystemRecords struct {
	Path string
}

// Load returns (nil, nil) when the record does not exist.
func (r FilesystemRecords) Load() (*Record, error) {
	data, err := os.ReadFile(r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseRecord(data)
}

// RecordPath reports where this store persists the record.
func (r FilesystemRecords) RecordPath() string { return r.Path }

// RecordedKubeconfig reports the kubeconfig the record names, and whether there
// is a record at all. It is kube.Records: a command that only has to know where
// the cluster is (`box apply`, and root's `box adduser` next) asks the store
// this and never sees the record's schema.
func (r FilesystemRecords) RecordedKubeconfig() (string, bool, error) {
	return recordedKubeconfig(r.Load)
}

// recordedKubeconfig is the one reading of "where is the recorded cluster" both
// stores answer with.
func recordedKubeconfig(load func() (*Record, error)) (string, bool, error) {
	rec, err := load()
	if err != nil {
		return "", false, err
	}
	if rec == nil {
		return "", false, nil
	}
	return rec.Kubeconfig, true, nil
}

// Save writes the record atomically, mode 0600, creating the directory first.
func (r FilesystemRecords) Save(rec *Record) error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(r.Path, encodeRecord(rec), 0o600)
}

// ReadOnlyRecords wraps a store: Load still reads, Save is a no-op. box
// bootstrap --dry-run uses it so a dry run never writes the record.
type ReadOnlyRecords struct {
	Inner RecordStore
}

func (r ReadOnlyRecords) Load() (*Record, error) { return r.Inner.Load() }
func (r ReadOnlyRecords) RecordPath() string     { return r.Inner.RecordPath() }
func (r ReadOnlyRecords) Save(*Record) error     { return nil }

// RecordedKubeconfig is the read half of kube.Records; a dry run still answers
// where the cluster is, it just never writes.
func (r ReadOnlyRecords) RecordedKubeconfig() (string, bool, error) {
	return recordedKubeconfig(r.Inner.Load)
}
