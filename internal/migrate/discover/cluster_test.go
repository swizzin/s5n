package discover

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"testing/fstest"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

func TestClusterReadsALiveSource(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/name": "sonarr"}
	statefulset := func() *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "sonarr", Namespace: "media"},
			Spec: appsv1.StatefulSetSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "sonarr",
							Image: "lscr.io/linuxserver/sonarr:4.0.14",
							Ports: []corev1.ContainerPort{{ContainerPort: 8989}},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: "/config"},
								{Name: "library", MountPath: "/library"},
							},
						}},
						Volumes: []corev1.Volume{
							{Name: "config", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/apps/sonarr"}}},
							{Name: "library", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "library"}}},
						},
					},
				},
			},
		}
	}
	qbt := func() *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "qbittorrent", Namespace: "media"},
			Spec: appsv1.StatefulSetSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "qbittorrent"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "qbittorrent",
							Image: "ghcr.io/home-operations/qbittorrent:5.3.0",
							Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: "/config"},
								{Name: "downloads", MountPath: "/downloads"},
							},
						}},
						Volumes: []corev1.Volume{
							{Name: "config", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/apps/qbittorrent"}}},
							{Name: "downloads", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/library/Downloads"}}},
						},
					},
				},
			},
		}
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "sonarr", Namespace: "media"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": "sonarr"},
			// The API server defaults an unset targetPort to the service
			// port; a live object carries both, and the rule has to keep the
			// one a stored address uses.
			Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8989)}},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "library", Namespace: "media"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "library"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "library"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/srv/library"}},
		},
	}

	files := fstest.MapFS{
		"srv/apps/sonarr/sonarr.db":                         {Data: arrDB(t, "/library/media/TV")},
		"srv/apps/sonarr/config.xml":                        {Data: []byte("<Config><Port>8989</Port></Config>")},
		"srv/apps/qbittorrent/qBittorrent/qBittorrent.conf": {Data: []byte("[BitTorrent]\nSession\\DefaultSavePath=/downloads\n")},
	}
	// Objects handed to the fake are mutated by it, so each is built once.
	cs := fake.NewClientset(statefulset(), qbt(), service, pvc, pv)
	src := NewClusterFor(cs, nil)
	p, rep, err := Discover(context.Background(), src, Options{ConfigFS: files, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]plan.App{
		"sonarr": {
			Kind: plan.Kubernetes, Image: "lscr.io/linuxserver/sonarr:4.0.14",
			Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media", Ports: []int{8989},
			Paths: []plan.PathRule{
				{From: "/config", To: "/config"},
				{From: "/library/Downloads", To: "/data/downloads"},
				{From: "/library/media", To: "/data/media"},
			},
		},
		"qbittorrent": {
			Kind: plan.Kubernetes, Image: "ghcr.io/home-operations/qbittorrent:5.3.0",
			Source: "qbittorrent", Config: "/srv/apps/qbittorrent", Namespace: "media", Ports: []int{8080},
			Paths: []plan.PathRule{
				{From: "/config", To: "/config"},
				{From: "/downloads", To: "/data/downloads"},
			},
		},
	}
	if !reflect.DeepEqual(p.Apps, want) {
		t.Fatalf("apps:\n got %#v\nwant %#v", p.Apps, want)
	}
	if !reflect.DeepEqual(p.Data, &plan.Data{Root: "/srv/library", Downloads: "Downloads", Media: "media"}) {
		t.Fatalf("data: %#v", p.Data)
	}
	if !reflect.DeepEqual(p.Hosts, []plan.HostRule{{Service: "sonarr.media", App: "sonarr", Port: 80}}) {
		t.Fatalf("hosts: %#v", p.Hosts)
	}
	roundTrip(t, p)
	if _, ok := rep.Raw["PersistentVolume/library"]; !ok {
		t.Fatalf("the volume the claim resolved through is not recorded: %#v", keys(rep.Raw))
	}
	// Every call the fake saw is a read.
	for _, action := range cs.Actions() {
		switch action.GetVerb() {
		case "get", "list":
		default:
			t.Fatalf("discovery wrote to the source: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

func TestClusterReadsEveryOwnerlessKind(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/name": "prowlarr"}
	prowlarrPod := func(name string, owned bool) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "media", Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "prowlarr", Image: "lscr.io/linuxserver/prowlarr:2.3.0",
			}}},
			Status: corev1.PodStatus{PodIP: "10.244.0.9", PodIPs: []corev1.PodIP{{IP: "10.244.0.9"}, {IP: "fd00::9"}}},
		}
		if owned {
			pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "prowlarr"}}
		}
		return pod
	}
	one := int32(1)
	zero := int32(0)
	daemonset := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "prowlarr", Namespace: "media"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "prowlarr", Image: "lscr.io/linuxserver/prowlarr:2.3.0"}}},
			},
		},
	}
	replicaset := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tautulli", Namespace: "media"},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &zero,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "tautulli"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "tautulli"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "tautulli", Image: "lscr.io/linuxserver/tautulli:2.14.6"}}},
			},
		},
	}
	deploymentSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sonarr-5d8f7c", Namespace: "media",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "sonarr"}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "sonarr"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "sonarr"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "sonarr", Image: "lscr.io/linuxserver/sonarr:4.0.14"}}},
			},
		},
	}
	barePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "autobrr", Namespace: "media", Labels: map[string]string{"app.kubernetes.io/name": "autobrr"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "autobrr", Image: "ghcr.io/autobrr/autobrr:v1.60.0",
		}}},
		Status: corev1.PodStatus{PodIP: "10.244.0.21"},
	}
	cs := fake.NewClientset(daemonset, replicaset, deploymentSet, barePod,
		prowlarrPod("prowlarr-x2k9d", true), prowlarrPod("prowlarr-bare", false))
	src := NewClusterFor(cs, nil)

	wls, err := src.Workloads(context.Background())
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	byName := make(map[string]Workload, len(wls))
	for _, wl := range wls {
		byName[wl.Name] = wl
	}
	if len(byName) != 4 {
		t.Fatalf("workloads = %v, want one per kind nothing owns: the DaemonSet, the ownerless ReplicaSet, the bare Pod and the pod nobody owns", sortedKeys(byName))
	}
	for want, kind := range map[string]string{"prowlarr": "DaemonSet", "tautulli": "ReplicaSet", "autobrr": "Pod"} {
		wl, ok := byName[want]
		if !ok {
			t.Fatalf("%s is not a workload: %v", want, sortedKeys(byName))
		}
		if wl.Kind != kind {
			t.Errorf("%s kind = %q, want %q", want, wl.Kind, kind)
		}
	}
	if got := byName["tautulli"].Replicas; got != 0 {
		t.Errorf("the ReplicaSet scaled to 0 says %d replicas, want 0: that is what says it is stopped", got)
	}
	if got := byName["prowlarr"].Selector; !reflect.DeepEqual(got, labels) {
		t.Errorf("the DaemonSet's selector = %#v, want the labels its pods carry", got)
	}
	if _, ok := byName["sonarr-5d8f7c"]; ok {
		t.Error("a ReplicaSet a Deployment made is read as a workload of its own")
	}
	if _, ok := byName["prowlarr-x2k9d"]; ok {
		t.Error("a pod the DaemonSet made is read as a workload of its own")
	}

	pods, err := src.Pods(context.Background())
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}
	if len(pods) != 3 {
		t.Fatalf("pods = %d, want every pod the source holds, owned or not", len(pods))
	}
	// A pod's addresses are what a stored pod IP is matched against, and a
	// dual-stack pod has two.
	owned := pods[0]
	for _, pod := range pods {
		if pod.Name == "prowlarr-x2k9d" {
			owned = pod
		}
	}
	if !reflect.DeepEqual(owned.IPs, []string{"10.244.0.9", "fd00::9"}) {
		t.Errorf("the pod's addresses = %v, want both it held", owned.IPs)
	}
	if !byName["prowlarr"].Owns(owned) {
		t.Error("the DaemonSet does not own the pod its template made")
	}
	// Every call the fake saw is a read.
	for _, action := range cs.Actions() {
		switch action.GetVerb() {
		case "get", "list":
		default:
			t.Fatalf("discovery wrote to the source: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

// sortedKeys is a map's keys, sorted, for a failure that names what it got.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
