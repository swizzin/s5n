package admission

import (
	"fmt"
	"strings"
)

// The image rule. policy.yaml tests, per pod:
//
//	variables.blessed.exists(r, i.startsWith(r + ':') || i.startsWith(r + '@'))
//
// a prefix test on a boundary, not a parse of the reference. This file is that
// rule in Go, deliberately just as literal: anything smarter (splitting the tag
// off, normalising docker.io, matching path segments) would disagree with the
// cluster, and the cluster is what refuses the write.
//
// So `repo:tag` and `repo@sha256:…` are blessed and bare `repo` is not; `-` and
// `/` are not boundaries (`repo-evil:1`, `repo/extra:1` are refused); there is no
// implied docker.io/library; and a registry hostname blessed whole admits every
// image under it on any port, because the port's `:` is a boundary. The CEL has
// that hole too. It is why the params are the chart's own list, never
// hand-written.

// Blessed reports whether the policy would admit image: some blessed repository
// r with image equal to r plus a `:tag` or `@digest`.
func Blessed(image string, repositories []string) bool {
	for _, r := range repositories {
		// The policy filters empty entries out of the params split; a trailing
		// comma in the ConfigMap must not bless every image with a leading colon.
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
// ephemeralContainers: only what the rule reads.
type Container struct {
	Name  string
	Image string
}

// Pod is the part of a pod the image rule reads: where it lives and its three
// container lists. It is not corev1.Pod on purpose: this package imports no
// client-go, and a caller converts what it listed.
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
// does not admit, in pod order and containers/init/ephemeral order, which is the
// policy message's order.
//
// A binding never re-checks a running pod, so these keep running; their next
// update is refused. adduser calls this before it writes the binding, naming the
// refusal while root can still replace the pod.
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
