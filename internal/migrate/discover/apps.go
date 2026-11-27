package discover

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plex"
	"github.com/swizzin/s5n/internal/qbtconf"
)

// configPath is where box mounts an app's config claim
// (charts/s5n/templates/workloads.yaml); Seerr is the exception.
const configPath = "/config"

// preferencesSection is [Preferences]: QSettings writes a key A/B as section [A]
// and key B\C, and an upgraded conf still carries the old spelling.
const preferencesSection = "Preferences"

// appInfo is what discovery needs of a catalog entry. It mirrors
// charts/s5n/catalog.yaml and workloads.yaml because discovery runs before any
// chart is installed; TestTableMatchesTheChart keeps them in step.
type appInfo struct {
	// instances says the app takes more than one instance (the Arrs do).
	instances bool
	// library says the app keeps root folders in its database.
	library bool
	// config is where a source may mount the config: the chart's path, then the
	// upstream image's.
	config []string
	// port reads the port the app listened on from its own config.
	port portReader
	// scratch is the chart's emptyDir paths, the same on the box whatever the
	// source mounted; a stored path there maps onto itself.
	scratch []string
	// aliases are the image names the same app answers to after a rename.
	aliases []string
	// snapshot is what a snapshot takes when the config is more than state (Plex's
	// Metadata tree); nil for every app whose config is its state.
	snapshot func(full bool) plex.Scope
}

// catalogApps is every app box can install, by the catalog's name.
var catalogApps = map[string]appInfo{
	"qbittorrent": {config: []string{configPath}, port: qbittorrentPort},
	"sonarr":      {instances: true, library: true, config: []string{configPath}, port: arrPort},
	"radarr":      {instances: true, library: true, config: []string{configPath}, port: arrPort},
	// Lidarr is the third Arr (PathExtensions.cs at v3.1.2.4902).
	"lidarr": {instances: true, library: true, config: []string{configPath}, port: arrPort},
	// Bazarr takes instances but is not an Arr: no library, no root folders, and
	// its port is `general.port` in YAML.
	"bazarr": {instances: true, config: []string{configPath}, port: bazarrPort},
	// SABnzbd is not an Arr: no library, port in [misc] sabnzbd.ini.
	"sabnzbd":  {config: []string{configPath}, port: sabnzbdPort},
	"prowlarr": {config: []string{configPath}, port: arrPort},
	// Plex's config is mostly its library (Metadata, Media: artwork and previews
	"plex": {config: []string{configPath}, port: plexPort, scratch: []string{"/transcode"},
		snapshot: plex.Snapshot},
	"tautulli": {config: []string{configPath}, port: tautulliPort},
	// Jellyfin's /config is the image's JELLYFIN_DATA_DIR; /cache is scratch, mapped
	// onto itself like Plex's /transcode.
	"jellyfin": {config: []string{configPath}, port: jellyfinPort, scratch: []string{"/cache"},
		aliases: []string{"emby"}},
	// Seerr's images default CONFIG_DIRECTORY to /app/config; the chart mounts its
	// claim there, though its own deployments use /config. It takes instances because a
	// box runs one per media server: a source with seerr and seerr-jellyfin is two.
	"seerr": {instances: true, config: []string{"/app/config", configPath}, port: seerrPort, aliases: []string{"overseerr", "jellyseerr"}},
	"qui":   {config: []string{configPath}, port: tomlConfigPort},
	// autobrr runs `--config /config` with HOME there.
	"autobrr": {config: []string{configPath}, port: tomlConfigPort},
	// PBH's -Dpbh.datadir is the config claim; the image defaults under $HOME.
	"pbh": {config: []string{configPath, "/app/data"}, port: peerBanHelperPort, aliases: []string{"peerbanhelper"}},
	// Maintainerr's DATA_DIR is the config claim; the image defaults to /opt/data.
	"maintainerr": {config: []string{configPath, "/opt/data"}, port: maintainerrPort},
	// FileBrowser keeps one SQLite database beside its config.yaml; a source keeps
	// both at /config, the image's default being under its home.
	"filebrowser": {config: []string{configPath, "/home/filebrowser/data"}, port: filebrowserPort},
}

// appAliases maps a renamed image repository onto the catalog's app. imageApp reads
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

// Carry is what a snapshot takes out of one app's config directory.
type Carry struct {
	// Full says the app's whole data travels, less Skip.
	Full bool
	// Sub is the directory the app's own data is in, when that is not the whole
	// config directory.
	Sub string
	// Files is the state carried when Full is false, relative to Sub.
	Files []CarryFile
	// Skip is where a full carry leaves the app's caches behind, relative to Sub.
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

// SnapshotCarry is what a snapshot carries for an app when that is a choice; ok is
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

// SnapshotSize is a full carry's bytes for the app whose config is more than its
func SnapshotSize(instance, dir string) (int64, bool, error) {
	info, found := instanceApps(instance)
	if !found || info.snapshot == nil {
		return 0, false, nil
	}
	size, err := plex.Size(dir)
	return size, true, err
}

// instanceApps is an instance's app: sonarr-4k is sonarr's.
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

// instanceName is box's name for a workload: sonarr4k and sonarr-4k are both
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

// findConfigMount is the config's container directory and its volume.
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
	if in.app == "seerr" {
		if err := d.readSeerrServer(in); err != nil {
			return fmt.Errorf("workload %s: %w", workloadID(in.wl), err)
		}
	}

	port, ok, err := listenPort(in.app, d.opt.ConfigFS, in.configDir, in.container.Env)
	if err != nil {
		return fmt.Errorf("workload %s: %w", workloadID(in.wl), err)
	}
	if !ok {
		// The pod's declaration can be stale (5006 declared while the app served
		// 9898), so a Service selecting only this app is evidence of its port.
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

// selectingTargets is the container ports of Services that select only this app,
// with each Service's description. A Service selecting two apps is a guess about
// which serves the port, so it does not count.
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

// portReader reads the port from the app's own config; ok is false when it says
// nothing, and the caller takes the pod's ports.
type portReader func(app string, fsys fs.FS, dir string, env map[string]string) (int, bool, error)

// listenPort reads the port an app listened on from its own config. An app whose
func listenPort(app string, fsys fs.FS, dir string, env map[string]string) (int, bool, error) {
	info, ok := catalogApps[app]
	if !ok || info.port == nil {
		return 0, false, nil
	}
	return info.port(app, fsys, dir, env)
}

// arrPort is <PREFIX>__SERVER__PORT first -- the environment wins over
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

// bazarrPort is BAZARR__PORT, then general.port in config.yaml (config.py:519 at
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

// sabnzbdPort is SABNZBD__PORT, then the first `port` (section [misc]) in
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

// filebrowserPort is http.port in config.yaml, default 80: it has no port
// environment variable, so the file is the only place a source's port can be.
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

// qbittorrentPort is WebUI/Port; an upgraded conf may still carry the pre-4.x
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

// tautulliPort is matched without case: Tautulli and PlexPy wrote it differently.
func tautulliPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config.ini")
	if err != nil || !ok {
		return 0, false, err
	}
	return iniPort(data, "http_port")
}

// tomlConfigPort is qui's and autobrr's top-level port in config.toml.
func tomlConfigPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config.toml")
	if err != nil || !ok {
		return 0, false, err
	}
	return tomlPort(data, "port")
}

// peerBanHelperPort is `server.http` in config.yml, not `port`: the template names
func peerBanHelperPort(_ string, fsys fs.FS, dir string, _ map[string]string) (int, bool, error) {
	data, ok, err := readConfigFile(fsys, dir, "config/config.yml")
	if err != nil || !ok {
		return 0, false, err
	}
	return yamlBlockPort(data, "server", "http")
}

// jellyfinPort is NetworkConfiguration.InternalHttpPort in config/network.xml, or
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

// The servers a Seerr's settings.main.mediaServerType names (server/constants/server.ts):
// 4, or no key at all in a Seerr older than it, is one nobody set up.
const (
	seerrPlex     = 1
	seerrJellyfin = 2
	seerrEmby     = 3
)

// readSeerrServer reads which media server a Seerr signs in to. The config the
// snapshot carries is that sign-in, so the instance box runs it as must be that
// server's; a missing file is a Seerr nobody set up, and box's default serves it.
func (d *discovery) readSeerrServer(in *instance) error {
	rel := strings.TrimPrefix(path.Join(in.configDir, "settings.json"), "/")
	raw, err := fs.ReadFile(d.opt.ConfigFS, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s's settings.json: %w", in.name, err)
	}
	var settings struct {
		Main struct {
			MediaServerType int `json:"mediaServerType"`
		} `json:"main"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("%s's settings.json under %s is not JSON Seerr wrote (%v): it is the config box carries, so stop the app and run discovery again", in.name, in.configDir, err)
	}
	switch settings.Main.MediaServerType {
	case seerrPlex:
		in.mediaServer = "plex"
	case seerrJellyfin:
		in.mediaServer = "jellyfin"
	case seerrEmby:
		d.note("%s was set up against Emby, and box sets a Seerr up against Plex or Jellyfin: it runs against the release's default server, and its setup is left for you to finish on its own page or to reset", in.name)
	}
	return nil
}

// seerrPort is PORT, or its default.
func seerrPort(_ string, _ fs.FS, _ string, env map[string]string) (int, bool, error) {
	if p, ok := envPort(env, "PORT"); ok {
		return p, true, nil
	}
	return 5055, true, nil
}

// plexPort is 32400; the image has no setting to read.
func plexPort(_ string, _ fs.FS, _ string, _ map[string]string) (int, bool, error) {
	return 32400, true, nil
}

// maintainerrPort is UI_PORT, or its default.
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

// xmlPort reads an element's integer text; deliberately not a decoder, since a
// namespace or comment must not change what it means.
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

// iniPort reads a key in any section and any case: upgraded installs keep the
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

// tomlPort reads a top-level integer; a full parser is a dependency box does not
func tomlPort(data []byte, key string) (int, bool, error) {
	for _, line := range lines(data) {
		if strings.HasPrefix(line, "[") {
			// A later table's keys are not top-level.
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

// yamlBlockPort reads an integer from PeerBanHelper's `server:` block; a block
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
			// The block ended without the key.
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
