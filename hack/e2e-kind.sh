#!/usr/bin/env bash
# Phase-0/1 smoke test on kind: cluster -> Envoy Gateway (Gateway API) -> MetalLB (L2)
# -> s5n chart. Asserts pods Ready, routes Accepted, every app answers through the
# Gateway's LoadBalancer address (and Plex refuses an unauthenticated library call
# there, instead of trusting the pod network the proxy dials from), the peers
# NodePort accepts a TCP connect on the node's own address, the hardlink invariant
# holds on /data, and the wiring Job does its job twice: the first run connects
# everything, the second changes nothing.
# Usage: hack/e2e-kind.sh [--keep]
set -euo pipefail
cd "$(dirname "$0")/.."

KIND=${KIND:-kind}
CLUSTER=s5n
NS=seedbox
# Envoy Gateway's Helm chart ships its own Gateway API CRDs (crds.enabled: true), so
# there is no separate standard-install apply here: the chart's experimental channel is
# the exact set Envoy Gateway v1.9.3 supports, and a separately-pinned channel could
# drift from (or conflict with) it.
ENVOY_GATEWAY_CHART_VERSION=${ENVOY_GATEWAY_CHART_VERSION:-1.9.3}
METALLB_VERSION=${METALLB_VERSION:-v0.16.1}
KEEP=${1:-}
CTX="kind-${CLUSTER}"
K="kubectl --context ${CTX}"
HELM="helm --kube-context ${CTX}"
# Built under the repository the chart's s5n variant names, so the release uses
# the local image by tag instead of pulling the published one.
IMAGE=ghcr.io/swizzin/s5n:e2e
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

log() { printf '\n==> %s\n' "$*"; }
fail=0

# kind writes the cluster's context into KUBECONFIG and makes it the
# current-context, then `kind delete cluster` drops that context without putting
# the previous one back: a plain `kind create` would leave the operator pointed at
# a cluster that no longer exists, and `--keep` would leave it pointed at ours.
# So the e2e brings its own kubeconfig and kind, kubectl and helm all read that
# file; ~/.kube/config is never opened. It lives outside $WORK so a kept cluster
# stays reachable after this script exits.
KUBECONFIG=${S5N_E2E_KUBECONFIG:-${XDG_CACHE_HOME:-$HOME/.cache}/s5n-e2e/kubeconfig}
export KUBECONFIG
mkdir -p "$(dirname "$KUBECONFIG")"
[ -e "$KUBECONFIG" ] || : >"$KUBECONFIG"
chmod 600 "$KUBECONFIG"
log "kubeconfig: ${KUBECONFIG} (~/.kube/config is not used)"

if $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  # A kept cluster can outlive its kubeconfig (a cleared cache): export it again
  # rather than fail on a missing kind-<cluster> context.
  if ! kubectl config get-contexts "$CTX" -o name 2>/dev/null | grep -qx "$CTX"; then
    log "exporting the existing kind cluster's kubeconfig"
    $KIND export kubeconfig --name "$CLUSTER"
  fi
else
  log "creating kind cluster"
  $KIND create cluster --config hack/kind.yaml --wait 120s
fi

log "building the s5n image (wiring Job + qBittorrent credential seed)"
docker build -q -t "$IMAGE" .
$KIND load docker-image --name "$CLUSTER" "$IMAGE"

log "preparing host paths inside the node (the installer does this on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data/downloads/{tv,movies,tv-4k,movies-4k,misc} \
           /srv/s5n/data/media/{tv,movies,tv-4k,movies-4k} \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,plex,tautulli,seerr}
  chown -R 1000:1000 /srv/s5n'

log "Envoy Gateway ${ENVOY_GATEWAY_CHART_VERSION} as the Gateway (installs Gateway API CRDs)"
$HELM upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version "$ENVOY_GATEWAY_CHART_VERSION" -n envoy-gateway-system --create-namespace \
  --wait --timeout 5m >/dev/null

log "MetalLB ${METALLB_VERSION} (L2) so LoadBalancer Services get real addresses"
$K apply -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml" >/dev/null
$K -n metallb-system rollout status deploy/controller --timeout=180s
$K -n metallb-system rollout status daemonset/speaker --timeout=180s
# The IPAddressPool/L2Advertisement below pass MetalLB's validating webhook, which the
# controller serves; wait for its endpoints so the apply does not race it.
for i in $(seq 1 30); do
  ep=$($K -n metallb-system get endpoints metallb-webhook-service -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)
  [ -n "$ep" ] && break
  sleep 2
done

# Carve a small L2 pool from the top of the kind docker network's IPv4 subnet, so the
# host reaches LoadBalancer addresses directly — the same trick GitHub ubuntu runners
# rely on, and the same reason no extraPortMappings are needed.
kind_subnet=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -v ':' | head -1)
pool=$(python3 - "$kind_subnet" <<'PY'
import ipaddress, sys
net = ipaddress.ip_network(sys.argv[1], strict=False)
ips = list(net.hosts())[-16:]
print(f"{ips[0]}-{ips[-1]}")
PY
)
echo "  MetalLB pool: ${pool}"
# An endpoint address only says the pod is Ready; the webhook server can still be
# refusing connections while it loads its certificate (seen in CI). So the apply
# itself is retried until the webhook answers.
metallb_pool() {
  $K apply -f - >/dev/null <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: s5n-pool
  namespace: metallb-system
spec:
  addresses:
    - ${pool}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: s5n-l2
  namespace: metallb-system
spec:
  ipAddressPools:
    - s5n-pool
EOF
}
for i in $(seq 1 30); do
  metallb_pool 2>"$WORK/metallb.err" && break
  [ "$i" = 30 ] && { cat "$WORK/metallb.err" >&2; exit 1; }
  sleep 2
done

log "namespace ${NS} with Pod Security restricted (enforced)"
$K create namespace "$NS" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K label --overwrite namespace "$NS" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted >/dev/null

log "GatewayClass + Gateway (the chart's default parentRef target)"
$K create namespace s5n-gateway --dry-run=client -o yaml | $K apply -f - >/dev/null
$K apply -f install/platform/gateway/gateway.yaml >/dev/null

# ---- the grant, enforced by the cluster ---------------------------------------
# Root installs the policies with the platform layer; `box adduser` binds them to
# one namespace with that user's grant as params (root-owned s5n-system, never
# the user's namespace). The e2e stands in for both, then acts as the user with
# a ServiceAccount holding the namespace RBAC a user's box gets.
log "platform layer: the grant's admission policies"
$K apply -f install/platform/admission/policy.yaml >/dev/null
# A rerun on a --keep cluster starts unbound, so the "predates the binding"
# step is true again. DELETEs are not matched by the policy.
$K delete validatingadmissionpolicybinding "s5n-grant-${NS}" "s5n-grant-${NS}-service" --ignore-not-found >/dev/null
$K -n "$NS" delete deploy unblessed --ignore-not-found >/dev/null

# One pod, admitted before any binding exists, to answer the question every
# admission policy raises: what happens to what is already running? Nothing --
# admission only sees new writes. Kept out of the app label selectors on
# purpose.
cat >"$WORK/unblessed.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: unblessed
spec:
  replicas: 1
  selector:
    matchLabels:
      app: unblessed
  template:
    metadata:
      labels:
        app: unblessed
    spec:
      # Pod Security restricted is enforced here, so the pod is otherwise
      # admissible: the only reason left to refuse it is the repository.
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: unblessed
          image: docker.io/curlimages/curl:8.11.1
          command: ["sleep", "3600"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF

# A second copy under another name: `create` of an object that already exists
# answers AlreadyExists, which is not the denial under test.
sed 's/name: unblessed/name: admission-probe/; s/app: unblessed/app: admission-probe/g' \
  "$WORK/unblessed.yaml" >"$WORK/probe.yaml"

log "a pod that predates the binding (admission never re-checks running pods)"
$K -n "$NS" apply -f "$WORK/unblessed.yaml" >/dev/null
$K -n "$NS" wait --for=condition=Available deploy/unblessed --timeout=180s || fail=1

log "bind the policy to ${NS} with the grant as params (root-owned s5n-system)"
# The renderer is Go and reads the chart's variants.yaml, the same file the
# templates resolve images from; it is the package box will call.
go run ./hack/render-grant charts/s5n "$NS" 31111 NodePort | $K apply -f - >/dev/null

# A binding takes effect a moment after it is created, so wait for a denial
# instead of sleeping. A dry-run CREATE of a new object is a real admission
# request that persists nothing.
effective=no
for i in $(seq 1 30); do
  if ! $K -n "$NS" create --dry-run=server -f "$WORK/probe.yaml" >/dev/null 2>"$WORK/deny-image.txt"; then
    grep -q "not blessed by the chart" "$WORK/deny-image.txt" && { effective=yes; break; }
  fi
  sleep 1
done
echo "policy effective: ${effective}"
[ "$effective" = yes ] || { cat "$WORK/deny-image.txt"; fail=1; }

log "a namespace-scoped user identity, as a user's box gets"
$K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: box-user
  namespace: ${NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: box-user
  namespace: ${NS}
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["*"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: box-user
  namespace: ${NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: box-user
subjects:
  - kind: ServiceAccount
    name: box-user
    namespace: ${NS}
EOF
SA="system:serviceaccount:${NS}:box-user"

log "admission: the user cannot run an image the chart has not blessed"
sed 's/name: unblessed/name: user-unblessed/; s/app: unblessed/app: user-unblessed/g' \
  "$WORK/unblessed.yaml" >"$WORK/user-unblessed.yaml"
if $K --as="$SA" -n "$NS" create -f "$WORK/user-unblessed.yaml" >"$WORK/allow-image.txt" 2>&1; then
  echo "user created an unblessed Deployment"; cat "$WORK/allow-image.txt"; fail=1
else
  echo "$(cat "$WORK/allow-image.txt")"
  grep -q "not blessed by the chart" "$WORK/allow-image.txt" || fail=1
fi

log "admission: the user can only use the granted nodePort"
cat >"$WORK/unblessed-svc.yaml" <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: unblessed
spec:
  type: NodePort
  selector:
    app: unblessed
  ports:
    - port: 6881
      targetPort: 6881
      nodePort: 30001
EOF
if $K --as="$SA" -n "$NS" create -f "$WORK/unblessed-svc.yaml" >"$WORK/allow-svc.txt" 2>&1; then
  echo "user created a Service on a nodePort the grant did not assign"; fail=1
else
  echo "$(cat "$WORK/allow-svc.txt")"
  grep -q "nodePort may only be the peer port" "$WORK/allow-svc.txt" || fail=1
fi

log "admission: the user cannot attach externalIPs to a Service"
# A ClusterIP Service is otherwise admitted, so only spec.externalIPs stands
# between a tenant and claiming an arbitrary address (CVE-2020-8554).
cat >"$WORK/external-ips-svc.yaml" <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: external-ips
spec:
  type: ClusterIP
  externalIPs:
    - 203.0.113.10
  selector:
    app: unblessed
  ports:
    - port: 6881
      targetPort: 6881
EOF
if $K --as="$SA" -n "$NS" create -f "$WORK/external-ips-svc.yaml" >"$WORK/deny-external-ips.txt" 2>&1; then
  echo "user created a ClusterIP Service with externalIPs"; fail=1
else
  echo "$(cat "$WORK/deny-external-ips.txt")"
  grep -q "may not set externalIPs" "$WORK/deny-external-ips.txt" || fail=1
fi

log "admission: a plain ClusterIP Service is still admitted"
# The control for the externalIPs rule: the grant's own peer Service is a
# ClusterIP, so an ordinary one must not be refused along with it.
cat >"$WORK/plain-svc.yaml" <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: clusterip-probe
spec:
  type: ClusterIP
  selector:
    app: unblessed
  ports:
    - port: 6881
      targetPort: 6881
EOF
if $K --as="$SA" -n "$NS" create -f "$WORK/plain-svc.yaml" >"$WORK/allow-plain-svc.txt" 2>&1; then
  echo "plain ClusterIP Service admitted"
else
  echo "$(cat "$WORK/allow-plain-svc.txt")"; fail=1
fi
$K -n "$NS" delete svc clusterip-probe --ignore-not-found >/dev/null

log "admission: the user cannot reach the params that define the grant"
# can-i exits 1 for "no", which is the answer here; set -e must not see it.
ssar_params=$($K auth can-i patch configmaps -n s5n-system --as="$SA" 2>/dev/null | tail -1 || true)
echo "patch configmaps in s5n-system: ${ssar_params}"
[ "$ssar_params" = no ] || fail=1
if $K --as="$SA" -n s5n-system patch cm "s5n-grant-${NS}" --type merge \
  -p '{"data":{"blessedRepositories":"docker.io/library/evil"}}' >"$WORK/patch.txt" 2>&1; then
  echo "user patched the params"; fail=1
else
  echo "$(cat "$WORK/patch.txt")"
  grep -q "Forbidden" "$WORK/patch.txt" || fail=1
fi
for res in validatingadmissionpolicies validatingadmissionpolicybindings; do
  can=$($K auth can-i update "${res}.admissionregistration.k8s.io" --as="$SA" 2>/dev/null | tail -1 || true)
  echo "update ${res}: ${can}"; [ "$can" = no ] || fail=1
done
# Exactly one seedbox is bound, and its params are that seedbox's: the policy
# is tenancy, not a cluster-wide image allowlist.
bindings=$($K get validatingadmissionpolicybindings -o jsonpath='{range .items[*]}{.spec.paramRef.name}{"\n"}{end}')
echo "bindings:"; echo "$bindings"
[ "$(echo "$bindings" | grep -c .)" = 2 ] || fail=1
echo "$bindings" | grep -qx "s5n-grant-${NS}" || fail=1

log "admission: an unbound namespace still admits an unblessed image"
$K -n default apply -f "$WORK/unblessed.yaml" >/dev/null || fail=1
$K -n default get deploy unblessed >/dev/null || fail=1
$K -n default delete deploy unblessed --wait=false >/dev/null || fail=1

log "admission: the pod already running survives; the next one does not"
phase=$($K -n "$NS" get pod -l app=unblessed -o jsonpath='{.items[0].status.phase}')
echo "pod admitted before the binding: ${phase}"
[ "$phase" = Running ] || fail=1
if $K -n "$NS" rollout restart deploy/unblessed >"$WORK/restart.txt" 2>&1; then
  echo "rollout restart of an unblessed Deployment was admitted"; fail=1
else
  echo "$(cat "$WORK/restart.txt")"
  grep -q "not blessed by the chart" "$WORK/restart.txt" || fail=1
fi
# Wait for the pod to actually be gone: a terminating pod still counts, and
# the ReplicaSet's replacement attempt is what must fail.
$K -n "$NS" delete pod -l app=unblessed --wait=true --timeout=120s >/dev/null
sleep 5
left=$($K -n "$NS" get pod -l app=unblessed --no-headers | wc -l)
echo "pods the ReplicaSet could recreate: ${left}"
[ "$left" = 0 ] || fail=1
replica_failure=$($K -n "$NS" get rs -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="ReplicaFailure")].message}{"\n"}{end}' | grep -c "not blessed by the chart" || true)
echo "ReplicaSets reporting the denial: ${replica_failure}"
[ "$replica_failure" -ge 1 ] || fail=1
$K -n "$NS" delete deploy unblessed --ignore-not-found >/dev/null

# The claims, as the plain-helm operator creates them: the chart creates no
# storage, and its defaults are the names below. A static hostPath PV bound to
# exactly one claim, and the claim pinned to it, with an empty storageClassName
# so no provisioner is involved. On a real host this is `box init`'s job (box
# writes the same objects); here it is the operator's, which is the point of
# this script.
log "creating the storage claims (the chart creates none)"
$K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s5n-data
spec:
  capacity: {storage: 10Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  volumeMode: Filesystem
  claimRef: {namespace: ${NS}, name: s5n-data}
  hostPath: {path: /srv/s5n/data, type: Directory}
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: s5n-config
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  volumeMode: Filesystem
  claimRef: {namespace: ${NS}, name: s5n-config}
  hostPath: {path: /srv/s5n/config, type: Directory}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s5n-data
  namespace: ${NS}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: s5n-data
  resources: {requests: {storage: 10Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s5n-config
  namespace: ${NS}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: s5n-config
  resources: {requests: {storage: 1Gi}}
EOF
for pvc in s5n-data s5n-config; do
  phase=$($K -n "$NS" get pvc "$pvc" -o jsonpath='{.status.phase}')
  echo "  claim ${pvc}: ${phase}"; [ "$phase" = Bound ] || fail=1
done

# The chart under test, with the locally built image instead of the published one.
install_release() {
  $HELM upgrade --install s5n charts/s5n -n "$NS" \
    --set wiring.image.tag=e2e \
    --wait --timeout 15m
}

log "installing s5n (the wiring Job runs as the post-install hook)"
install_release
# The claims are not release objects: the chart must not own a PV or a PVC, or
# `helm uninstall` would be a data-loss command.
$HELM get manifest s5n -n "$NS" >"$WORK/manifest.yaml"
if grep -q '^kind: PersistentVolume' "$WORK/manifest.yaml"; then
  echo "the chart's release manifest owns a PersistentVolume"; fail=1
fi
if grep -q '^kind: PersistentVolumeClaim' "$WORK/manifest.yaml"; then
  echo "the chart's release manifest owns a PersistentVolumeClaim"; fail=1
fi
echo "chart objects: $(grep -c '^kind: ' "$WORK/manifest.yaml"), none a PV or PVC"

log "asserting"
$K -n "$NS" get pods -o wide
# Only the apps: the wiring Job's pod shares the instance label and ends Completed,
# which never satisfies Ready.
$K -n "$NS" wait --for=condition=Ready pod -l 'app.kubernetes.io/instance=s5n,s5n.io/app' --timeout=300s || fail=1

# `kubectl debug` adds containers through the pods/ephemeralcontainers
# subresource: an UPDATE a rule for "pods" does not match. The policy sees the
# whole Pod there, so the target must already be blessed in every container.
# sonarr-0 is; a pod admitted before the binding that runs an unblessed image
# could not be debugged at all. Strategic merge is what kubectl debug sends and
# appends by container name, so a rerun on a --keep cluster stays a no-op.
log "admission: the user cannot add an unblessed image as an ephemeral container"
pod=sonarr-0
echo "ephemeral container target: ${pod} ($($K -n "$NS" get pod "$pod" -o jsonpath='{.status.phase}'))"
cat >"$WORK/ephemeral-unblessed.json" <<'EOF'
{"spec":{"ephemeralContainers":[{"name":"debugger-deny","image":"docker.io/library/busybox:1.36","command":["sleep","3600"],"securityContext":{"allowPrivilegeEscalation":false,"runAsNonRoot":true,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}]}}
EOF
if $K --as="$SA" -n "$NS" patch pod "$pod" --subresource=ephemeralcontainers --type=strategic \
  -p "$(cat "$WORK/ephemeral-unblessed.json")" >"$WORK/deny-ephemeral.txt" 2>&1; then
  echo "user added an unblessed ephemeral container"; fail=1
else
  echo "$(cat "$WORK/deny-ephemeral.txt")"
  grep -q "not blessed by the chart" "$WORK/deny-ephemeral.txt" || fail=1
  # The denial must name the ephemeral image, not just the pod's own container:
  # that is the proof this path is actually evaluated.
  grep -q "busybox:1.36" "$WORK/deny-ephemeral.txt" || fail=1
fi

log "admission: a blessed image through the same subresource is admitted"
# The denied PATCH persisted nothing, so this appends the first ephemeral
# container. The image is the one this e2e built and loaded, under the
# repository the chart's s5n variant names.
cat >"$WORK/ephemeral-blessed.json" <<'EOF'
{"spec":{"ephemeralContainers":[{"name":"debugger-allow","image":"ghcr.io/swizzin/s5n:e2e","command":["/usr/local/bin/s5n","version"],"securityContext":{"allowPrivilegeEscalation":false,"runAsNonRoot":true,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}]}}
EOF
if $K --as="$SA" -n "$NS" patch pod "$pod" --subresource=ephemeralcontainers --type=strategic \
  -p "$(cat "$WORK/ephemeral-blessed.json")" >"$WORK/allow-ephemeral.txt" 2>&1; then
  got=$($K -n "$NS" get pod "$pod" \
    -o jsonpath='{range .spec.ephemeralContainers[*]}{.name}={.image}{"\n"}{end}' |
    grep '^debugger-allow=' || true)
  echo "ephemeral image admitted: ${got}"
  [ "$got" = "debugger-allow=ghcr.io/swizzin/s5n:e2e" ] || fail=1
else
  echo "$(cat "$WORK/allow-ephemeral.txt")"; fail=1
fi

for r in $($K -n "$NS" get httproute -o name); do
  acc=$($K -n "$NS" get "$r" -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}')
  echo "$r Accepted=$acc"; [ "$acc" = True ] || fail=1
done

# The Gateway's address is MetalLB's, assigned through Envoy Gateway's proxy Service.
GW_ADDR=""
for i in $(seq 1 60); do
  GW_ADDR=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)
  [ -n "$GW_ADDR" ] && break
  sleep 5
done
[ -n "$GW_ADDR" ] || { echo "Gateway never got an address"; fail=1; }
echo "Gateway address: ${GW_ADDR}"

# Through the Gateway from the host, on its LoadBalancer address.
for path in /sonarr/ping /radarr/ping /prowlarr/ping; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://${GW_ADDR}${path}")
  echo "GET ${path} via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
done
code=$(curl -s -o /dev/null -w '%{http_code}' "http://${GW_ADDR}/qbittorrent/")
echo "GET /qbittorrent/ via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
# Plex's /identity answers unauthenticated whether or not it is claimed, and
# Seerr's /api/v1/settings/public answers before any admin user exists —
# both are exactly what makes them usable readiness/reachability checks here.
code=$(curl -s -o /dev/null -w '%{http_code}' "http://${GW_ADDR}/plex/identity")
echo "GET /plex/identity via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
# The regression test for the no-auth hole: Envoy connects to Plex from its own
# pod IP, inside the cluster pod CIDR. If Plex trusted a network range covering
# that (PLEX_NO_AUTH_NETWORKS), this library call would answer 200 with full
# admin for anyone who can reach the Gateway. It must be 401.
code=$(curl -s -o /dev/null -w '%{http_code}' "http://${GW_ADDR}/plex/library/sections")
echo "GET /plex/library/sections via gateway -> ${code} (want 401)"
[ "$code" = 401 ] || { echo "Plex served /library/sections as ${code} through the Gateway: unauthenticated access"; fail=1; }
code=$(curl -s -o /dev/null -w '%{http_code}' "http://${GW_ADDR}/seerr/api/v1/settings/public")
echo "GET /seerr/api/v1/settings/public via gateway -> ${code}"; [ "$code" = 200 ] || fail=1

# The peers Service is a NodePort on the node's own address (how a single-IP box
# runs: the Gateway holds the LoadBalancer address, peers use the node port), so a
# TCP connect from the host to <node>:31111 must succeed.
peer_type=$($K -n "$NS" get svc qbittorrent-peers -o jsonpath='{.spec.type}/{.spec.ports[0].nodePort}')
echo "peers Service: ${peer_type}"; [ "$peer_type" = NodePort/31111 ] || fail=1
PEER_ADDR=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER}-control-plane")
echo "node address: ${PEER_ADDR}"
[ -n "$PEER_ADDR" ] || fail=1
if python3 - "$PEER_ADDR" <<'PY'
import socket, sys
s = socket.create_connection((sys.argv[1], 31111), timeout=10)
s.close()
print("TCP", sys.argv[1], "31111 -> connected")
PY
then
  :
else
  echo "TCP ${PEER_ADDR}:31111 -> REFUSED/TIMEOUT"; fail=1
fi

# Hardlink invariant: downloads/ and media/ are one filesystem, as seen by an Arr pod.
$K -n "$NS" exec sonarr-0 -- sh -c '
  set -e
  f=/data/downloads/tv/.s5n-linkcheck; l=/data/media/tv/.s5n-linkcheck
  rm -f "$f" "$l"; echo x > "$f"; ln "$f" "$l"
  n=$(stat -c %h "$l"); d1=$(stat -c %d "$f"); d2=$(stat -c %d "$l")
  rm -f "$f" "$l"
  echo "hardlink nlink=$n dev=$d1/$d2"; [ "$n" = 2 ] && [ "$d1" = "$d2" ]' || fail=1

# NetworkPolicy: the release policies exist, prowlarr may reach sonarr, and a pod
# outside the release may not.
for p in s5n-default-deny s5n-egress s5n-wiring-egress; do
  if $K -n "$NS" get networkpolicy "$p" >/dev/null 2>&1; then
    echo "policy $p: present"
  else
    echo "policy $p: MISSING"; fail=1
  fi
done
$K -n "$NS" exec prowlarr-0 -- curl -s -o /dev/null -w 'prowlarr->sonarr %{http_code}\n' --max-time 5 http://sonarr:8989/sonarr/ping || fail=1
if $K -n default run np-probe --rm -i --restart=Never --image=curlimages/curl:8.11.1 --quiet -- \
     curl -s -o /dev/null --max-time 5 "http://sonarr.${NS}:8989/sonarr/ping"; then
  echo "outsider->sonarr: REACHABLE (policy not enforced)"; fail=1
else
  echo "outsider->sonarr: blocked"
fi

# ---- wiring -------------------------------------------------------------------
status_field() { # status_field <python expression over `d`>
  $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.status\.json}' |
    python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

arr_port() { $K -n "$NS" get svc "$1" -o jsonpath='{.spec.ports[0].port}'; }
arr_key() { $K -n "$NS" get secret s5n-keys -o jsonpath="{.data.$1-apikey}" | base64 -d; }

# Snapshot everything the wiring Job is allowed to change, so a second run can be
# compared field for field rather than trusted. One file per endpoint; free space is
# scrubbed by state-diff.py because it moves for reasons that are not configuration.
snapshot() { # snapshot <dir>
  local dir=$1 key port app
  mkdir -p "$dir"
  for app in sonarr radarr; do
    key=$(arr_key "$app"); port=$(arr_port "$app")
    for path in downloadclient rootfolder config/mediamanagement; do
      $K -n "$NS" exec "${app}-0" -- curl -s -H "X-Api-Key: $key" \
        "http://localhost:${port}/${app}/api/v3/${path}" >"$dir/${app}-${path//\//-}.json"
    done
  done
  key=$(arr_key prowlarr); port=$(arr_port prowlarr)
  $K -n "$NS" exec prowlarr-0 -- curl -s -H "X-Api-Key: $key" \
    "http://localhost:${port}/prowlarr/api/v1/applications" >"$dir/prowlarr-applications.json"
  # qBittorrent categories, using the seeded WebUI credential: this is also the
  # proof that the hash the init container wrote is one qBittorrent accepts.
  local qpw
  qpw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' | base64 -d)
  $K -n "$NS" exec qbittorrent-0 -- sh -c "curl -s -c /tmp/s5n-jar -o /dev/null -X POST \
    --data-urlencode 'username=admin' --data-urlencode 'password=$qpw' \
    http://localhost:8080/api/v2/auth/login && curl -s -b /tmp/s5n-jar http://localhost:8080/api/v2/torrents/categories" \
    >"$dir/qbittorrent-categories.json"
  # Tautulli's settings, using the seeded api_key: also the proof that the
  # tautulli-config init container's seed is one Tautulli actually accepts. The
  # PMS section is legitimately empty here: this e2e supplies no Plex token, so
  # the link stays pending rather than being pointed at Plex tokenless.
  local tpw
  tpw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.tautulli-apikey}' | base64 -d)
  $K -n "$NS" exec tautulli-0 -- curl -s \
    "http://localhost:8181/api/v2?apikey=${tpw}&cmd=get_settings" >"$dir/tautulli-settings.json"
}

log "wiring run 1: asserts the release is actually connected"
$K -n "$NS" wait --for=condition=complete job/s5n-wiring --timeout=600s || fail=1
first_status=$(status_field 'd["status"]')
echo "wiring status = ${first_status}"
[ "$first_status" = ok ] || { fail=1; $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.summary\.txt}'; }

for expected in \
  sonarr/downloadclient sonarr/rootfolder sonarr/hardlinks \
  radarr/downloadclient radarr/rootfolder radarr/hardlinks \
  prowlarr/application/sonarr prowlarr/application/radarr \
  qbittorrent/data-contract qbittorrent/category/tv qbittorrent/category/movies \
  data/hardlink/tv data/hardlink/movies \
  plex/ready tautulli/ready seerr/ready; do
  res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$expected'][0]" 2>/dev/null || echo missing)
  echo "  ${expected}: ${res}"; [ "$res" = ok ] || fail=1
done

# No claim token and no Plex server token are given anywhere in this e2e, so
# Plex stays unclaimed, Tautulli has nothing to authenticate its PMS link with,
# and Seerr has no signed-in admin: all three rows must say so in the receipt
# rather than failing the release (AGENTS.md's "Next work" item 2). The receipt
# never claims a PMS link it was given no token for.
for pending in plex/claim tautulli/plex seerr/plex; do
  res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$pending'][0]" 2>/dev/null || echo missing)
  echo "  ${pending}: ${res} (expected pending: no Plex token in this e2e)"
  [ "$res" = pending ] || fail=1
done
claimed=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='plex/ready'][0]")
echo "  plex/ready detail: ${claimed}"
case "$claimed" in *claimed=false*) ;; *) echo "expected an unclaimed Plex in this e2e"; fail=1 ;; esac

snapshot "$WORK/before"

log "wiring run 2: an unchanged release must produce zero writes"
install_release
$K -n "$NS" wait --for=condition=complete job/s5n-wiring --timeout=600s || fail=1
second_status=$(status_field 'd["status"]')
created=$(status_field 'd["writes"]["created"]')
updated=$(status_field 'd["writes"]["updated"]')
echo "wiring status = ${second_status} (created=${created}, updated=${updated})"
[ "$second_status" = ok ] || fail=1
[ "$created" = 0 ] || { echo "second run created ${created} objects"; fail=1; }
[ "$updated" = 0 ] || { echo "second run updated ${updated} objects"; fail=1; }

snapshot "$WORK/after"
if python3 hack/state-diff.py "$WORK/before" "$WORK/after" >"$WORK/wiring.diff"; then
  echo "app state after run 2: $(cat "$WORK/wiring.diff")"
else
  echo "app state after run 2: CHANGED"; head -40 "$WORK/wiring.diff"; fail=1
fi

# ---- wiring run 3: narrow ownership ---------------------------------------
# Edit live objects through the app APIs the same way a human would, then
# prove the next install respects ownership: a rename and a non-owned field
# survive, an owned field drifts and is restored (and the receipt says so), a
# deleted owned object is recreated (and the receipt names the old id), and an
# object the Job never created is left alone.
log "wiring run 3: rename, drift, delete and a user-added object"
python3 hack/e2e-run3.py setup "$CTX" "$NS" "$WORK" || fail=1

install_release
$K -n "$NS" wait --for=condition=complete job/s5n-wiring --timeout=600s || fail=1
$K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.status\.json}' >"$WORK/status-run3.json"
third_status=$(status_field 'd["status"]')
echo "wiring status = ${third_status}"
[ "$third_status" = ok ] || fail=1

if ! python3 hack/e2e-run3.py assert "$CTX" "$NS" "$WORK"; then
  fail=1
fi

if [ "$fail" != 0 ]; then
  log "wiring Job logs"
  $K -n "$NS" logs job/s5n-wiring --tail=80 || true
fi

if [ "$fail" = 0 ]; then log "PASS"; else log "FAIL"; fi
[ "$KEEP" = --keep ] || $KIND delete cluster --name "$CLUSTER"
exit "$fail"
