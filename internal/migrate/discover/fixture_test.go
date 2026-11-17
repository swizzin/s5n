package discover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/swizzin/s5n/internal/migrate/plan"
	"github.com/swizzin/s5n/internal/migrate/sqlitedb"
)

// fixture is one source install in memory: the manifests saved to disk, and the
// host's files as discovery reads them. Every path, name and image here is
// invented; the shape is what a real install has.
type fixture struct {
	manifests fstest.MapFS
	files     fstest.MapFS
	next      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{manifests: fstest.MapFS{}, files: fstest.MapFS{}}
}

// mount is one volume of a fixture app: the volume's name, where it appears in
// the container and the host directory behind it.
type mount struct {
	volume, path, host string
}

// appSpec is one fixture app as a pod: one container, one image, the mounts the
// test cares about.
type appSpec struct {
	name      string
	namespace string
	// kind is the object discovery reads it from: a StatefulSet when empty, or
	// a Deployment, a DaemonSet, an ownerless ReplicaSet or a bare Pod.
	kind  string
	image string
	port  int
	// mounts are host-backed volumes; emptyDirs are the ones that hold nothing
	// worth migrating.
	mounts    []mount
	emptyDirs []mount
	// podLabels is what a Service selects by; nil means app.kubernetes.io/name.
	podLabels map[string]string
	// replicas is the count the workload asks for, nil for what the API server
	// defaults (one).
	replicas *int
	// owner makes the object a pod template something above it owns, which is
	// never a workload of its own.
	owner *ownerRef
	// podIP is the address the object answered on, written into a pod's status.
	// A workload's own pods are separate objects (fixture.pod).
	podIP string
}

// ownerRef is the object above a fixture workload.
type ownerRef struct {
	kind, name string
}

// app adds one app workload, shaped like the chart's own StatefulSet unless the
// spec names another kind.
func (f *fixture) app(t *testing.T, spec appSpec) {
	t.Helper()
	if spec.namespace == "" {
		spec.namespace = "media"
	}
	kind := spec.kind
	if kind == "" {
		kind = "StatefulSet"
	}
	labels := spec.podLabels
	if labels == nil {
		labels = map[string]string{"app.kubernetes.io/name": spec.name}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\n",
		groupOf(kind), kind, spec.name, spec.namespace)
	if spec.owner != nil {
		fmt.Fprintf(&b, "  ownerReferences:\n    - apiVersion: apps/v1\n      kind: %s\n      name: %s\n",
			spec.owner.kind, spec.owner.name)
	}
	// A bare Pod is its own template: its labels and its spec are the object's.
	if kind == "Pod" {
		fmt.Fprintf(&b, "  labels:\n%s\n", labelsYAML(labels, "    "))
		b.WriteString("spec:\n")
		f.writePod(&b, "  ", spec)
	} else {
		b.WriteString("spec:\n")
		if kind == "StatefulSet" {
			fmt.Fprintf(&b, "  serviceName: %s\n", spec.name)
		}
		fmt.Fprintf(&b, "  selector:\n    matchLabels:\n%s\n", labelsYAML(labels, "      "))
		if spec.replicas != nil {
			fmt.Fprintf(&b, "  replicas: %d\n", *spec.replicas)
		}
		fmt.Fprintf(&b, "  template:\n    metadata:\n      labels:\n%s\n    spec:\n",
			labelsYAML(labels, "        "))
		f.writePod(&b, "      ", spec)
	}
	if spec.podIP != "" {
		fmt.Fprintf(&b, "status:\n  podIP: %s\n", spec.podIP)
	}
	f.object(t, spec.name, b.String())
}

// writePod writes the containers and volumes of one pod spec at an indentation.
func (f *fixture) writePod(b *strings.Builder, indent string, spec appSpec) {
	fmt.Fprintf(b, "%scontainers:\n%s  - name: %s\n%s    image: %s\n%s    ports:\n%s      - name: http\n%s        containerPort: %d\n%s    volumeMounts:\n",
		indent, indent, spec.name, indent, spec.image, indent, indent, indent, spec.port, indent)
	for _, m := range spec.mounts {
		fmt.Fprintf(b, "%s      - name: %s\n%s        mountPath: %s\n", indent, m.volume, indent, m.path)
	}
	for _, m := range spec.emptyDirs {
		fmt.Fprintf(b, "%s      - name: %s\n%s        mountPath: %s\n", indent, m.volume, indent, m.path)
	}
	fmt.Fprintf(b, "%svolumes:\n", indent)
	for _, m := range spec.mounts {
		fmt.Fprintf(b, "%s  - name: %s\n%s    hostPath:\n%s      path: %s\n%s      type: Directory\n",
			indent, m.volume, indent, indent, m.host, indent)
	}
	for _, m := range spec.emptyDirs {
		fmt.Fprintf(b, "%s  - name: %s\n%s    emptyDir: {}\n", indent, m.volume, indent)
	}
}

// groupOf is the API group an object's kind is in, as a manifest names it.
func groupOf(kind string) string {
	switch kind {
	case "Pod", "Service", "Node":
		return "v1"
	case "Ingress":
		return "networking.k8s.io/v1"
	}
	return "apps/v1"
}

// pod adds one pod the fixture source holds, owned by the workload at owner
// unless owner is nil: an owned pod is never a workload of its own, but its
// address is one discovery reads.
func (f *fixture) pod(t *testing.T, name, namespace string, labels map[string]string, ip string, owner *ownerRef) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
%s
`, name, namespace, labelsYAML(labels, "    "))
	if owner != nil {
		fmt.Fprintf(&b, "  ownerReferences:\n    - apiVersion: apps/v1\n      kind: %s\n      name: %s\n", owner.kind, owner.name)
	}
	fmt.Fprintf(&b, `spec:
  containers:
    - name: %s
      image: example.com/nothing:1
status:
  podIP: %s
`, name, ip)
	f.object(t, name, b.String())
}

// ingress adds one Ingress serving a host and path through a Service.
func (f *fixture) ingress(t *testing.T, name, namespace, host, path, service string) {
	t.Helper()
	f.object(t, "ingress-"+name, fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %s
  namespace: %s
spec:
  rules:
    - host: %s
      http:
        paths:
          - path: %s
            pathType: Prefix
            backend:
              service:
                name: %s
                port:
                  number: 80
`, name, namespace, host, path, service))
}

// node adds one Node, which is how a single-node source names its host.
func (f *fixture) node(t *testing.T, name string) {
	t.Helper()
	f.object(t, "node-"+name, fmt.Sprintf(`apiVersion: v1
kind: Node
metadata:
  name: %s
`, name))
}

// object files one manifest under its own name, so a refusal can name the file
// it came from.
func (f *fixture) object(t *testing.T, name, doc string) {
	t.Helper()
	f.next++
	f.manifests[fmt.Sprintf("manifests/%02d-%s.yaml", f.next, name)] = &fstest.MapFile{Data: []byte(doc)}
}

// servicePort is one port of a fixture Service: the port it answers on and what
// it forwards to, a container port's number or the name it declares.
type servicePort struct {
	port   int
	target string
}

// service adds one Service selecting a fixture app by its pod label, on port 80
// forwarding to the app's named http port.
func (f *fixture) service(t *testing.T, name, namespace, app string) {
	t.Helper()
	f.servicePorts(t, name, namespace, app, servicePort{port: 80, target: "http"})
}

// servicePorts adds one Service selecting a fixture app, on the ports given.
func (f *fixture) servicePorts(t *testing.T, name, namespace, app string, ports ...servicePort) {
	t.Helper()
	var b strings.Builder
	for _, p := range ports {
		fmt.Fprintf(&b, "    - port: %d\n      targetPort: %s\n", p.port, p.target)
	}
	f.object(t, "service-"+name, fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app.kubernetes.io/name: %s
  ports:
%s`, name, namespace, app, b.String()))
}

// file puts one file on the host, where discovery reads it through ConfigFS.
func (f *fixture) file(t *testing.T, hostPath, content string) {
	t.Helper()
	f.bytes(t, hostPath, []byte(content))
}

func (f *fixture) bytes(t *testing.T, hostPath string, content []byte) {
	t.Helper()
	f.files[strings.TrimPrefix(hostPath, "/")] = &fstest.MapFile{Data: content}
}

// discover runs discovery over the fixture and requires it to succeed.
func (f *fixture) discover(t *testing.T) (*plan.Plan, Report) {
	t.Helper()
	p, rep, err := f.tryDiscover(t)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return p, rep
}

// discoverErr runs discovery over the fixture and returns its refusal.
func (f *fixture) discoverErr(t *testing.T) error {
	t.Helper()
	_, _, err := f.tryDiscover(t)
	if err == nil {
		t.Fatal("Discover accepted a source it should have refused")
	}
	return err
}

func (f *fixture) tryDiscover(t *testing.T) (*plan.Plan, Report, error) {
	t.Helper()
	return Discover(context.Background(), NewDirectory(f.manifests), Options{
		ConfigFS: f.files,
		TempDir:  t.TempDir(),
		// The plan records when the source was read; a whole-plan comparison
		// needs the clock to be the test's, not the wall's.
		Now: func() time.Time { return readAt },
	})
}

// readAt is the time every fixture discovery says it read its source: the plan
// is a file, so the one field that would otherwise change every run is fixed.
var readAt = time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)

// arrDB is an Arr database holding only the table discovery reads: a real SQLite
// file, because discovery copies it, checks it and queries it.
func arrDB(t *testing.T, folders ...string) []byte {
	t.Helper()
	name := filepath.Join(t.TempDir(), "app.db")
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		t.Fatalf("create database: %v", err)
	}
	ctx := context.Background()
	db, err := sqlitedb.OpenReadWrite(ctx, name)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE "RootFolders" ("Id" INTEGER PRIMARY KEY, "Path" TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for _, folder := range folders {
		if _, err := db.ExecContext(ctx, `INSERT INTO "RootFolders" ("Path") VALUES (?)`, folder); err != nil {
			t.Fatalf("insert root folder: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	return data
}

// labelsYAML renders a pod's labels at an indentation.
func labelsYAML(labels map[string]string, indent string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s%s: %s", indent, k, labels[k])
	}
	return b.String()
}

// roundTrip writes the plan as the plan file and reads it back, which is what
// apply does with it: a plan discovery cannot write and plan.Parse cannot read
// is not a plan.
func roundTrip(t *testing.T, p *plan.Plan) *plan.Plan {
	t.Helper()
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	parsed, err := plan.Parse(raw)
	if err != nil {
		t.Fatalf("parse the plan back:\n%s\n%v", raw, err)
	}
	if !reflect.DeepEqual(*p, *parsed) {
		t.Fatalf("the plan changed on the way through the file:\nwrote:\n%s\ngot: %#v", raw, *parsed)
	}
	return parsed
}
