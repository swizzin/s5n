package apps

import (
	"context"
	"fmt"
	pathpkg "path"
	"strings"
	"time"
)

// Kind is an Arr application type: the three share an API shape and differ in the
// version, the category field name and the endpoint of the same name.
type Kind string

const (
	Sonarr Kind = "sonarr"
	Radarr Kind = "radarr"
	// Lidarr is the third Arr: the same API with a different version and one extra
	// step for its root folder; see apiVersion and EnsureRootFolder.
	Lidarr Kind = "lidarr"
)

// apiVersion is the version each app serves: Sonarr and Radarr /api/v3, Lidarr
// /api/v1, because its controllers are [V1ApiController] and the route template is
// api/v{version}/{resource} (src/Lidarr.Http/VersionedApiControllerAttribute.cs at
// v3.1.2.4902).
func (k Kind) apiVersion() string {
	if k == Lidarr {
		return "v1"
	}
	return "v3"
}

// Bool returns a boolean member.
func (i Item) Bool(key string) bool {
	b, _ := i[key].(bool)
	return b
}

// Arr is a Sonarr, Radarr or Lidarr instance: the same code with an API version, a
// download-category field name, and for Lidarr a root folder carrying its default
// profiles.
type Arr struct {
	Kind    Kind
	Name    string
	client  *Client
	urlBase string
}

// NewArr builds a client. urlBase is the app's own URL base ("/sonarr"), set by the
// chart through its SERVER__URLBASE env var.
func NewArr(kind Kind, name, baseURL, urlBase, apiKey string) *Arr {
	c := NewClient(baseURL, nil)
	c.Header.Set("X-Api-Key", apiKey)
	return &Arr{Kind: kind, Name: name, client: c, urlBase: strings.TrimRight(urlBase, "/")}
}

func (a *Arr) api(path string) string { return a.urlBase + "/api/" + a.Kind.apiVersion() + path }

// Ping hits the unauthenticated health endpoint the chart uses as its readiness
// probe.
func (a *Arr) Ping(ctx context.Context) error {
	var s string
	return a.client.send(ctx, "GET", a.urlBase+"/ping", "", nil, nil, &s)
}

// WaitReady blocks until the app answers or timeout passes.
func (a *Arr) WaitReady(ctx context.Context, timeout time.Duration) error {
	return Retry(ctx, timeout, a.Ping)
}

func (a *Arr) downloadClients(ctx context.Context) ([]Item, error) {
	var out []Item
	err := a.client.Get(ctx, a.api("/downloadclient"), &out)
	return out, err
}

func (a *Arr) downloadClientSchema(ctx context.Context) ([]Item, error) {
	var out []Item
	err := a.client.Get(ctx, a.api("/downloadclient/schema"), &out)
	return out, err
}

// DownloadClientSpec is one download client every Arr instance gets: the release's
// qBittorrent, and SABnzbd when it is on.
type DownloadClientSpec struct {
	// Implementation is the Arr's own name for the client type; empty means
	// qBittorrent, the client every release has.
	Implementation string
	// Protocol is "torrent" or "usenet" (NzbDrone.Core.Indexers.DownloadProtocol).
	Protocol string
	Name     string
	Category string
	Host     string
	Port     int
	UseSSL   bool
	// URLBase is the client's prefix ("/sabnzbd"), or empty for qBittorrent.
	URLBase  string
	Username string
	Password string
	// APIKey is SABnzbd's key: a masked field, overlaid like a password and never
	// compared.
	APIKey string
}

func (s DownloadClientSpec) implementation() string {
	if s.Implementation == "" {
		return "QBittorrent"
	}
	return s.Implementation
}

func (s DownloadClientSpec) protocol() string {
	if s.Protocol == "" {
		return "torrent"
	}
	return s.Protocol
}

// categoryField is the field each app's qBittorrent settings call the download
// category (Download/Clients/QBittorrent/QBittorrentSettings.cs at each tag).
func (a *Arr) categoryField() string {
	switch a.Kind {
	case Radarr:
		return "movieCategory"
	case Lidarr:
		return "musicCategory"
	}
	return "tvCategory"
}

// EnsureDownloadClient makes the Arr's client match the spec and proves it with the
// app's own test endpoint: a passing test is the only evidence a stored secret works.
//
// A nonzero ownedID is authoritative, so a UI rename never creates a duplicate; zero
// adopts the enabled client pointing at the release's host (locateArrClient), with
// spec.Name as the fallback.
//
// On update only the data-contract fields are overlaid; everything else stays as the
// user set it. A deleted owned client is recreated.
//
// username and password are written empty: qBittorrent's login is bypassed for the
// release's addresses, so an old install's credential is cleared and nothing can
// rotate.
func (a *Arr) EnsureDownloadClient(ctx context.Context, spec DownloadClientSpec, ownedID int) (EnsureResult, error) {
	list, err := a.downloadClients(ctx)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("list download clients: %w", err)
	}
	cur, found, recreated, previousID, err := locateArrClient(list, spec, ownedID)
	if err != nil {
		return EnsureResult{}, err
	}

	fields := []ownedField{
		{"host", spec.Host, false},
		{"port", spec.Port, false},
		{"useSsl", spec.UseSSL, false},
		{"urlBase", spec.URLBase, false},
		{"username", spec.Username, false},
		{"password", spec.Password, true},
		{fieldAPIKey, spec.APIKey, true},
		{a.categoryField(), spec.Category, false},
	}

	var desired Item
	var drift []FieldChange
	change := Created
	if found {
		desired, drift = overlayOwned(cur, fields)
		desired["id"] = cur["id"]
		if len(drift) > 0 {
			change = Updated
		} else {
			change = Unchanged
		}
	} else {
		schemas, err := a.downloadClientSchema(ctx)
		if err != nil {
			return EnsureResult{}, fmt.Errorf("read download client schema: %w", err)
		}
		tmpl, ok := FindImplementation(schemas, spec.implementation())
		if !ok {
			return EnsureResult{}, fmt.Errorf("this %s build has no %s download client implementation", a.Kind, spec.implementation())
		}
		desired = tmpl.Clone()
		desired["name"] = spec.Name
		desired["enable"] = true
		desired["protocol"] = spec.protocol()
		desired["priority"] = 1
		desired["tags"] = []any{}
		for _, f := range fields {
			desired.SetField(f.name, f.value)
		}
	}

	// Test before writing. The Arr validates the payload as if saving, so carrying
	// the id is what keeps the test from reporting this client's own name as a clash.
	if err := a.client.Post(ctx, a.api("/downloadclient/test"), desired, nil); err != nil {
		return EnsureResult{Change: change, ID: desired.Int("id")}, fmt.Errorf("test download client %q: %w", spec.Name, err)
	}

	switch change {
	case Created:
		var out Item
		if err := a.client.Post(ctx, a.api("/downloadclient"), desired, &out); err != nil {
			return EnsureResult{}, fmt.Errorf("create download client %q: %w", spec.Name, err)
		}
		desired = out
	case Updated:
		if err := a.client.Put(ctx, fmt.Sprintf("%s/%d", a.api("/downloadclient"), desired.Int("id")), desired, nil); err != nil {
			return EnsureResult{}, fmt.Errorf("update download client %q: %w", spec.Name, err)
		}
	}
	return EnsureResult{Change: change, ID: desired.Int("id"), Drift: drift, Recreated: recreated, PreviousID: previousID}, nil
}

// EnsureRootFolder adds a root folder and asserts it is writable.
//
// Lidarr's is more than a path: its SharedValidator requires a name and positive
// default quality and metadata profile ids (Lidarr.Api.V1/RootFolders/RootFolderController.cs
// at v3.1.2.4902; RuleBuilderExtensions.ValidId is GreaterThan(0)), so a bare path
// is refused. It is created with the directory's name and the app's first two
// profiles.
func (a *Arr) EnsureRootFolder(ctx context.Context, path string) (Change, error) {
	var folders []Item
	if err := a.client.Get(ctx, a.api("/rootfolder"), &folders); err != nil {
		return Unchanged, fmt.Errorf("list root folders: %w", err)
	}
	for _, f := range folders {
		if strings.TrimRight(f.Str("path"), "/") != strings.TrimRight(path, "/") {
			continue
		}
		if !f.Bool("accessible") {
			return Unchanged, fmt.Errorf("root folder %s exists but %s cannot write to it", path, a.Kind)
		}
		return Unchanged, nil
	}
	body := Item{"path": path}
	if a.Kind == Lidarr {
		name := pathpkg.Base(strings.TrimRight(path, "/"))
		if name == "" || name == "." || name == "/" {
			name = path
		}
		quality, err := a.firstProfile(ctx, "/qualityprofile", "quality profile")
		if err != nil {
			return Unchanged, err
		}
		metadata, err := a.firstProfile(ctx, "/metadataprofile", "metadata profile")
		if err != nil {
			return Unchanged, err
		}
		body["name"] = name
		body["defaultQualityProfileId"] = quality
		body["defaultMetadataProfileId"] = metadata
	}
	if err := a.client.Post(ctx, a.api("/rootfolder"), body, nil); err != nil {
		return Unchanged, fmt.Errorf("create root folder %s: %w", path, err)
	}
	return Created, nil
}

// firstProfile is the lowest id an app offers in one of its profile lists: the app's
// own first choice, not a name this package invents.
func (a *Arr) firstProfile(ctx context.Context, endpoint, what string) (int, error) {
	var list []Item
	if err := a.client.Get(ctx, a.api(endpoint), &list); err != nil {
		return 0, fmt.Errorf("list %ss: %w", what, err)
	}
	best := 0
	for _, p := range list {
		if id := p.Int("id"); id > 0 && (best == 0 || id < best) {
			best = id
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("this %s build has no %s to put on a root folder", a.Kind, what)
	}
	return best, nil
}

// EnsureHardlinks turns on hardlink imports: the setting the whole /data contract
// exists for, so it is asserted rather than assumed.
func (a *Arr) EnsureHardlinks(ctx context.Context) (Change, error) {
	var mm Item
	if err := a.client.Get(ctx, a.api("/config/mediamanagement"), &mm); err != nil {
		return Unchanged, fmt.Errorf("read media management: %w", err)
	}
	if mm.Bool("copyUsingHardlinks") {
		return Unchanged, nil
	}
	mm["copyUsingHardlinks"] = true
	if err := a.client.Put(ctx, a.api("/config/mediamanagement"), mm, nil); err != nil {
		return Unchanged, fmt.Errorf("enable hardlinks: %w", err)
	}
	return Updated, nil
}
