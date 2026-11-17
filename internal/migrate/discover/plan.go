package discover

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// plan assembles the whole file: one entry per app with its own path table, the
// data root, the Services, and the workloads left behind.
func (d *discovery) plan(data *plan.Data) *plan.Plan {
	p := &plan.Plan{
		Version: plan.Version,
		// Only the cluster reader exists so far, and it is the one that
		// answered; a docker or swizzin reader adds its kind here. The host
		// and the owner are what a reader has to learn before they can be
		// filled in, so a plan from this box says nothing about them yet.
		Source: &plan.Source{
			Taken: d.now().UTC().Format(time.RFC3339),
			Kinds: []string{plan.Kubernetes},
		},
		Data:  data,
		Apps:  make(map[string]plan.App, len(d.instances)),
		Hosts: d.hostRules(),
	}
	for _, in := range d.instances {
		p.Apps[in.name] = plan.App{
			Kind:      plan.Kubernetes,
			Source:    in.wl.Name,
			Image:     in.container.Image,
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
	// The release mounts the app's scratch directories at the same paths, so a
	// path the app stored under one (Plex's transcoder directory) is a rule onto
	// itself, not a gap the operator has to settle. A source that kept data
	// there under the data root already has its rule above, and that one wins.
	for _, dir := range in.info.scratch {
		if !seen[dir] {
			seen[dir] = true
			rules = append(rules, plan.PathRule{From: dir, To: dir})
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
func (d *discovery) hostRules() []plan.HostRule {
	var out []plan.HostRule
	for _, svc := range d.svcs {
		if !d.inNamespaces(svc.Namespace) || len(svc.Selector) == 0 {
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
			for _, rule := range rules {
				out = append(out, rule)
				out = append(out, clusterIPRules(svc, rule)...)
			}
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
	out = append(out, d.ingressRules()...)
	out = append(out, d.podRules()...)
	sortHostRules(out)
	return out
}

// inNamespaces reports whether a source namespace holds one of the planned apps.
// An object elsewhere cannot be an address one of them stored.
func (d *discovery) inNamespaces(namespace string) bool {
	for _, in := range d.instances {
		if in.wl.Namespace == namespace {
			return true
		}
	}
	return false
}

// clusterIPRules is one Service rule's cluster addresses: an app that stored
// 10.96.0.14:8989 stored sonarr.media's address. A cluster IP outlives the pods
// behind it, so it resolves outright rather than as a hint, and the port is the
// Service port the rule already names, because that is the number an address
// carries.
func clusterIPRules(svc Service, rule plan.HostRule) []plan.HostRule {
	out := make([]plan.HostRule, 0, len(svc.ClusterIPs))
	for _, ip := range svc.ClusterIPs {
		out = append(out, plan.HostRule{
			Host: ip,
			Port: rule.Port,
			App:  rule.App,
			Why: fmt.Sprintf("rewrite to %s: %s:%d is %s.%s's cluster address",
				rule.App, ip, rule.Port, svc.Name, svc.Namespace),
		})
	}
	return out
}

// ingressRules is every public address the source serves one app at: an Ingress
// host and path whose backend Service reaches one app's own port. An app that
// stored https://<name>/qbittorrent for another stored that app's address, so it
// resolves outright, on whatever port it named: an Ingress is reached the way a
// browser reaches it, not on the number the Service forwards to.
//
// A host that serves two apps maps to neither: which one an address meant is
// the path, and a rule matches a host, so guessing would point one app's
// connection at the other.
func (d *discovery) ingressRules() []plan.HostRule {
	// host -> app -> the paths that app is served at, so one host serving one
	// app at several paths is one rule.
	served := map[string]map[string][]string{}
	names := map[string]map[string]string{} // host -> app -> ingress for the note
	for _, ing := range d.ings {
		if !d.inNamespaces(ing.Namespace) {
			continue
		}
		for _, rule := range ing.Rules {
			// A wildcard names a family of hosts rather than an address, and an
			// address an app stored never carries one.
			if rule.Host == "" || strings.Contains(rule.Host, "*") {
				continue
			}
			in, ok := d.ingressOwner(ing, rule)
			if !ok {
				continue
			}
			if served[rule.Host] == nil {
				served[rule.Host] = map[string][]string{}
				names[rule.Host] = map[string]string{}
			}
			served[rule.Host][in.name] = append(served[rule.Host][in.name], rule.Path)
			names[rule.Host][in.name] = ing.Name
		}
	}
	var out []plan.HostRule
	for _, host := range sortedKeysOf(served) {
		apps := served[host]
		if len(apps) > 1 {
			which := make([]string, 0, len(apps))
			for app, paths := range apps {
				which = append(which, fmt.Sprintf("%s at %s", app, strings.Join(ingressPlaces(host, paths), ", ")))
			}
			sort.Strings(which)
			d.note("ingress host %s serves %s, so no rule maps it and apply will list every address that referred to it",
				host, strings.Join(which, " and "))
			continue
		}
		for app, paths := range apps {
			out = append(out, plan.HostRule{
				Host: host,
				App:  app,
				Why:  ingressWhy(app, host, paths),
			})
		}
	}
	return out
}

// ingressWhy is one Ingress rule's reason, one line, saying the action: the
// address is rewritten to the app, and the public path it named is why.
func ingressWhy(app, host string, paths []string) string {
	places := ingressPlaces(host, paths)
	if len(places) == 1 {
		return fmt.Sprintf("rewrite to %s: %s is its Ingress path", app, places[0])
	}
	return fmt.Sprintf("rewrite to %s: %s are its Ingress paths", app, strings.Join(places, ", "))
}

// ingressPlaces is a host and its paths as one address each: panel.example.com
// serving /qbittorrent is panel.example.com/qbittorrent, and a host serving /
// is the host on its own.
func ingressPlaces(host string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || p == "/" {
			out = append(out, host)
			continue
		}
		out = append(out, host+"/"+strings.TrimPrefix(p, "/"))
	}
	sort.Strings(out)
	return out
}

// ingressOwner is the app an Ingress rule reaches, when it reaches exactly one:
// the backend Service has to exist, select one app, and forward the port the
// rule names to a port that app listens on. Everything else is reported rather
// than guessed at, because the rule would rewrite an address that was never the
// app's.
func (d *discovery) ingressOwner(ing Ingress, rule IngressRule) (*instance, bool) {
	at := fmt.Sprintf("ingress %s/%s serves %s", ing.Namespace, ing.Name, strings.Join(ingressPlaces(rule.Host, []string{rule.Path}), ""))
	var svc Service
	found := false
	for _, candidate := range d.svcs {
		if candidate.Namespace == ing.Namespace && candidate.Name == rule.Service {
			svc, found = candidate, true
			break
		}
	}
	if !found {
		d.note("%s through service %s, which the source does not have, so no rule maps it", at, rule.Service)
		return nil, false
	}
	var picked []*instance
	for _, in := range d.instances {
		if selects(svc.Selector, in.wl.Labels) {
			picked = append(picked, in)
		}
	}
	switch len(picked) {
	case 0:
		// A Service no catalog app runs behind is the rest of the install (a
		// dashboard, a proxy): no rule can name an app for it.
		return nil, false
	case 1:
	default:
		names := make([]string, len(picked))
		for i, in := range picked {
			names[i] = in.name
		}
		sort.Strings(names)
		d.note("%s through service %s.%s, which selects both %s, so no rule maps it", at, svc.Name, svc.Namespace, strings.Join(names, " and "))
		return nil, false
	}
	in := picked[0]
	port, ok := ingressServicePort(svc, rule)
	if !ok {
		d.note("%s through service %s.%s on port %s, which it does not declare, so no rule maps it",
			at, svc.Name, svc.Namespace, ingressPortName(rule))
		return nil, false
	}
	target, ok := port.target(in.wl)
	if !ok || !hasPort(in.ports, target) {
		d.note("%s through service %s.%s port %s, which forwards to %s, a port %s does not listen on, so no rule maps it",
			at, svc.Name, svc.Namespace, port.String(), describeTarget(port, target, ok), in.name)
		return nil, false
	}
	return in, true
}

// ingressServicePort is the Service port an Ingress rule's backend names, by
// number or by the name the Service declares it under.
func ingressServicePort(svc Service, rule IngressRule) (ServicePort, bool) {
	for _, p := range svc.Ports {
		if rule.PortName != "" && p.Name == rule.PortName {
			return p, true
		}
		if rule.PortName == "" && p.Port == rule.Port {
			return p, true
		}
	}
	return ServicePort{}, false
}

// ingressPortName is a rule's backend port as a note names it.
func ingressPortName(rule IngressRule) string {
	if rule.PortName != "" {
		return rule.PortName
	}
	return strconv.Itoa(rule.Port)
}

// describeTarget is what a Service port forwards to, as a note says it.
func describeTarget(port ServicePort, target int, ok bool) string {
	if !ok {
		return ingressPortName(IngressRule{PortName: port.TargetName})
	}
	return strconv.Itoa(target)
}

// podRules is every address a pod of the source answered on, as the hint it is:
// the app that ran there is one box will run, so an address an app stored may be
// that app's, but a pod's address does not survive the move and may belong to
// another pod by now. It is written as a default, with a reason, so apply
// rewrites it and the report says it did -- a default is never silent.
//
// The port is the one the app listened on, because a pod's address is not the
// same thing as the app's own address: a metrics sidecar beside it answers on
// the same IP.
func (d *discovery) podRules() []plan.HostRule {
	var out []plan.HostRule
	for _, pod := range d.pods {
		var owners []*instance
		for _, in := range d.instances {
			if in.wl.Owns(pod) {
				owners = append(owners, in)
			}
		}
		if len(owners) == 0 {
			continue
		}
		if len(owners) > 1 {
			names := make([]string, len(owners))
			for i, in := range owners {
				names[i] = in.name
			}
			sort.Strings(names)
			d.note("pod %s/%s matches both %s, so no rule maps its address and apply will list every address that referred to it",
				pod.Namespace, pod.Name, strings.Join(names, " and "))
			continue
		}
		in := owners[0]
		for _, ip := range pod.IPs {
			for _, port := range in.ports {
				out = append(out, plan.HostRule{
					Host: ip,
					Port: port,
					App:  in.name,
					Why: fmt.Sprintf("rewrite to %s: %s:%d was its pod's address, which does not survive the move and may be another pod's by now",
						in.name, ip, port),
					Default: true,
				})
			}
		}
	}
	return out
}

// sortedKeysOf is a map's keys, sorted, so a plan file reads the same twice.
func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortHostRules is the order a plan file is written in: Services first, then the
// hosts, each by name, with the app and port breaking a tie.
func sortHostRules(rules []plan.HostRule) {
	sort.Slice(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		switch {
		case a.Service != b.Service:
			return a.Service < b.Service
		case a.Host != b.Host:
			return a.Host < b.Host
		case a.App != b.App:
			return a.App < b.App
		}
		return a.Port < b.Port
	})
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
