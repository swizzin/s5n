package arr

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/rewrite"
)

// The field names more than one download client implementation uses for a
// path.
const (
	fieldDirectory   = "directory"
	fieldTVDirectory = "tvdirectory"
	// schemeHTTP is what every in-cluster connection is written as: the
	// release's pod listens on plain HTTP behind the Gateway.
	schemeHTTP = "http"
)

// historyPathKeys are the History and DownloadHistory JSON keys that hold an
// absolute path: dropped/imported episode files, and the rename events' paths
// (sonarr@v4.0.14.2938:src/NzbDrone.Core/History/HistoryService.cs:219-220,315-317,
// src/NzbDrone.Core/Download/History/DownloadHistoryService.cs:159-160).
// RelativePath and SourceRelativePath sit beside them and are relative to the
// library folder, so they follow the folder and need no rule.
var historyPathKeys = []string{"droppedpath", "importedpath", "path", "sourcepath", "destinationpath"}

// downloadPaths is the JSON field each download client implementation keeps a
// filesystem path in, keyed by the Implementation column lowercased
// (research: §2.2, read off Download/Clients/*/…Settings.cs at the source
// tags). A client with no path here keeps its paths on a machine that is not
// this app's, or has no path at all: qBittorrent learns them over its API.
var downloadPaths = map[string][]string{
	"aria2":            {fieldDirectory},
	"deluge":           {"downloaddirectory", "completeddirectory"},
	"downloadstation":  {fieldTVDirectory},
	"flood":            {"directorydefault"},
	"freeboxdownload":  {"destinationdirectory"},
	"pneumatic":        {"nzbfolder", "strmfolder"},
	"rtorrent":         {fieldTVDirectory, "moviedirectory", fieldDirectory},
	"torrentblackhole": {"torrentfolder", "watchfolder"},
	"transmission":     {fieldTVDirectory, "moviedirectory", fieldDirectory},
	"tribler":          {fieldTVDirectory},
	"usenetblackhole":  {"nzbfolder", "watchfolder"},
}

// downloadClientsKnown is every implementation whose settings were checked for
// paths, including the ones that have none, so an unknown client is the only
// thing that raises a note.
var downloadClientsKnown = map[string]bool{
	"hadouken": true, "nzbget": true, "nzbvortex": true, "qbittorrent": true,
	"sabnzbd": true, "utorrent": true, "vuze": true,
}

// downloadClientsBoxRuns are the download clients this box runs: qBittorrent,
// and only qBittorrent (charts/s5n/catalog.yaml). Every other implementation a
// source Arr holds is a client of a type box does not run, and its connection is
// left exactly as it is and called out: rewriting it would point that client at
// an app that does not speak its protocol, and deleting it is the operator's
// call, never migrate's.
var downloadClientsBoxRuns = map[string]bool{"qbittorrent": true}

// notificationTargets are the notifications whose settings name another
// machine on the source: a media server box may run, or the same one under a
// different name. Every other notifier (a chat service, a webhook, a mail
// relay) points outside the source and is left alone.
var notificationTargets = map[string]bool{
	"plexserver":   true,
	"mediabrowser": true,
	"jellyfin":     true,
	"xbmc":         true,
}

// fields is a provider's settings blob kept as raw values: a field this
// package does not touch is written back exactly as the app stored it, a
// number keeps its own text, and a value that is not a string is never
// mistaken for one (Settings is a JSON text column,
// sonarr@v4.0.14.2938:src/NzbDrone.Core/ThingiProvider/ProviderDefinition.cs:15-30).
type fields map[string]json.RawMessage

// parseFields reads a settings column. An empty or NULL column is an empty
// object: a provider with no settings has nothing to rewrite.
func parseFields(col sql.NullString) (fields, error) {
	if !col.Valid || strings.TrimSpace(col.String) == "" {
		return fields{}, nil
	}
	var f fields
	if err := json.Unmarshal([]byte(col.String), &f); err != nil {
		return nil, fmt.Errorf("settings are not JSON: %w", err)
	}
	if f == nil {
		return nil, fmt.Errorf("settings are not a JSON object")
	}
	return f, nil
}

// find returns the key the blob stores under name, whatever case the
// serializer gave it. The app writes provider properties camel-cased while the
// classes name them PascalCase, and History's dictionary keys are camel-cased
// on disk although the code adds them PascalCase
// (sonarr@v4.0.14.2938:src/NzbDrone.Core/History/HistoryService.cs:219).
func (f fields) find(name string) (string, bool) {
	for key := range f {
		if strings.EqualFold(key, name) {
			return key, true
		}
	}
	return "", false
}

// str reads a string field.
func (f fields) str(key string) (string, bool) {
	var v string
	if err := json.Unmarshal(f[key], &v); err != nil {
		return "", false
	}
	return v, true
}

// integer reads a numeric field.
func (f fields) integer(key string) (int, bool) {
	var n json.Number
	if err := json.Unmarshal(f[key], &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// setString writes a string field and reports whether it changed.
func (f fields) setString(key, value string) bool {
	want := mustJSON(value)
	if bytes.Equal(f[key], want) {
		return false
	}
	f[key] = want
	return true
}

// setInt writes a numeric field and reports whether it changed.
func (f fields) setInt(key string, value int) bool {
	want := json.RawMessage(strconv.Itoa(value))
	if bytes.Equal(f[key], want) {
		return false
	}
	f[key] = want
	return true
}

// setBool writes a boolean field and reports whether it changed.
func (f fields) setBool(key string, value bool) bool {
	want := json.RawMessage(strconv.FormatBool(value))
	if bytes.Equal(f[key], want) {
		return false
	}
	f[key] = want
	return true
}

func (f fields) marshal() (string, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// mustJSON renders one string as a JSON value. It cannot fail: a string always
// has an encoding.
func mustJSON(v string) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("arr: encoding a string: " + err.Error())
	}
	return data
}

// providerEnable is the column each provider table stores "this provider is on"
// in. An app's definition may compute the flag instead of storing it
// (NotificationDefinition.Enable is derived from its event flags,
// sonarr@v4.0.14.2938:src/NzbDrone.Core/Notifications/NotificationDefinition.cs),
// and then the table has no column and every row is on. The three that do store
// it: DownloadClients.Enable (ProviderDefinition.Enable,
// src/NzbDrone.Core/ThingiProvider/ProviderDefinition.cs), an Arr import list's
// EnableAutomaticAdd (ImportListDefinition.Enable, src/NzbDrone.Core/ImportLists)
// and a Prowlarr application's SyncLevel (ApplicationDefinition.Enable,
// prowlarr@v2.7.0.5723:src/NzbDrone.Core/Applications/ApplicationDefinition.cs,
// Disabled = 0).
var providerEnable = map[string]string{
	"DownloadClients": "Enable",
	"ImportLists":     "EnableAutomaticAdd",
	"Applications":    "SyncLevel",
}

// provider is one row of a table that stores a provider's settings as JSON.
type provider struct {
	id             int64
	name           string
	implementation string
	// enabled is false for a provider the app never dials, so an address it
	// stores that cannot be placed is left alone rather than made a refusal. A
	// row in a table without the flag is on.
	enabled  bool
	settings fields
}

// providerTable reads a provider table. Every one of them has the same four
// columns this pass needs: the row's id and name, the implementation that
// decides the settings' shape, and the settings blob. A table that also stores
// whether the provider is on is read with it (providerEnable); a row without the
// flag is one the app dials.
func (a *analysis) providerTable(ctx context.Context, db *sql.DB, table string, visit func(provider) error) error {
	withEnable := false
	if column := providerEnable[table]; column != "" {
		ok, err := hasColumn(ctx, db, table, column)
		if err != nil {
			return err
		}
		withEnable = ok
	}
	columns := "Id, Name, Implementation, Settings"
	if withEnable {
		columns = "Id, Name, " + providerEnable[table] + ", Implementation, Settings"
	}
	//nolint:gosec // the table name and the column are constants in this package
	rows, err := db.QueryContext(ctx, "SELECT "+columns+" FROM "+table)
	if err != nil {
		return fmt.Errorf("read %s in %s: %w", table, a.file, err)
	}
	defer rows.Close()
	for rows.Next() {
		var p provider
		var implementation, settings sql.NullString
		if withEnable {
			var enabled sql.NullInt64
			err = rows.Scan(&p.id, &p.name, &enabled, &implementation, &settings)
			p.enabled = !enabled.Valid || enabled.Int64 != 0
		} else {
			err = rows.Scan(&p.id, &p.name, &implementation, &settings)
			p.enabled = true
		}
		if err != nil {
			return fmt.Errorf("read %s in %s: %w", table, a.file, err)
		}
		p.implementation = implementation.String
		f, err := parseFields(settings)
		if err != nil {
			return fmt.Errorf("%s row %d in %s: %w", table, p.id, a.file, err)
		}
		p.settings = f
		if err := visit(p); err != nil {
			return err
		}
	}
	return rows.Err()
}

// hasColumn reports whether a table the database has carries a column of that
// name. A schema is the app's, not this package's: versions differ, and a
// synthetic one omits what a real app always writes.
func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("read %s schema: %w", table, err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

// providers rewrites what the apps' settings rows say about paths and about
// the other apps they talk to.
func (a *analysis) providers(ctx context.Context, db *sql.DB) error {
	steps := []func(context.Context, *sql.DB) error{a.downloadClients, a.notifications}
	if a.kind == kindProwlarr {
		steps = append(steps, a.applications)
	} else {
		steps = append(steps, a.importLists)
	}
	for _, step := range steps {
		if err := step(ctx, db); err != nil {
			return err
		}
	}
	return nil
}

func (a *analysis) downloadClients(ctx context.Context, db *sql.DB) error {
	return a.providerTable(ctx, db, "DownloadClients", a.downloadClient)
}

// downloadClient points a client's connection at the box, and rewrites the
// paths it stores.
//
// A client with no host (a blackhole folder, pneumatic) keeps its files on
// this app's filesystem, so its paths go through this app's table. A client
// that is left where it is keeps its own paths as well: rewriting them with
// this app's table would tell that machine about a directory it does not have.
//
// Two clients are never rewritten and never a refusal, because neither one can
// be wrong: one this box does not run (nothing in the release speaks its
// protocol), and a disabled one whose address cannot be placed (the app never
// dials it). Both are left exactly as they are, with the reason, in the report's
// Left list.
func (a *analysis) downloadClient(p provider) error {
	const where = "DownloadClients.Settings"
	ad, has, err := a.hostPort(where, p.settings)
	if err != nil {
		return err
	}
	impl := strings.ToLower(p.implementation)
	if has && !downloadClientsBoxRuns[impl] {
		a.left = append(a.left, rewrite.Left{
			File: a.file, Where: where + ".host", What: ad.text(),
			Why: fmt.Sprintf("%q is a %s client and this box runs only qBittorrent, so a rewrite would point it at an app that does not speak its protocol; delete it in the Arr if you do not need it",
				p.name, p.implementation),
		})
		return nil
	}
	if has && ad.verdict == plan.Unclassified {
		if !p.enabled {
			a.left = append(a.left, rewrite.Left{
				File: a.file, Where: where + ".host", What: ad.text(),
				Why: fmt.Sprintf("%q is switched off, so %s is never dialled and nothing is migrated to it",
					p.name, ad.text()),
			})
			return nil
		}
		a.unclass = append(a.unclass, ad.address(a.app, a.file, where+".host"))
		return nil
	}
	if has && ad.verdict != plan.Rewrite {
		return nil
	}
	changed := false
	table := a.app
	if has {
		table = ad.target
		a.clientTargets[strings.ToLower(ad.host)] = ad.target
		changed = a.pointAt(where, p.settings, ad)
	}
	for _, key := range downloadPaths[impl] {
		if a.objectPath(table, where, p.settings, key) {
			changed = true
		}
	}
	if !has && !downloadClientsKnown[impl] {
		a.notes = append(a.notes, fmt.Sprintf(
			"%s: download client %q is a type this tool does not know; its settings were not searched for paths",
			p.name, p.implementation))
	}
	if has {
		// Wiring adopts the client by id, so the one the source already had
		// becomes the one the release manages (docs/box.md, "Migrated
		// connections are adopted by ID").
		a.owned = append(a.owned, rewrite.Owned{Kind: "downloadclient", ID: int(p.id), Name: p.name, Target: ad.target})
	}
	return a.store("DownloadClients", p, changed)
}

func (a *analysis) notifications(ctx context.Context, db *sql.DB) error {
	return a.providerTable(ctx, db, "Notifications", a.notification)
}

// notification rewrites a notifier that names a media server. A CustomScript
// runs on this app's filesystem, so its path goes through this app's table; a
// notifier with no server of its own (a chat service, a webhook) is left
// alone, because box has nothing to say about where it lives.
func (a *analysis) notification(p provider) error {
	const where = "Notifications.Settings"
	changed := false
	if strings.EqualFold(p.implementation, "CustomScript") {
		if a.objectPath(a.app, where, p.settings, "path") {
			changed = true
		}
	}
	if notificationTargets[strings.ToLower(p.implementation)] {
		ad, has, err := a.hostPort(where, p.settings)
		if err != nil {
			return err
		}
		switch {
		case has && ad.verdict == plan.Unclassified:
			a.unclass = append(a.unclass, ad.address(a.app, a.file, where+".host"))
		case has && ad.verdict == plan.Rewrite:
			if a.pointAt(where, p.settings, ad) {
				changed = true
			}
		}
	}
	return a.store("Notifications", p, changed)
}

// importLists points a list at the box. The Sonarr and Radarr list types take
// the remote server's address in baseUrl and its folders in RootFolderPaths;
// the folders are a filter over the *other* server's filesystem and are left
// alone, because rewriting them would silently disable the filter
// (research: §2.2, §5.5). A list the app does not sync is left where it is.
func (a *analysis) importLists(ctx context.Context, db *sql.DB) error {
	return a.providerTable(ctx, db, "ImportLists", func(p provider) error {
		const where = "ImportLists.Settings.baseUrl"
		key, ok := p.settings.find("baseUrl")
		if !ok {
			return nil
		}
		stored, ok := p.settings.str(key)
		if !ok || stored == "" {
			return nil
		}
		out, _, resolved, miss, err := a.resolveURL(where, stored)
		if err != nil {
			return err
		}
		if miss != nil {
			if !p.enabled {
				a.left = append(a.left, rewrite.Left{
					File: a.file, Where: where, What: miss.Text(),
					Why: fmt.Sprintf("%q is not synced, so %s is never dialled and nothing is migrated to it",
						p.name, miss.Text()),
				})
				return nil
			}
			a.unclass = append(a.unclass, *miss)
			return nil
		}
		if !resolved || out == stored {
			return nil
		}
		p.settings[key] = mustJSON(out)
		a.add(a.file, "ImportLists.Settings", "host")
		return a.store("ImportLists", p, true)
	})
}

// applications rewrites Prowlarr's applications: the URL Prowlarr reaches the
// Arr at (baseUrl) and the URL the Arr reaches Prowlarr at (prowlarrUrl).
// Prowlarr's application settings hold no filesystem path
// (prowlarr@v2.7.0.5723:src/NzbDrone.Core/Applications/Sonarr/SonarrSettings.cs:32-35).
// An application whose sync is off is left where it is.
func (a *analysis) applications(ctx context.Context, db *sql.DB) error {
	return a.providerTable(ctx, db, "Applications", func(p provider) error {
		const where = "Applications.Settings"
		changed := false
		target := ""
		for _, name := range []string{"baseUrl", "prowlarrUrl"} {
			key, ok := p.settings.find(name)
			if !ok {
				continue
			}
			stored, ok := p.settings.str(key)
			if !ok || stored == "" {
				continue
			}
			out, app, resolved, miss, err := a.resolveURL(where+"."+key, stored)
			if err != nil {
				return err
			}
			if miss != nil {
				if !p.enabled {
					a.left = append(a.left, rewrite.Left{
						File: a.file, Where: where + "." + key, What: miss.Text(),
						Why: fmt.Sprintf("%q has its sync switched off, so %s is never dialled and nothing is indexed through it",
							p.name, miss.Text()),
					})
					continue
				}
				a.unclass = append(a.unclass, *miss)
				continue
			}
			if !resolved {
				continue
			}
			if name == "baseUrl" {
				target = app
			}
			if out == stored {
				continue
			}
			p.settings[key] = mustJSON(out)
			a.add(a.file, where, "host")
			changed = true
		}
		if target != "" {
			a.owned = append(a.owned, rewrite.Owned{Kind: "application", ID: int(p.id), Name: p.name, Target: target})
		}
		return a.store("Applications", p, changed)
	})
}

// store writes a provider's settings back when they changed, and counts it
// against the row's own key when a path changed.
func (a *analysis) store(table string, p provider, changed bool) error {
	if !changed {
		return nil
	}
	blob, err := p.settings.marshal()
	if err != nil {
		return fmt.Errorf("%s row %d in %s: %w", table, p.id, a.file, err)
	}
	a.edits = append(a.edits, edit{table: table, column: "Settings", id: p.id, value: blob})
	return nil
}

// objectPath rewrites one path-valued field of a settings blob. The path is
// looked up by its field name, never by replacing text: a path is one value
// among others and a string replacement would also hit a category name, a
// comment or a password that happens to look like one.
func (a *analysis) objectPath(table, where string, f fields, name string) bool {
	key, ok := f.find(name)
	if !ok {
		return false
	}
	stored, ok := f.str(key)
	if !ok || stored == "" {
		// An empty value means "use the default", which follows whatever the
		// app is given; a relative one follows the directory it sits under.
		return false
	}
	out, covered := a.in.Plan.Rewrite(table, stored)
	if !covered {
		a.gap(table, a.file, where+"."+key, stored)
		return false
	}
	if out == stored {
		return false
	}
	f[key] = mustJSON(out)
	a.add(a.file, where+"."+key, "path")
	return true
}

// address is what the plan said about a stored host and port.
type address struct {
	verdict plan.Verdict
	target  string // the box instance the address now means
	host    string // the host as stored, which is also the name a mapping uses
	port    int    // the port as stored, 0 when the settings hold none
	ep      rewrite.Endpoint
}

// text is the address as the app stored it, for a line the operator reads.
func (ad address) text() string {
	if ad.port == 0 {
		return ad.host
	}
	return fmt.Sprintf("%s:%d", ad.host, ad.port)
}

// address is the report entry for an address the plan does not settle.
func (ad address) address(app, file, where string) rewrite.Address {
	return rewrite.Address{App: app, File: file, Where: where, Host: ad.host, Port: ad.port}
}

// hostPort settles the host and port a provider's settings store.
//
// has is false when the blob stores no host at all: a blackhole client has
// none, and nothing about its address can be wrong. The verdict is in.Resolve's:
// the plan's rules and the apps' ports decide, and an address the plan does not
// settle but that already names one of the box's endpoints is that app, which is
// what a second pass over a copy the first one wrote needs. Recording an
// unclassified address is the caller's: one client is never dialled, so its
// address is left instead of reported.
func (a *analysis) hostPort(where string, f fields) (address, bool, error) {
	hostKey, ok := f.find("host")
	if !ok {
		return address{}, false, nil
	}
	host, ok := f.str(hostKey)
	if !ok || host == "" {
		return address{}, false, nil
	}
	port := 0
	if portKey, ok := f.find("port"); ok {
		if n, ok := f.integer(portKey); ok {
			port = n
		}
	}
	res := a.in.Resolve(host, port)
	switch res.Verdict {
	case plan.Unclassified:
		return address{verdict: plan.Unclassified, host: host, port: port}, true, nil
	case plan.Leave:
		return address{verdict: plan.Leave, host: host, port: port}, true, nil
	}
	ep, ok := a.in.Targets[res.App]
	if !ok {
		return address{}, false, fmt.Errorf("%s in %s: the plan maps %s to %s, which this run has no endpoint for", where, a.file, host, res.App)
	}
	return address{verdict: plan.Rewrite, target: res.App, host: host, port: port, ep: ep}, true, nil
}

// pointAt writes the box's address into a provider's settings. HTTPS is off
// because everything in the release sits behind one Gateway and the pod's own
// listener is plain HTTP.
func (a *analysis) pointAt(where string, f fields, ad address) bool {
	changed := false
	if key, ok := f.find("host"); ok && f.setString(key, ad.ep.Host) {
		changed = true
	}
	if key, ok := f.find("port"); ok && f.setInt(key, ad.ep.Port) {
		changed = true
	}
	if key, ok := f.find("urlBase"); ok && f.setString(key, ad.ep.URLBase) {
		changed = true
	}
	if key, ok := f.find("useSsl"); ok && f.setBool(key, false) {
		changed = true
	}
	if changed {
		a.add(a.file, where, "host")
	}
	return changed
}

// resolveURL settles a stored URL that names another app and returns the URL
// the box serves it at. resolved is false when the plan leaves the address alone
// or notes the value is not a URL at all; miss is the address when the plan does
// not settle it, for the caller to report or leave. target is the box instance
// for a rewritten URL.
func (a *analysis) resolveURL(where, stored string) (newURL, target string, resolved bool, miss *rewrite.Address, err error) {
	u, perr := url.Parse(stored)
	if perr != nil || (u.Scheme != schemeHTTP && u.Scheme != "https") || u.Hostname() == "" {
		a.notes = append(a.notes, fmt.Sprintf("%s in %s: %q is not an http URL; left as it is", where, a.file, stored))
		return "", "", false, nil, nil
	}
	// A URL's port is the one it names, or the one its scheme implies: a
	// stored https URL with no port is port 443, not port 0, and a rule
	// narrowed to 443 is what settles it.
	port := rewrite.URLPort(u)
	if p := u.Port(); p != "" {
		if _, convErr := strconv.Atoi(p); convErr != nil {
			a.notes = append(a.notes, fmt.Sprintf("%s in %s: %q names a port that is not a number; left as it is", where, a.file, stored))
			return "", "", false, nil, nil
		}
	}
	res := a.in.ResolveURL(u)
	switch res.Verdict {
	case plan.Unclassified:
		ad := rewrite.Address{App: a.app, File: a.file, Where: where, Host: u.Hostname(), Port: port}
		return "", "", false, &ad, nil
	case plan.Leave:
		return "", "", false, nil, nil
	}
	ep, ok := a.in.Targets[res.App]
	if !ok {
		return "", "", false, nil, fmt.Errorf("%s in %s: the plan maps %s to %s, which this run has no endpoint for", where, a.file, u.Host, res.App)
	}
	// The box's own address, whole: plain HTTP, the Service, the chart's port
	// and the chart's URL base. A path the source's URL carried is the source's
	// shape, and box serves the app under its own base.
	return ep.URL(), res.App, true, nil, nil
}
