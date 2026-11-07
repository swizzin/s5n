# Admission: the grant, enforced by the cluster

A seedbox user holds a kubeconfig scoped to their own namespace. Anything box
refuses in a values file (`apps.*.image.repository`, a port, a grant key) can
still be written with `kubectl` or `helm`, so those refusals are not
enforcement on their own. These policies are the enforcement:

- **`s5n-blessed-images`** admits a Pod, or the pod template of a Deployment,
  StatefulSet, DaemonSet, ReplicaSet, Job or CronJob, only when every container,
  init container and ephemeral container image uses a repository in the params
  list. It also matches `pods/ephemeralcontainers`, the subresource
  `kubectl debug` writes to: a rule for `pods` alone does not cover a
  subresource, and an unguarded subresource would let a user run any image
  alongside an already-admitted pod.
- **`s5n-granted-service`** admits a Service only as ClusterIP or the granted
  type, and admits a `nodePort` only when it is the granted peer port. It
  refuses `spec.externalIPs` outright: a tenant who can set one can claim an
  arbitrary address and intercept traffic to it (CVE-2020-8554). `ExternalName`
  is never admitted either, because it is neither ClusterIP nor a type the
  grant can assign.

Because a policy evaluates the whole Pod, a pod admitted before its namespace
was bound is held to the list on its next write: if it still runs an unblessed
image, every update to it is denied — `kubectl debug` included — until it is
replaced with a blessed pod.

Both use `failurePolicy: Fail`: a params object that is missing or a CEL error
refuses the write rather than admitting it.

## Where this lives and who installs it

`policy.yaml` is the platform layer, applied by root at `box bootstrap`, next to
the Gateway and MetalLB manifests. It is not part of `charts/s5n`: a release
stays namespace-scoped and carries no cluster RBAC.

`render-grant.py` runs as root at `box adduser` (and again at `box install`,
`box update` and `box bootstrap`) and prints, for one user:

1. `ConfigMap s5n-grant-<namespace>` in **`s5n-system`** — the grant as params:
   the blessed repositories, the granted Service type and the torrent nodePort.
   Root owns `s5n-system`; a ConfigMap in the user's own namespace would be
   writable by them and enforce nothing.
2. two cluster-scoped `ValidatingAdmissionPolicyBinding`s selecting exactly that
   namespace. Cluster-scoped objects are outside the user's namespace RBAC.

Root's own seedbox gets no binding: this is tenancy, not a cluster-wide image
policy. An unbound namespace admits what its own RBAC allows.

## One source of truth for the blessed list

The repository list is read from `charts/s5n/variants.yaml`, the same file
`_helpers.tpl` resolves every release image from. There is no second list:
adding an app means a values entry, its schema definition and its variants
entry, and the next params render carries it. `box` replaces this script with
the same logic in Go against the chart artifact it is about to install; it must
re-render the params on chart updates so a release that blesses a new
repository reaches existing seedboxes without a manual step.

Until `box` exists, the e2e is the caller:

```sh
kubectl apply -f install/platform/admission/policy.yaml
python3 install/platform/admission/render-grant.py charts/s5n seedbox 31111 NodePort | kubectl apply -f -
```
