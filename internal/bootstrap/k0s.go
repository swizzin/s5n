package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/fetch"
)

// The real implementations behind the installer's seams: the download, the command
// runner and the readiness check. Kept apart so the sequence reads and tests without
// them, and nothing here is reached on a path that refuses.

// k0sEvidence is what the host shows before box writes: a k0s binary where box would
// put one, its state directory, its unit, something listening on the API port.
func k0sEvidence() []Finding {
	var out []Finding
	if _, err := os.Stat(K0sBinaryPath); err == nil {
		out = append(out, Finding{Kind: "binary", Name: K0sBinaryPath})
	}
	if k0sStatePresent(K0sStateDir) {
		out = append(out, Finding{Kind: "state", Name: K0sStateDir})
	}
	out = append(out, installedUnits("k0scontroller.service", "k0sworker.service")...)
	out = append(out, listeningOn(6443)...)
	return out
}

// k0sStatePresent reports a k0s state directory with something in it: an empty one is
// a prepared mount point or a refused run's leftover, and refusing on it would block a
// host whose operator laid the directory out.
func k0sStatePresent(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

// HTTPDownloader fetches a release asset over HTTPS. The zero value is usable.
type HTTPDownloader struct {
	// Client, when set, replaces the client box builds, retries and timeout included: a
	// test replaces the network entirely.
	Client *http.Client
	// Timeout covers the whole download, body included: the binary is hundreds of
	// megabytes, and a stalled connection must not hang bootstrap.
	Timeout time.Duration
	// Retry is the policy for a transient upstream failure: one 5xx from a public CDN
	// must not fail an install the operator cannot fix. Zero is box's policy (four
	// attempts, 1s/2s/4s); a test replaces the sleeper.
	Retry fetch.Policy
}

// client is box's client with the retrying transport, unless a test supplied one.
func (d HTTPDownloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &http.Client{Timeout: timeout, Transport: fetch.Transport{Policy: d.Retry}}
}

// Fetch opens the URL and refuses anything that is not a 200.
func (d HTTPDownloader) Fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return nil, withoutMethodAndURL(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// withoutMethodAndURL drops net/http's transport-error wrapper when box's retrying
// transport already said what happened, with the URL and attempt count. Any other
// failure keeps the wrapper.
func withoutMethodAndURL(err error) error {
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return err
	}
	var failure *fetch.Failure
	if errors.As(uerr.Err, &failure) {
		return failure
	}
	return err
}

// ExecRunner runs one command with no shell, and returns its combined output.
type ExecRunner struct{}

// Run runs path with args. Connector output is returned even on failure, so
// the caller can quote it.
func (ExecRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	return cmd.CombinedOutput()
}

// APIReadiness asks the cluster box just started, through the admin kubeconfig k0s
// wrote, whether it can serve.
//
// /readyz alone is not the answer: it turns true a moment before the node object
// exists, and the next decision refuses a cluster listing no nodes. So it waits for
// both: readyz, and one node whose Ready condition is true.
type APIReadiness struct {
	// ConfigPath is the admin kubeconfig. Empty means K0sAdminConfig.
	ConfigPath string
	// Timeout bounds one attempt; zero means 10s.
	Timeout time.Duration
}

// Ready performs one attempt: read the kubeconfig, hit /readyz, list nodes. The
// kubeconfig is re-read each attempt, since k0s may still be writing its PKI.
func (a APIReadiness) Ready(ctx context.Context) error {
	client, base, err := a.client()
	if err != nil {
		return err
	}
	if err := readyz(ctx, client, base); err != nil {
		return err
	}
	nodes, err := listNodes(ctx, client, base)
	if err != nil {
		return err
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("no node has registered yet")
	}
	for _, item := range nodes.Items {
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == conditionTrue {
				return nil
			}
		}
	}
	return fmt.Errorf("the node is not Ready yet")
}

// NodeIPs reads the InternalIPs the nodes registered, once after Ready: a kubelet
// that ignored --node-ip would still be Ready, and the failure would surface later as
// packets that fragment.
func (a APIReadiness) NodeIPs(ctx context.Context) ([]string, error) {
	client, base, err := a.client()
	if err != nil {
		return nil, err
	}
	nodes, err := listNodes(ctx, client, base)
	if err != nil {
		return nil, err
	}
	var ips []string
	for _, item := range nodes.Items {
		for _, addr := range item.Status.Addresses {
			if addr.Type == "InternalIP" {
				ips = append(ips, addr.Address)
			}
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("the node registered no InternalIP")
	}
	return ips, nil
}

// client is one attempt's API client: the admin kubeconfig, read fresh every
// time, and the base URL it reaches.
func (a APIReadiness) client() (*http.Client, string, error) {
	path := a.ConfigPath
	if path == "" {
		path = K0sAdminConfig
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client, base, err := kubeClient(data, timeout)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return client, base, nil
}

// nodeList is the part of /api/v1/nodes box reads: Ready, and registered addresses.
type nodeList struct {
	Items []struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			Addresses []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"status"`
	} `json:"items"`
}

func listNodes(ctx context.Context, client *http.Client, base string) (nodeList, error) {
	var list nodeList
	body, status, err := get(ctx, client, base+"/api/v1/nodes")
	if err != nil {
		return list, err
	}
	if status != http.StatusOK {
		return list, fmt.Errorf("list nodes: %d: %s", status, firstLine(body))
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return list, fmt.Errorf("list nodes: %w", err)
	}
	return list, nil
}

func readyz(ctx context.Context, client *http.Client, base string) error {
	body, status, err := get(ctx, client, base+"/readyz")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("readyz: %d: %s", status, firstLine(body))
	}
	return nil
}

func get(ctx context.Context, client *http.Client, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// firstLine keeps a server error short enough for box's sentence.
func firstLine(body []byte) string {
	line := strings.TrimSpace(string(body))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	if line == "" {
		return "(no body)"
	}
	return line
}
