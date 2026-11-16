package discover

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

// TestDirectoryReadsEveryDocumentShape: a saved source is whatever `kubectl get
// -o yaml` wrote, which is several documents in one file, a List of objects, or
// JSON. All of them are the same source.
func TestDirectoryReadsEveryDocumentShape(t *testing.T) {
	fsys := fstest.MapFS{
		"save/a.yaml": {Data: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: plex
  namespace: media
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: plex
    spec:
      containers:
        - name: plex
          image: linuxserver/plex:1.41.5
          volumeMounts:
            - name: config
              mountPath: /config
      volumes:
        - name: config
          hostPath: {path: /srv/apps/plex}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: sonarr
  namespace: media
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: sonarr
    spec:
      containers:
        - name: sonarr
          image: lscr.io/linuxserver/sonarr:4.0.14
`)},
		"save/b.yaml": {Data: []byte(`apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Service
    metadata:
      name: plex
      namespace: media
    spec:
      selector:
        app.kubernetes.io/name: plex
      ports:
        - port: 80
          targetPort: 32400
  - apiVersion: v1
    kind: Service
    metadata:
      name: sonarr
      namespace: media
    spec:
      selector:
        app.kubernetes.io/name: sonarr
      ports:
        - port: 80
          targetPort: http
`)},
		"save/c.json":    {Data: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"not-a-workload","namespace":"media"}}`)},
		"save/notes.txt": {Data: []byte("this is not a manifest")},
	}
	src := NewDirectory(fsys)
	ctx := context.Background()
	wls, err := src.Workloads(ctx)
	if err != nil {
		t.Fatalf("workloads: %v", err)
	}
	if len(wls) != 2 {
		t.Fatalf("workloads: %#v", wls)
	}
	svcs, err := src.Services(ctx)
	if err != nil {
		t.Fatalf("services: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("services: %#v", svcs)
	}
	// A saved Service carries the ports it answers on, because a rule is only
	// made for a port that reaches the app it selects.
	wantPorts := map[string]ServicePort{
		"plex":   {Port: 80, TargetPort: 32400},
		"sonarr": {Port: 80, TargetName: "http"},
	}
	for _, svc := range svcs {
		if len(svc.Ports) != 1 || svc.Ports[0] != wantPorts[svc.Name] {
			t.Fatalf("service %s ports: %#v, want %#v", svc.Name, svc.Ports, wantPorts[svc.Name])
		}
	}
	raw, err := src.Raw(ctx)
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	for _, want := range []string{"Deployment/media/plex", "StatefulSet/media/sonarr", "Service/media/plex", "Service/media/sonarr", "ConfigMap/media/not-a-workload"} {
		if _, ok := raw[want]; !ok {
			t.Fatalf("%s was not recorded: %#v", want, keys(raw))
		}
	}
	if len(raw) != 5 {
		t.Fatalf("a non-manifest file was read: %#v", keys(raw))
	}
}

// TestDirectoryNamesTheFileItCannotRead: a saved source is an operator's file, so
// a document that does not decode says which one.
func TestDirectoryNamesTheFileItCannotRead(t *testing.T) {
	fsys := fstest.MapFS{
		"save/good.yaml": {Data: []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: x\n")},
		"save/broken.yaml": {Data: []byte(`apiVersion: apps/v1
kind: Deployment
	metadata: this indentation is not YAML
`)},
	}
	_, err := NewDirectory(fsys).Workloads(context.Background())
	if err == nil {
		t.Fatal("a broken document was accepted")
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("the refusal does not name the file: %v", err)
	}
}

// TestDirectoryNeedsManifests: an empty directory is a refusal rather than a
// source with no apps, because the two look the same from the plan.
func TestDirectoryNeedsManifests(t *testing.T) {
	_, err := NewDirectory(fstest.MapFS{"readme.md": {Data: []byte("nothing")}}).Workloads(context.Background())
	if err == nil {
		t.Fatal("a directory with no manifests was accepted")
	}
}
