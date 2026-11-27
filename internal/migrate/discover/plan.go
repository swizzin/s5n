package discover

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/migrate/plan"
)

// plan assembles the file: apps with their path tables, the data root, addresses
// and leftovers.
func (d *discovery) plan(data *plan.Data) *plan.Plan {
	// Host rules come first: the Ingress names are the domain box must answer to.
	hosts := d.hostRules()
	p := &plan.Plan{
		Version: plan.Version,
		// Only the cluster reader exists; another reader adds its kind here.
		Source: &plan.Source{
			Taken: d.now().UTC().Format(time.RFC3339),
			Kinds: []string{plan.Kubernetes},
			Host:  d.sourceHost(),
			Owner: d.owner(),
			// The scope asked for: half of what makes two reads one source, which
			// cutover matches on.
			Namespaces: sortedNamespaces(d.opt.Namespaces),
		},
		Domain: d.domain(),
		Data:   data,
		Apps:   make(map[string]plan.App, len(d.instances)),
		Hosts:  hosts,
	}
	variants := d.imageVariants()
	for _, in := range d.instances {
		p.Apps[in.name] = plan.App{
			Kind:             plan.Kubernetes,
			Source:           in.wl.Name,
			Image:            in.container.Image,
			Variant:          d.variant(in, variants),
			Config:           in.configDir,
			Namespace:        in.wl.Namespace,
			Ports:            in.ports,
			Category:         d.categoryOf(in.name),
			DownloadCategory: in.downloadCategory,
			MediaServer:      in.mediaServer,
			Paths:            d.pathRules(in, data),
		}
	}
	for _, s := range d.rep.Skipped {
		p.Outside = append(p.Outside, s.Workload)
	}
	sort.Strings(p.Outside)
	return p
}

// sortedNamespaces is --namespace deduped and sorted: the operator's order must
// not make two reads of one source differ.
func sortedNamespaces(list []string) []string {
	seen := make(map[string]bool, len(list))
	var out []string
	for _, ns := range list {
		name := strings.TrimSpace(ns)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sourceHost is the single node's name; several nodes, or none readable, name no
// host rather than picking one that may hold none of the data.
func (d *discovery) sourceHost() string {
	switch {
	case len(d.nodes) == 1:
		return d.nodes[0].Name
	case len(d.nodes) > 1:
		d.note("the source cluster runs on %d nodes (%s), so the plan names no source host",
			len(d.nodes), strings.Join(nodeNames(d.nodes), ", "))
	case d.nodeErr != nil:
		d.note("the source's nodes could not be read (%v), so the plan names no source host", d.nodeErr)
	}
	return ""
}

// owner is the uid:gid every planned app agrees on; disagreement or silence names
// none and leaves the question to the run.
func (d *discovery) owner() *plan.Owner {
	type appUser struct {
		name     string
		uid, gid int
	}
	users := make([]appUser, 0, len(d.instances))
	for _, in := range d.instances {
		uid, gid, ok := appIDs(in)
		if !ok {
			d.note("%s's pod names no uid:gid (no PUID/PGID in its environment and no securityContext), so the plan names no owner", in.name)
			return nil
		}
		if uid == 0 {
			// box never runs an app as root, so root's files are not adoptable.
			d.note("%s's pod runs as root, which box does not, so the plan names no owner", in.name)
			return nil
		}
		users = append(users, appUser{name: in.name, uid: uid, gid: gid})
	}
	if len(users) == 0 {
		return nil
	}
	for _, u := range users[1:] {
		if u.uid == users[0].uid && u.gid == users[0].gid {
			continue
		}
		said := make([]string, len(users))
		for i, u := range users {
			said[i] = fmt.Sprintf("%s as %d:%d", u.name, u.uid, u.gid)
		}
		d.note("the source's apps run as different users (%s), so the plan names no owner; the run asks for one", strings.Join(said, ", "))
		return nil
	}
	return &plan.Owner{UID: users[0].uid, GID: users[0].gid}
}

// appIDs is one app's uid:gid: the container's securityContext, then the pod's,
// then PUID/PGID, the only place a linuxserver source says it.
func appIDs(in *instance) (uid, gid int, ok bool) {
	uid, gid = in.container.RunAsUser, in.container.RunAsGroup
	if uid < 0 {
		uid = in.wl.RunAsUser
	}
	if gid < 0 {
		gid = in.wl.RunAsGroup
	}
	if uid >= 0 && gid >= 0 {
		return uid, gid, true
	}
	if u, ok := envID(in.container.Env, "PUID"); ok {
		if g, ok := envID(in.container.Env, "PGID"); ok {
			return u, g, true
		}
	}
	return uid, gid, false
}

// envID reads one uid or gid out of an environment variable.
func envID(env map[string]string, name string) (int, bool) {
	v, ok := env[name]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// pathRules is one app's table. It is per app because each mounted the same data
// differently, and one app's table on another's files would move a path it never
// had (internal/migrate/plan).
func (d *discovery) pathRules(in *instance, data *plan.Data) []plan.PathRule {
	// The config mount maps to box's /config: the same directory, a rule rather
	// than a gap.
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
			// No host directory is not data -- scratch, a config map, a secret --
			// unless it is persistent, which is data box cannot reach.
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
				// One container path is one directory; a second rule would leave
				// the winner to the list's order.
				continue
			}
			seen[r.From] = true
			rules = append(rules, r)
		}
	}
	// Scratch directories are mounted at the same paths in the release, so a stored
	// one is a rule onto itself, not a gap; a data rule above wins.
	for _, dir := range in.info.scratch {
		if !seen[dir] {
			seen[dir] = true
			rules = append(rules, plan.PathRule{From: dir, To: dir})
		}
	}
	return rules
}

// mountRules turns one mount into its rules. A mount of the data root names both
func mountRules(mountPath, rel string, data *plan.Data) []plan.PathRule {
	if rel == "" {
		return append([]plan.PathRule{
			{From: path.Join(mountPath, data.Downloads), To: "/data/" + plan.DownloadsDir},
			{From: path.Join(mountPath, data.Media), To: "/data/" + plan.MediaDir},
		}, folderRules(path.Join(mountPath, data.Media), data)...)
	}
	if rel == data.Media {
		return append([]plan.PathRule{
			{From: mountPath, To: "/data/" + plan.MediaDir},
		}, folderRules(mountPath, data)...)
	}
	return []plan.PathRule{{From: mountPath, To: contractPath(rel, data)}}
}

// folderRules is every library directory under mediaPath, mapped onto the
func folderRules(mediaPath string, data *plan.Data) []plan.PathRule {
	out := make([]plan.PathRule, 0, len(data.Folders))
	for _, f := range data.Folders {
		if f.From == f.To {
			continue
		}
		out = append(out, plan.PathRule{From: path.Join(mediaPath, f.From), To: path.Join("/data/"+plan.MediaDir, f.To)})
	}
	return out
}

// contractPath maps a path relative to the data root onto the path the app will
func contractPath(rel string, data *plan.Data) string {
	for _, d := range []struct{ from, to string }{
		{data.Downloads, plan.DownloadsDir},
		{data.Media, plan.MediaDir},
	} {
		if rel == d.from {
			return "/data/" + d.to
		}
		if rest, ok := strings.CutPrefix(rel, d.from+"/"); ok {
			return "/data/" + d.to + folderRest(rest, data)
		}
	}
	return "/data/" + rel
}

// folderRest maps what is left of a path under the media directory onto the
func folderRest(rest string, data *plan.Data) string {
	for _, f := range data.Folders {
		if rest == f.From {
			return "/" + f.To
		}
		if tail, ok := strings.CutPrefix(rest, f.From+"/"); ok {
			return "/" + f.To + "/" + tail
		}
	}
	return "/" + rest
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
// Service port the rule already names, because that is the number an address
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
// A host that serves one app gets one host-only rule, which answers every path
// on it. A host that serves several -- box's own shape is one domain with each
// app under a path -- gets one rule per path, because the path is then the only
// thing that says which app an address meant. A path two apps are routed at is
// one address with two answers, so neither is a rule and the operator hears
// about it.
func (d *discovery) ingressRules() []plan.HostRule {
	// host -> app -> the paths that app is served at, so one host serving one
	// app at several paths is one rule.
	served := map[string]map[string][]string{}
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
			}
			served[rule.Host][in.name] = append(served[rule.Host][in.name], rule.Path)
		}
	}
	var out []plan.HostRule
	for _, host := range sortedKeysOf(served) {
		apps := served[host]
		if len(apps) == 1 {
			for app, paths := range apps {
				out = append(out, plan.HostRule{
					Host: host,
					App:  app,
					Why:  ingressWhy(app, host, paths),
				})
			}
			continue
		}
		out = append(out, d.ingressPathRules(host, apps)...)
	}
	// The same read gives the plan its domain default: the names this seedbox
	// answers to are the ones the source's own Ingress answers to.
	d.served = sortedKeysOf(served)
	return out
}

// ingressPathRules is one host that serves several apps, as one rule per path. The path
// is box's own shape -- each app under its own on one name -- so it is what a
// stored address on that host means. A path two apps are routed at maps to
// neither: the address has two answers, which is the operator's to settle.
func (d *discovery) ingressPathRules(host string, apps map[string][]string) []plan.HostRule {
	claims := map[string][]string{}
	for _, app := range sortedKeysOf(apps) {
		for _, path := range apps[app] {
			p := ingressPath(path)
			if owners := claims[p]; len(owners) == 0 || owners[len(owners)-1] != app {
				claims[p] = append(owners, app)
			}
		}
	}
	var out []plan.HostRule
	for _, p := range sortedKeysOf(claims) {
		owners := claims[p]
		if len(owners) > 1 {
			d.note("ingress host %s routes %s to %s, so no rule maps it and apply will list every address that referred to it",
				host, ingressPlaces(host, []string{p})[0], strings.Join(owners, " and "))
			continue
		}
		out = append(out, plan.HostRule{
			Host: host,
			Path: p,
			App:  owners[0],
			Why:  ingressWhy(owners[0], host, []string{p}),
		})
	}
	return out
}

// ingressPath is an Ingress path as a rule carries it: one prefix starting with
func ingressPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// ingressWhy is one Ingress rule's reason, one line, saying the action: the
func ingressWhy(app, host string, paths []string) string {
	places := ingressPlaces(host, paths)
	if len(places) == 1 {
		return fmt.Sprintf("rewrite to %s: %s is its Ingress path", app, places[0])
	}
	return fmt.Sprintf("rewrite to %s: %s are its Ingress paths", app, strings.Join(places, ", "))
}

// ingressPlaces is a host and its paths as one address each: panel.example.com
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
	target, reached := port.target(in.wl)
	if !reached || !hasPort(in.ports, target) {
		d.note("%s through service %s.%s port %s, which forwards to %s, a port %s does not listen on, so no rule maps it",
			at, svc.Name, svc.Namespace, port.String(), forwarded(port, target, reached), in.name)
		return nil, false
	}
	return in, true
}

// ingressServicePort is the Service port an Ingress rule's backend names, by
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

// forwarded is what a Service port forwards to, as a note says it: the name it
func forwarded(port ServicePort, target int, reached bool) string {
	if !reached {
		return port.TargetName
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

// domain is the plan's DNS name default: what the Ingress hosts the source
// serves its apps at share. One name is the domain -- box serves the apps under
// paths on it, as the source did -- and several names give the suffix they
// share. It is a default for the run's question and never a question discovery
// asks, so a source whose names share none, or whose one name is not a domain
// box can serve, names none, and the run asks.
func (d *discovery) domain() string {
	domain := domainFrom(d.served)
	switch {
	case domain != "" || len(d.served) == 0:
		return domain
	case len(d.served) == 1:
		d.note("the source's Ingress serves %s, which is not a DNS name box can serve: the plan names none and the run asks for it", d.served[0])
	default:
		d.note("the source's Ingress serves %s, which share no domain: the plan names none and the run asks for it", strings.Join(d.served, ", "))
	}
	return ""
}

// domainFrom is the DNS name every one of these hosts shares, when they share
// that name is what the seedbox is served under. A single name that is not a
func domainFrom(hosts []string) string {
	var names []string
	wildcards := 0
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if rest, ok := strings.CutPrefix(host, "*."); ok {
			wildcards++
			host = rest
		}
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		names = append(names, host)
	}
	if len(names) == 1 {
		if plan.IsDNSName(names[0]) {
			return names[0]
		}
		return ""
	}
	common := commonSuffix(names)
	if len(common) < 2 {
		return ""
	}
	if wildcards == 0 {
		// The shared part has to leave a name above a host: if every host is
		// a domain over the hosts.
		above := false
		for _, name := range names {
			if len(strings.Split(name, ".")) > len(common) {
				above = true
			}
		}
		if !above {
			return ""
		}
	}
	return strings.Join(common, ".")
}

// commonSuffix is the longest run of trailing labels every name ends in.
func commonSuffix(names []string) []string {
	var common []string
	for _, name := range names {
		labels := strings.Split(name, ".")
		if common == nil {
			common = labels
			continue
		}
		n := 0
		for n < len(common) && n < len(labels) && labels[len(labels)-1-n] == common[len(common)-1-n] {
			n++
		}
		common = common[len(common)-n:]
	}
	return common
}

// imageVariants is the chart's variants.yaml as its image table, parsed the one
// way internal/admission parses it. Nil when the caller gave discovery no chart,
// or a file box cannot read: nothing is mapped to a variant then, and the report
// says so once.
func (d *discovery) imageVariants() map[string]map[string]admission.Image {
	if d.variantsRead {
		return d.variants
	}
	d.variantsRead = true
	if len(d.opt.Variants) == 0 {
		d.note("box was given no chart variants, so no source image is mapped to a variant and every app keeps the chart's default (apps.<app>.image.variant names one)")
		return nil
	}
	images, err := admission.Images(d.opt.Variants)
	if err != nil {
		d.note("the chart's variants could not be read (%v), so no source image is mapped to a variant", err)
		return nil
	}
	d.variants = images
	return images
}

// variant is the blessed variant a source image already is, and "" when it is
// none of them.
//
// A source image is only ever mapped onto what the chart ships (docs/box.md,
// "Images come from the blessed variants"): the same repository, and the
// variant inside it that the digest, then the tag, says. The project whose tags
// name a build beside its app's version is read by the tag's name, so a source
// on an older qBittorrent of a build the chart ships is that build. box never
// carries the source's image, so an image none of the chart's variants is keeps
// the chart's default, and the report names the key that would choose another
// one and the list box would accept.
func (d *discovery) variant(in *instance, variants map[string]map[string]admission.Image) string {
	if variants == nil {
		return ""
	}
	byVariant, ok := variants[in.app]
	if !ok || len(byVariant) == 0 {
		d.note("the chart blesses no image for %s, so the plan maps %s onto no variant", in.app, in.container.Image)
		return ""
	}
	repo, tag, digest := splitImageRef(in.container.Image)
	candidates := make([]string, 0, len(byVariant))
	for name, image := range byVariant {
		if image.Repository == repo {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		d.note("%s runs %s, which is not one of the chart's blessed variants, so the chart's default runs: apps.%s.image.variant names one of %s",
			in.name, in.container.Image, in.app, variantList(byVariant))
		return ""
	case 1:
		return candidates[0]
	}
	// Several of the chart's variants are the same repository, which is how one
	if name := named(candidates, byVariant, func(image admission.Image) bool {
		return digest != "" && image.Digest == digest
	}); name != "" {
		return name
	}
	if name := named(candidates, byVariant, func(image admission.Image) bool {
		return tag != "" && image.Tag == tag
	}); name != "" {
		return name
	}
	if repo == libtorrentRepo {
		if name := libtorrentVariant(tag); name != "" {
			if image, ok := byVariant[name]; ok {
				d.variantVersionNote(in, tag, name, image)
				return name
			}
		}
	}
	// Nothing of the source's is one of them, and the note says what box
	d.note("%s runs %s, and no blessed variant for that repository has its tag or digest (%s), so the chart's default runs: apps.%s.image.variant names one of %s",
		in.name, in.container.Image, comparedTags(candidates, byVariant), in.app, variantList(byVariant))
	return ""
}

// named is the variant a picker answers among the chart's variants that share
// as well, because both entries are that image, so the variant's own name wins
func named(candidates []string, byVariant map[string]admission.Image, pick func(admission.Image) bool) string {
	var matching []string
	for _, name := range candidates {
		if pick(byVariant[name]) {
			matching = append(matching, name)
		}
	}
	if len(matching) == 1 {
		return matching[0]
	}
	var own []string
	for _, name := range matching {
		if _, alias := legacyVariants[name]; !alias {
			own = append(own, name)
		}
	}
	if len(own) == 1 {
		return own[0]
	}
	return ""
}

// libtorrentRepo is the repository whose tags name libtorrent's build beside
// qBittorrent's own version (charts/s5n/variants.yaml). It is the only place a
// tag means more than a version, so it is the only place discovery reads one.
const libtorrentRepo = "ghcr.io/swizzin/qbittorrent"

// legacyVariants is the build names the project has since renamed, each mapped
// onto the variant it is today (charts/s5n/variants.yaml). A name is a fact the
// project changed rather than a version, and the image under both names is the
// same build, so a source that ran the old one is the variant. A name that never
// changed needs no entry: the tag's own suffix is the variant's name.
var legacyVariants = map[string]string{
	// The Zen 3 build carried its allocator in its name until every 10G build
	// linked mimalloc, which is why the suffix went away.
	"lt1.2-10g-znver3-mimalloc": "lt1.2-10g-znver3",
}

// libtorrentVariant is the variant a swizzin qBittorrent tag names, whatever
// Empty when the tag does not begin with a version, or names no build at all:
// box does not guess at what a tag it does not know means.
func libtorrentVariant(tag string) string {
	version := qbittorrentVersion(tag)
	if version == "" || len(version) == len(tag) || tag[len(version)] != '-' {
		return ""
	}
	name := tag[len(version)+1:]
	if line, ok := libtorrentLine(name); ok {
		name = line
	}
	if renamed, ok := legacyVariants[name]; ok {
		name = renamed
	}
	return name
}

// qbittorrentVersion is the version a swizzin tag begins with (5.3.1 in
func qbittorrentVersion(tag string) string {
	i := 0
	for i < len(tag) && (tag[i] == '.' || (tag[i] >= '0' && tag[i] <= '9')) {
		i++
	}
	return tag[:i]
}

// libtorrentLine folds libtorrent's patch release back into its line: the build
// 1.2.20 is the 1.2 line, so lt1.2.20-10g names the variant lt1.2-10g, because a
func libtorrentLine(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "lt")
	if !ok {
		return "", false
	}
	end := 0
	for end < len(rest) && (rest[end] == '.' || (rest[end] >= '0' && rest[end] <= '9')) {
		end++
	}
	parts := strings.Split(rest[:end], ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return "lt" + parts[0] + "." + parts[1] + rest[end:], true
}

// variantVersionNote says when the chart's build of a variant is not the
// qBittorrent release the source ran. A variant is the libtorrent line and the
// build profile, so the chart's release is what runs, and that is a version
// change for the migration to read like any other.
func (d *discovery) variantVersionNote(in *instance, tag, name string, image admission.Image) {
	source, box := qbittorrentVersion(tag), qbittorrentVersion(image.Tag)
	if source == "" || box == "" || source == box {
		return
	}
	d.note("%s runs %s, and the chart's %s variant is %s, so the chart's qBittorrent version runs",
		in.name, in.container.Image, name, image.Tag)
}

// comparedTags is the tags of the variants that share the source's repository,
func comparedTags(candidates []string, byVariant map[string]admission.Image) string {
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		image := byVariant[name]
		var said string
		switch {
		case image.Tag != "":
			said = image.Tag
		case image.Digest != "":
			said = image.Digest
		}
		if said == "" || seen[said] {
			continue
		}
		seen[said] = true
		out = append(out, said)
	}
	return strings.Join(out, ", ")
}

// splitImageRef splits an image reference into the repository, its tag and its
func splitImageRef(image string) (repo, tag, digest string) {
	repo = image
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo, digest = repo[:i], repo[i+1:]
	}
	// A colon after the last slash is the tag; before it, it is a registry's
	// port.
	if i := strings.LastIndexByte(repo, ':'); i >= 0 && !strings.Contains(repo[i:], "/") {
		repo, tag = repo[:i], repo[i+1:]
	}
	return repo, tag, digest
}

// variantList is one app's blessed variants as a note says them.
func variantList(byVariant map[string]admission.Image) string {
	names := sortedKeysOf(byVariant)
	out := make([]string, len(names))
	for i, name := range names {
		image := byVariant[name]
		ref := image.Repository
		if image.Tag != "" {
			ref += ":" + image.Tag
		}
		out[i] = fmt.Sprintf("%s (%s)", name, ref)
	}
	return strings.Join(out, ", ")
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
		case a.Port != b.Port:
			return a.Port < b.Port
		}
		return a.Path < b.Path
	})
}

// reachingRules is the rules one Service that selects one app earns: one per
// The rule's port is the Service's own, because that is the number the address
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
