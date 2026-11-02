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

// EnsureDownloadClient makes the Arr's qBittorrent download client match the spec
// and then proves the connection with the app's own test endpoint. Secrets are
// never read back by the API, so a passing test is the only honest evidence that
// the stored password still works.
func (a *Arr) EnsureDownloadClient(ctx context.Context, spec DownloadClientSpec) (Change, error) {
	schemas, err := a.downloadClientSchema(ctx)
	if err != nil {
		return Unchanged, fmt.Errorf("read download client schema: %w", err)
	}
	tmpl, ok := FindImplementation(schemas, "QBittorrent")
	if !ok {
		return Unchanged, fmt.Errorf("this %s build has no qBittorrent download client implementation", a.Kind)
	}
	desired := tmpl.Clone()
	desired["name"] = spec.Name
	desired["enable"] = true
	desired["protocol"] = "torrent"
	desired["priority"] = 1
	desired["tags"] = []any{}

	// Only the fields the s5n data contract depends on are forced; everything
	// else keeps the app's own schema default, so a version bump does not fight
	// us over settings we do not care about.
	set := []struct {
		name  string
		value any
	}{
		{"host", spec.Host},
		{"port", spec.Port},
		{"useSsl", spec.UseSSL},
		{"urlBase", ""},
		{"username", spec.Username},
		{"password", spec.Password},
		{a.categoryField(), spec.Category},
	}
	compared := make([]string, 0, len(set))
	for _, f := range set {
		if desired.SetField(f.name, f.value) {
			compared = append(compared, f.name)
		}
	}

	list, err := a.downloadClients(ctx)
	if err != nil {
		return Unchanged, fmt.Errorf("list download clients: %w", err)
	}
	change := Created
	if cur, ok := FindByName(list, spec.Name); ok {
		// Carry the id so the connection test's uniqueness check skips this very
		// client instead of reporting its own name as a duplicate.
		desired["id"] = cur["id"]
		if SameValue(cur["enable"], true) && SameValue(cur["protocol"], "torrent") &&
			SameValue(cur["priority"], 1) && MatchesFields(desired, cur, compared) {
			change = Unchanged
		} else {
			change = Updated
		}
	}

	// Test before writing. Arr validates the payload as if it were being saved, so
	// sending it with the id above is what makes the test idempotent.
	if err := a.client.Post(ctx, a.api("/downloadclient/test"), desired, nil); err != nil {
		return change, fmt.Errorf("test download client %q: %w", spec.Name, err)
	}

	switch change {
	case Created:
		if err := a.client.Post(ctx, a.api("/downloadclient"), desired, nil); err != nil {
			return Unchanged, fmt.Errorf("create download client %q: %w", spec.Name, err)
		}
	case Updated:
		if err := a.client.Put(ctx, fmt.Sprintf("%s/%d", a.api("/downloadclient"), desired.Int("id")), desired, nil); err != nil {
			return Unchanged, fmt.Errorf("update download client %q: %w", spec.Name, err)
		}
	}
	return change, nil
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
