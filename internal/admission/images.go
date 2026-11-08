package admission

import (
	"fmt"
	"strings"
)

// The image rule. policy.yaml evaluates, per pod:
//
//	variables.blessed.exists(r, i.startsWith(r + ':') || i.startsWith(r + '@'))
//
// which is a prefix test on a boundary, not a parse of the reference. This file
// is that rule in Go, and it is deliberately just as literal: anything smarter
// (splitting the tag off, normalising docker.io, matching path segments) would
// disagree with the cluster, and the cluster is what refuses the write.
//
// What the rule therefore does and does not do, case by case:
//
//	ghcr.io/o/sonarr:5.0.0            blessed: `:tag`
//	ghcr.io/o/sonarr@sha256:ab…       blessed: `@digest`
//	ghcr.io/o/sonarr:5.0.0@sha256:ab… blessed: `:tag` comes first
//	ghcr.io/o/sonarr                  refused: no tag, no digest, no boundary
//	ghcr.io/o/sonarr-evil:1           refused: `-` is not a boundary
//	ghcr.io/o/sonarr/extra:1           refused: `/` is not a boundary
//	busybox:1.36                      only if `busybox` is itself blessed:
//	                                  there is no implied docker.io/library
//	docker.io/library/busybox:1.36    refused even when `busybox` is blessed:
//	                                  nothing is normalised
//	registry.example.com:5000/app:t   blessed by `registry.example.com:5000/app`
//	registry.example.com:5000/app:t   also blessed by `registry.example.com`,
//	                                  because the port's `:` is a boundary. The
//	                                  CEL has the same hole. It cannot bite with
//	                                  the chart's path-shaped ghcr.io
//	                                  repositories, and narrowing it here would
//	                                  make Go and the cluster disagree.
//
// A registry hostname that is a whole blessed entry is the one case worth
// re-reading: blessing `registry.example.com` admits every image under any
// port on that host, which is broader than "one repository". It is why the
// params are the chart's own list and never hand-written.

// Blessed reports whether the policy would admit image: some blessed
// repository r with image equal to r plus a `:tag` or `@digest`.
func Blessed(image string, repositories []string) bool {
	for _, r := range repositories {
		// The policy filters empty entries out of the params split; a trailing
		// comma in the ConfigMap must not bless every image with a leading
		// colon.
		if r == "" {
			continue
		}
		if strings.HasPrefix(image, r+":") || strings.HasPrefix(image, r+"@") {
			return true
		}
	}
	return false
}

// Container is one entry of a pod's containers, initContainers or
// ephemeralContainers. Only what the rule reads: the policy's images variable
// maps over `image` and nothing else.
type Container struct {
	Name  string
	Image string
}

// Pod is the part of a pod the image rule reads: where it lives and its three
// container lists. It is not corev1.Pod on purpose — this package has no
// client-go import (AGENTS.md keeps the dependency out of the tree until box
// needs it) and a caller converts what it listed from the API server.
type Pod struct {
	Namespace string
	Name      string
	// Containers, InitContainers and EphemeralContainers are the three lists
	// the policy concatenates. EphemeralContainers are what `kubectl debug`
	// adds; on a running pod they are the ones a new binding strands.
	Containers          []Container
	InitContainers      []Container
	EphemeralContainers []Container
}

// UnblessedImage is one container a binding would refuse: which pod it is in,
// which list it came from, and the image as written.
type UnblessedImage struct {
	Namespace string
	Pod       string
	// Container is the container's name, or "" when the pod (or its ephemeral
	// list) names it only by image.
	Container string
	// Kind is "container", "initContainer" or "ephemeralContainer": the field
	// the policy's images variable read it from.
	Kind  string
	Image string
}

// String is the line adduser prints. The namespace is omitted because adduser
// prints one namespace's pods.
func (u UnblessedImage) String() string {
	name := u.Pod
	if u.Container != "" {
		name += "/" + u.Container
	}
	return fmt.Sprintf("%s (%s): %s", name, u.Kind, u.Image)
}

// UnblessedImages returns every container in pods whose image the blessed list
// does not admit, in pod order and, within a pod, containers before init
// containers before ephemeral containers — the order the policy's message lists
// them in.
//
// A binding never re-checks a running pod, so these pods keep running; they are
// the ones whose next update is refused. adduser calls this before it writes
// the binding, so the refusal is named while root can still replace the pod.
func UnblessedImages(pods []Pod, blessed []string) []UnblessedImage {
	var out []UnblessedImage
	for _, pod := range pods {
		lists := []struct {
			kind string
			cs   []Container
		}{
			{"container", pod.Containers},
			{"initContainer", pod.InitContainers},
			{"ephemeralContainer", pod.EphemeralContainers},
		}
		for _, list := range lists {
			for _, c := range list.cs {
				if Blessed(c.Image, blessed) {
					continue
				}
				out = append(out, UnblessedImage{
					Namespace: pod.Namespace,
					Pod:       pod.Name,
					Container: c.Name,
					Kind:      list.kind,
					Image:     c.Image,
				})
			}
		}
	}
	return out
}
