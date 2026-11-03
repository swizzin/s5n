package apps

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Kind is an Arr application type. The two share an API shape and differ in the
// category field name and the endpoint of the same name.
type Kind string

const (
	Sonarr Kind = "sonarr"
	Radarr Kind = "radarr"
)

// Bool returns a boolean member.
func (i Item) Bool(key string) bool {
	b, _ := i[key].(bool)
	return b
}

// Arr is a Sonarr or Radarr instance.
type Arr struct {
	Kind    Kind
	Name    string
	client  *Client
	urlBase string
}

// NewArr builds a client. urlBase is the app's own URL base ("/sonarr"), which is
// what the chart sets through the Arr's SERVER__URLBASE env var.
func NewArr(kind Kind, name, baseURL, urlBase, apiKey string) *Arr {
	c := NewClient(baseURL, nil)
	c.Header.Set("X-Api-Key", apiKey)
	return &Arr{Kind: kind, Name: name, client: c, urlBase: strings.TrimRight(urlBase, "/")}
}

func (a *Arr) api(path string) string { return a.urlBase + "/api/v3" + path }

// Ping hits the unauthenticated health endpoint the chart also uses as its
// readiness probe.
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

// DownloadClientSpec is the qBittorrent client every Arr instance gets.
type DownloadClientSpec struct {
	Name     string
	Category string
	Host     string
	Port     int
	UseSSL   bool
	Username string
	Password string
}

func (a *Arr) categoryField() string {
	if a.Kind == Radarr {
		return "movieCategory"
	}
	return "tvCategory"
}

// EnsureDownloadClient makes the Arr's qBittorrent download client match the
// spec and then proves the connection with the app's own test endpoint.
// Secrets are never read back by the API, so a passing test is the only
// honest evidence that the stored password still works.
//
// ownedID, when nonzero, is the id the wiring Job recorded the last time it
// created or adopted this client: only an id match counts as "ours", so a
// rename in the Sonarr/Radarr UI is the user's business and never creates a
// duplicate. A zero ownedID means nothing is recorded (first run, or an
// owned.json the Job could not read), so an existing client named spec.Name is
// adopted instead.
//
// On update the live object is cloned and only the data-contract fields are
// overlaid (host, port, useSsl, urlBase, username, password, the category
// field) — name, enable, priority, tags and anything else are left exactly as
// the user set them. A deleted owned client is recreated.
func (a *Arr) EnsureDownloadClient(ctx context.Context, spec DownloadClientSpec, ownedID int) (EnsureResult, error) {
	list, err := a.downloadClients(ctx)
	if err != nil {
		return EnsureResult{}, fmt.Errorf("list download clients: %w", err)
	}
	cur, found, recreated, previousID := locateOwned(list, ownedID, spec.Name)

	fields := []ownedField{
		{"host", spec.Host, false},
		{"port", spec.Port, false},
		{"useSsl", spec.UseSSL, false},
		{"urlBase", "", false},
		{"username", spec.Username, false},
		{"password", spec.Password, true},
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
		tmpl, ok := FindImplementation(schemas, "QBittorrent")
		if !ok {
			return EnsureResult{}, fmt.Errorf("this %s build has no qBittorrent download client implementation", a.Kind)
		}
		desired = tmpl.Clone()
		desired["name"] = spec.Name
		desired["enable"] = true
		desired["protocol"] = "torrent"
		desired["priority"] = 1
		desired["tags"] = []any{}
		for _, f := range fields {
			desired.SetField(f.name, f.value)
		}
	}

	// Test before writing. Arr validates the payload as if it were being saved,
	// so sending it with the id above (when present) is what makes the test
	// idempotent instead of reporting this very client's own name as a clash.
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

// EnsureRootFolder adds a root folder and asserts it is writable by the app.
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
	if err := a.client.Post(ctx, a.api("/rootfolder"), Item{"path": path}, nil); err != nil {
		return Unchanged, fmt.Errorf("create root folder %s: %w", path, err)
	}
	return Created, nil
}

// EnsureHardlinks turns on hardlink imports. It is the setting the whole /data
// contract exists for, so it is asserted rather than assumed.
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
