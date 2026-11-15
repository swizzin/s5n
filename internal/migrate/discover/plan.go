package discover

import (
	"fmt"
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

// hostRules classifies every address the source's apps reached each other by.
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
//
// A Service that selects one app gets one rule per port that reaches it, because
// selecting a pod is not the same as being an address of the app's own. An
// install commonly runs a metrics Service beside an app's pod, forwarding to a
// sidecar's port, and a rule for that would rewrite an address that never was
// the app's -- a stored plex-exporter:9000 would become Plex's web endpoint.
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
			in := picked[0]
			rules := reachingRules(svc, in)
			if len(rules) == 0 {
				d.note("service %s.%s selects %s, but %s, so no rule maps it and apply will list every address that referred to it",
					svc.Name, svc.Namespace, in.name, whyNoRule(svc, in))
				continue
			}
			out = append(out, rules...)
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
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// reachingRules is the rules one Service that selects one app earns: one per
// Service port whose target port is one the app listens on.
//
// The rule's port is the Service's own, because that is the number the address
// an app stored carries; a Service on 80 forwarding to 8080 is reached as :80,
// and no app ever stored 8080 for it.
func reachingRules(svc Service, in *instance) []plan.HostRule {
	var out []plan.HostRule
	for _, sp := range svc.Ports {
		target, ok := sp.target(in.wl)
		if !ok || !hasPort(in.ports, target) {
			continue
		}
		out = append(out, plan.HostRule{Service: svc.Name + "." + svc.Namespace, App: in.name, Port: sp.Port})
	}
	return out
}

// whyNoRule says why a Service that selects one app earns no rule for it.
func whyNoRule(svc Service, in *instance) string {
	if len(svc.Ports) == 0 {
		return "it declares no ports"
	}
	return fmt.Sprintf("none of its ports (%s) forwards to a port %s listens on (%s)",
		portList(svc.Ports), in.name, joinInts(in.ports))
}

// portList renders a Service's ports the way a note says them: what each
// answers on, and what it forwards to.
func portList(ports []ServicePort) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// hasPort reports whether ports holds p.
func hasPort(ports []int, p int) bool {
	for _, got := range ports {
		if got == p {
			return true
		}
	}
	return false
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
