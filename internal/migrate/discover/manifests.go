package discover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Directory is a Source over manifests saved to disk: a directory of YAML or
// JSON documents, `kind: List` included, as `kubectl get -o yaml` writes them.
//
// It exists twice over. A plan has to be rebuildable after the source is gone --
// a cluster box is replacing is torn down between discovery and apply -- and a
// test needs a source without an API server, which is how every refusal in this
// package is pinned.
type Directory struct {
	fsys fs.FS
	once bool
	objs []savedObject
	err  error
}

// savedObject is one document as read: the key it is filed under, what it says
// it is, and the typed object for the kinds discovery reads (nil for every
// other kind).
type savedObject struct {
	key  string
	raw  []byte
	kind string
	obj  any
}

// NewDirectory reads a saved source from fsys. Files ending in .yaml, .yml or
// .json are read, in name order; anything else is left alone, so pointing
// discovery at a directory that also holds a backup or a log is not an error.
func NewDirectory(fsys fs.FS) *Directory { return &Directory{fsys: fsys} }

// Workloads is the saved Deployments and StatefulSets.
func (d *Directory) Workloads(ctx context.Context) ([]Workload, error) {
	objs, err := d.load(ctx)
	if err != nil {
		return nil, err
	}
	claims := indexSavedClaims(objs)
	var out []Workload
	for _, o := range objs {
		switch obj := o.obj.(type) {
		case *appsv1.Deployment:
			out = append(out, workloadOf("Deployment", obj.Namespace, obj.Name, obj.Spec.Template.Labels, obj.Spec.Template.Spec, claims))
		case *appsv1.StatefulSet:
			out = append(out, workloadOf("StatefulSet", obj.Namespace, obj.Name, obj.Spec.Template.Labels, obj.Spec.Template.Spec, claims))
		}
	}
	return out, nil
}

// Services is the saved Services.
func (d *Directory) Services(ctx context.Context) ([]Service, error) {
	objs, err := d.load(ctx)
	if err != nil {
		return nil, err
	}
	var out []Service
	for _, o := range objs {
		if svc, ok := o.obj.(*corev1.Service); ok {
			out = append(out, serviceOf(svc))
		}
	}
	return out, nil
}

// Raw is every document read, as the plan directory keeps it.
func (d *Directory) Raw(ctx context.Context) (map[string][]byte, error) {
	objs, err := d.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(objs))
	for _, o := range objs {
		out[o.key] = o.raw
	}
	return out, nil
}

// load reads the directory once: a saved source is a few files, and decoding
// them once for three questions is the whole point of it being a value.
func (d *Directory) load(ctx context.Context) ([]savedObject, error) {
	if d.once {
		return d.objs, d.err
	}
	d.once = true
	d.objs, d.err = d.read(ctx)
	return d.objs, d.err
}

func (d *Directory) read(ctx context.Context) ([]savedObject, error) {
	var files []string
	err := fs.WalkDir(d.fsys, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		switch strings.ToLower(path.Ext(p)) {
		case ".yaml", ".yml", ".json":
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no .yaml, .yml or .json manifests to read")
	}
	sort.Strings(files)
	var out []savedObject
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := fs.ReadFile(d.fsys, name)
		if err != nil {
			return nil, err
		}
		objs, err := decodeDocuments(name, data)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

// indexSavedClaims files the claims and volumes a saved directory holds, so a
// claim-backed mount resolves exactly as it does in the live source.
func indexSavedClaims(objs []savedObject) claimIndex {
	var pvcs []*corev1.PersistentVolumeClaim
	var pvs []*corev1.PersistentVolume
	for _, o := range objs {
		switch obj := o.obj.(type) {
		case *corev1.PersistentVolumeClaim:
			pvcs = append(pvcs, obj)
		case *corev1.PersistentVolume:
			pvs = append(pvs, obj)
		}
	}
	return indexClaims(pvcs, pvs)
}

// decodeDocuments reads one file's documents, in order. A `kind: List` is the
// items it holds rather than a document of its own, because `kubectl get
// deploy,sts -o yaml` writes one and the objects inside are the manifests.
func decodeDocuments(name string, data []byte) ([]savedObject, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []savedObject
	for i := 1; ; i++ {
		var u unstructured.Unstructured
		if err := dec.Decode(&u); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("%s: document %d: %w", name, i, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		objs, err := savedObjects(name, i, &u)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
}

// savedObjects is one decoded document, or the items of a List.
func savedObjects(file string, index int, u *unstructured.Unstructured) ([]savedObject, error) {
	if u.GetKind() == "List" {
		items, _, err := unstructured.NestedSlice(u.Object, "items")
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", file, index, err)
		}
		var out []savedObject
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: document %d: a list item is not an object", file, index)
			}
			objs, err := savedObjects(file, index, &unstructured.Unstructured{Object: m})
			if err != nil {
				return nil, err
			}
			out = append(out, objs...)
		}
		return out, nil
	}
	raw, err := manifestBytes(u.Object)
	if err != nil {
		return nil, fmt.Errorf("%s: document %d: %w", file, index, err)
	}
	kind := u.GetKind()
	if kind == "" {
		kind = "document"
	}
	key := objectKey(kind, u.GetNamespace(), u.GetName())
	if u.GetName() == "" {
		key = fmt.Sprintf("%s#%d", kind, index)
	}
	obj, err := typed(u)
	if err != nil {
		return nil, fmt.Errorf("%s: document %d: %w", file, index, err)
	}
	return []savedObject{{key: key, raw: raw, kind: kind, obj: obj}}, nil
}
