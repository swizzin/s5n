package discover

import (
	"context"
	"reflect"
	"testing"
	"testing/fstest"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/swizzin/s5n/internal/migrate/plan"
)

// TestClusterReadsALiveSource: the client-go source flattens the same install
// the saved manifests describe, resolves a claim to the host directory behind its
// volume, and never asks for a Secret.
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
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/name": "sonarr"}},
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
			Source: "sonarr", Config: "/srv/apps/sonarr", Namespace: "media", Ports: []int{8989},
			Paths: []plan.PathRule{
				{From: "/config", To: "/config"},
				{From: "/library/Downloads", To: "/data/downloads"},
				{From: "/library/media", To: "/data/media"},
			},
		},
		"qbittorrent": {
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
	if !reflect.DeepEqual(p.Hosts, []plan.HostRule{{Service: "sonarr.media", App: "sonarr"}}) {
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
