// Package wiring is the idempotent post-install step: it connects qBittorrent, the
// Arrs and Prowlarr, proves the /data hardlink contract, and records what it
// verified in a ConfigMap.
//
// It is swizzin's install.sh as a reconciler: every step reads the live state,
// changes only what differs, and proves the result; a second run on an unchanged
// release performs zero writes, which the e2e asserts.
package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// The field names the component blocks share: both the JSON key the chart writes
// and the name a validation error prints.
const (
	fieldURL          = "url"
	fieldURLBase      = "urlBase"
	fieldAPIKeyFile   = "apiKeyFile"
	fieldPasswordFile = "passwordFile"
	fieldTokenFile    = "tokenFile"
	fieldName         = "name"
)

// Config is rendered by the chart into /etc/s5n/wiring/wiring.json. It contains no
// secrets: every credential is a file reference into the mounted Secret.
type Config struct {
	Namespace      string             `json:"namespace"`
	ConfigMap      string             `json:"configMap"`
	TimeoutSeconds int                `json:"timeoutSeconds"`
	Qbittorrent    *QbitConfig        `json:"qbittorrent,omitempty"`
	Prowlarr       *ProwlarrConfig    `json:"prowlarr,omitempty"`
	Plex           *PlexConfig        `json:"plex,omitempty"`
	Tautulli       *TautulliConfig    `json:"tautulli,omitempty"`
	Seerr          *SeerrConfig       `json:"seerr,omitempty"`
	Maintainerr    *MaintainerrConfig `json:"maintainerr,omitempty"`
	Qui            *QuiConfig         `json:"qui,omitempty"`
	Sabnzbd        *SabnzbdConfig     `json:"sabnzbd,omitempty"`
	Autobrr        *AutobrrConfig     `json:"autobrr,omitempty"`
	Pbh            *PbhConfig         `json:"pbh,omitempty"`
	Jellyfin       *JellyfinConfig    `json:"jellyfin,omitempty"`
	Instances      []Instance         `json:"instances"`
	Bazarrs        []BazarrConfig     `json:"bazarrs,omitempty"`
}

// JellyfinConfig carries what the Job needs to make the server usable: the
// person's account, the automation account, the proxies the server honours, and the
// libraries the Arrs write into.
//
// None of this can be sealed into a file before the app starts: users, API keys and
// libraries are rows in data/jellyfin.db, and the first-run wizard is an API. The
// chart's init container completes it inside the pod, and the Job checks it.
//
// Username is the account root's login admits, and UserPasswordFile is the first-run
// administrator's password, needed once to make the box's own account.
type JellyfinConfig struct {
	URL              string            `json:"url"`
	Username         string            `json:"username"`
	UserPasswordFile string            `json:"userPasswordFile"`
	BoxPasswordFile  string            `json:"boxPasswordFile"`
	KnownProxies     []string          `json:"knownProxies,omitempty"`
	Libraries        []JellyfinLibrary `json:"libraries,omitempty"`
}

// JellyfinLibrary is one virtual folder: the Arr instance's library directory, its
// collection type, and the directory itself (created by the data-contract step).
type JellyfinLibrary struct {
	Name           string `json:"name"`
	CollectionType string `json:"collectionType"`
	Path           string `json:"path"`
}

// QuiConfig points at qui's API. PasswordFile is the release Secret's qui-password,
// which qui's init container writes into the app's user table on every start. No API
// key is minted: qui issues one only to a session, and the Job can open one.
type QuiConfig struct {
	URL          string `json:"url"`
	URLBase      string `json:"urlBase"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
}

// AutobrrConfig points at autobrr's API, with the same account arrangement as qui's.
type AutobrrConfig struct {
	URL          string `json:"url"`
	URLBase      string `json:"urlBase"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
}

// PbhConfig points at PeerBanHelper's WebAPI. TokenFile is the release Secret's
// pbh-token, which is PBH's own WebUI token: the chart gives the app the same value
// through its environment, and the Job sends it as a Bearer header.
type PbhConfig struct {
	URL       string `json:"url"`
	TokenFile string `json:"tokenFile"`
}

// PlexConfig points at Plex. There is no API key: /identity answers unauthenticated
// on every build, claimed or not (see internal/apps.Plex).
type PlexConfig struct {
	URL string `json:"url"`
}

// TautulliConfig points at Tautulli. APIKeyFile is seeded by the tautulli-config
// init container, because the Job has no mount into Tautulli's /config.
//
// PlexTokenFile is the optional Plex server token the PMS link uses: absent means
// "no token yet", which makes the link pending rather than configuring Tautulli
// against a Plex that trusts the pod network.
type TautulliConfig struct {
	URL           string `json:"url"`
	APIKeyFile    string `json:"apiKeyFile"`
	PlexTokenFile string `json:"plexTokenFile"`
}

// MaintainerrConfig points at Maintainerr's API and at the Plex token its connection
// uses.
//
// URLBase is the prefix the chart serves it under, and every route carries it:
// Maintainerr applies BASE_PATH to its API and its health route alike
// (templates/workloads.yaml). PlexTokenFile is the same file Tautulli reads, and
// optional the same way.
type MaintainerrConfig struct {
	URL           string `json:"url"`
	URLBase       string `json:"urlBase"`
	PlexTokenFile string `json:"plexTokenFile"`
}

// The media servers a Seerr is set up against. Seerr talks to exactly one, and which
// one decides who its first admin is (catalog.yaml's seerr entry).
const (
	mediaServerPlex     = "plex"
	mediaServerJellyfin = "jellyfin"
)

// SeerrConfig points at Seerr. There is no API key: every settings endpoint needs an
// authenticated admin session.
//
// PublicURL is where a person opens Seerr: its bundle loads assets from the site
// root, so the chart serves it at <app>.<domain> (catalog.yaml's `exposure: host`).
//
// MediaServer decides whether the Job can make that admin itself: on Jellyfin it is
// the person's own server account, whose password is in the release Secret; on Plex
// only an interactive sign-in creates one, which is a person's.
type SeerrConfig struct {
	URL       string `json:"url"`
	PublicURL string `json:"publicUrl"`
	// MediaServer is the server this instance is set up against: plex or jellyfin.
	MediaServer string `json:"mediaServer"`
	// MediaServerURL is where people reach that server (https://jellyfin.<domain>):
	// Seerr stores it as the Jellyfin external URL its links are built from.
	MediaServerURL string `json:"mediaServerUrl,omitempty"`
}

// QbitConfig points at the qBittorrent WebUI. It carries no credential: the WebUI
// skips its login for the release's addresses (internal/qbtconf; docs/box.md,
// "Logins").
//
// SavePath is /data/downloads; a category with no save path of its own downloads
// into <SavePath>/<category>, which is what a migrated category already has.
type QbitConfig struct {
	URL      string `json:"url"`
	SavePath string `json:"savePath"`
}

// ProwlarrConfig points at Prowlarr. SelfURL is Prowlarr's own address as the Arrs
// reach it, which it hands to each application.
type ProwlarrConfig struct {
	URL        string `json:"url"`
	URLBase    string `json:"urlBase"`
	SelfURL    string `json:"selfUrl"`
	APIKeyFile string `json:"apiKeyFile"`
}

// SabnzbdConfig points at SABnzbd. SavePath is where a finished job is left for an
// Arr to import, DownloadDir where it is unpacked. Both are on /data, so SAB's move
// is a rename and the import a hardlink.
type SabnzbdConfig struct {
	URL         string `json:"url"`
	URLBase     string `json:"urlBase"`
	APIKeyFile  string `json:"apiKeyFile"`
	SavePath    string `json:"savePath"`
	DownloadDir string `json:"downloadDir"`
}

// BazarrConfig is one Bazarr instance and the Arr pair it reads: upstream keeps one
// Sonarr and one Radarr per instance (bazarr/app/config.py at v1.6.2: the sonarr.*
// and radarr.* sections), so a 4K pair is a second pod. A side with no Arr is nil
// and turned off: one left pointing at nothing is a search that never runs.
type BazarrConfig struct {
	Name       string      `json:"name"`
	URL        string      `json:"url"`
	URLBase    string      `json:"urlBase"`
	APIKeyFile string      `json:"apiKeyFile"`
	Sonarr     *BazarrSide `json:"sonarr,omitempty"`
	Radarr     *BazarrSide `json:"radarr,omitempty"`
}

// BazarrSide is one Arr a Bazarr instance reads: the Service it dials and the Arr's
// key.
type BazarrSide struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	URLBase    string `json:"urlBase"`
	APIKeyFile string `json:"apiKeyFile"`
}

// Instance is one Arr instance (sonarr, sonarr-4k, radarr, ...).
//
// Category is its library directory (/data/media/<category>), DownloadCategory its
// qBittorrent category (/data/downloads/<downloadCategory>). They are equal on a
// fresh box; a migrated one keeps the source's download category, because box never
// moves torrents.
type Instance struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	URL              string `json:"url"`
	URLBase          string `json:"urlBase"`
	APIKeyFile       string `json:"apiKeyFile"`
	Category         string `json:"category"`
	DownloadCategory string `json:"downloadCategory"`
	SavePath         string `json:"savePath"`
	RootFolder       string `json:"rootFolder"`
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
	// A config from before the download category existed downloads into its library
	// directory: the one value is both.
	for i := range c.Instances {
		if c.Instances[i].DownloadCategory == "" {
			c.Instances[i].DownloadCategory = c.Instances[i].Category
		}
	}
	// A Seerr entry from before the media server was named is a Plex one: that is the
	// only setup it could have had.
	if c.Seerr != nil && c.Seerr.MediaServer == "" {
		c.Seerr.MediaServer = mediaServerPlex
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate checks the rendered config. An empty app list is valid: a seedbox can be
// installed with apps off, and a release with no Arr and no qBittorrent has nothing
// to reconcile. The Job is a Helm hook, so refusing it fails the release and `box
// apply` waits on a receipt that can never come.
func (c *Config) validate() error {
	for i, inst := range c.Instances {
		if err := validateInstance(i, inst); err != nil {
			return err
		}
	}
	if c.Qbittorrent != nil {
		if err := requireFields("qbittorrent", map[string]string{
			fieldURL: c.Qbittorrent.URL,
		}); err != nil {
			return err
		}
	}
	if c.Sabnzbd != nil {
		if err := requireFields("sabnzbd", map[string]string{
			fieldURL: c.Sabnzbd.URL, fieldURLBase: c.Sabnzbd.URLBase, fieldAPIKeyFile: c.Sabnzbd.APIKeyFile,
			"savePath": c.Sabnzbd.SavePath, "downloadDir": c.Sabnzbd.DownloadDir,
		}); err != nil {
			return err
		}
	}
	if c.Prowlarr != nil {
		if err := requireFields("prowlarr", map[string]string{
			fieldURL: c.Prowlarr.URL, "selfUrl": c.Prowlarr.SelfURL, fieldAPIKeyFile: c.Prowlarr.APIKeyFile,
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
	if c.Seerr != nil {
		if err := requireFields("seerr", map[string]string{
			fieldURL: c.Seerr.URL, "publicUrl": c.Seerr.PublicURL,
		}); err != nil {
			return err
		}
		switch c.Seerr.MediaServer {
		case mediaServerPlex:
		case mediaServerJellyfin:
			if err := requireFields("seerr", map[string]string{
				"mediaServerUrl": c.Seerr.MediaServerURL,
			}); err != nil {
				return err
			}
			if c.Jellyfin == nil {
				return fmt.Errorf("seerr: mediaServer is jellyfin and jellyfin is not deployed, so Seerr has no server to sign its owner in to")
			}
		default:
			return fmt.Errorf("seerr: mediaServer is %q, and Seerr is set up against one server: plex or jellyfin", c.Seerr.MediaServer)
		}
	}
	if c.Qui != nil {
		if err := requireFields("qui", map[string]string{
			fieldURL: c.Qui.URL, fieldURLBase: c.Qui.URLBase, "username": c.Qui.Username,
			fieldPasswordFile: c.Qui.PasswordFile,
		}); err != nil {
			return err
		}
	}
	if c.Autobrr != nil {
		if err := requireFields("autobrr", map[string]string{
			fieldURL: c.Autobrr.URL, fieldURLBase: c.Autobrr.URLBase, "username": c.Autobrr.Username,
			fieldPasswordFile: c.Autobrr.PasswordFile,
		}); err != nil {
			return err
		}
	}
	if c.Maintainerr != nil {
		if err := requireFields("maintainerr", map[string]string{
			fieldURL: c.Maintainerr.URL, fieldURLBase: c.Maintainerr.URLBase,
		}); err != nil {
			return err
		}
		if c.Plex == nil {
			return fmt.Errorf("maintainerr is configured but plex is not")
		}
	}
	if c.Pbh != nil {
		if err := requireFields("pbh", map[string]string{
			fieldURL: c.Pbh.URL, fieldTokenFile: c.Pbh.TokenFile,
		}); err != nil {
			return err
		}
	}
	if c.Jellyfin != nil {
		if err := requireFields("jellyfin", map[string]string{
			fieldURL: c.Jellyfin.URL, "username": c.Jellyfin.Username,
			"userPasswordFile": c.Jellyfin.UserPasswordFile, "boxPasswordFile": c.Jellyfin.BoxPasswordFile,
		}); err != nil {
			return err
		}
		for i, l := range c.Jellyfin.Libraries {
			if err := requireFields(fmt.Sprintf("jellyfin libraries[%d]", i), map[string]string{
				fieldName: l.Name, "collectionType": l.CollectionType, "path": l.Path,
			}); err != nil {
				return err
			}
			switch l.CollectionType {
			case "movies", "tvshows", "music":
			default:
				return fmt.Errorf("jellyfin libraries[%d] (%s): %q is not a collection type Jellyfin has (movies, tvshows, music)", i, l.Name, l.CollectionType)
			}
		}
	}
	for i, b := range c.Bazarrs {
		if err := validateBazarr(i, b); err != nil {
			return err
		}
	}
	// Categories drive qBittorrent save paths; two instances sharing a name but
	// not a path would fight over the same downloads.
	return validateCategories(c.Instances)
}

// downloadCategory is the qBittorrent category an instance downloads into: its
// own, or the directory its library is in for a release from before the two
// were told apart.
func downloadCategory(inst Instance) string {
	if inst.DownloadCategory != "" {
		return inst.DownloadCategory
	}
	return inst.Category
}

// validateInstance checks one Arr instance: a kind the Job can register, and every
// field it registers set.
func validateInstance(index int, inst Instance) error {
	switch inst.Kind {
	case "sonarr", "radarr", "lidarr":
	default:
		return fmt.Errorf("instance %d: unknown kind %q", index, inst.Kind)
	}
	return requireFields(fmt.Sprintf("instance %q", inst.Name), map[string]string{
		"name": inst.Name, "url": inst.URL, fieldAPIKeyFile: inst.APIKeyFile,
		"category": inst.Category, "savePath": inst.SavePath, "rootFolder": inst.RootFolder,
	})
}

// validateBazarr checks one Bazarr instance and that it has at least one Arr to read:
// one with neither would run and do nothing.
func validateBazarr(index int, b BazarrConfig) error {
	where := fmt.Sprintf("bazarrs[%d] (%s)", index, b.Name)
	if err := requireFields(where, map[string]string{
		"name": b.Name, fieldURL: b.URL, fieldURLBase: b.URLBase, fieldAPIKeyFile: b.APIKeyFile,
	}); err != nil {
		return err
	}
	if b.Sonarr == nil && b.Radarr == nil {
		return fmt.Errorf("%s: has no sonarr and no radarr to read, so it would fetch subtitles for nothing", where)
	}
	for name, side := range map[string]*BazarrSide{"sonarr": b.Sonarr, "radarr": b.Radarr} {
		if side == nil {
			continue
		}
		if err := requireFields(where+" "+name, map[string]string{
			"host": side.Host, "port": strconv.Itoa(side.Port), fieldURLBase: side.URLBase,
			fieldAPIKeyFile: side.APIKeyFile,
		}); err != nil {
			return err
		}
	}
	return nil
}

// validateTautulli checks the fields the Tautulli steps need, and that Plex exists.
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

// validateCategories refuses one download category pointing at two save paths: both
// instances would write into the same downloads.
func validateCategories(instances []Instance) error {
	seen := map[string]string{}
	for _, inst := range instances {
		if prev, ok := seen[inst.DownloadCategory]; ok && prev != inst.SavePath {
			return fmt.Errorf("category %q is used with two save paths (%s, %s)", inst.DownloadCategory, prev, inst.SavePath)
		}
		seen[inst.DownloadCategory] = inst.SavePath
	}
	return nil
}

// requireFields reports the first empty field, named with its component.
func requireFields(component string, fields map[string]string) error {
	for field, value := range fields {
		if value == "" {
			return fmt.Errorf("%s: %s is empty", component, field)
		}
	}
	return nil
}

// ReadSecret reads a secret file, tolerating one trailing newline.
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
// An unconfigured path, a missing file or an empty one all mean "not supplied yet".
// Any other read error is returned: a credential that cannot be read must never
// degrade into configuring the app without it.
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
