// Package wiring is the idempotent post-install step: it connects qBittorrent,
// the Arrs and Prowlarr to each other, proves the /data hardlink contract, and
// records what it verified in a ConfigMap.
//
// It is swizzin's install.sh written as a reconciler: every step reads the live
// state, changes only what differs, and proves the result. A second run on an
// unchanged release performs zero writes, which the e2e asserts.
package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is rendered by the chart into /etc/s5n/wiring/wiring.json. It contains
// no secrets: every credential is a file reference into the mounted Secret.
type Config struct {
	Namespace      string          `json:"namespace"`
	ConfigMap      string          `json:"configMap"`
	TimeoutSeconds int             `json:"timeoutSeconds"`
	Qbittorrent    *QbitConfig     `json:"qbittorrent,omitempty"`
	Prowlarr       *ProwlarrConfig `json:"prowlarr,omitempty"`
	Instances      []Instance      `json:"instances"`
}

// QbitConfig points at the qBittorrent WebUI.
type QbitConfig struct {
	URL          string `json:"url"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
}

// ProwlarrConfig points at Prowlarr. SelfURL is Prowlarr's own address as the
// Arrs reach it, which Prowlarr hands to each application it registers.
type ProwlarrConfig struct {
	URL        string `json:"url"`
	URLBase    string `json:"urlBase"`
	SelfURL    string `json:"selfUrl"`
	APIKeyFile string `json:"apiKeyFile"`
}

// Instance is one Arr instance (sonarr, sonarr-4k, radarr, ...).
type Instance struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	URL        string `json:"url"`
	URLBase    string `json:"urlBase"`
	APIKeyFile string `json:"apiKeyFile"`
	Category   string `json:"category"`
	SavePath   string `json:"savePath"`
	RootFolder string `json:"rootFolder"`
}

// Load reads and validates the rendered config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read wiring config: %w", err)
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.TimeoutSeconds <= 0 {
		c.TimeoutSeconds = 300
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Instances) == 0 && c.Qbittorrent == nil {
		return fmt.Errorf("wiring config has nothing enabled")
	}
	for i, inst := range c.Instances {
		switch inst.Kind {
		case "sonarr", "radarr":
		default:
			return fmt.Errorf("instance %d: unknown kind %q", i, inst.Kind)
		}
		for field, value := range map[string]string{
			"name": inst.Name, "url": inst.URL, "apiKeyFile": inst.APIKeyFile,
			"category": inst.Category, "savePath": inst.SavePath, "rootFolder": inst.RootFolder,
		} {
			if value == "" {
				return fmt.Errorf("instance %q: %s is empty", inst.Name, field)
			}
		}
	}
	if c.Qbittorrent != nil {
		for field, value := range map[string]string{
			"url": c.Qbittorrent.URL, "username": c.Qbittorrent.Username,
			"passwordFile": c.Qbittorrent.PasswordFile,
		} {
			if value == "" {
				return fmt.Errorf("qbittorrent: %s is empty", field)
			}
		}
	}
	if c.Prowlarr != nil {
		for field, value := range map[string]string{
			"url": c.Prowlarr.URL, "selfUrl": c.Prowlarr.SelfURL, "apiKeyFile": c.Prowlarr.APIKeyFile,
		} {
			if value == "" {
				return fmt.Errorf("prowlarr: %s is empty", field)
			}
		}
	}
	// Categories drive qBittorrent save paths; two instances sharing a name but
	// not a path would fight over the same downloads.
	seen := map[string]string{}
	for _, inst := range c.Instances {
		if prev, ok := seen[inst.Category]; ok && prev != inst.SavePath {
			return fmt.Errorf("category %q is used with two save paths (%s, %s)", inst.Category, prev, inst.SavePath)
		}
		seen[inst.Category] = inst.SavePath
	}
	return nil
}

// ReadSecret reads a secret file, tolerating one trailing newline so that
// hand-written Secrets behave like generated ones.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret %s: %w", path, err)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", fmt.Errorf("secret %s is empty", path)
	}
	return s, nil
}

// Context returns a context bounded by the config's timeout.
func (c *Config) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeoutDuration(c.TimeoutSeconds))
}
