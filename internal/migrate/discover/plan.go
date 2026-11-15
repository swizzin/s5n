package discover

import (
	"path"
	"sort"
	"strings"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// plan assembles the whole file: one entry per app with its own path table, the
// data root, the Services, and the workloads left behind.
func (d *discovery) plan(data *plan.Data, svcs []Service) *plan.Plan {
	p := &plan.Plan{
		Version: plan.Version,
		Data:    data,
		Apps:    make(map[string]plan.App, len(d.instances)),
		Hosts:   d.hostRules(svcs),
	}
	for _, in := range d.instances {
		p.Apps[in.name] = plan.App{
			Source:    in.wl.Name,
			Config:    in.configDir,
			Namespace: in.wl.Namespace,
			Ports:     in.ports,
			Paths:     d.pathRules(in, data),
		}
	}
	for _, s := range d.rep.Skipped {
		p.Outside = append(p.Outside, s.Workload)
	}
	sort.Strings(p.Outside)
	return p
}

// pathRules is one app's table: the paths it stored, as it stored them, mapped
// onto the paths box's release gives it.
//
// The table is per app because every app saw its own paths: the client mounted
// the downloads at /downloads while an Arr may have mounted the same data at
// /library, and one app's table applied to another app's files would move a path
// that app never had (internal/migrate/plan).
func (d *discovery) pathRules(in *instance, data *plan.Data) []plan.PathRule {
	// An app stores paths into its own config directory too (Plex's metadata
	// paths, Tautulli's backups, qui's), so the config mount maps to where box
	// puts it. It is the same directory either way, which is a rule and not a
	// gap: apply copies the config into box's config root.
	rules := []plan.PathRule{{From: in.configMount, To: "/config"}}
	seen := map[string]bool{in.configMount: true}
	for _, m := range in.container.Mounts {
		if path.Clean(m.Path) == in.configMount {
			continue
		}
		vol, ok := in.wl.Volumes[m.Volume]
		if !ok {
			continue
		}
		if vol.HostPath == "" {
			// A mount that holds no host directory is not data: an emptyDir's
			// scratch space, a config map, a secret. A persistent volume with
			// none is data box cannot reach, and the operator has to hear
			// about it.
			if vol.Persistent {
				d.rep.Ignored = append(d.rep.Ignored, Ignored{
					App: in.name, Mount: path.Clean(m.Path), Volume: vol.String(),
					Why: "the volume is not a host directory, so nothing on this host holds what the app kept there",
				})
			}
			continue
		}
		rel, ok := trimDir(vol.HostPath, data.Root)
		if !ok {
			d.rep.Ignored = append(d.rep.Ignored, Ignored{
				App: in.name, Mount: path.Clean(m.Path), Volume: vol.String(), Host: vol.HostPath,
				Why: "the host directory is not under the data root " + data.Root,
			})
			continue
		}
		for _, r := range mountRules(path.Clean(m.Path), rel, data) {
			if seen[r.From] {
				// One container path can only be one directory; a second rule
				// for it would leave the winner to the order of the list.
				continue
			}
			seen[r.From] = true
			rules = append(rules, r)
		}
	}
	return rules
}

// mountRules turns one mount into the rules its app needs, given where the host
// directory sits under the data root.
//
// A mount of the root itself is the whole data directory under another name, so
// the app saw both of the contract's directories: the rules name each of them
// under that mount. A mount of one of them, or of something below one, is one
// rule, and its target is what apply's renames leave behind -- a source whose
// downloads are in Downloads/ files under /data/downloads/ once apply has
// renamed the directory.
func mountRules(mountPath, rel string, data *plan.Data) []plan.PathRule {
	if rel == "" {
		return []plan.PathRule{
			{From: path.Join(mountPath, data.Downloads), To: "/data/" + plan.DownloadsDir},
			{From: path.Join(mountPath, data.Media), To: "/data/" + plan.MediaDir},
		}
	}
	return []plan.PathRule{{From: mountPath, To: contractPath(rel, data)}}
}

// contractPath maps a path relative to the data root onto the path the app will
// see, as apply's renames leave it: the downloads directory is called
// /data/downloads whatever the source called it, and everything below it keeps
// its name.
func contractPath(rel string, data *plan.Data) string {
	for _, d := range []struct{ from, to string }{
		{data.Downloads, plan.DownloadsDir},
		{data.Media, plan.MediaDir},
	} {
		if rel == d.from {
			return "/data/" + d.to
		}
		if rest, ok := strings.CutPrefix(rel, d.from+"/"); ok {
			return "/data/" + d.to + "/" + rest
		}
	}
	return "/data/" + rel
}

// hostRules classifies every Service the source's apps reached each other by.
//
// A rule names a Service discovery found, never an app name, so box keeps naming
// no apps: an address is local when it is one of these Services, in any of the
// forms the cluster's DNS answers, and every other address is the operator's to
// classify (docs/box.md, "Only local addresses are rewritten").
//
// Only namespaces a catalog app ran in are considered: a Service elsewhere
// cannot be an address one of them stored by a name that resolves inside its own
// namespace. A Service that selects nothing catalog, or two apps at once, gets
// no rule: apply then lists every stored address that meant it, which is the
// honest consequence of not guessing which app it was.
func (d *discovery) hostRules(svcs []Service) []plan.HostRule {
	namespaces := make(map[string]bool, len(d.instances))
	for _, in := range d.instances {
		namespaces[in.wl.Namespace] = true
	}
	var out []plan.HostRule
	for _, svc := range svcs {
		if !namespaces[svc.Namespace] || len(svc.Selector) == 0 {
			continue
		}
		var picked []*instance
		for _, in := range d.instances {
			if selects(svc.Selector, in.wl.Labels) {
				picked = append(picked, in)
			}
		}
		switch len(picked) {
		case 0:
			// A Service beside the apps that selects none of them is some
			// other part of the install (a database, a proxy), and no rule
			// can name an app for it.
		case 1:
			out = append(out, plan.HostRule{Service: svc.Name + "." + svc.Namespace, App: picked[0].name})
		default:
			names := make([]string, len(picked))
			for i, in := range picked {
				names[i] = in.name
			}
			sort.Strings(names)
			d.note("service %s.%s selects both %s, so no rule maps it and apply will list every address that referred to it",
				svc.Name, svc.Namespace, strings.Join(names, " and "))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].App < out[j].App
	})
	return out
}

// selects reports whether a Service's selector matches a pod's labels. An empty
// selector selects nothing: a Service with none is an ExternalName or one an
// operator points at addresses by hand, and neither is an app.
func selects(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
