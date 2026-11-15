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

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// Workload is one Deployment or StatefulSet, flattened to what discovery reads:
// the pod template's labels, which a Service selects by, its containers, and the
// volumes their mounts name.
type Workload struct {
	Namespace  string
	Name       string
	Kind       string // "Deployment" or "StatefulSet"
	Labels     map[string]string
	Containers []Container
	Volumes    map[string]Volume
}

// Container is one container of a workload's pod template.
type Container struct {
	Name   string
	Image  string
	Env    map[string]string
	Ports  []int
	Mounts []Mount
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
// needs: where it is and what it selects.
type Service struct {
	Namespace string
	Name      string
	Selector  map[string]string
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
	Raw(ctx context.Context) (map[string][]byte, error)
}

// Options is what discovery is told that the source cannot say.
type Options struct {
	// Namespaces limits discovery to these source namespaces; empty means
	// every namespace. A workload outside them is neither migrated nor
	// reported: the operator said it is not part of this migration.
	Namespaces []string
	// ConfigFS is the host's filesystem rooted at its /, read-only. Discovery
	// opens each app's config directory and the Arrs' databases through it and
	// writes nothing anywhere. It is required: the mounts name host
	// directories, and without it there is nothing to read them from.
	ConfigFS fs.FS
	// TempDir is where a database is copied before it is opened; empty uses
	// the system's temporary directory. The source's own file is never opened,
	// because a read of a running Arr's database can leave a -shm beside it.
	TempDir string
}

// Report is everything the operator should see that the plan does not hold.
type Report struct {
	// Skipped are the source's workloads that are not in the plan, with the
	// reason. They are also the plan's Outside list, which is what apply
	// prints; the reason is why discovery refused them.
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

// Skipped is one source workload that is not migrated.
type Skipped struct {
	Workload string // <namespace>/<name>
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
	opt       Options
	rep       Report
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
	p := d.plan(data, svcs)
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
}

// classify decides which workloads are catalog apps, and what box will call
// each one. Workloads it does not migrate go to the report and the plan's
// Outside list: an install commonly runs something box has no app for, and
// migrating half of it silently is the failure this list exists to prevent.
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
