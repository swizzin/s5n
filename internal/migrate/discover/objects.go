package discover

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// objects.go is the one place a Kubernetes object becomes what discovery reads
// from it, so the live source and a saved directory cannot disagree about a
// mount, a selector or a host directory.

// template is one workload's pod template as a source hands it over: what the
// object calls itself, the labels its pods carry, the template itself, and the
// two things only the workload knows -- what selects its pods, and how many it
// asks for.
type template struct {
	kind      string
	namespace string
	name      string
	labels    map[string]string
	spec      corev1.PodSpec
	// selector is what the workload's pods carry. A manifest saved off disk
	// does not always carry one (an older export, a hand-written file), so an
	// empty selector falls back to the template's own labels, which is what
	// its pods will carry once it runs.
	selector map[string]string
	// replicas is the count the workload asks for, -1 for a kind that has no
	// count of its own: a DaemonSet runs one pod per node, and a bare Pod is
	// the pod.
	replicas int
}

// workloadsOf is every pod template these objects hold, flattened the one way:
// Deployments, StatefulSets and DaemonSets as they are, and a ReplicaSet or a
// Pod only when nothing owns it. A ReplicaSet a Deployment made, and every pod
// a workload made, mirror a template discovery has already read; reading one of
// them as its own workload would count one app twice and could name the app
// after a template hash.
func workloadsOf(deploys []*appsv1.Deployment, stss []*appsv1.StatefulSet, daemonsets []*appsv1.DaemonSet, sets []*appsv1.ReplicaSet, pods []*corev1.Pod, claims claimIndex) []Workload {
	var out []Workload
	for _, d := range deploys {
		out = append(out, workloadOf(template{
			kind: kindDeployment, namespace: d.Namespace, name: d.Name,
			labels: d.Spec.Template.Labels, spec: d.Spec.Template.Spec,
			selector: selectorOf(d.Spec.Selector), replicas: replicasOf(d.Spec.Replicas),
		}, claims))
	}
	for _, s := range stss {
		out = append(out, workloadOf(template{
			kind: kindStatefulSet, namespace: s.Namespace, name: s.Name,
			labels: s.Spec.Template.Labels, spec: s.Spec.Template.Spec,
			selector: selectorOf(s.Spec.Selector), replicas: replicasOf(s.Spec.Replicas),
		}, claims))
	}
	for _, ds := range daemonsets {
		out = append(out, workloadOf(template{
			kind: kindDaemonSet, namespace: ds.Namespace, name: ds.Name,
			labels: ds.Spec.Template.Labels, spec: ds.Spec.Template.Spec,
			selector: selectorOf(ds.Spec.Selector), replicas: -1,
		}, claims))
	}
	for _, rs := range sets {
		out = append(out, workloadOf(template{
			kind: kindReplicaSet, namespace: rs.Namespace, name: rs.Name,
			labels: rs.Spec.Template.Labels, spec: rs.Spec.Template.Spec,
			selector: selectorOf(rs.Spec.Selector), replicas: replicasOf(rs.Spec.Replicas),
		}, claims))
	}
	for _, pod := range pods {
		out = append(out, workloadOf(template{
			kind: kindPod, namespace: pod.Namespace, name: pod.Name,
			labels: pod.Labels, spec: pod.Spec, replicas: -1,
		}, claims))
	}
	return out
}

// selectorOf is what a workload's pods carry: the matchLabels its selector
// names, or nil when it names none, which template falls back from to the
// pod template's own labels. A selector's matchExpressions are not kept: a pod
// is known to be a replica by the labels it carries, and an expression's
// answer is what the cluster has already applied to them.
func selectorOf(sel *metav1.LabelSelector) map[string]string {
	if sel == nil || len(sel.MatchLabels) == 0 {
		return nil
	}
	return sel.MatchLabels
}

// replicasOf is a workload's desired count: the API server defaults an absent
// one to 1, and a manifest read off disk carries whatever was saved.
func replicasOf(replicas *int32) int {
	if replicas == nil {
		return 1
	}
	return int(*replicas)
}

// workloadOf flattens a pod template into the Workload both sources hand out.
func workloadOf(t template, claims claimIndex) Workload {
	wl := Workload{
		Namespace:  t.namespace,
		Name:       t.name,
		Kind:       t.kind,
		Labels:     t.labels,
		Selector:   t.selector,
		Replicas:   t.replicas,
		Volumes:    make(map[string]Volume, len(t.spec.Volumes)),
		Containers: make([]Container, 0, len(t.spec.Containers)),
		RunAsUser:  -1,
		RunAsGroup: -1,
	}
	if wl.Selector == nil {
		wl.Selector = wl.Labels
	}
	if sc := t.spec.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			wl.RunAsUser = int(*sc.RunAsUser)
		}
		if sc.RunAsGroup != nil {
			wl.RunAsGroup = int(*sc.RunAsGroup)
		} else if sc.FSGroup != nil {
			wl.RunAsGroup = int(*sc.FSGroup)
		}
	}
	for _, v := range t.spec.Volumes {
		wl.Volumes[v.Name] = volumeOf(t.namespace, v, claims)
	}
	for _, c := range t.spec.Containers {
		wl.Containers = append(wl.Containers, containerOf(c))
	}
	return wl
}

// containerOf keeps what discovery asks a container: its image, the literal
// environment the app reads its port from, the ports it declares, where the
// volumes appear and the account it runs as.
func containerOf(c corev1.Container) Container {
	out := Container{
		Name: c.Name, Image: c.Image, Env: make(map[string]string, len(c.Env)),
		RunAsUser: -1, RunAsGroup: -1,
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			out.RunAsUser = int(*sc.RunAsUser)
		}
		if sc.RunAsGroup != nil {
			out.RunAsGroup = int(*sc.RunAsGroup)
		}
	}
	for _, e := range c.Env {
		if e.Value != "" {
			out.Env[e.Name] = e.Value
		}
	}
	for _, p := range c.Ports {
		if p.ContainerPort > 0 {
			out.Ports = append(out.Ports, int(p.ContainerPort))
			if p.Name != "" {
				if out.PortNames == nil {
					out.PortNames = make(map[string]int, len(c.Ports))
				}
				out.PortNames[p.Name] = int(p.ContainerPort)
			}
		}
	}
	for _, m := range c.VolumeMounts {
		out.Mounts = append(out.Mounts, Mount{Volume: m.Name, Path: m.MountPath})
	}
	return out
}

// serviceOf flattens a Service to what a host rule is made of: where it is,
// what it selects, the ports it answers on and the addresses it answers at.
func serviceOf(svc *corev1.Service) Service {
	out := Service{Namespace: svc.Namespace, Name: svc.Name, Selector: svc.Spec.Selector}
	for _, p := range svc.Spec.Ports {
		out.Ports = append(out.Ports, servicePortOf(p))
	}
	// A Service written to disk has one address and a live one has the list;
	// either way the short one was once the way to reach it.
	seen := make(map[string]bool, 2)
	for _, ip := range append([]string{svc.Spec.ClusterIP}, svc.Spec.ClusterIPs...) {
		// "None" is a headless Service: it has no address of its own, which is
		// exactly what an app that stored one never used.
		if ip == "" || ip == "None" || seen[ip] {
			continue
		}
		seen[ip] = true
		out.ClusterIPs = append(out.ClusterIPs, ip)
	}
	return out
}

// servicesOf is every Service a source holds, flattened the one way.
func servicesOf(svcs []*corev1.Service) []Service {
	out := make([]Service, 0, len(svcs))
	for _, svc := range svcs {
		out = append(out, serviceOf(svc))
	}
	return out
}

// ingressOf flattens an Ingress to the rules an app's stored address can be one
// of: a public host and path, and the Service behind them. A rule with no host
// catches every name, so no address is that rule rather than another one; the
// default backend has no name at all and is not a rule.
func ingressOf(ing *networkingv1.Ingress) Ingress {
	out := Ingress{Namespace: ing.Namespace, Name: ing.Name}
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" || rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil || path.Backend.Service.Name == "" {
				continue
			}
			number, name := serviceBackendPort(path.Backend.Service.Port)
			out.Rules = append(out.Rules, IngressRule{
				Host:     rule.Host,
				Path:     path.Path,
				Service:  path.Backend.Service.Name,
				Port:     number,
				PortName: name,
			})
		}
	}
	return out
}

// ingressesOf is every Ingress a source holds, flattened the one way.
func ingressesOf(ings []*networkingv1.Ingress) []Ingress {
	out := make([]Ingress, 0, len(ings))
	for _, ing := range ings {
		out = append(out, ingressOf(ing))
	}
	return out
}

// serviceBackendPort is the port an Ingress rule names for its Service: the
// number, or the name the Service's own ports declare it under. Exactly one is
// set, and both are empty only in an Ingress the API server would have refused.
func serviceBackendPort(port networkingv1.ServiceBackendPort) (number int, name string) {
	return int(port.Number), port.Name
}

// podsOf is every pod a source holds, flattened to the two questions discovery
// asks of one: which addresses it answered on, and whether the workload that
// made it still has a pod.
func podsOf(pods []*corev1.Pod) []Pod {
	out := make([]Pod, 0, len(pods))
	for _, pod := range pods {
		out = append(out, Pod{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			Labels:    pod.Labels,
			IPs:       podIPs(pod),
		})
	}
	return out
}

// podIPs is what a pod answers on, status.podIPs (a dual-stack pod has two)
// with status.podIP beside it for a manifest saved before the list existed.
func podIPs(pod *corev1.Pod) []string {
	seen := make(map[string]bool, len(pod.Status.PodIPs)+1)
	var out []string
	for _, ip := range append([]corev1.PodIP{{IP: pod.Status.PodIP}}, pod.Status.PodIPs...) {
		if ip.IP == "" || seen[ip.IP] {
			continue
		}
		seen[ip.IP] = true
		out = append(out, ip.IP)
	}
	return out
}

// nodeNames is every node's name, which is how a source says what host it ran
// on: a single-node cluster is named by its one node, and a cluster with more
// than one names no host at all.
func nodeNames(nodes []*corev1.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.Name)
	}
	return out
}

// ownerless reports whether an object belongs to no other object: a ReplicaSet
// a Deployment made, or a pod any workload made, mirrors a template discovery
// already reads, so it is not a workload of its own.
func ownerless(refs []metav1.OwnerReference) bool { return len(refs) == 0 }

// servicePortOf keeps one Service port as an app's stored address saw it: the
// port the address carries, and what it forwards to.
//
// A Service that leaves targetPort unset forwards to its own port -- an identity
// map (k8s.io/api@v0.37.1:core/v1/types.go:6555) -- which the API server
// defaults but a manifest read off disk does not, so the default is applied here
// rather than left to which source read the object.
func servicePortOf(p corev1.ServicePort) ServicePort {
	sp := ServicePort{Name: p.Name, Port: int(p.Port)}
	if p.TargetPort.Type == intstr.String && p.TargetPort.StrVal != "" {
		sp.TargetName = p.TargetPort.StrVal
		return sp
	}
	sp.TargetPort = int(p.TargetPort.IntVal)
	if sp.TargetPort == 0 {
		sp.TargetPort = sp.Port
	}
	return sp
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

// The groups the objects discovery reads live in.
const (
	appsV1       = "apps/v1"
	networkingV1 = "networking.k8s.io/v1"
)

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
	case u.GetAPIVersion() == appsV1 && u.GetKind() == "DaemonSet":
		return convert(&appsv1.DaemonSet{})
	case u.GetAPIVersion() == appsV1 && u.GetKind() == "ReplicaSet":
		return convert(&appsv1.ReplicaSet{})
	case u.GetAPIVersion() == "v1" && u.GetKind() == "Pod":
		return convert(&corev1.Pod{})
	case u.GetAPIVersion() == "v1" && u.GetKind() == "Service":
		return convert(&corev1.Service{})
	case u.GetAPIVersion() == networkingV1 && u.GetKind() == "Ingress":
		return convert(&networkingv1.Ingress{})
	case u.GetAPIVersion() == "v1" && u.GetKind() == "Node":
		return convert(&corev1.Node{})
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
