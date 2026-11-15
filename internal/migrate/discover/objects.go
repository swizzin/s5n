package discover

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// objects.go is the one place a Kubernetes object becomes what discovery reads
// from it, so the live source and a saved directory cannot disagree about a
// mount, a selector or a host directory.

// workloadOf flattens a pod template into the Workload both sources hand out.
func workloadOf(kind, namespace, name string, labels map[string]string, spec corev1.PodSpec, claims claimIndex) Workload {
	wl := Workload{
		Namespace:  namespace,
		Name:       name,
		Kind:       kind,
		Labels:     labels,
		Volumes:    make(map[string]Volume, len(spec.Volumes)),
		Containers: make([]Container, 0, len(spec.Containers)),
	}
	for _, v := range spec.Volumes {
		wl.Volumes[v.Name] = volumeOf(namespace, v, claims)
	}
	for _, c := range spec.Containers {
		wl.Containers = append(wl.Containers, containerOf(c))
	}
	return wl
}

// containerOf keeps what discovery asks a container: its image, the literal
// environment the app reads its port from, the ports it declares and where the
// volumes appear.
func containerOf(c corev1.Container) Container {
	out := Container{Name: c.Name, Image: c.Image, Env: make(map[string]string, len(c.Env))}
	for _, e := range c.Env {
		if e.Value != "" {
			out.Env[e.Name] = e.Value
		}
	}
	for _, p := range c.Ports {
		if p.ContainerPort > 0 {
			out.Ports = append(out.Ports, int(p.ContainerPort))
		}
	}
	for _, m := range c.VolumeMounts {
		out.Mounts = append(out.Mounts, Mount{Volume: m.Name, Path: m.MountPath})
	}
	return out
}

// volumeOf says where a pod volume's bytes live. Only two kinds of volume hold a
// host directory -- a hostPath, and a claim bound to a hostPath or local volume
// -- and everything else is either scratch or data on a filesystem this host
// does not have.
func volumeOf(namespace string, v corev1.Volume, claims claimIndex) Volume {
	switch {
	case v.HostPath != nil:
		return Volume{Kind: "hostPath", HostPath: v.HostPath.Path, Persistent: true}
	case v.PersistentVolumeClaim != nil:
		vol := Volume{Kind: "claim", Claim: v.PersistentVolumeClaim.ClaimName, Persistent: true}
		if host, ok := claims.hostPath(namespace, v.PersistentVolumeClaim.ClaimName); ok {
			vol.HostPath = host
		}
		return vol
	case v.EmptyDir != nil:
		return Volume{Kind: "emptyDir"}
	case v.ConfigMap != nil:
		return Volume{Kind: "configMap"}
	case v.Secret != nil:
		return Volume{Kind: "secret"}
	case v.Projected != nil:
		return Volume{Kind: "projected"}
	case v.DownwardAPI != nil:
		return Volume{Kind: "downwardAPI"}
	case v.Ephemeral != nil:
		// A generic ephemeral volume is a claim that dies with the pod: data
		// that outlives nothing and is not worth adopting.
		return Volume{Kind: "ephemeral"}
	case v.CSI != nil:
		return Volume{Kind: "csi", Persistent: true}
	case v.NFS != nil:
		return Volume{Kind: "nfs", Persistent: true}
	}
	return Volume{Kind: "volume", Persistent: true}
}

// claimIndex is what a claim-backed mount resolves through: the claim, and the
// volume the claim is bound to. A claim with no volume yet, or one a provisioner
// filled, has no host directory, and that is the answer Volume.HostPath gives.
type claimIndex struct {
	claims  map[string]*corev1.PersistentVolumeClaim // <namespace>/<name>
	volumes map[string]*corev1.PersistentVolume      // by volume name
}

// indexClaims files the claims and volumes of one source.
func indexClaims(pvcs []*corev1.PersistentVolumeClaim, pvs []*corev1.PersistentVolume) claimIndex {
	idx := claimIndex{
		claims:  make(map[string]*corev1.PersistentVolumeClaim, len(pvcs)),
		volumes: make(map[string]*corev1.PersistentVolume, len(pvs)),
	}
	for _, pvc := range pvcs {
		idx.claims[pvc.Namespace+"/"+pvc.Name] = pvc
	}
	for _, pv := range pvs {
		idx.volumes[pv.Name] = pv
	}
	return idx
}

// hostPath is the host directory behind a claim, when the volume bound to it is
// a hostPath or a local volume. A local volume is node-affine host storage: the
// same bytes on the same machine, which is exactly what a migration in place
// needs.
func (idx claimIndex) hostPath(namespace, claim string) (string, bool) {
	pvc, ok := idx.claims[namespace+"/"+claim]
	if !ok || pvc.Spec.VolumeName == "" {
		return "", false
	}
	pv, ok := idx.volumes[pvc.Spec.VolumeName]
	if !ok {
		return "", false
	}
	switch {
	case pv.Spec.HostPath != nil:
		return pv.Spec.HostPath.Path, true
	case pv.Spec.Local != nil:
		return pv.Spec.Local.Path, true
	}
	return "", false
}

// appsV1 is the group the two workload kinds are in.
const appsV1 = "apps/v1"

// typed turns one decoded document into the object discovery reads. A kind it
// has no use for is not an error: a saved directory holds whatever the operator
// saved, and the document is still recorded.
func typed(u *unstructured.Unstructured) (any, error) {
	convert := func(into any) (any, error) {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, into); err != nil {
			return nil, fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
		return into, nil
	}
	switch {
	case u.GetAPIVersion() == appsV1 && u.GetKind() == "Deployment":
		return convert(&appsv1.Deployment{})
	case u.GetAPIVersion() == appsV1 && u.GetKind() == "StatefulSet":
		return convert(&appsv1.StatefulSet{})
	case u.GetAPIVersion() == "v1" && u.GetKind() == "Service":
		return convert(&corev1.Service{})
	case u.GetAPIVersion() == "v1" && u.GetKind() == "PersistentVolumeClaim":
		return convert(&corev1.PersistentVolumeClaim{})
	case u.GetAPIVersion() == "v1" && u.GetKind() == "PersistentVolume":
		return convert(&corev1.PersistentVolume{})
	}
	return nil, nil
}

// objectKey is how a manifest is filed in the plan directory.
func objectKey(kind, namespace, name string) string {
	switch {
	case name == "":
		return kind
	case namespace == "":
		return kind + "/" + name
	}
	return kind + "/" + namespace + "/" + name
}

// manifestBytes is one object as the plan directory keeps it: JSON, which is a
// YAML document too, with the status and the field manager's bookkeeping
// dropped. What is recorded is the source's specification, not the state it
// happened to be in while discovery read it.
func manifestBytes(obj map[string]any) ([]byte, error) {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == "status" {
			continue
		}
		out[k] = v
	}
	if meta, ok := out["metadata"].(map[string]any); ok {
		trimmed := make(map[string]any, len(meta))
		for k, v := range meta {
			if k == "managedFields" {
				continue
			}
			trimmed[k] = v
		}
		out["metadata"] = trimmed
	}
	return json.MarshalIndent(out, "", "  ")
}

// unstructuredOf is a typed object back in the shape manifestBytes writes.
func unstructuredOf(obj any) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

// anySlice is a typed list as record walks it.
func anySlice[T any](items []*T) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// record files every object of one kind in the plan directory's manifests. The
// apiVersion and kind are written here because a typed Go object carries no
// TypeMeta once it has been decoded out of the API's response.
func record(into map[string][]byte, apiVersion, kind string, objs []any) error {
	for _, obj := range objs {
		m, err := unstructuredOf(obj)
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		m["apiVersion"], m["kind"] = apiVersion, kind
		data, err := manifestBytes(m)
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		meta, _ := m["metadata"].(map[string]any)
		namespace, _ := meta["namespace"].(string)
		name, _ := meta["name"].(string)
		into[objectKey(kind, namespace, name)] = data
	}
	return nil
}
