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

// This file holds the real implementations behind the installer's three
// seams: the network, the command runner and the readiness check. They are
// separated from the sequence so the sequence can be read and tested without
// any of them, and so nothing here is reached on a path that refuses.

// k0sEvidence is what the host shows before box writes anything: a k0s binary
// where box would put one, k0s's state directory, its systemd unit, or
// something already listening on the API port. It is read-only and cheap.
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

// k0sStatePresent reports whether path holds a k0s state directory: it exists
// and has something in it. An empty directory is not evidence of an install --
// it is what a prepared mount point or a leftover from a refused run looks
// like -- and refusing on it would make box unable to install onto a host whose
// operator had already laid the directory out.
func k0sStatePresent(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

// HTTPDownloader fetches a release asset over HTTPS. The zero value is usable.
type HTTPDownloader struct {
	// Client, when set, is used as it is instead of the client box builds:
	// no retries, no timeout of box's. It exists so a test can replace the
	// network entirely.
	Client *http.Client
	// Timeout covers the whole download, body included: a k0s binary is a few
	// hundred megabytes, and a stalled connection must not hang bootstrap.
	Timeout time.Duration
	// Retry is the policy for a transient upstream failure. A release asset
	// comes from a public CDN on a first run, and one 5xx there must not fail
	// an install the operator cannot fix from this side. Zero means box's
	// policy (four attempts, 1s/2s/4s); a test replaces the sleeper so the
	// backoff costs no time.
	Retry fetch.Policy
}

// client is what one fetch goes through: box's client, with the retrying
// transport, unless a test supplied its own.
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

// withoutMethodAndURL drops the wrapper net/http puts around a transport
// error ("Get \"https://…\": …") when box's retrying transport already said
// what happened. That sentence names the URL and the attempt count, and box
// does not say it twice; any other failure keeps the wrapper, which names the
// method and the URL.
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

// APIReadiness asks the cluster box just started, through the admin kubeconfig
// k0s wrote, whether it can serve.
//
// /readyz alone is not the question box needs answered: it turns true a moment
// before the node object exists, and the decision that runs next refuses a
// cluster that lists no nodes. So this waits for both: readyz, and one node
// whose Ready condition is true.
type APIReadiness struct {
	// ConfigPath is the admin kubeconfig. Empty means K0sAdminConfig.
	ConfigPath string
	// Timeout bounds one attempt. Zero means 10s.
	Timeout time.Duration
}

// Ready performs one attempt: read the kubeconfig, hit /readyz, then list
// nodes. The kubeconfig is re-read every attempt because k0s may still be
// writing its PKI the first time box asks.
func (a APIReadiness) Ready(ctx context.Context) error {
	path := a.ConfigPath
	if path == "" {
		path = K0sAdminConfig
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client, base, err := kubeClient(data, timeout)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	if err := readyz(ctx, client, base); err != nil {
		return err
	}
	return nodeReady(ctx, client, base)
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

func nodeReady(ctx context.Context, client *http.Client, base string) error {
	body, status, err := get(ctx, client, base+"/api/v1/nodes")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("list nodes: %d: %s", status, firstLine(body))
	}
	var list struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("no node has registered yet")
	}
	for _, item := range list.Items {
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				return nil
			}
		}
	}
	return fmt.Errorf("the node is not Ready yet")
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

// firstLine keeps a server error short enough to sit inside box's sentence.
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
