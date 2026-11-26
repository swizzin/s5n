package discover

import (
	"encoding/xml"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/qbtconf"
)

// configPath is where box mounts an app's config claim: one directory per app
// instance, which every app but Seerr sees as /config
// (charts/s5n/templates/workloads.yaml).
const configPath = "/config"

// preferencesSection is qBittorrent's [Preferences]: QSettings writes a key A/B
// as section [A] and key B\C, and the keys an older version wrote are still
// there under it.
const preferencesSection = "Preferences"

// appInfo is the part of a catalog entry discovery needs: whether the app takes
// instances, where a source install keeps its config, and how to read the port
// it listened on.
//
// It mirrors charts/s5n/catalog.yaml (the app list and `instances:`) and
// charts/s5n/templates/workloads.yaml (the config mount) because discovery has
// to know them before a chart release is installed: the source is a cluster box
// did not make, and nothing in it says which app box would call this.
// TestTableMatchesTheChart keeps the two in step, so a chart that renames an app
// or moves an app's config fails here instead of migrating the wrong directory.
type appInfo struct {
	// instances says the app takes more than one instance (the Arrs do).
	instances bool
	// library says the app keeps root folders in its database, which is what
	// names the directory its library lives in.
	library bool
	// config lists the container directories a source may mount the app's
	// config at: the chart's own path first, then the upstream image's, for a
	// source that ran that image unmodified.
	config []string
	// port reads the port the app listened on from its own config.
	port portReader
	// scratch lists the container directories the chart gives the app as
	// scratch space (an emptyDir), which exist at the same path on the box
	// whatever the source mounted there. A path the app stored under one is as
	// good after the move, so discovery maps each onto itself.
	scratch []string
	// aliases are the image repository names the same app answers to after its
	// upstream renamed them, so a source install still carrying the old name is
	// the app it is rather than a workload box has no use for.
	aliases []string
	// snapshot is what a migration snapshot takes out of the app's config
	// directory, when its config is more than its state: Plex's Metadata tree is
	// a library of covers, artwork and thumbnails it rebuilds from the media it
	// is given, so a snapshot of it is a choice between its state and its whole
	// data. Nil for every app whose config is its state.
	snapshot func(full bool) plex.Scope
}

// catalogApps is every app box can install, by the catalog's name.
var catalogApps = map[string]appInfo{
	"qbittorrent": {config: []string{configPath}, port: qbittorrentPort},
	"sonarr":      {instances: true, library: true, config: []string{configPath}, port: arrPort},
	"radarr":      {instances: true, library: true, config: []string{configPath}, port: arrPort},
	// Lidarr is the third Arr: the same config directory, the same
	// <PREFIX>__SERVER__PORT and config.xml's <Port>, and root folders in its
	// own database (PathExtensions.cs at v3.1.2.4902).
	"lidarr": {instances: true, library: true, config: []string{configPath}, port: arrPort},
	// Bazarr takes instances too, but it is not an Arr: no library of its own
	// (its tables are the Arrs' libraries as they last saw them), no root
	// folders, and its port is a plain `general.port` in a YAML settings file.
	"bazarr": {instances: true, config: []string{configPath}, port: bazarrPort},
	// SABnzbd is not an Arr either: it keeps no library, and its port is
	// [misc] port in sabnzbd.ini, the one file its image passes to
	// --config-file.
	"sabnzbd":  {config: []string{configPath}, port: sabnzbdPort},
	"prowlarr": {config: []string{configPath}, port: arrPort},
	// Plex's images write the transcoder's directory into Preferences.xml as
	// /transcode, which the chart mounts as an emptyDir.
	//
	// A Plex config directory is mostly its library: Metadata and Media hold
	// covers, artwork, thumbnails and the preview thumbnails Plex builds from the
	// media it is given, and on a real library that is tens of gigabytes. What a
	// migration snapshot carries of it is the app's own rule
	// (internal/migrate/plex): its whole data when the space is there, its state
	// -- preferences and databases -- when it is not. It is box's own and
	// deliberately not the catalog's `snapshot:`, which is what an upgrade may
	// rewrite.
	"plex": {config: []string{configPath}, port: plexPort, scratch: []string{"/transcode"},
		snapshot: plex.Snapshot},
	"tautulli": {config: []string{configPath}, port: tautulliPort},
	// Jellyfin keeps its settings, its database and its cache under the one
	// config claim: the chart mounts /config, which is the image's own
	// JELLYFIN_DATA_DIR, and its scratch is /cache (also mounted, as an
	// emptyDir the chart does not carry). A source install's -cache directory
	// is the same idea, and discovery maps it onto itself for the same reason
	// Plex's /transcode is mapped.
	"jellyfin": {config: []string{configPath}, port: jellyfinPort, scratch: []string{"/cache"},
		aliases: []string{"emby"}},
	// Seerr's images default CONFIG_DIRECTORY to /app/config, and the chart
	// mounts its claim there (the one app whose config is not /config); the
	// deployments Seerr documents set it to /config.
	"seerr": {config: []string{"/app/config", configPath}, port: seerrPort, aliases: []string{"overseerr", "jellyseerr"}},
	"qui":   {config: []string{configPath}, port: tomlConfigPort},
	// autobrr's image runs `--config /config` and sets HOME there.
	"autobrr": {config: []string{configPath}, port: tomlConfigPort},
	// PeerBanHelper keeps its whole data directory in one place and the chart
	// points -Dpbh.datadir at its config claim; the image's own default is a
	// directory under $HOME.
	"pbh": {config: []string{configPath, "/app/data"}, port: peerBanHelperPort, aliases: []string{"peerbanhelper"}},
	// Maintainerr moves everything it writes to DATA_DIR, which the chart sets
	// to its config claim; the image's own default is /opt/data.
	"maintainerr": {config: []string{configPath, "/opt/data"}, port: maintainerrPort},
	// FileBrowser Quantum keeps no library: it is a file manager over the data
	// contract, and its state is one SQLite database beside its config.yaml.
	// The chart puts that settings file in a ConfigMap and the directory the
	// claim is mounted on keeps the database; a source install keeps both in
	// the same directory, which is /config in the images it is published as.
	// The image's own default path is under its home directory.
	"filebrowser": {config: []string{configPath, "/home/filebrowser/data"}, port: filebrowserPort},
}

// appAliases maps an image repository name that is the same app under another
// name onto the catalog's. Only renames go here: an upstream that renamed its
// images leaves a source install carrying the old one, and the app is the same
// app. A name that is not a catalog app is not a catalog app.
//
// imageApp returns the catalog app an image belongs to, from the last segment of
// its repository: ghcr.io/hotio/seerr:latest and seerr@sha256:... are both
// seerr. The tag and digest are dropped rather than compared, because a source
// install pins whichever it likes and box's variants are its own business.
func imageApp(image string) (string, bool) {
	ref := image
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		ref = ref[i+1:]
	}
	if i := strings.LastIndexByte(ref, ':'); i >= 0 {
		ref = ref[:i]
	}
	name := strings.ToLower(ref)
	if _, ok := catalogApps[name]; ok {
		return name, true
	}
	for app, info := range catalogApps {
		for _, alias := range info.aliases {
			if alias == name {
				return app, true
			}
		}
	}
	return name, false
}

// Carry is what a migration snapshot takes out of one app's config directory:
// the files, which of them the app cannot start without, the directories a full
// carry leaves out, and the one line a read's report says about it.
type Carry struct {
	// Full says the app's whole data travels, less Skip.
	Full bool
	// Sub is the directory inside the app's config directory its own data is
	// in, in slash form, when a snapshot takes that and not the whole of it.
	Sub string
	// Files is the state a snapshot carries when Full is false, relative to
	// Sub.
	Files []CarryFile
	// Skip is where a full carry leaves the app's caches behind, relative to
	// Sub.
	Skip []string
	// Note is the one line the report and the confirm say.
	Note string
	// Summary is the app's row on the confirm, in a few words.
	Summary string
}

// CarryFile is one file of a Carry, relative to Sub.
type CarryFile struct {
	Path string
	// Required says a snapshot without this file is refused.
	Required bool
}

// SnapshotCarry is what a snapshot carries for one planned app, by box's
// instance name, when that is a choice: full says the app's whole data travels,
// and false its state alone. ok is false when the app's config travels as its
// whole tree whatever full says, which is every app but the one whose config
// holds a library as well as its state.
func SnapshotCarry(instance string, full bool) (Carry, bool) {
	info, found := instanceApps(instance)
	if !found || info.snapshot == nil {
		return Carry{}, false
	}
	scope := info.snapshot(full)
	c := Carry{Full: scope.Full, Sub: scope.Sub, Skip: append([]string(nil), scope.Skip...), Note: scope.Note, Summary: scope.Summary}
	c.Files = make([]CarryFile, len(scope.Files))
	for i, f := range scope.Files {
		c.Files[i] = CarryFile{Path: f.Path, Required: f.Required}
	}
	return c, true
}

// SnapshotSize is the bytes a full carry of one app's config directory at dir
// takes, for the app whose config is more than its state. ok is false for every
// other app, whose whole tree a run counts as it copies it.
func SnapshotSize(instance, dir string) (int64, bool, error) {
	info, found := instanceApps(instance)
	if !found || info.snapshot == nil {
		return 0, false, nil
	}
	size, err := plex.Size(dir)
	return size, true, err
}

// instanceApps is the app one instance is: its own entry, or -- for an app that
// takes more than one -- the entry its name starts with, so sonarr-4k is
// sonarr's app.
func instanceApps(instance string) (appInfo, bool) {
	if info, found := catalogApps[instance]; found {
		return info, true
	}
	for app, cand := range catalogApps {
		if !cand.instances {
			continue
		}
		if rest, ok := strings.CutPrefix(instance, app+"-"); ok && rest != "" {
			return cand, true
		}
	}
	return appInfo{}, false
}

// instanceName is box's name for one workload of an app: the app itself, or for
// an app that takes instances, the app and whatever the source called the second
// one. sonarr4k and sonarr-4k are both sonarr-4k, because that is what the chart
// calls the pod and what every rewriter is handed, and a workload still called
// by the app's old name is that app under box's name.
func instanceName(app string, info appInfo, workload string) (string, bool) {
	name := strings.ToLower(workload)
	for _, known := range append([]string{app}, info.aliases...) {
		if name == known {
			return app, true
		}
		if !info.instances {
			continue
		}
		rest, ok := strings.CutPrefix(name, known)
		if !ok {
			continue
		}
		rest = strings.TrimPrefix(rest, "-")
		if rest == "" {
			return app, true
		}
		return app + "-" + rest, true
	}
	return "", false
}

// findConfigMount is the container directory this app's config is mounted at and
// the volume behind it, trying the chart's path first and then the upstream
// image's.
func (in *instance) findConfigMount() (string, Volume, bool) {
	for _, dir := range in.info.config {
		m, ok := in.container.mountAt(dir)
		if !ok {
			continue
		}
		return path.Clean(m.Path), in.wl.Volumes[m.Volume], true
	}
	return "", Volume{}, false
}

// readApp settles one app's config directory and the port it listened on.
func (d *discovery) readApp(in *instance) error {
	dir, vol, ok := in.findConfigMount()
	if !ok {
		return fmt.Errorf("workload %s mounts no config directory: %s keeps its config at %s",
			workloadID(in.wl), in.app, strings.Join(in.info.config, " or "))
	}
	if vol.HostPath == "" {
		return fmt.Errorf("workload %s mounts its config at %s from %s, which is not a host directory: discovery reads the config off the host, so the volume has to be a hostPath or a claim bound to one",
			workloadID(in.wl), dir, vol)
	}
	in.configMount, in.configDir = dir, vol.HostPath

	port, ok, err := listenPort(in.app, d.opt.ConfigFS, in.configDir, in.container.Env)
	if err != nil {
		return fmt.Errorf("workload %s: %w", workloadID(in.wl), err)
	}
	if !ok {
		// The pod's own declaration is the weaker answer, and it can be stale:
		// a source declared 5006 while the app served 9898 and its Service
		// forwarded there. A Service that selects only this app forwards to a
		// port the app itself listens behind, so its target port is evidence of
		// the app's port rather than of the pod spec's age.
		pod := dedupe(in.container.Ports)
		fromServices, services := d.selectingTargets(in)
		ports := dedupe(append(append([]int(nil), pod...), fromServices...))
		if len(ports) == 0 {
			return fmt.Errorf("workload %s: neither %s, the container, nor a Service that selects only it says which port it listens on, and a localhost address another app stored can only be resolved by its port",
				workloadID(in.wl), in.app)
		}
		in.ports = ports
		switch {
		case len(services) == 0:
			d.note("%s keeps no port in its config, so its ports are the pod's own (%s)", in.name, joinInts(pod))
		case len(pod) == 0:
			d.note("%s keeps no port in its config, so its ports are the target ports of the Services that select only it (%s)",
				in.name, strings.Join(services, ", "))
		default:
			d.note("%s keeps no port in its config, so its ports are the pod's own (%s) and the target ports of the Services that select only it (%s)",
				in.name, joinInts(pod), strings.Join(services, ", "))
		}
		return nil
	}
	in.ports = []int{port}
	return nil
}

// selectingTargets is what a Service adds to an app whose config names no port:
// the container ports the Services that select only it forward to, and each
// Service's own description for the note.
//
// Only a Service that selects this app and no other one counts. A Service that
// selects two apps is an address neither can claim -- the host rules leave it
// unclassified for the same reason -- and its port would be a guess about which
// app serves it.
func (d *discovery) selectingTargets(in *instance) (ports []int, from []string) {
	for _, svc := range d.svcs {
		if svc.Namespace != in.wl.Namespace || !d.selectsOnly(svc, in) {
			continue
		}
		for _, sp := range svc.Ports {
			target, ok := sp.target(in.wl)
			if !ok || target < 1 {
				continue
			}
			ports = append(ports, target)
			from = append(from, fmt.Sprintf("%s.%s %s", svc.Name, svc.Namespace, sp))
		}
	}
	sort.Strings(from)
	return ports, from
}

// selectsOnly reports whether a Service's selector matches this app's pod and no
// other instance's.
func (d *discovery) selectsOnly(svc Service, in *instance) bool {
	if !selects(svc.Selector, in.wl.Labels) {
		return false
	}
	for _, other := range d.instances {
		if other != in && selects(svc.Selector, other.wl.Labels) {
			return false
		}
	}
	return true
}

// portReader reads one app's listen port out of its own config, as the app
// itself would: the file the app writes, in the shape the app writes it. ok is
// false when nothing in the config says, and the caller then takes the pod's
// ports.
type portReader func(app string, fsys fs.FS, dir string, env map[string]string) (int, bool, error)

// listenPort reads the port an app listened on from its own config. An app whose
// catalog entry names no reader has no such setting discovery knows about.
func listenPort(app string, fsys fs.FS, dir string, env map[string]string) (int, bool, error) {
	info, ok := catalogApps[app]
	if !ok || info.port == nil {
		return 0, false, nil
	}
	return info.port(app, fsys, dir, env)
}

// arrPort is an Arr's HTTP port: the environment first, because every image that
// runs one reads <PREFIX>__SERVER__PORT and the environment wins over the file
// (charts/s5n/templates/workloads.yaml sets it for exactly that reason), then
// config.xml's <Port>.
func arrPort(app string, fsys fs.FS, dir string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, strings.ToUpper(app)+"__SERVER__PORT"); ok {
		return p, true, nil
	}
	data, ok, err := readConfigFile(fsys, dir, "config.xml")
	if err != nil || !ok {
		return 0, false, err
	}
	return xmlPort(data, "Port")
}

// bazarrPort is Bazarr's web port: BAZARR__PORT, which its image passes to
// `bazarr.py --port`, then general.port in config/config.yaml (config.py:519 at
// v1.6.2). The flag wins over the file, so the environment is read first for the
// same reason as the Arrs'.
func bazarrPort(_ string, fsys fs.FS, dir string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, "BAZARR__PORT"); ok {
		return p, true, nil
	}
	data, ok, err := readConfigFile(fsys, dir, "config/config.yaml")
	if err != nil || !ok {
		return 0, false, err
	}
	return yamlBlockPort(data, "general", "port")
}

// sabnzbdPort is SABnzbd's web port: SABNZBD__PORT, which its image passes to
// --server and which wins over the file, then [misc] port in sabnzbd.ini
// (apps/sabnzbd/entrypoint.sh in home-operations/containers). The key is the
// first `port` in the file: [misc] is the first section, and the nested server
// sections write their own ports below it.
func sabnzbdPort(_ string, fsys fs.FS, dir string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, "SABNZBD__PORT"); ok {
		return p, true, nil
	}
	data, ok, err := readConfigFile(fsys, dir, "sabnzbd.ini")
	if err != nil || !ok {
		return 0, false, err
	}
	return iniPort(data, "port")
}

// filebrowserPort is FileBrowser Quantum's web port: http.port in its
// config.yaml, and the image's own default of 80 when the file does not set
// one. Unlike the Arrs it has no port environment variable -- its only global
// flag is the config file's path (`FILEBROWSER_CONFIG`, cmd/kong.go at
// v2.1.0-stable) -- so the file is the only place a source's port can be.
func filebrowserPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config.yaml")
	if err != nil || !ok {
		return 0, false, err
	}
	port, ok, err := yamlBlockPort(data, "http", "port")
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 80, true, nil
	}
	return port, true, nil
}

// qbittorrentPort is qBittorrent's WebUI port. QSettings writes a key A/B/C as
// section [A] and key B\C, so it is WebUI/Port in the settings tree; an install
// upgraded in place can still carry the pre-4.x spelling under [Preferences].
func qbittorrentPort(_ string, fsys fs.FS, dir string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, "QBT_WEBUI_PORT"); ok {
		return p, true, nil
	}
	data, ok, err := readConfigFile(fsys, dir, "qBittorrent/qBittorrent.conf")
	if err != nil || !ok {
		return 0, false, err
	}
	doc := qbtconf.Parse(data)
	for _, key := range []struct{ section, key string }{
		{"WebUI", "Port"},
		{preferencesSection, `WebUI\Port`},
	} {
		if v, ok := doc.Get(key.section, key.key); ok {
			if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return p, true, nil
			}
		}
	}
	return 0, false, nil
}

// tautulliPort is Tautulli's web port. Tautulli and its PlexPy ancestor spell the
// key in the case of the version that wrote the file, so it is matched without
// case.
func tautulliPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config.ini")
	if err != nil || !ok {
		return 0, false, err
	}
	return iniPort(data, "http_port")
}

// tomlConfigPort is qui's and autobrr's listen port, both at the top of
// config.toml.
func tomlConfigPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config.toml")
	if err != nil || !ok {
		return 0, false, err
	}
	return tomlPort(data, "port")
}

// peerBanHelperPort is PeerBanHelper's WebUI port, `server.http` in config.yml.
// The key is `http`, not `port`: the template's block names the listen port
// there (src/main/resources/config.yml at v9.5.4), and a reader looking for
// `port` would take the pod's stale declaration instead.
func peerBanHelperPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config/config.yml")
	if err != nil || !ok {
		return 0, false, err
	}
	return yamlBlockPort(data, "server", "http")
}

// jellyfinPort is Jellyfin's: NetworkConfiguration.InternalHttpPort in
// config/network.xml, or its default (MediaBrowser.Common/Net/
// NetworkConfiguration.cs at v12.4 sets DefaultHttpPort = 8096). The file is
// XML and the element is the property's name: Jellyfin's configuration stores
// are serialised whole.
func jellyfinPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config/network.xml")
	if err != nil || !ok {
		return 0, false, err
	}
	var conf struct {
		InternalHttpPort int `xml:"InternalHttpPort"`
	}
	if err := xml.Unmarshal(data, &conf); err != nil {
		return 0, false, nil
	}
	if conf.InternalHttpPort > 0 {
		return conf.InternalHttpPort, true, nil
	}
	return 8096, true, nil
}

// seerrPort is Seerr's listen port: PORT, or its default.
func seerrPort(_ string, _ fs.FS, _ string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, "PORT"); ok {
		return p, true, nil
	}
	return 5055, true, nil
}

// plexPort is Plex's: the server serves 32400 and the image is told nothing
// else, so there is no setting to read.
func plexPort(_ string, _ fs.FS, _ string, _ map[string]string) (int, bool, error) {
	return 32400, true, nil
}

// maintainerrPort is Maintainerr's: UI_PORT, or its default.
func maintainerrPort(_ string, _ fs.FS, _ string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, "UI_PORT"); ok {
		return p, true, nil
	}
	return 6246, true, nil
}

// envPort reads one integer environment variable.
func envPort(env map[string]string, name string) (int, bool) {
	v, ok := env[name]
	if !ok {
		return 0, false
	}
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

// xmlPort reads an element's integer text from an XML file. The files read here
// are a flat list of elements (an Arr's config.xml) and the value is one
// integer, so this is deliberately not a decoder: a doc that grew a namespace or
// a comment cannot change what it means.
func xmlPort(data []byte, element string) (int, bool, error) {
	open, closing := "<"+element+">", "</"+element+">"
	s := string(data)
	i := strings.Index(s, open)
	if i < 0 {
		return 0, false, nil
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return 0, false, fmt.Errorf("%s is not closed", open)
	}
	p, err := strconv.Atoi(strings.TrimSpace(rest[:j]))
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", open, err)
	}
	return p, true, nil
}

// iniPort reads an integer key from an INI file, in any section and without
// case: Tautulli and PlexPy wrote the same key in different case across
// versions, and an install upgraded in place keeps whichever wrote it.
func iniPort(data []byte, key string) (int, bool, error) {
	for _, line := range lines(data) {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false, fmt.Errorf("%s: %w", key, err)
		}
		return p, true, nil
	}
	return 0, false, nil
}

// tomlPort reads a top-level integer key from a TOML file. qui and autobrr keep
// their listen port at the top of config.toml, and nothing else in the file is
// read: a full TOML parser is a dependency box does not need to learn two
// integers.
func tomlPort(data []byte, key string) (int, bool, error) {
	for _, line := range lines(data) {
		if strings.HasPrefix(line, "[") {
			// A later table's keys are not the top-level ones.
			return 0, false, nil
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		p, err := strconv.Atoi(strings.Trim(strings.TrimSpace(v), `"'`))
		if err != nil {
			return 0, false, fmt.Errorf("%s: %w", key, err)
		}
		return p, true, nil
	}
	return 0, false, nil
}

// yamlBlockPort reads an integer key from one YAML block, for PeerBanHelper's
// `server:` in config.yml. The file is the app's own and it is read for one
// number: a block scan answers that without teaching box to parse YAML a second
// time.
func yamlBlockPort(data []byte, block, key string) (int, bool, error) {
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !inBlock {
			if strings.HasPrefix(line, block+":") {
				inBlock = true
			}
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			// The next top-level key: the block ended without the key.
			return 0, false, nil
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false, fmt.Errorf("%s.%s: %w", block, key, err)
		}
		return p, true, nil
	}
	return 0, false, nil
}

// lines is a file's non-empty, trimmed lines.
func lines(data []byte) []string {
	raw := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// dedupe is the ports of a pod, once each, in order.
func dedupe(ports []int) []int {
	seen := make(map[int]bool, len(ports))
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// joinInts renders ports the way a note says them.
func joinInts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}
