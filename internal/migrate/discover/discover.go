// Package discover reads an install box did not make and works out the plan
// `box migrate` applies.
//
// Discovery is migrate's read half, and it changes nothing: the source cluster
// is only ever got and listed, each app's config directory is read through a
// read-only view of the host's filesystem, and a database is copied before it
// is opened so the running app keeps its own file. What discovery works out
// goes into a plan.Plan -- which catalog apps the source ran and what box will
// call them, where their config came from, the namespaces and ports, the path
// table per app and the Services the apps reached each other by -- and
// everything else it saw is in the Report, so the operator reads one page
// before apply writes one byte (docs/box.md, "migrate").
//
// Nothing here decides an app-specific rewrite: the rewriters do that, on the
// staged copy, against the plan this package writes. What discovery owns is
// where things were, which is the part the stopped source will still answer
// when apply runs.
package discover

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/swizzin/s5n/internal/admission"
	"github.com/swizzin/s5n/internal/migrate/plan"
)

// The workload kinds discovery reads, by the name the object carries. A kind is
// on Workload because everything after the read is kind-neutral -- an app is an
// app wherever it ran -- and what the kind decides is which pods are the
// workload's own.
const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
	kindReplicaSet  = "ReplicaSet"
	kindPod         = "Pod"
)

// Workload is one pod template the source runs and nothing above it owns, or a
// bare Pod, flattened to what discovery reads: the pod template's labels, which
// a Service selects by, its containers, and the volumes their mounts name.
//
// The kind is on it because discovery reads every one: a Deployment, a
// StatefulSet, a DaemonSet, a ReplicaSet nothing owns, and a Pod nothing owns.
// Everything after the read is kind-neutral -- an app is an app wherever it ran.
type Workload struct {
	Namespace string
	Name      string
	// Kind is the object's Kubernetes kind: kindDeployment, kindStatefulSet,
	// kindDaemonSet, kindReplicaSet or kindPod.
	Kind   string
	Labels map[string]string
	// Selector is what this workload's pods carry, which is how a pod the
	// source still holds is known to be one of its replicas. For a bare Pod it
	// is the pod's own labels, because that pod is itself.
	Selector map[string]string
	// Replicas is how many replicas the workload asks for, -1 for a kind with
	// no count of its own: a DaemonSet runs one pod per node, and a bare Pod is
	// the pod. With the pods the source still holds (Owns), it is what says a
	// workload is stopped.
	Replicas int
	// RunAsUser and RunAsGroup are the pod's securityContext as the source
	// wrote it, -1 when it pins neither. RunAsGroup is the pod's runAsGroup, or
	// its fsGroup when that is unset: the group a container is given is the one
	// it writes files as.
	RunAsUser  int
	RunAsGroup int
	Containers []Container
	Volumes    map[string]Volume
}

// Owns reports whether one of the source's pods is a replica of this workload:
// a pod carrying its selector in its namespace, or, for a bare Pod, that pod
// itself. A pod that is terminating is still a pod -- the kubelet has it until
// it is gone -- so a caller proving a workload stopped counts it.
func (wl Workload) Owns(p Pod) bool {
	if p.Namespace != wl.Namespace {
		return false
	}
	if wl.Kind == kindPod {
		return p.Name == wl.Name
	}
	return selects(wl.Selector, p.Labels)
}

// Container is one container of a workload's pod template.
type Container struct {
	Name  string
	Image string
	Env   map[string]string
	Ports []int
	// PortNames are the ports the container declares a name for, which is what
	// a Service's targetPort resolves through.
	PortNames map[string]int
	Mounts    []Mount
	// RunAsUser and RunAsGroup are the container's own securityContext, -1 when
	// it pins neither: a pod-level setting is Workload's.
	RunAsUser  int
	RunAsGroup int
}

// Mount is one of a container's volumeMounts: the volume it reads and where that
// volume appears inside the container.
type Mount struct {
	Volume string
	Path   string
}

// Volume is where one of a workload's volumes keeps its bytes.
type Volume struct {
	// Kind names the volume the way the operator has to think about it:
	// "hostPath", "claim", "emptyDir", "configMap", "secret", "projected",
	// "csi", ... It is what a report prints, and all a non-host volume is.
	Kind string
	// HostPath is the host directory behind the volume, "" when the volume
	// holds no host directory: an emptyDir, a config map, or a claim a
	// provisioner filled.
	HostPath string
	// Claim is the claim the mount went through, when it went through one.
	Claim string
	// Persistent says the volume's contents outlive the pod, so a persistent
	// volume with no host path is data discovery cannot adopt where it is.
	Persistent bool
}

// String describes the volume the way a report reads it.
func (v Volume) String() string {
	if v.Claim != "" {
		return "claim " + v.Claim
	}
	return v.Kind
}

// Service is one Service of the source cluster, flattened to what a host rule
// needs: where it is, what it selects, the ports it answers on and the addresses
// it answers at.
type Service struct {
	Namespace string
	Name      string
	Selector  map[string]string
	// Ports are the Service's own ports. A Service beside an app's pod is not
	// necessarily an address of that app -- a metrics sidecar, the torrent
	// port -- so a rule is made only for the ports that reach the app.
	Ports []ServicePort
	// ClusterIPs are the addresses the Service answers at, its cluster IP and
	// any others the cluster gave it. An app that stored one of these stored an
	// address of this Service, which is why it resolves outright.
	ClusterIPs []string
}

// ServicePort is one port of a Service: the port it answers on, which is what a
// stored address carries, and the container port it forwards to.
type ServicePort struct {
	// Name is what the Service calls this port, which is how an Ingress rule's
	// backend names it. Empty for an unnamed port.
	Name string
	// Port is the Service's port: the number another app stored in an address.
	Port int
	// TargetPort is the container port it forwards to, and TargetName is the
	// name the container declares that port under when the Service names it
	// instead of numbering it. Exactly one is set.
	TargetPort int
	TargetName string
}

// target is the container port this Service port forwards to, as the workload's
// own pod declares it. A name that no container of the pod declares forwards to
// nothing, which is what the kubelet does with it too: a named targetPort is
// looked up in the target pod's container ports
// (k8s.io/api@v0.37.1:core/v1/types.go:6553).
func (p ServicePort) target(wl Workload) (int, bool) {
	if p.TargetName != "" {
		return wl.portNamed(p.TargetName)
	}
	return p.TargetPort, p.TargetPort > 0
}

// String renders a Service port the way a note says it: what it answers on, and
// what it forwards to.
func (p ServicePort) String() string {
	if p.TargetName != "" {
		return fmt.Sprintf("%d->%s", p.Port, p.TargetName)
	}
	return fmt.Sprintf("%d->%d", p.Port, p.TargetPort)
}

// Ingress is one Ingress of the source, flattened to what a host rule needs: the
// public names and paths it serves, and the Service behind each.
type Ingress struct {
	Namespace string
	Name      string
	Rules     []IngressRule
}

// IngressRule is one public address of the source: a host and path an Ingress
// serves, and the Service it hands that path to. An app that stored the address
// reached that Service's app, whatever port it named, because an Ingress is
// reached the way a browser reaches it.
type IngressRule struct {
	Host string
	// Path is the path the rule matches, as the Ingress writes it: "/" for the
	// whole host, "/qbittorrent" for a prefix.
	Path string
	// Service is the name of the backend Service, in the Ingress's namespace.
	Service string
	// Port is the backend port's number, and PortName is the name the Service's
	// own port is declared under, when the Ingress names it that way instead.
	// Exactly one is set, as a ServicePort's target is.
	Port     int
	PortName string
}

// Pod is one pod the source still holds, flattened to the three questions
// discovery asks it: which addresses it answered on -- an address an app stored
// may be one of them -- whether it is on its way out, and, with
// Workload.Owns, whether a workload is stopped.
type Pod struct {
	Namespace string
	Name      string
	Labels    map[string]string
	// IPs are the addresses the pod held, empty for a pod that never ran.
	IPs []string
	// Terminating is a pod with a deletion timestamp. It has not exited yet,
	// so a stop proof counts it as running: the process is still writing.
	Terminating bool
}

// Node is one node of the source cluster, as far as discovery reads it: its
// name, which is the host a single-node source ran on, and the kubelet version,
// which is how a cluster says what installed it (`v1.30.4+k0s.0`,
// `v1.30.4+k3s1`). The version is what makes `box migrate cutover
// --replace-cluster` able to print a teardown for the cluster that is actually
// there instead of one for a distribution it guessed.
type Node struct {
	Name string
	// KubeletVersion is status.nodeInfo.kubeletVersion as the node reports it,
	// build metadata and all. Empty for a node a saved manifest left it out of.
	KubeletVersion string
}

// Source is what discovery reads. There are two: Cluster, over a live install
// read-only through client-go, and Directory, over manifests saved to disk so a
// plan can be rebuilt after the source is gone and a test needs no API server.
//
// Raw is the manifests read, for the plan directory, keyed
// <kind>/<namespace>/<name> (a cluster-scoped object carries no namespace). It
// is the record of what the plan was built from; nothing reads it back.
type Source interface {
	Workloads(ctx context.Context) ([]Workload, error)
	Services(ctx context.Context) ([]Service, error)
	// Ingresses are the source's public addresses, which is how a stored
	// address that named a public host resolves to the app behind it.
	Ingresses(ctx context.Context) ([]Ingress, error)
	// Pods are the pods the source still has, for the two things a workload's
	// template cannot say: the addresses the app answered on, and whether any
	// pod of a workload is left.
	Pods(ctx context.Context) ([]Pod, error)
	// Nodes are the source cluster's nodes. Its error is not fatal: a
	// kubeconfig scoped to a namespace may not read a cluster-scoped object,
	// and the host a single-node source ran on only decorates the plan.
	Nodes(ctx context.Context) ([]Node, error)
	Raw(ctx context.Context) (map[string][]byte, error)
}

// Options is what discovery is told that the source cannot say.
type Options struct {
	// Namespaces limits discovery to these source namespaces; empty means
	// every namespace. A workload outside them is neither migrated nor
	// reported: the operator said it is not part of this migration.
	Namespaces []string
	// Variants is the chart's variants.yaml: the blessed image list a source
	// image is mapped to a variant through (charts/s5n/variants.yaml, read the
	// way internal/admission reads it). Nil when the caller has no chart to
	// hand over, which maps nothing and says so.
	Variants []byte
	// Categories is the directory box's values lay each instance's library out
	// in, by box's instance name (charts/s5n/values.yaml, an instance's
	// `category`). It is what the source's library directories are renamed
	// onto and what every app that stored one is rewritten to. Nil when the
	// caller has no chart: nothing is renamed then, and the report says so.
	Categories map[string]string
	// ConfigFS is the host's filesystem rooted at its /, read-only. Discovery
	// opens each app's config directory and the Arrs' databases through it and
	// writes nothing anywhere. It is required: the mounts name host
	// directories, and without it there is nothing to read them from.
	ConfigFS fs.FS
	// TempDir is where a database is copied before it is opened; empty uses
	// the system's temporary directory. The source's own file is never opened,
	// because a read of a running Arr's database can leave a -shm beside it.
	TempDir string
	// Now is the clock the plan's `source.taken` comes from. Empty means
	// time.Now: a test that compares whole plans hands it one.
	Now func() time.Time
}

// Report is everything the operator should see that the plan does not hold.
type Report struct {
	// Skipped are the source's workloads that are not in the plan, with the
	// reason, and the containers of a migrated app's pod that the chart's pod
	// does not carry. They are also the plan's Outside list, which is what
	// apply prints; the reason is why discovery refused them.
	Skipped []Skipped
	// Ignored are the mounts discovery did not turn into a path rule.
	Ignored []Ignored
	// Notes are the decisions and fallbacks that stop nothing: a port read
	// from the pod because the app's config did not say, a Service that
	// selects two apps, the library found without an Arr to ask.
	Notes []string
	// Raw is the source's manifests as Source.Raw returned them, for the
	// caller to write into the plan directory.
	Raw map[string][]byte
}

// Skipped is one source workload that is not migrated, or one container of a
// migrated app's pod that the chart's pod does not carry.
type Skipped struct {
	Workload string // <namespace>/<name>, or <namespace>/<name>/<container>
	Why      string
}

// Ignored is one mount that is not in the plan: its host directory is not under
// the data root, or a volume backs it that is not a host directory at all.
type Ignored struct {
	App    string // box instance name
	Mount  string // the container directory
	Volume string // the volume, as Volume.String says
	Host   string // the host directory, empty when the volume has none
	Why    string
}

// discovery is one run's state. The port of every method is that the plan, the
// report and the refusals are built from the same read of one source.
type discovery struct {
	opt Options
	rep Report
	// svcs is the source's Services, kept for the fallback that needs them (an
	// app whose config names no port takes the ports a Service that selects
	// only it forwards to) and for the rules a Service's own addresses earn.
	svcs []Service
	// ings are the source's Ingress rules, which turn a public address into an
	// app and give the plan its domain default.
	ings []Ingress
	// pods are the pods the source still holds: their addresses are hints, and
	// a workload's pods are what says it is stopped.
	pods []Pod
	// nodes are the source cluster's nodes, and nodeErr why there are none:
	// the host the source ran on is a single node's name, and the distribution
	// that installed it is the marker in its kubelet's version.
	nodes   []Node
	nodeErr error
	// served are the Ingress hosts that reach a planned app, which the plan's
	// domain default is read from.
	served []string
	// variants is the chart's blessed image list, and variantsRead says the
	// question has been answered once (nil when there is no chart to read).
	variants     map[string]map[string]admission.Image
	variantsRead bool

	instances []*instance
}

// Discover reads a source and returns the plan that migrates it, with what it
// could not settle in the report. Nothing is written anywhere, here or by the
// filesystem it reads.
//
// An error means no plan: the caller gets the report as far as it got, and the
// message names the workload, directory or app that stopped it. A source
// discovery cannot map onto the chart's data contract is refused here rather
// than half-migrated, which is why the checks name the move the operator has to
// make by hand (docs/box.md, "Data is adopted by renaming, never by moving").
func Discover(ctx context.Context, src Source, opt Options) (*plan.Plan, Report, error) {
	d := &discovery{opt: opt}
	if src == nil {
		return nil, d.rep, errors.New("discover: no source to read")
	}
	if opt.ConfigFS == nil {
		return nil, d.rep, errors.New("discover: no config filesystem: the apps' config directories and databases are read through one rooted at the host's /")
	}
	wls, err := src.Workloads(ctx)
	if err != nil {
		return nil, d.rep, fmt.Errorf("read the source's workloads: %w", err)
	}
	svcs, err := src.Services(ctx)
	if err != nil {
		return nil, d.rep, fmt.Errorf("read the source's services: %w", err)
	}
	d.svcs = svcs
	ings, err := src.Ingresses(ctx)
	if err != nil {
		return nil, d.rep, fmt.Errorf("read the source's ingress rules: %w", err)
	}
	d.ings = ings
	pods, err := src.Pods(ctx)
	if err != nil {
		return nil, d.rep, fmt.Errorf("read the source's pods: %w", err)
	}
	d.pods = pods
	// The node list is the one read discovery can do without: a kubeconfig
	// scoped to a namespace may not read a cluster-scoped object, and the host
	// only decorates the plan, so its failure is a line in the report.
	if nodes, err := src.Nodes(ctx); err != nil {
		d.nodeErr = err
	} else {
		d.nodes = nodes
	}
	if err := d.classify(wls); err != nil {
		return nil, d.rep, err
	}
	if len(d.instances) == 0 {
		return nil, d.rep, errors.New("no catalog app runs in the source: discovery found no workload whose image is one of the apps box installs")
	}
	for _, in := range d.instances {
		if err := d.readApp(in); err != nil {
			return nil, d.rep, err
		}
	}
	data, err := d.findData(ctx)
	if err != nil {
		return nil, d.rep, err
	}
	p := d.plan(data)
	raw, err := src.Raw(ctx)
	if err != nil {
		return nil, d.rep, fmt.Errorf("read the source's manifests: %w", err)
	}
	d.rep.Raw = raw
	return p, d.rep, nil
}

// note records something the operator should know that stops nothing.
func (d *discovery) note(format string, args ...any) {
	d.rep.Notes = append(d.rep.Notes, fmt.Sprintf(format, args...))
}

// now is when the plan says the source was read.
func (d *discovery) now() time.Time {
	if d.opt.Now != nil {
		return d.opt.Now()
	}
	return time.Now()
}

// inScope says whether a workload is part of this migration: an operator who
// named namespaces meant the rest of the cluster is not this seedbox. It filters
// what a saved directory holds, because a directory is every namespace it was
// saved from; the cluster source also lists only what those namespaces allow.
func (d *discovery) inScope(wl Workload) bool {
	if len(d.opt.Namespaces) == 0 {
		return true
	}
	for _, ns := range d.opt.Namespaces {
		if ns == wl.Namespace {
			return true
		}
	}
	return false
}

// instance is one catalog app as the source ran it.
type instance struct {
	// name is box's instance name: the key in the plan, and what every
	// rewriter is handed.
	name string
	// app is the catalog app it is an instance of.
	app string
	// info is that app's entry in the catalog.
	info appInfo
	// wl is the source workload.
	wl Workload
	// container is the workload's container that runs the app.
	container Container
	// configMount is the container directory the app's config is mounted at,
	// and configDir the host directory behind it.
	configMount string
	configDir   string
	// ports are the ports the app listened on in the source.
	ports []int
	// downloadCategory is the qBittorrent category this app's own download
	// client uses, read from its database: the directory its torrents are in,
	// which a migrated box keeps.
	downloadCategory string
}

// classify decides which workloads are catalog apps, and what box will call
// each one. Workloads it does not migrate go to the report and the plan's
// Outside list: an install commonly runs something box has no app for, and
// migrating half of it silently is the failure this list exists to prevent. The
// containers of a migrated app's own pod that the chart's pod cannot carry are
// listed there too, for the same reason.
func (d *discovery) classify(wls []Workload) error {
	byName := make(map[string]*instance, len(wls))
	for _, wl := range wls {
		if !d.inScope(wl) {
			continue
		}
		in, why, err := classifyWorkload(wl)
		if err != nil {
			return err
		}
		if in == nil {
			d.rep.Skipped = append(d.rep.Skipped, Skipped{Workload: workloadID(wl), Why: why})
			continue
		}
		if prev, dup := byName[in.name]; dup {
			return fmt.Errorf("two workloads are both box's %s instance: %s and %s; migrate one seedbox, so the second has to be removed or renamed",
				in.name, workloadID(prev.wl), workloadID(wl))
		}
		byName[in.name] = in
		d.sidecars(wl, in)
	}
	for _, in := range byName {
		d.instances = append(d.instances, in)
	}
	sort.Slice(d.instances, func(i, j int) bool { return d.instances[i].name < d.instances[j].name })
	sort.Slice(d.rep.Skipped, func(i, j int) bool { return d.rep.Skipped[i].Workload < d.rep.Skipped[j].Workload })
	return nil
}

// classifyWorkload maps one workload onto a catalog app and box's instance
// name, or says why it is not one. An error is two apps in one pod, which no
// instance name can express.
func classifyWorkload(wl Workload) (*instance, string, error) {
	app, container, other, ok := wl.catalogContainer()
	if !ok {
		return nil, fmt.Sprintf("no container runs an image the catalog knows: %s", strings.Join(wl.images(), ", ")), nil
	}
	if other != "" {
		return nil, "", fmt.Errorf("workload %s runs two catalog apps (%s and %s): one workload is one app, so split it first", workloadID(wl), app, other)
	}
	info := catalogApps[app]
	name, ok := instanceName(app, info, wl.Name)
	if !ok {
		return nil, fmt.Sprintf("%s takes no second instance, and %q is not its name", app, wl.Name), nil
	}
	return &instance{name: name, app: app, info: info, wl: wl, container: container}, "", nil
}

// catalogContainer is the container whose image is a catalog app, and the other
// app a container of the same workload runs if there is one.
func (wl Workload) catalogContainer() (app string, c Container, other string, ok bool) {
	for _, ct := range wl.Containers {
		name, found := imageApp(ct.Image)
		if !found {
			continue
		}
		if !ok {
			app, c, ok = name, ct, true
			continue
		}
		if name != app {
			return app, c, name, true
		}
	}
	return app, c, "", ok
}

// sidecars records the pod's other containers. The chart writes one pod spec and
// it carries the app alone, so a second container -- a real source ran a metrics
// exporter beside Plex -- has no home after the migration. It is listed as
// <namespace>/<workload>/<container> so the operator can find what has to come
// back by hand, and the reason names the image discovery dropped.
func (d *discovery) sidecars(wl Workload, in *instance) {
	for _, c := range wl.Containers {
		if c.Name == in.container.Name {
			continue
		}
		d.rep.Skipped = append(d.rep.Skipped, Skipped{
			Workload: workloadID(wl) + "/" + c.Name,
			Why: fmt.Sprintf("its pod's %s container (image %s) is not the app, and the chart's pod carries only %s",
				c.Name, c.Image, in.app),
		})
	}
}

// workloadID is a workload as a message names it.
func workloadID(wl Workload) string { return wl.Namespace + "/" + wl.Name }

// images is the workload's container images, for a report that has to say what
// it did not recognise.
func (wl Workload) images() []string {
	out := make([]string, 0, len(wl.Containers))
	for _, c := range wl.Containers {
		out = append(out, c.Image)
	}
	return out
}

// trimDir returns what p has after dir when dir covers it whole at a segment
// boundary, and false otherwise. It is the plan's rule: /downloads2 is not
// under /downloads, and mapping it as though it were would point an app at a
// directory holding nothing it knows.
func trimDir(p, dir string) (string, bool) {
	p, dir = path.Clean(p), path.Clean(dir)
	if dir == "/" {
		if p == "/" {
			return "", true
		}
		return p, true
	}
	if p == dir {
		return "", true
	}
	if rest, ok := strings.CutPrefix(p, dir+"/"); ok {
		return rest, true
	}
	return "", false
}

// readConfigFile reads one file under a host directory through the config
// filesystem, which is rooted at the host's /. ok is false when the file is
// absent, which is a normal state for an app that has never started.
func readConfigFile(fsys fs.FS, dir, name string) ([]byte, bool, error) {
	p := strings.TrimPrefix(path.Join(dir, name), "/")
	if p == "" {
		return nil, false, nil
	}
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// mountAt returns the container's mount whose path is dir, cleaned.
func (c Container) mountAt(dir string) (Mount, bool) {
	for _, m := range c.Mounts {
		if path.Clean(m.Path) == path.Clean(dir) {
			return m, true
		}
	}
	return Mount{}, false
}

// portNamed is a port of the workload's pod by the name its container declares
// it under. Every container is searched and not just the app's, because a
// Service's targetPort may name a sidecar's port: that Service then forwards to
// the sidecar, which is exactly what a rule must not call the app's address.
func (wl Workload) portNamed(name string) (int, bool) {
	for _, c := range wl.Containers {
		if p, ok := c.PortNames[name]; ok {
			return p, true
		}
	}
	return 0, false
}

// hostDirOf maps a path inside the container onto the host directory that holds
// it: the deepest mount covering it, and whatever the path adds below that
// mount. A path no host-backed mount covers has no host directory, which is the
// answer the callers refuse on.
func (wl Workload) hostDirOf(c Container, containerPath string) (string, bool) {
	best := -1
	host := ""
	for _, m := range c.Mounts {
		vol, ok := wl.Volumes[m.Volume]
		if !ok || vol.HostPath == "" {
			continue
		}
		rest, ok := trimDir(containerPath, m.Path)
		if !ok || len(path.Clean(m.Path)) <= best {
			continue
		}
		best = len(path.Clean(m.Path))
		host = path.Join(vol.HostPath, rest)
	}
	return host, best >= 0
}
