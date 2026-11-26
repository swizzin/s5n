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

// Record is the persisted cluster record (hostroot.RecordPath), in a fixed box-owned
// schema; a re-run reads it before any probing so the decision short-circuits.
type Record struct {
	Source     Source
	Kubeconfig string
	// Server and CAHash identify the cluster the kubeconfig reached when this record
	// was written: the API server URL and the sha256 of its CA bundle. A path is not a
	// cluster, so a re-run compares the current identity and refuses one that now
	// reaches another cluster. Both empty is an older record: accepted, and the next
	// one carries them.
	Server       string
	CAHash       string
	Distribution Distribution
	// Nodes is what the writing run saw. A re-run refuses it cheaply before touching
	// the network, then re-probes: the record is a cached decision, not evidence.
	Nodes    int
	Platform PlatformConfig
	// GatewayClass is the class box attached its Gateway to. A re-run keeps it while
	// the cluster serves it: asking again could silently move a box's routes. It must
	// still be Accepted and owned by Envoy Gateway's controller.
	GatewayClass string
}

// encodeRecord renders a Record as the fixed schema; the path is quoted so spaces
// survive.
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

// parseRecord decodes the fixed schema, rejecting unknown keys and malformed lines.
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
				// The key only existed for a class publishing no feature list, and box
				// accepts only Envoy Gateway. Refused by name, so an operator upgrading
				// knows which line to remove and why.
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

// RecordedKubeconfig reports the kubeconfig the record names and whether one
// exists: kube.Records, so a command that only needs where the cluster is never sees
// the schema.
func (r FilesystemRecords) RecordedKubeconfig() (string, bool, error) {
	return recordedKubeconfig(r.Load)
}

// recordedKubeconfig is the one reading of "where is the recorded cluster".
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

// ReadOnlyRecords wraps a store: Load reads, Save does nothing. --dry-run uses it so
// a dry run never writes the record.
type ReadOnlyRecords struct {
	Inner RecordStore
}

func (r ReadOnlyRecords) Load() (*Record, error) { return r.Inner.Load() }
func (r ReadOnlyRecords) RecordPath() string     { return r.Inner.RecordPath() }
func (r ReadOnlyRecords) Save(*Record) error     { return nil }

// RecordedKubeconfig is the read half: a dry run still answers where the cluster is.
func (r ReadOnlyRecords) RecordedKubeconfig() (string, bool, error) {
	return recordedKubeconfig(r.Inner.Load)
}
