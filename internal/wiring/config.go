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
	"errors"
	"fmt"
	"io/fs"
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
	Plex           *PlexConfig     `json:"plex,omitempty"`
	Tautulli       *TautulliConfig `json:"tautulli,omitempty"`
	Seerr          *SeerrConfig    `json:"seerr,omitempty"`
	Qui            *QuiConfig      `json:"qui,omitempty"`
	Autobrr        *AutobrrConfig  `json:"autobrr,omitempty"`
	Instances      []Instance      `json:"instances"`
}

// QuiConfig points at qui's API. PasswordFile is the release Secret's
// qui-password: the chart generates it once and qui's init container writes it
// into the app's own user table on every start, so this is the one account the
// Job signs in with. No API key is minted or stored — qui only issues one to a
// session, and a session is something this Job can open by itself.
type QuiConfig struct {
	URL          string `json:"url"`
	URLBase      string `json:"urlBase"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
}

// AutobrrConfig points at autobrr's API, with the same account arrangement as
// qui's: autobrr-password in the release Secret, written into autobrr's own
// user table by its init container at every start.
type AutobrrConfig struct {
	URL          string `json:"url"`
	URLBase      string `json:"urlBase"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
}

// PlexConfig points at Plex. There is no API key: /identity answers
// unauthenticated on every build, claimed or not (see internal/apps.Plex).
type PlexConfig struct {
	URL string `json:"url"`
}

// TautulliConfig points at Tautulli. APIKeyFile is a file reference into the
// mounted Secret, seeded by the tautulli-config init container — the wiring
// Job has no mount into Tautulli's own /config to read its generated key back.
//
// PlexTokenFile is the Plex server token (X-Plex-Token) the PMS link uses, also
// a file reference into the mounted Secret. It is optional: absent or empty
// means "no token yet", which makes the PMS link pending rather than
// configuring Tautulli against a Plex that trusts the pod network.
type TautulliConfig struct {
	URL           string `json:"url"`
	APIKeyFile    string `json:"apiKeyFile"`
	PlexTokenFile string `json:"plexTokenFile"`
}

// SeerrConfig points at Seerr. There is no API key here either: every
// settings endpoint needs an authenticated admin session the wiring Job
// cannot create (see internal/apps.Seerr), so this is only ever used to wait
// for readiness before reporting the media rows pending.
type SeerrConfig struct {
	URL string `json:"url"`
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
		if err := validateInstance(i, inst); err != nil {
			return err
		}
	}
	if c.Qbittorrent != nil {
		if err := requireFields("qbittorrent", map[string]string{
			"url": c.Qbittorrent.URL, "username": c.Qbittorrent.Username,
			"passwordFile": c.Qbittorrent.PasswordFile,
		}); err != nil {
			return err
		}
	}
	if c.Prowlarr != nil {
		if err := requireFields("prowlarr", map[string]string{
			"url": c.Prowlarr.URL, "selfUrl": c.Prowlarr.SelfURL, "apiKeyFile": c.Prowlarr.APIKeyFile,
		}); err != nil {
			return err
		}
	}
	if c.Plex != nil && c.Plex.URL == "" {
		return fmt.Errorf("plex: url is empty")
	}
	if c.Tautulli != nil {
		if err := validateTautulli(c.Tautulli, c.Plex); err != nil {
			return err
		}
	}
	if c.Seerr != nil && c.Seerr.URL == "" {
		return fmt.Errorf("seerr: url is empty")
	}
	if c.Qui != nil {
		if err := requireFields("qui", map[string]string{
			"url": c.Qui.URL, "urlBase": c.Qui.URLBase, "username": c.Qui.Username,
			"passwordFile": c.Qui.PasswordFile,
		}); err != nil {
			return err
		}
	}
	if c.Autobrr != nil {
		if err := requireFields("autobrr", map[string]string{
			"url": c.Autobrr.URL, "urlBase": c.Autobrr.URLBase, "username": c.Autobrr.Username,
			"passwordFile": c.Autobrr.PasswordFile,
		}); err != nil {
			return err
		}
	}
	// Categories drive qBittorrent save paths; two instances sharing a name but
	// not a path would fight over the same downloads.
	return validateCategories(c.Instances)
}

// validateInstance checks one Arr instance: its kind must be one the wiring
// job knows how to register, and every field it registers must be set.
func validateInstance(index int, inst Instance) error {
	switch inst.Kind {
	case "sonarr", "radarr":
	default:
		return fmt.Errorf("instance %d: unknown kind %q", index, inst.Kind)
	}
	return requireFields(fmt.Sprintf("instance %q", inst.Name), map[string]string{
		"name": inst.Name, "url": inst.URL, "apiKeyFile": inst.APIKeyFile,
		"category": inst.Category, "savePath": inst.SavePath, "rootFolder": inst.RootFolder,
	})
}

// validateTautulli checks the fields the Tautulli steps need, and the one
// dependency those steps have: the PMS link has no meaning without Plex.
func validateTautulli(t *TautulliConfig, plex *PlexConfig) error {
	if err := requireFields("tautulli", map[string]string{
		"url": t.URL, "apiKeyFile": t.APIKeyFile,
	}); err != nil {
		return err
	}
	if plex == nil {
		return fmt.Errorf("tautulli is configured but plex is not")
	}
	return nil
}

// validateCategories refuses one category pointing at two save paths, because
// both instances would then write into the same downloads.
func validateCategories(instances []Instance) error {
	seen := map[string]string{}
	for _, inst := range instances {
		if prev, ok := seen[inst.Category]; ok && prev != inst.SavePath {
			return fmt.Errorf("category %q is used with two save paths (%s, %s)", inst.Category, prev, inst.SavePath)
		}
		seen[inst.Category] = inst.SavePath
	}
	return nil
}

// requireFields reports the first empty field, named with the component it
// belongs to. The fields are a map because each component's set is written
// once here, next to the struct it mirrors.
func requireFields(component string, fields map[string]string) error {
	for field, value := range fields {
		if value == "" {
			return fmt.Errorf("%s: %s is empty", component, field)
		}
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

// ReadOptionalSecret reads a secret file that may legitimately be absent.
//
// An unconfigured path, a missing file, or an empty one all mean "not supplied
// yet" — the caller reports its dependent step as pending, which is a normal
// state and not an error. Any other read error is returned: a credential that
// cannot be read must never degrade into configuring the app without it.
func ReadOptionalSecret(path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read secret %s: %w", path, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", false, nil
	}
	return s, true, nil
}

// Context returns a context bounded by the config's timeout.
func (c *Config) Context(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeoutDuration(c.TimeoutSeconds))
}
