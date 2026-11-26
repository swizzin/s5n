#!/usr/bin/env bash
# The chart's acceptance test on kind: cluster -> Envoy Gateway -> MetalLB -> s5n
# chart. Asserts pods Ready, routes Accepted, every app answering through the Gateway's
# LoadBalancer address (and Plex refusing an unauthenticated library call there), the
# peers NodePort accepting a TCP connect, the hardlink invariant on /data, and the
# wiring Job twice: the first run connects everything, the second changes nothing.
# Usage: hack/e2e-kind.sh [--keep]
#
# It takes one of the machine-wide slots hack/e2e-box.sh's scenarios take, for its
# whole life, so a fourth run waits instead of meeting another.
set -euo pipefail
cd "$(dirname "$0")/.."

# Every host-side name carries an id derived from this checkout, so it never meets
# another worktree's run (or hack/e2e-box.sh's scenarios). On CI the id only lengthens
# the names: nothing reads them.
E2E_ID=${E2E_ID:-$(printf '%s' "$PWD" | sha256sum | cut -c1-6)}

KIND=${KIND:-kind}
CLUSTER="s5n-${E2E_ID}"
NS=seedbox
# Plex and Seerr are served at plex.<domain> and seerr.<domain> (their UIs write
# absolute asset paths), and every request through the Gateway names the domain: the
# routes are host-matched, so a release needs a domain to render at all.
DOMAIN=${DOMAIN:-box.example.test}
PLEX_HOST="plex.${DOMAIN}"
SEERR_HOST="seerr.${DOMAIN}"
# PeerBanHelper's WebUI calls /api/... from the site root, so it has a host too.
PBH_HOST="pbh.${DOMAIN}"
# Jellyfin's players are given a server address, so it has a name of its own.
# APP_LOGIN is the account its first user is named after: this script installs with
# Helm, so the value is hack/e2e-values.yaml's.
JELLYFIN_HOST="jellyfin.${DOMAIN}"
APP_LOGIN="e2e"
# Envoy Gateway's chart ships its own Gateway API CRDs (crds.enabled), the exact set
# the pinned version supports, so nothing applies the standard channel separately.
#
# The platform versions are the chart's: charts/s5n/platform.yaml, printed by
# hack/versions through the parser box uses, so this e2e installs what the release
# carries and the host needs no YAML parser. MetalLB's manifest tag has a leading "v"
# and its chart pin has none, so the tag is derived from the one pin.
pins=$(go run ./hack/versions)
pin() { printf '%s\n' "$pins" | sed -n "s/^$1=//p"; }
v_tag() { case "$1" in v*) printf '%s' "$1" ;; *) printf 'v%s' "$1" ;; esac; }
for pin_key in k0s envoyGateway metallb certManager; do
  [ -n "$(pin "$pin_key")" ] || {
    echo "charts/s5n/platform.yaml has no ${pin_key} pin"
    exit 1
  }
done
ENVOY_GATEWAY_CHART_VERSION=${ENVOY_GATEWAY_CHART_VERSION:-$(pin envoyGateway)}
METALLB_VERSION=${METALLB_VERSION:-$(v_tag "$(pin metallb)")}
CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-$(pin certManager)}

# The qBittorrent variants, read from the chart's own file rather than copied here:
# Renovate bumps charts/s5n/variants.yaml only. The shape is the file's fixed one
# (repository, tag, digest inside one entry), which is why no YAML parser is needed.
# An entry name is matched as a whole line, never a prefix: `lt1.2` and `lt1.2-10g`
# are both real, and a prefix match would hand one entry's pin to the other.
variant_field() { # <entry> <field> -> that entry's value, quotes stripped
  awk -v entry="$1" -v want="$2" '
    /^[A-Za-z]/ { inqbt = ($0 ~ /^qbittorrent:/); invar = 0; next }
    inqbt && $0 == "  " entry ":" { invar = 1; next }
    inqbt && /^  [^[:space:]]/ { invar = 0 }
    invar && $1 == want ":" {
      line = $0
      sub(/^[^:]*:[[:space:]]*/, "", line)
      gsub(/"/, "", line)
      print line
      exit
    }
  ' charts/s5n/variants.yaml
}
qbit_repository=$(variant_field lt1.2 repository)
qbit_tag=$(variant_field lt1.2 tag)
qbit_digest=$(variant_field lt1.2 digest)
if [ -z "$qbit_repository" ] || [ -z "$qbit_tag" ] || [ -z "$qbit_digest" ]; then
  echo "charts/s5n/variants.yaml: qbittorrent.lt1.2 needs all three of repository, tag and digest (read '${qbit_repository}' '${qbit_tag}' '${qbit_digest}')"
  exit 1
fi
# Two entries that share a prefix must stay two, or every assertion tests the wrong build.
if [ "$(variant_field lt1.2 tag)" = "$(variant_field lt1.2-10g tag)" ]; then
  echo "charts/s5n/variants.yaml: lt1.2 and lt1.2-10g read back the same entry ('$(variant_field lt1.2 tag)')"
  exit 1
fi
# The variant NAMED lt1.2: a values file that falls back to the default must still fail below.
QBIT_LT12="${qbit_repository}:${qbit_tag}@${qbit_digest}"

# Test-only images, pinned by tag and digest where Renovate watches them
# (.github/renovate.json); the tag stays in the reference so a bump is one line.
CURL_IMAGE=${CURL_IMAGE:-docker.io/curlimages/curl:8.23.0@sha256:3fb7e13e5d179a845c97d78204f3f1064d1907037f26a3ebf91710571151fd93}  # renovate: datasource=docker
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-docker.io/library/busybox:1.38@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4}    # renovate: datasource=docker
# Pebble 2.10.3, the ACME test server: the same image hack/e2e-box.sh pins.
PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:2.10.3@sha256:0c81f05db371f436c97fb894bd6d2ab8fc9c8e2ce40f8bef8a8e15941bcd75ef}  # renovate: datasource=docker
KEEP=${1:-}
CTX="kind-${CLUSTER}"
K="kubectl --context ${CTX}"
HELM="helm --kube-context ${CTX}"
# Built under the chart's repository, so the release uses the local image by tag. The
# tag carries the checkout's id: the docker store is the machine's.
IMAGE_TAG="e2e-${E2E_ID}"
IMAGE="ghcr.io/swizzin/s5n:${IMAGE_TAG}"
WORK=$(mktemp -d)
# The last line of every log is the run's own answer, whatever the cleanup says.
trap 'code=$?; rm -rf "$WORK"; printf "E2E EXIT: %s\n" "$code"' EXIT

log() {
  # The seconds into the run, from bash's own counter, as hack/e2e-box.sh's lines have them.
  printf '\n==> %s  [+%ss]\n' "$*" "$SECONDS"
}
fail=0

# ---- the machine's slots -----------------------------------------------------
# One of the same slots hack/e2e-box.sh's scenarios take, for this script's whole life.
# E2E_SLOTS overrides the count (three measured well on this machine), the files live in
# the user's cache so every checkout shares them, and the slot is what queues: there is
# no flock, and the id in every name keeps two runs apart.
E2E_SLOTS=${E2E_SLOTS:-3}
case "$E2E_SLOTS" in
  '' | *[!0-9]* | 0)
    echo "E2E_SLOTS=${E2E_SLOTS} is not a number of slots" >&2
    exit 2
    ;;
esac
SLOT_DIR=${S5N_E2E_SLOT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/s5n-e2e/slots}

# take_slot, called once: locks the first free slot file and keeps the descriptor open,
# which holds the slot until this process exits. A wait does not count in the run's own
# seconds.
take_slot() {
  local i fd started=$SECONDS waited=""
  mkdir -p "$SLOT_DIR"
  while :; do
    for i in $(seq 1 "$E2E_SLOTS"); do
      exec {fd}>"$SLOT_DIR/slot-${i}.lock"
      if flock -n "$fd"; then
        SLOT_FD=$fd
        [ -z "$waited" ] || SECONDS=$started
        return 0
      fi
      exec {fd}>&-
    done
    if [ -z "$waited" ]; then
      log "all ${E2E_SLOTS} e2e slots are taken: waiting (${SLOT_DIR})"
      waited=1
    fi
    sleep 5
  done
}

# One of the machine's slots for the whole life of this run.
take_slot

# kind writes its context into KUBECONFIG and `kind delete cluster` drops it without
# restoring the previous one, so the e2e brings its own file and kind, kubectl and helm
# all read it -- ~/.kube/config is never opened, and a kept cluster stays reachable.
KUBECONFIG=${S5N_E2E_KUBECONFIG:-${XDG_CACHE_HOME:-$HOME/.cache}/s5n-e2e/${E2E_ID}/kind/kubeconfig}
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
  # --name as well as the config's own name: the id this run adds to CLUSTER has
  # to be the cluster kind creates, or every later --name "$CLUSTER" (load,
  # export, delete) looks for a cluster that does not exist.
  $KIND create cluster --name "$CLUSTER" --config hack/kind.yaml --wait 120s
fi

if [ -n "${S5N_E2E_PREBUILT:-}" ]; then
  # The directory hack/e2e-box.sh all builds into and passes its scenarios: it
  # holds the image's tag, not the image, so this only refuses a directory with
  # no tag or another one. This script needs nothing else from it.
  prebuilt_tag=$(cat "${S5N_E2E_PREBUILT}/image" 2>/dev/null || true)
  [ "$prebuilt_tag" = "$IMAGE" ] || {
    echo "S5N_E2E_PREBUILT=${S5N_E2E_PREBUILT} carries image '${prebuilt_tag}', not ${IMAGE}"
    exit 1
  }
  log "loading the s5n image all built once (wiring Job + qBittorrent credential seed)"
else
  log "building the s5n image (wiring Job + qBittorrent credential seed)"
  docker build -q -t "$IMAGE" .
fi
$KIND load docker-image --name "$CLUSTER" "$IMAGE"

# Only what `box init` creates on a real host, repeated inside the node because
# this host is not the node: the two volume roots and one /config/<instance>
# directory per enabled app (setup.settingsDirs). The data categories are
# deliberately absent — downloads/<category> and media/<category> are the wiring
# Job's, and pre-creating them would hide a first install that cannot make the
# root folders its Arr steps need.
log "preparing host paths inside the node (what box init creates on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,lidarr,bazarr,bazarr-4k,sabnzbd,filebrowser,plex,tautulli,seerr,jellyfin,qui,autobrr,maintainerr,pbh}
  chown -R 1000:1000 /srv/s5n'

# Nothing below may prepare the data categories: the wiring Job owns them, and a first
# install that cannot create its own root folders is the failure this e2e catches.
for d in /srv/s5n/data/downloads /srv/s5n/data/media; do
  if docker exec "${CLUSTER}-control-plane" test -e "$d"; then
    echo "${d} already exists before the install: the e2e prepared a path the wiring Job must create"; fail=1
  fi
done

log "Envoy Gateway ${ENVOY_GATEWAY_CHART_VERSION} as the Gateway (installs Gateway API CRDs)"
$HELM upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version "$ENVOY_GATEWAY_CHART_VERSION" -n envoy-gateway-system --create-namespace \
  --wait --timeout 5m >/dev/null

log "MetalLB ${METALLB_VERSION} (L2) so LoadBalancer Services get real addresses"
$K apply -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml" >/dev/null
$K -n metallb-system rollout status deploy/controller --timeout=180s
$K -n metallb-system rollout status daemonset/speaker --timeout=180s
# The pool below passes MetalLB's validating webhook; wait for its endpoints first.
for i in $(seq 1 30); do
  ep=$($K -n metallb-system get endpoints metallb-webhook-service -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)
  [ -n "$ep" ] && break
  sleep 2
done

# A small L2 pool from the top of the kind subnet, so the host reaches LoadBalancer
# addresses directly. Every kind cluster on this machine announces on that bridge, so
# the block is chosen by this checkout's id: two clusters on one block would answer
# each other's ARP.
kind_subnet=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -v ':' | head -1)
pool=$(python3 - "$kind_subnet" "${E2E_ID}:kind" <<'PY'
import hashlib, ipaddress, sys
hosts = list(ipaddress.ip_network(sys.argv[1], strict=False).hosts())
block = int(hashlib.sha256(sys.argv[2].encode()).hexdigest()[:8], 16) % 32
ips = hosts[-8 * (block + 1):len(hosts) - 8 * block]
print(f"{ips[0]}-{ips[-1]}")
PY
)
echo "  MetalLB pool: ${pool}"
# An endpoint address only says the pod is Ready; the webhook can still be loading its
# certificate (seen in CI), so the apply is retried until it answers.
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
# Root installs the policies; `box adduser` binds them to one namespace with that user's
# grant as params (root-owned s5n-system, never the user's namespace). The e2e stands in
# for both, then acts as the user through a ServiceAccount with a namespace Role.
log "platform layer: the grant's admission policies"
$K apply -f install/platform/admission/policy.yaml >/dev/null
# A --keep re-run starts unbound, so the "predates the binding" step is true again.
$K delete validatingadmissionpolicybinding "s5n-grant-${NS}" "s5n-grant-${NS}-service" --ignore-not-found >/dev/null
$K -n "$NS" delete deploy unblessed --ignore-not-found >/dev/null

# One pod admitted before any binding exists, to answer what happens to what is already
# running: nothing, since admission only sees new writes. Kept out of the app selectors.
cat >"$WORK/unblessed.yaml" <<EOF
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
          image: ${CURL_IMAGE}
          command: ["sleep", "3600"]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF

# A second copy under another name: `create` of an existing object answers AlreadyExists,
# which is not the denial under test.
sed 's/name: unblessed/name: admission-probe/; s/app: unblessed/app: admission-probe/g' \
  "$WORK/unblessed.yaml" >"$WORK/probe.yaml"

log "a pod that predates the binding (admission never re-checks running pods)"
$K -n "$NS" apply -f "$WORK/unblessed.yaml" >/dev/null
$K -n "$NS" wait --for=condition=Available deploy/unblessed --timeout=180s || fail=1

log "bind the policy to ${NS} with the grant as params (root-owned s5n-system)"
# The renderer is Go and reads the chart's variants.yaml, the same file the templates use.
go run ./hack/render-grant charts/s5n "$NS" 30080 NodePort | $K apply -f - >/dev/null

# A binding takes effect a moment after it is created, so this waits for a denial. A
# dry-run create is a real admission request that persists nothing.
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

# The control: the chart blesses two qBittorrent repositories, and the params are
# generated from variants.yaml, so the second must reach this namespace or a user could
# not run an image the chart ships. A server-side dry run creates nothing.
log "admission: the user can run both blessed qBittorrent repositories"
sed -e 's/name: unblessed/name: blessed-qbittorrent/' -e 's/app: unblessed/app: blessed-qbittorrent/g' \
  -e "s#${CURL_IMAGE}#${QBIT_LT12}#" \
  "$WORK/unblessed.yaml" >"$WORK/blessed-qbittorrent.yaml"
if $K --as="$SA" -n "$NS" create --dry-run=server -f "$WORK/blessed-qbittorrent.yaml" >"$WORK/allow-qbittorrent.txt" 2>&1; then
  echo "the lt1.2 qBittorrent image is admitted"
else
  echo "$(cat "$WORK/allow-qbittorrent.txt")"; fail=1
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
# The control for the externalIPs rule: an ordinary ClusterIP must not be refused.
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
# Exactly one seedbox is bound: the policy is tenancy, not a cluster-wide allowlist.
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
# A terminating pod still counts, and the ReplicaSet's replacement attempt is what must fail.
$K -n "$NS" delete pod -l app=unblessed --wait=true --timeout=120s >/dev/null
sleep 5
left=$($K -n "$NS" get pod -l app=unblessed --no-headers | wc -l)
echo "pods the ReplicaSet could recreate: ${left}"
[ "$left" = 0 ] || fail=1
replica_failure=$($K -n "$NS" get rs -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="ReplicaFailure")].message}{"\n"}{end}' | grep -c "not blessed by the chart" || true)
echo "ReplicaSets reporting the denial: ${replica_failure}"
[ "$replica_failure" -ge 1 ] || fail=1
$K -n "$NS" delete deploy unblessed --ignore-not-found >/dev/null

# Before the claims exist, a render that can see the cluster must refuse the release by
# name rather than install pods that wait for storage forever.
log "asserting a release without its claims is refused at render"
noclaim=$($HELM install s5n charts/s5n -n "$NS" -f hack/e2e-values.yaml --set gateway.host="$DOMAIN" --set wiring.image.tag="$IMAGE_TAG" --dry-run=server 2>&1) && {
  echo "a release whose claims do not exist rendered"; fail=1; } || true
if grep -qF 'storage.data.existingClaim: PersistentVolumeClaim "s5n-data" does not exist in namespace "seedbox"' <<<"$noclaim"; then
  echo "  refused: missing claim s5n-data"
else
  echo "the missing-claim refusal was not printed:"; echo "$noclaim" | tail -5; fail=1
fi

# The claims, as a plain-helm operator creates them: the chart creates no storage, so a
# static hostPath PV bound to exactly one claim, the claim pinned to it, and an empty
# storageClassName. On a real host this is `box init`'s job.
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
# The PV controller binds a pre-bound pair on its next pass, not inside the apply.
$K -n "$NS" wait --for=jsonpath='{.status.phase}'=Bound pvc/s5n-data pvc/s5n-config --timeout=60s >/dev/null 2>&1 || true
for pvc in s5n-data s5n-config; do
  phase=$($K -n "$NS" get pvc "$pvc" -o jsonpath='{.status.phase}')
  echo "  claim ${pvc}: ${phase}"; [ "$phase" = Bound ] || fail=1
done

# The catalog's opt-in apps, switched on for the assertions below: they default off, and
# each one's own serving path is proven in hack/e2e-box.sh.
cat >"$WORK/apps-values.yaml" <<'EOF'
apps:
  lidarr:
    enabled: true
  # Both Bazarr instances: the 4K one reads radarr-4k alone, because this install
  # switches on the 4K Radarr and not the 4K Sonarr (hack/e2e-values.yaml). The Job turns
  # the missing side's switch off rather than pointing at a Service this release lacks.
  bazarr:
    enabled: true
    instances:
      - { suffix: "", enabled: true }
      - { suffix: 4k, enabled: true }
  qui:
    enabled: true
  autobrr:
    enabled: true
  maintainerr:
    enabled: true
  pbh:
    enabled: true
  sabnzbd:
    enabled: true
  filebrowser:
    enabled: true
EOF

# The chart under test, with the locally built image instead of the published one.
install_release() {
  # hack/e2e-values.yaml carries that image and one 4K instance named the way an
  # existing seedbox may already use (upper case, a qBittorrent subcategory level), so
  # every install drives a nested category through qBittorrent, both /data trees and an
  # Arr root folder.
  $HELM upgrade --install s5n charts/s5n -n "$NS" \
    -f hack/e2e-values.yaml \
    -f "$WORK/apps-values.yaml" \
    --set gateway.host="$DOMAIN" \
    --set wiring.image.tag="$IMAGE_TAG" \
    --wait --timeout 15m
}

log "installing s5n (the wiring Job runs as the post-install hook)"
install_release
# The claims are not release objects: a PV or PVC in the manifest would make `helm
# uninstall` a data-loss command.
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
# Only the apps: the wiring Job's pod shares the instance label and ends Completed.
$K -n "$NS" wait --for=condition=Ready pod -l 'app.kubernetes.io/instance=s5n,s5n.io/app' --timeout=300s || fail=1

# The variant this run tests: hack/e2e-values.yaml selects lt1.2. A values file that
# lost the line, or names another blessed build, would leave every assertion below green
# against the default image, so the pod spec and the runtime's own imageID are read back.
qbit_variant=$QBIT_LT12
qbit_spec=$($K -n "$NS" get pod qbittorrent-0 -o jsonpath='{.spec.containers[?(@.name=="qbittorrent")].image}')
qbit_id=$($K -n "$NS" get pod qbittorrent-0 -o jsonpath='{.status.containerStatuses[?(@.name=="qbittorrent")].imageID}')
echo "qBittorrent runs: ${qbit_spec}"
echo "            image: ${qbit_id}"
[ "$qbit_spec" = "$qbit_variant" ] ||
  { echo "qBittorrent is specified as ${qbit_spec}, want the lt1.2 variant ${qbit_variant}: hack/e2e-values.yaml did not select it"; fail=1; }
case "$qbit_id" in
  *"${qbit_variant##*@}"*) ;;
  *) echo "the running qBittorrent is ${qbit_id}, not the digest the lt1.2 variant pins"; fail=1 ;;
esac

# `kubectl debug` writes pods/ephemeralcontainers, a subresource a rule for "pods" does
# not match; the policy sees the whole Pod there, so the target must already be blessed in
# every container. sonarr-0 is, and a pod admitted before the binding could not be
# debugged at all. Strategic merge appends by name, so a --keep re-run is a no-op.
log "admission: the user cannot add an unblessed image as an ephemeral container"
pod=sonarr-0
echo "ephemeral container target: ${pod} ($($K -n "$NS" get pod "$pod" -o jsonpath='{.status.phase}'))"
cat >"$WORK/ephemeral-unblessed.json" <<EOF
{"spec":{"ephemeralContainers":[{"name":"debugger-deny","image":"${BUSYBOX_IMAGE}","command":["sleep","3600"],"securityContext":{"allowPrivilegeEscalation":false,"runAsNonRoot":true,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}]}}
EOF
if $K --as="$SA" -n "$NS" patch pod "$pod" --subresource=ephemeralcontainers --type=strategic \
  -p "$(cat "$WORK/ephemeral-unblessed.json")" >"$WORK/deny-ephemeral.txt" 2>&1; then
  echo "user added an unblessed ephemeral container"; fail=1
else
  echo "$(cat "$WORK/deny-ephemeral.txt")"
  grep -q "not blessed by the chart" "$WORK/deny-ephemeral.txt" || fail=1
  # The denial must name the ephemeral image, which proves this path was evaluated.
  grep -qF "${BUSYBOX_IMAGE%%@*}" "$WORK/deny-ephemeral.txt" || fail=1
fi

log "admission: a blessed image through the same subresource is admitted"
# The denied PATCH persisted nothing, so this appends the first ephemeral container.
cat >"$WORK/ephemeral-blessed.json" <<EOF
{"spec":{"ephemeralContainers":[{"name":"debugger-allow","image":"${IMAGE}","command":["/usr/local/bin/s5n","version"],"securityContext":{"allowPrivilegeEscalation":false,"runAsNonRoot":true,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}]}}
EOF
if $K --as="$SA" -n "$NS" patch pod "$pod" --subresource=ephemeralcontainers --type=strategic \
  -p "$(cat "$WORK/ephemeral-blessed.json")" >"$WORK/allow-ephemeral.txt" 2>&1; then
  got=$($K -n "$NS" get pod "$pod" \
    -o jsonpath='{range .spec.ephemeralContainers[*]}{.name}={.image}{"\n"}{end}' |
    grep '^debugger-allow=' || true)
  echo "ephemeral image admitted: ${got}"
  [ "$got" = "debugger-allow=${IMAGE}" ] || fail=1
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

# Every request below goes through the Gateway by name, over HTTPS: the routes are
# host-matched and once this host has a certificate port 80 only redirects.
gw_get() {
  curl -sS --resolve "${DOMAIN}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" "$@"
}

# An app's own API is read through a port-forward: its ingress policy admits the Gateway
# and the wiring Job alone, and the Job's image is distroless. A port-forward is the API
# server talking to kubelet, which no policy applies to, and it arrives on 127.0.0.1 --
# a secure context for curl. It is also the only way to ask qBittorrent, whose blessed
# lt1.2 build ships no curl.
port_forward() { # port_forward <pod> <container-port>
  local pod=$1 port=$2 i
  PF_PORT=$(python3 -c 'import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
  $K -n "$NS" port-forward "pod/${pod}" "${PF_PORT}:${port}" >"$WORK/pf-${pod}.log" 2>&1 &
  PF_PID=$!
  for i in $(seq 1 30); do
    if curl -sS -o /dev/null --max-time 2 "http://127.0.0.1:${PF_PORT}/" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "port-forward to ${pod}:${port} never answered"
  sed 's/^/    /' "$WORK/pf-${pod}.log" 2>/dev/null || true
  return 1
}

port_forward_stop() {
  kill "$PF_PID" 2>/dev/null || true
  wait "$PF_PID" 2>/dev/null || true
}

# ---- the other blessed qBittorrent builds ------------------------------------
# The chart blesses eight entries: the default image, six of the project's own builds and
# the former name of one of those. lt1.2 is asserted above; the three below are switched
# in one at a time, because a variant that is only blessed is a promise nobody keeps.
# Each is proven three ways: the pod spec names variants.yaml's repository, tag and
# digest, the runtime's imageID carries the digest, and qBittorrent's own buildInfo
# reports the libtorrent line the variant's name promises. The two Zen 3 builds are the
# exemption, explained in their block in variants.yaml.
for spec in lt2.1:2.1. lt1.2-10g:1.2. lt2.1-10g:2.1.; do
  variant=${spec%%:*}; line=${spec##*:}
  v_repository=$(variant_field "$variant" repository)
  v_tag=$(variant_field "$variant" tag)
  v_digest=$(variant_field "$variant" digest)
  if [ -z "$v_repository" ] || [ -z "$v_tag" ] || [ -z "$v_digest" ]; then
    echo "charts/s5n/variants.yaml: qbittorrent.${variant} needs all three of repository, tag and digest (read '${v_repository}' '${v_tag}' '${v_digest}')"
    fail=1
    continue
  fi
  want="${v_repository}:${v_tag}@${v_digest}"
  log "qBittorrent variant ${variant}: it rolls, and it reports libtorrent ${line}x"
  if ! $HELM upgrade s5n charts/s5n -n "$NS" --reuse-values \
    --set apps.qbittorrent.image.variant="$variant" --wait --timeout 15m \
    >"$WORK/qbit-${variant}.upgrade" 2>&1; then
    echo "helm upgrade to the ${variant} variant failed:"
    sed 's/^/    /' "$WORK/qbit-${variant}.upgrade"
    fail=1
    continue
  fi
  if ! $K -n "$NS" rollout status statefulset/qbittorrent --timeout=300s; then
    echo "qbittorrent-0 is not Ready on the ${variant} variant"
    fail=1
    continue
  fi
  got_spec=$($K -n "$NS" get pod qbittorrent-0 -o jsonpath='{.spec.containers[?(@.name=="qbittorrent")].image}' || true)
  got_id=$($K -n "$NS" get pod qbittorrent-0 -o jsonpath='{.status.containerStatuses[?(@.name=="qbittorrent")].imageID}' || true)
  echo "  runs: ${got_spec}"
  echo "  image: ${got_id}"
  [ "$got_spec" = "$want" ] ||
    { echo "qBittorrent on ${variant} is specified as '${got_spec}', want '${want}'"; fail=1; }
  case "$got_id" in
    *"${v_digest}"*) ;;
    *) echo "the running qBittorrent is '${got_id}', not the digest the ${variant} variant pins"; fail=1 ;;
  esac
  build_info=""
  if port_forward qbittorrent-0 8080; then
    build_info=$(curl -s "http://127.0.0.1:${PF_PORT}/api/v2/app/buildInfo" 2>/dev/null || true)
  else
    echo "could not reach qBittorrent to read its buildInfo on the ${variant} variant"
    fail=1
  fi
  port_forward_stop
  echo "  buildInfo: ${build_info}"
  # `libtorrent` is the linked library's version string (1.2.20.0, 2.1.2.0): its prefix
  # is what a variant promises.
  got_line=$(printf '%s' "$build_info" |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("libtorrent", ""))' 2>/dev/null || true)
  echo "  libtorrent ${got_line:-<not reported>} (want ${line}x)"
  case "$got_line" in
    "${line}"*) ;;
    *) echo "the ${variant} variant reports libtorrent '${got_line}', want ${line}x"; fail=1 ;;
  esac
done

# Back to the variant this run installed: a kept cluster stays usable by
# hack/grab-test.sh, and a failure above does not leave the release on a build the rest
# of the script never expected.
if $HELM upgrade s5n charts/s5n -n "$NS" --reuse-values \
  --set apps.qbittorrent.image.variant=lt1.2 --wait --timeout 15m \
  >"$WORK/qbit-lt1.2.upgrade" 2>&1; then
  $K -n "$NS" rollout status statefulset/qbittorrent --timeout=300s ||
    { echo "qbittorrent-0 did not come back up on the lt1.2 variant"; fail=1; }
  back=$($K -n "$NS" get pod qbittorrent-0 -o jsonpath='{.spec.containers[?(@.name=="qbittorrent")].image}' || true)
  echo "qBittorrent back on lt1.2: ${back}"
  [ "$back" = "$QBIT_LT12" ] ||
    { echo "qBittorrent did not return to the lt1.2 variant ('${back}')"; fail=1; }
else
  echo "helm upgrade back to the lt1.2 variant failed:"
  sed 's/^/    /' "$WORK/qbit-lt1.2.upgrade"
  fail=1
fi

# ---- TLS: one certificate per host, Pebble issuing over HTTP-01 --------------
# All three hosts are certified the way box does after an apply: Pebble is the ACME
# server, cert-manager solves HTTP-01 through a host's own port-80 listener, and each
# certificate chains to Pebble's CA. hack/issue-cert is the same
# platform.CertificateDecision box runs; this script installs with Helm.
patch_coredns() { # patch_coredns <gateway-address> <host>...
  local addr=$1; shift
  $K -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' >"$WORK/Corefile" || return 1
  python3 - "$WORK/Corefile" "$addr" "$@" <<'PYEOF' || return 1
import sys
path, addr, hosts = sys.argv[1], sys.argv[2], sys.argv[3:]
text = open(path).read()
block = "    hosts {\n"
for host in hosts:
    if host in text:
        continue
    block += "        " + addr + " " + host + "\n"
block += "        fallthrough\n    }\n"
marker = "    forward . /etc/resolv.conf"
if marker not in text:
    raise SystemExit("the CoreDNS Corefile has no forward block to patch")
text = text.replace(marker, block + marker, 1)
open(path, "w").write(text)
PYEOF
  $K -n kube-system create cm coredns --from-file=Corefile="$WORK/Corefile" --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K -n kube-system rollout restart deploy/coredns >/dev/null
  $K -n kube-system rollout status deploy/coredns --timeout=120s >/dev/null
}

# pebble_nodeports: Pebble's two nodePorts, acme then management, from the bottom of the
# static band, checked free first -- a pin in the upper band can collide with a nodePort
# the API server already gave another Service. A Service already there keeps its own.
pebble_nodeports() {
  local existing used p out=()
  existing=$($K -n pebble get svc pebble -o jsonpath='{.spec.ports[*].nodePort}' 2>/dev/null || true)
  if [ "$(wc -w <<<"$existing")" -eq 2 ]; then echo "$existing"; return 0; fi
  used=" $($K get svc -A -o jsonpath='{.items[*].spec.ports[*].nodePort}' 2>/dev/null) "
  for p in $(seq 30085 -1 30000); do
    case "$used" in *" $p "*) continue ;; esac
    out+=("$p")
    [ "${#out[@]}" -eq 2 ] && break
  done
  [ "${#out[@]}" -eq 2 ] || { echo "no two free nodePorts in 30000-30085 for Pebble" >&2; return 1; }
  echo "${out[@]}"
}

install_pebble() {
  log "Pebble 2.10.1 (the ACME test server) in the cluster, pinned by digest"
  read -r PEBBLE_ACME_PORT PEBBLE_MGMT_PORT < <(pebble_nodeports) || { echo "Pebble has no nodePorts"; fail=1; return 0; }
  pebble_node_ip=$($K get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  $K create namespace pebble --dry-run=client -o yaml | $K apply -f - >/dev/null
  cat <<EOF | $K apply -f - >/dev/null
apiVersion: v1
kind: ConfigMap
metadata:
  name: pebble
  namespace: pebble
data:
  pebble-config.json: |
    {
      "pebble": {
        "listenAddress": "0.0.0.0:14000",
        "managementListenAddress": "0.0.0.0:15000",
        "certificate": "/test/certs/localhost/cert.pem",
        "privateKey": "/test/certs/localhost/key.pem",
        "httpPort": 80,
        "tlsPort": 5001,
        "ocspResponderURL": "",
        "externalAccountBindingRequired": false
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pebble
  namespace: pebble
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pebble
  template:
    metadata:
      labels:
        app: pebble
    spec:
      containers:
        - name: pebble
          image: ${PEBBLE_IMAGE}
          args: ["-config", "/config/pebble-config.json"]
          ports:
            - {containerPort: 14000, name: acme}
            - {containerPort: 15000, name: management}
          readinessProbe:
            tcpSocket: {port: 14000}
            initialDelaySeconds: 2
          volumeMounts:
            - {name: config, mountPath: /config, readOnly: true}
      volumes:
        - name: config
          configMap:
            name: pebble
---
apiVersion: v1
kind: Service
metadata:
  name: pebble
  namespace: pebble
spec:
  type: NodePort
  selector:
    app: pebble
  ports:
    # Both are pinned, from the static band (pebble_nodeports).
    - {name: acme, port: 14000, targetPort: 14000, nodePort: ${PEBBLE_ACME_PORT}}
    - {name: management, port: 15000, targetPort: 15000, nodePort: ${PEBBLE_MGMT_PORT}}
EOF
  $K -n pebble rollout status deploy/pebble --timeout=180s || { echo "Pebble never rolled out"; fail=1; return 0; }
  for i in $(seq 1 60); do
    if curl -k -fsS "https://${pebble_node_ip}:${PEBBLE_MGMT_PORT}/roots/0" -o "$WORK/pebble-root.pem" 2>/dev/null; then
      break
    fi
    if [ "$i" = 60 ]; then
      echo "Pebble never served its root CA"; fail=1; return 0
    fi
    sleep 2
  done
  echo "  Pebble root CA in $WORK/pebble-root.pem"
}

log "Pebble and CoreDNS: the box's names answer where the Gateway is"
install_pebble
[ "$fail" = 0 ] || { log "FAIL"; exit 1; }
patch_coredns "$GW_ADDR" "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" "$PBH_HOST" "$JELLYFIN_HOST" ||
  { echo "could not patch CoreDNS for the box's names"; fail=1; }

log "cert-manager ${CERT_MANAGER_VERSION} and root's issuers, pointed at Pebble"
$HELM upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --version "$CERT_MANAGER_VERSION" -n cert-manager --create-namespace \
  --set crds.enabled=true --set config.gatewayAPI.enabled=true --wait --timeout 5m >/dev/null
$K apply -f install/platform/cert-manager/issuers.yaml >/dev/null
# The only test-only change to root's issuer is the ACME directory; skipTLSVerify is
# Pebble's own self-signed management certificate.
$K patch clusterissuer s5n-letsencrypt --type=merge \
  -p '{"spec":{"acme":{"server":"https://pebble.pebble.svc:14000/dir","skipTLSVerify":true}}}' >/dev/null

# The box's own name first: every app path is reached through it and the routes are
# host-matched, so there is no plain-HTTP rendering to fall back on.
for spec in "box:${DOMAIN}" "plex:${PLEX_HOST}" "seerr:${SEERR_HOST}" "pbh:${PBH_HOST}" "jellyfin:${JELLYFIN_HOST}"; do
  app=${spec%%:*}; host=${spec##*:}
  log "the ${app} host: certificate and listeners for ${host}"
  go run ./hack/issue-cert --kubeconfig "$KUBECONFIG" --host "$host" --issuer letsencrypt || fail=1
done

# Through the Gateway from the host, on its LoadBalancer address.
for path in /sonarr/ping /radarr/ping /prowlarr/ping /lidarr/ping; do
  code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}${path}")
  echo "GET ${path} via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
done

# The login is at the edge (docs/box.md, "Logins"), so every Arr runs External auth: its
# UI and /initialize.json answer with no credential and /api stays API-key-gated. Forms
# with no user is a lockout, and through the Gateway that is a 302 to /login.
for spec in sonarr:api/v3 radarr:api/v3 prowlarr:api/v1 lidarr:api/v1; do
  app=${spec%%:*}; api=${spec##*:}
  : >"$WORK/ui-headers"
  code=$(gw_get -s -D "$WORK/ui-headers" -o "$WORK/ui-body" -w '%{http_code}' "https://${DOMAIN}/${app}/" || true)
  loc=$(grep -i '^location:' "$WORK/ui-headers" | tr -d '\r' | awk '{print $2}' || true)
  echo "GET /${app}/ via gateway -> ${code} location='${loc}'"
  [ "$code" = 200 ] || { echo "/${app}/ did not serve the UI: ${code}"; fail=1; }
  case "$loc" in *login*) echo "/${app}/ redirected to ${loc}: the app is asking for a login of its own"; fail=1;; esac
  grep -qi '<!doctype html' "$WORK/ui-body" || { echo "/${app}/ answered ${code} without an HTML document"; fail=1; }
  if grep -qi 'login?returnUrl' "$WORK/ui-body"; then
    echo "/${app}/ served a login page instead of the app"; fail=1
  fi

  # The API gate must be untouched -- no key 401, the Secret's key 200 -- and the status
  # body says which method the app booted with, so a typo in the env name cannot pass.
  code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/${app}/${api}/system/status" || true)
  echo "GET /${app}/${api}/system/status without a key -> ${code} (want 401)"
  [ "$code" = 401 ] || { echo "${app} answered its API without a key: ${code}"; fail=1; }
  apikey=$($K -n "$NS" get secret s5n-keys -o jsonpath="{.data.${app}-apikey}" | base64 -d || true)
  status=$(gw_get -s -w '\n%{http_code}' -H "X-Api-Key: ${apikey}" "https://${DOMAIN}/${app}/${api}/system/status" || true)
  code=${status##*$'\n'}; body=${status%$'\n'*}
  method=$(printf '%s' "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("authentication"))' 2>/dev/null || true)
  echo "GET /${app}/${api}/system/status with the key -> ${code} authentication=${method}"
  [ "$code" = 200 ] || { echo "${app} refused its own API key: ${code}"; fail=1; }
  [ "$method" = external ] || { echo "${app} is not running External auth: ${method}"; fail=1; }
done
code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/sonarr/initialize.json" || true)
echo "GET /sonarr/initialize.json via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/qbittorrent/")
echo "GET /qbittorrent/ via gateway -> ${code}"; [ "$code" = 200 ] || fail=1

# qBittorrent's WebUI skips its login for the private ranges and loopback (docs/box.md,
# "Logins"), which is what lets every app that calls it hold no credential. A 403 here
# would mean the app still asks for a password.
#
# The request comes over a port-forward, the lt1.2 build shipping no curl: it dials
# localhost inside the pod's network namespace, so qBittorrent sees 127.0.0.1, which is
# the loopback bypass. The private-range half is the whitelist read below and the Arrs'
# and qui's own calls, made from their pods by the wiring Job.
port_forward qbittorrent-0 8080 || { echo "could not reach qBittorrent"; fail=1; }
qbit_prefs=$(curl -s "http://127.0.0.1:${PF_PORT}/api/v2/app/preferences" 2>/dev/null || true)
port_forward_stop
printf '%s' "$qbit_prefs" >"$WORK/qbittorrent-preferences.json"
if python3 - "$WORK/qbittorrent-preferences.json" <<'PYPREFS'
import json, sys
want = "10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, fc00::/7"
try:
    prefs = json.load(open(sys.argv[1], encoding="utf-8"))
except Exception as exc:
    print("  qBittorrent's preferences are not JSON (%s): the WebUI still wants a login" % exc)
    sys.exit(1)
ok = True
print("  bypass_local_auth=%r whitelist_enabled=%r whitelist=%r"
      % (prefs.get("bypass_local_auth"), prefs.get("bypass_auth_subnet_whitelist_enabled"),
         prefs.get("bypass_auth_subnet_whitelist")))
if prefs.get("bypass_local_auth") is not True:
    print("  loopback still needs a login: kubectl port-forward would ask for one")
    ok = False
if prefs.get("bypass_auth_subnet_whitelist_enabled") is not True:
    print("  the subnet whitelist is off: every pod that calls qBittorrent would need a password")
    ok = False
# The API answers the list newline-joined (appcontroller.cpp:362).
got = [line.strip() for line in str(prefs.get("bypass_auth_subnet_whitelist") or "").split("\n") if line.strip()]
if ", ".join(got) != want:
    print("  whitelist = %r, want %r" % (got, want.split(", ")))
    ok = False
sys.exit(0 if ok else 1)
PYPREFS
then :; else fail=1; fi

# The same three keys in the file, where qBittorrent reads them: a QSettings QStringList,
# comma-separated and unquoted (a quoted value is one bogus subnet).
qbit_conf=$($K -n "$NS" exec qbittorrent-0 -- sh -c 'grep -E "^(AuthSubnetWhitelist|AuthSubnetWhitelistEnabled|LocalHostAuth)=" /config/qBittorrent/qBittorrent.conf' 2>/dev/null || true)
echo "  qBittorrent.conf: $(printf '%s' "$qbit_conf" | tr '\n' ' ')"
printf '%s\n' "$qbit_conf" | grep -qxF "LocalHostAuth=false" ||
  { echo "qBittorrent.conf does not skip the login for loopback"; fail=1; }
printf '%s\n' "$qbit_conf" | grep -qxF "AuthSubnetWhitelistEnabled=true" ||
  { echo "qBittorrent.conf does not enable the subnet whitelist"; fail=1; }
printf '%s\n' "$qbit_conf" | grep -qxF "AuthSubnetWhitelist=10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, fc00::/7" ||
  { echo "qBittorrent.conf does not carry the five private ranges in QSettings' list form"; fail=1; }
# ---- apps with a host of their own ------------------------------------------
# Plex and Seerr are not under a path: their route is <app>.<domain>, their own listener
# serves the certificate Pebble issued, and their UI's absolute asset paths resolve from
# the site root -- which is why they cannot live under a prefix.
host_get() { # host_get <host> <path> <output-file-or-/dev/null>
  curl -sS --resolve "${1}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
    -o "$3" -w '%{http_code}' "https://${1}${2}" || true
}
for spec in "plex:${PLEX_HOST}" "seerr:${SEERR_HOST}" "pbh:${PBH_HOST}" "jellyfin:${JELLYFIN_HOST}"; do
  app=${spec%%:*}; host=${spec##*:}
  label=${host//./-}
  cert="s5n-tls-${label}"
  listener=$($K -n s5n-gateway get gateway s5n \
    -o jsonpath="{.spec.listeners[?(@.name==\"https-${label}\")].tls.certificateRefs[0].name}" || true)
  http_listener=$($K -n s5n-gateway get gateway s5n \
    -o jsonpath="{.spec.listeners[?(@.name==\"http-${label}\")].hostname}" || true)
  route_host=$($K -n "$NS" get httproute "$app" -o jsonpath='{.spec.hostnames[0]}' || true)
  # Every app's route is one catch-all rule on its own host; Jellyfin's route carries no
  # rule the others do not.
  route_path=$($K -n "$NS" get httproute "$app" -o jsonpath='{.spec.rules[-1:].matches[0].path.value}' || true)
  echo "  ${host}: route ${route_host}${route_path}, listener https-${label} -> ${listener}, http-${label} on ${http_listener}"
  [ "$route_host" = "$host" ] || { echo "the ${app} route is on ${route_host}, want its own host"; fail=1; }
  [ "$route_path" = / ] || { echo "the ${app} route's path is ${route_path}, want /"; fail=1; }
  [ "$listener" = "$cert" ] || { echo "the ${app} HTTPS listener references ${listener}, want ${cert}"; fail=1; }
  [ "$http_listener" = "$host" ] || { echo "the ${app} port-80 listener matches ${http_listener}, want ${host}"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s ||
    { echo "the ${app} host's Certificate never became Ready"; fail=1; }
  cert_issuer=$($K -n s5n-gateway get certificate "$cert" -o jsonpath='{.spec.issuerRef.name}' || true)
  [ "$cert_issuer" = s5n-letsencrypt ] ||
    { echo "the ${app} host's Certificate names issuer ${cert_issuer}, want the Pebble-backed production issuer"; fail=1; }
done

# Plex's /identity answers unauthenticated whether claimed or not, and Seerr's
# /api/v1/settings/public answers before any admin exists: both work as checks.
code=$(host_get "$PLEX_HOST" /identity /dev/null)
echo "GET https://${PLEX_HOST}/identity -> ${code}"; [ "$code" = 200 ] || fail=1
# The regression test for the no-auth hole: Envoy connects from its own pod IP, inside
# the pod CIDR, so if Plex trusted that range this library call would answer 200 with
# full admin. It must be 401.
code=$(host_get "$PLEX_HOST" /library/sections /dev/null)
echo "GET https://${PLEX_HOST}/library/sections -> ${code} (want 401)"
[ "$code" = 401 ] || { echo "Plex served /library/sections as ${code} through the Gateway: unauthenticated access"; fail=1; }

# Seerr's page, its absolute assets, and no path route left behind. Its root is a
# redirect of its own, fetched the way a browser gets it.
code=$(curl -sS -L --resolve "${SEERR_HOST}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
  -o "$WORK/seerr.html" -w '%{http_code}' "https://${SEERR_HOST}/" || true)
echo "GET https://${SEERR_HOST}/ (following Seerr's own redirect) -> ${code}"
[ "$code" = 200 ] || { echo "Seerr's own host did not serve its UI: ${code}"; fail=1; }
grep -qi '<!doctype html' "$WORK/seerr.html" || { echo "${SEERR_HOST} answered ${code} without an HTML document"; fail=1; }
asset=$(grep -oE '(src|href)="/[^"]+\.(js|css)"' "$WORK/seerr.html" | head -1 |
  sed 's/^[a-z]*="//; s/"$//' || true)
echo "  asset the page references: ${asset}"
case "$asset" in
  "/"*) ;;
  "") echo "${SEERR_HOST} referenced no script or stylesheet"; fail=1 ;;
  *) echo "the asset ${asset} is not an absolute path: Seerr is not served from its own root"; fail=1 ;;
esac
if [ -n "$asset" ]; then
  code=$(host_get "$SEERR_HOST" "$asset" /dev/null)
  echo "GET https://${SEERR_HOST}${asset} -> ${code}"; [ "$code" = 200 ] || fail=1
fi
code=$(host_get "$SEERR_HOST" /api/v1/settings/public /dev/null)
echo "GET https://${SEERR_HOST}/api/v1/settings/public -> ${code}"; [ "$code" = 200 ] || fail=1
# The old path is gone from the box's own host: /seerr finds no route.
code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/seerr/" || true)
echo "GET https://${DOMAIN}/seerr/ -> ${code} (want 404)"
[ "$code" = 404 ] || { echo "/seerr still answers ${code} on the box's host: a path route was left behind"; fail=1; }

# Port 80 for the box's own name is a redirect; each app host's port-80 listener exists
# for the challenge and the redirect.
for host in "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" "$PBH_HOST" "$JELLYFIN_HOST"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --resolve "${host}:80:${GW_ADDR}" "http://${host}/" || true)
  echo "GET http://${host}/ -> ${code} (want 301)"
  [ "$code" = 301 ] || { echo "${host} is not redirecting port 80 to HTTPS"; fail=1; }
done

# ---- Jellyfin: its own accounts, a closed first-run window, real addresses ---
# Jellyfin keeps its accounts, keys and library paths in data/jellyfin.db, so nothing can
# seed it before it starts: the chart's startup-wizard init container completes the
# first-run wizard inside the pod, on localhost only. The person's password is the one
# box made and showed once.
jf_user_pw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.jellyfin-user-password}' | base64 -d || true)
jf_box_pw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.jellyfin-box-password}' | base64 -d || true)
[ -n "$jf_user_pw" ] || { echo "the release Secret has no jellyfin-user-password: the chart did not make one"; fail=1; }
[ -n "$jf_box_pw" ] || { echo "the release Secret has no jellyfin-box-password"; fail=1; }

# The init container's record is the proof: it succeeded, it bound its server to
# loopback, and the app never had the person's password mounted.
jf_init_exit=$($K -n "$NS" get pod jellyfin-0 -o jsonpath='{.status.initContainerStatuses[?(@.name=="startup-wizard")].state.terminated.exitCode}' || true)
echo "startup-wizard init container exit: ${jf_init_exit}"
[ "$jf_init_exit" = 0 ] || { echo "the wizard's init container did not succeed (${jf_init_exit}): the app should not be serving"; fail=1; }
jf_init_log=$($K -n "$NS" logs jellyfin-0 -c startup-wizard 2>/dev/null || true)
printf '%s\n' "$jf_init_log" | grep -q 'startup wizard: complete' ||
  { echo "the init container has no 'startup wizard: complete' line: the wizard was not closed in the pod"; fail=1; }
printf '%s\n' "$jf_init_log" | grep -q 'Kestrel is listening on 127.0.0.1' ||
  { echo "the init container's server did not bind loopback: a pod-network peer could have reached the open wizard"; fail=1; }
jf_main_mounts=$($K -n "$NS" get pod jellyfin-0 -o jsonpath='{range .spec.containers[?(@.name=="jellyfin")]}{range .volumeMounts[*]}{.name}{"\n"}{end}{end}' || true)
if printf '%s\n' "$jf_main_mounts" | grep -qxF keys; then
  echo "the app container mounts the person's Jellyfin password"
  fail=1
fi
jf_init_mounts=$($K -n "$NS" get pod jellyfin-0 -o jsonpath='{range .spec.initContainers[?(@.name=="startup-wizard")]}{range .volumeMounts[*]}{.name}{"\n"}{end}{end}' || true)
printf '%s\n' "$jf_init_mounts" | grep -qxF keys ||
  { echo "the wizard's init container does not mount the person's password"; fail=1; }

# The host serves the app from its own root; its public info is both the readiness probe
# and the wizard's own answer.
code=$(host_get "$JELLYFIN_HOST" /System/Info/Public "$WORK/jf-info.json")
echo "GET https://${JELLYFIN_HOST}/System/Info/Public -> ${code}"
[ "$code" = 200 ] || fail=1
grep -q '"StartupWizardCompleted":true' "$WORK/jf-info.json" ||
  { echo "Jellyfin reports the wizard incomplete: the init container did not finish it"; fail=1; }
code=$(curl -sS -L --resolve "${JELLYFIN_HOST}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
  -o "$WORK/jellyfin.html" -w '%{http_code}' "https://${JELLYFIN_HOST}/" || true)
echo "GET https://${JELLYFIN_HOST}/ -> ${code}"
[ "$code" = 200 ] || { echo "Jellyfin's own host did not serve its UI: ${code}"; fail=1; }
grep -qi '<!doctype html' "$WORK/jellyfin.html" || { echo "${JELLYFIN_HOST} answered without an HTML document"; fail=1; }

# Every Jellyfin call carries the app's client header, a session's token in its Token
# parameter: a 12.4 server reads no other.
jf_client='MediaBrowser Client="s5n", Device="e2e", DeviceId="e2e", Version="1.0"'
jf_signin() { # jf_signin <user> <password>: the token of a session, empty when refused
  curl -sS --max-time 30 --resolve "${JELLYFIN_HOST}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
    -H "Authorization: ${jf_client}" -H 'Content-Type: application/json' \
    -d "{\"Username\":\"$1\",\"Pw\":\"$2\"}" \
    "https://${JELLYFIN_HOST}/Users/AuthenticateByName" 2>/dev/null |
    python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("AccessToken") or "")
except Exception: print("")' || true
}
jf_api() { # jf_api <token> <curl args...>: one authenticated call through the Gateway
  local token=$1; shift
  curl -sS --max-time 30 --resolve "${JELLYFIN_HOST}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
    -H "Authorization: ${jf_client}, Token=\"${token}\"" "$@"
}

jf_user_token=$(jf_signin "$APP_LOGIN" "$jf_user_pw")
echo "POST /Users/AuthenticateByName as ${APP_LOGIN} -> ${jf_user_token:+a session}"
[ -n "$jf_user_token" ] || { echo "${APP_LOGIN} cannot sign in to Jellyfin with the password the release made"; fail=1; }
jf_box_token=$(jf_signin s5n "$jf_box_pw")
[ -n "$jf_box_token" ] || { echo "the wiring Job's own Jellyfin account cannot sign in"; fail=1; }

# The automation account's key, minted once: the proof the Job reached the API as an
# administrator.
jf_api "$jf_box_token" "https://${JELLYFIN_HOST}/Auth/Keys" >"$WORK/jf-keys.json" || true
grep -q '"AppName":"s5n"' "$WORK/jf-keys.json" ||
  { echo "the Job minted no API key named s5n"; fail=1; }
jf_key=$(python3 - "$WORK/jf-keys.json" <<'PYKEY' || true
import json, sys
try:
    rows = json.load(open(sys.argv[1])).get("Items") or []
except Exception:
    rows = []
print(next((r.get("AccessToken") or "" for r in rows if r.get("AppName") == "s5n"), ""))
PYKEY
)
[ -n "$jf_key" ] || { echo "the s5n key has no token"; fail=1; }

# The libraries, one per Arr instance, over the Arrs' own directories: the
# administrator's own endpoint, which the old edge rule took away, must answer 200.
jf_libs_code=$(jf_api "$jf_user_token" -o "$WORK/jf-libs.json" -w '%{http_code}' \
  "https://${JELLYFIN_HOST}/Library/VirtualFolders" || true)
echo "GET /Library/VirtualFolders as ${APP_LOGIN} -> ${jf_libs_code}"
[ "$jf_libs_code" = 200 ] ||
  { echo "the person cannot manage their own libraries: /Library/VirtualFolders answered ${jf_libs_code}"; fail=1; }
for dir in /data/media/movies /data/media/4K/Movies /data/media/tv; do
  grep -q "\"${dir}\"" "$WORK/jf-libs.json" ||
    { echo "Jellyfin has no library over ${dir}: the Job's library step did not run"; fail=1; }
done
# And the box's own key lists them too, where an API key belongs.
port_forward jellyfin-0 8096 || fail=1
: >"$WORK/jf-key-libs.json"
curl -sS --max-time 30 -H "Authorization: ${jf_client}, Token=\"${jf_key}\"" \
  -o "$WORK/jf-key-libs.json" -w '%{http_code}' "http://127.0.0.1:${PF_PORT}/Library/VirtualFolders" >"$WORK/jf-key-libs.code" || true
port_forward_stop
echo "GET /Library/VirtualFolders as the s5n key -> $(cat "$WORK/jf-key-libs.code")"
[ "$(cat "$WORK/jf-key-libs.code")" = 200 ] ||
  { echo "the box key could not list the libraries: $(cat "$WORK/jf-key-libs.code")"; fail=1; }

# KnownProxies. Through the Gateway the peer is the proxy's pod, in the pod network, so
# a server that trusts nothing ignores X-Forwarded-For and records that pod as the
# client: every request reads as its own network, and the endpoints Jellyfin answers a
# local caller without a credential are open to whoever reaches the Gateway. The proof
# is SessionInfo.RemoteEndPoint, the test host's address, outside the proxy network. The
# list is read rather than toggled: the app reads it at startup only.
jf_user_token=$(jf_signin "$APP_LOGIN" "$jf_user_pw")
jf_api "$jf_user_token" "https://${JELLYFIN_HOST}/System/Configuration/network" >"$WORK/jf-network.json" || true
jf_api "$jf_user_token" "https://${JELLYFIN_HOST}/Sessions" >"$WORK/jf-sessions.json" || true
python3 - "$WORK/jf-network.json" "$WORK/jf-sessions.json" "$APP_LOGIN" <<'PYNET' || fail=1
import ipaddress, json, sys
try:
    network = json.load(open(sys.argv[1]))
    sessions = json.load(open(sys.argv[2]))
except Exception as exc:
    print("  Jellyfin's own report did not parse: %s" % exc)
    sys.exit(1)
proxies = network.get("KnownProxies") or network.get("knownProxies") or []
if not proxies:
    print("  KnownProxies is empty: the app is not told whose address to trust")
    sys.exit(1)
trusted = [ipaddress.ip_network(p, strict=False) for p in proxies]
rows = [s for s in sessions if (s.get("UserName") or s.get("userName")) == sys.argv[3]]
if not rows:
    print("  the app has no session for %s: nothing recorded the client's address" % sys.argv[3])
    sys.exit(1)
row = max(rows, key=lambda s: s.get("DateCreated") or s.get("dateCreated") or "")
remote = (row.get("RemoteEndPoint") or row.get("remoteEndPoint") or "").rsplit(":", 1)[0]
if not remote:
    print("  the session has no RemoteEndPoint")
    sys.exit(1)
addr = ipaddress.ip_address(remote)
if any(addr in net for net in trusted):
    print("  the server recorded the client as %s, inside the proxy network it trusts (%s): the forwarded address was ignored"
          % (remote, ", ".join(proxies)))
    sys.exit(1)
print("  KnownProxies: %s; the client it recorded: %s (outside them)" % (", ".join(proxies), remote))
PYNET

# The first-run window as an outsider sees it: past the wizard, every once-anonymous call
# is the app's own refusal. The spellings matter -- Gateway API path matching is
# case-sensitive, ASP.NET's is not.
for spelling in /Startup/User /startup/user /STARTUP/USER; do
  code=$(curl -sS --max-time 30 -X POST -H 'Content-Type: application/json' \
    -d '{"Name":"intruder","Password":"intruder-password"}' -o /dev/null -w '%{http_code}' \
    --resolve "${JELLYFIN_HOST}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
    "https://${JELLYFIN_HOST}${spelling}" || true)
  echo "POST https://${JELLYFIN_HOST}${spelling} from outside -> ${code} (want 401/403)"
  case "$code" in
    401|403) ;;
    *) echo "an outsider's ${spelling} was not refused by Jellyfin (${code})"; fail=1 ;;
  esac
done
code=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' \
  --resolve "${JELLYFIN_HOST}:443:${GW_ADDR}" --cacert "$WORK/pebble-root.pem" \
  "https://${JELLYFIN_HOST}/Environment/Drives" || true)
echo "GET https://${JELLYFIN_HOST}/Environment/Drives unauthenticated -> ${code} (want 401/403)"
case "$code" in
  401|403) ;;
  *) echo "an outsider browsed the server's filesystem (/Environment/Drives answered ${code})"; fail=1 ;;
esac
[ -z "$(jf_signin intruder intruder-password)" ] ||
  { echo "an outsider claimed Jellyfin: /Startup/User created a sign-in"; fail=1; }
[ -n "$(jf_signin "$APP_LOGIN" "$jf_user_pw")" ] ||
  { echo "the person cannot sign in to Jellyfin after the claim attempt"; fail=1; }

# ---- Jellyfin: a start that repairs a killed wizard's loopback --------------
# The wizard binds its server to 127.0.0.1 by writing LocalNetworkAddresses into
# network.xml, and a trap does not run on SIGKILL: an OOM kill, an eviction or a reboot
# leaves the value. The next start never runs the wizard again, so the app would bind
# loopback, unreachable and never Ready, for good. The pod is put in that state and
# restarted here, and the chart's network-config container must put the element back.
jf_net=/srv/s5n/config/jellyfin/config/network.xml
jf_before=$($K -n "$NS" get pod jellyfin-0 -o jsonpath='{.metadata.uid}')
docker exec "${CLUSTER}-control-plane" bash -c '
  set -eu
  net="$1"
  sys=/srv/s5n/config/jellyfin/config/system.xml
  test -f "$net" || { echo "no ${net}: the app saved no network.xml"; exit 1; }
  grep -q "<IsStartupWizardCompleted>true<" "$sys" ||
    { echo "system.xml does not say the wizard is complete"; exit 1; }
  sed -i "s|<LocalNetworkAddresses */>|<LocalNetworkAddresses><string>127.0.0.1</string></LocalNetworkAddresses>|" "$net"
  # An element the app wrote across several lines is collapsed the same way.
  awk "
    /<LocalNetworkAddresses>/ && !/<\/LocalNetworkAddresses>/ {
      print \"  <LocalNetworkAddresses><string>127.0.0.1</string></LocalNetworkAddresses>\"; skip = 1; next
    }
    skip && /<\/LocalNetworkAddresses>/ { skip = 0; next }
    skip { next }
    { print }
  " "$net" >"$net.new" && mv "$net.new" "$net"
  chown 1000:1000 "$net"
  chmod 0644 "$net"
  grep -q "<LocalNetworkAddresses><string>127.0.0.1</string></LocalNetworkAddresses>" "$net" ||
    { echo "the seeded network.xml does not name the loopback"; exit 1; }
  echo "  seeded network.xml: $(grep -o "<LocalNetworkAddresses><string>127.0.0.1</string></LocalNetworkAddresses>" "$net")"' _ "$jf_net" ||
  fail=1
$K -n "$NS" delete pod jellyfin-0 --wait=true --timeout=180s >/dev/null
$K -n "$NS" wait --for=condition=Ready pod/jellyfin-0 --timeout=300s >/dev/null ||
  { echo "jellyfin-0 did not come back Ready after the seeded loopback: the app is bound where nothing can reach it"; fail=1; }
jf_after=$($K -n "$NS" get pod jellyfin-0 -o jsonpath='{.metadata.uid}')
[ -n "$jf_after" ] && [ "$jf_after" != "$jf_before" ] ||
  { echo "jellyfin-0 was not replaced: the restart this check needs did not happen"; fail=1; }
jf_wiz_again=$($K -n "$NS" logs jellyfin-0 -c startup-wizard 2>/dev/null || true)
printf '%s\n' "$jf_wiz_again" | grep -q 'startup wizard: already complete' ||
  { echo "the restarted pod did not skip a complete wizard"; fail=1; }
jf_net_exit=$($K -n "$NS" get pod jellyfin-0 -o jsonpath='{.status.initContainerStatuses[?(@.name=="network-config")].state.terminated.exitCode}' || true)
jf_net_log=$($K -n "$NS" logs jellyfin-0 -c network-config 2>/dev/null || true)
echo "network-config on the restart: exit ${jf_net_exit}, $(printf '%s\n' "$jf_net_log" | grep -m1 'jellyfin-config:' || true)"
[ "$jf_net_exit" = 0 ] || { echo "network-config did not succeed on the restart (${jf_net_exit}): the bind address is not cleared"; fail=1; }
printf '%s\n' "$jf_net_log" | grep -q 'binds every interface' ||
  { echo "network-config did not clear the bind address on the restart"; fail=1; }
jf_live_net=$(docker exec "${CLUSTER}-control-plane" grep -o '<LocalNetworkAddresses[^>]*>' "$jf_net" | head -1 || true)
echo "network.xml after the restart: ${jf_live_net}"
case "$jf_live_net" in
  *127.0.0.1*) echo "the app's network.xml still binds the loopback: nothing outside the pod can reach it"; fail=1 ;;
esac
# And the app answers where the person reaches it, with its own data intact.
code=$(host_get "$JELLYFIN_HOST" /System/Info/Public "$WORK/jf-info-restart.json")
echo "GET https://${JELLYFIN_HOST}/System/Info/Public after the restart -> ${code}"
[ "$code" = 200 ] || { echo "Jellyfin did not come back through the Gateway after a killed wizard's loopback (${code})"; fail=1; }
grep -q '"StartupWizardCompleted":true' "$WORK/jf-info-restart.json" ||
  { echo "the restarted server does not report its wizard complete"; fail=1; }
[ -n "$(jf_signin "$APP_LOGIN" "$jf_user_pw")" ] ||
  { echo "the person cannot sign in after the restart: the repaired start is not the same server"; fail=1; }

# ---- PeerBanHelper: one login, and a downloader PBH itself proved ------------
# The app is served from its own host, and its own WebUI login is switched off:
# the page calls /api/... with a token it holds in the browser, which the edge
# cannot supply, so the chart turns the app's login off and the edge's is the
# only one. What makes that safe is the NetworkPolicy: only the Gateway and the
# wiring Job reach this port, and everything through the Gateway is behind the
# edge in a real box.
pbh_token=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.pbh-token}' | base64 -d || true)
[ -n "$pbh_token" ] || { echo "the release Secret has no pbh-token"; fail=1; }
code=$(host_get "$PBH_HOST" / "$WORK/pbh.html")
echo "GET https://${PBH_HOST}/ -> ${code}"
[ "$code" = 200 ] || fail=1
grep -qi '<!doctype html' "$WORK/pbh.html" || { echo "${PBH_HOST} answered without an HTML document"; fail=1; }
asset=$(grep -oE '(src|href)="[^"]+\.(js|css)"' "$WORK/pbh.html" | head -1 | sed 's/^[a-z]*="//; s/"$//' || true)
echo "  asset the page references: ${asset}"
case "$asset" in
  ./assets/*) ;;
  *) echo "the asset ${asset} is not the bundle's own relative path: PBH is not served from its root"; fail=1 ;;
esac

# The route the app's page calls, with no credential: 200 is the one-login decision.
code=$(host_get "$PBH_HOST" /api/downloaders /dev/null)
echo "GET https://${PBH_HOST}/api/downloaders with no token -> ${code}"
[ "$code" = 200 ] || { echo "PBH refused the request its own page makes: the app is asking for a second password"; fail=1; }

# The Job's downloader, read back with the Secret's token: HEALTHY is PBH's own verdict
# that it logged in with the password written, which nothing s5n does can fake.
code=$(curl -sS -o "$WORK/pbh-status.json" -w '%{http_code}' \
  -H "Authorization: Bearer ${pbh_token}" --resolve "${PBH_HOST}:443:${GW_ADDR}" \
  --cacert "$WORK/pebble-root.pem" "https://${PBH_HOST}/api/downloaders/qbittorrent/status" || true)
echo "GET https://${PBH_HOST}/api/downloaders/qbittorrent/status -> ${code}"
[ "$code" = 200 ] || fail=1
qbit_pw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' | base64 -d || true)
if python3 - "$WORK/pbh-status.json" >"$WORK/pbh-status.txt" <<'PY'
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
cfg = status.get("config") or {}
ok = True
print("  lastStatus %s, endpoint %s, username %r, password %d characters"
      % (status.get("lastStatus"), cfg.get("endpoint"), cfg.get("username"), len(cfg.get("password") or "")))
if status.get("lastStatus") != "HEALTHY":
    print("  PBH is not healthy on its qBittorrent: %s" % status.get("lastStatusMessage"))
    ok = False
if cfg.get("endpoint") != "http://qbittorrent:8080":
    print("  PBH dials %r, want the release's own qBittorrent, with no /api/v2" % cfg.get("endpoint"))
    ok = False
# Both empty: qBittorrent skips its login for the release's addresses.
if cfg.get("username") or cfg.get("password"):
    print("  PBH holds a qBittorrent credential: nothing should, and HEALTHY proves it needs none")
    ok = False
sys.exit(0 if ok else 1)
PY
then
  sed 's/^/  /' "$WORK/pbh-status.txt"
else
  sed 's/^/  /' "$WORK/pbh-status.txt"; fail=1
fi

# The token is a Secret key the app reads through its environment, never the pod spec.
if $K -n "$NS" get statefulset pbh -o json | grep -qF "$pbh_token"; then
  echo "PBH's WebUI token is in the pod spec"; fail=1
fi

# PBH turns qBittorrent's "allow multiple connections from the same IP" off itself on
# its first successful login (AbstractQbittorrent.updatePreferences at v9.5.4), which its
# peer matching depends on, so a seed that flipped it on would be the chart's fault. One
# session over a port-forward, the image having no curl.
port_forward qbittorrent-0 8080 || { echo "could not reach qBittorrent"; fail=1; }
multi=$(curl -s -c "$WORK/qbittorrent-multi-jar" -o /dev/null -X POST \
  --data-urlencode 'username=admin' --data-urlencode "password=${qbit_pw}" \
  "http://127.0.0.1:${PF_PORT}/api/v2/auth/login" &&
  curl -s -b "$WORK/qbittorrent-multi-jar" "http://127.0.0.1:${PF_PORT}/api/v2/app/preferences" 2>/dev/null |
  python3 -c 'import json,sys; print(json.load(sys.stdin).get("enable_multi_connections_from_same_ip"))' 2>/dev/null || true)
port_forward_stop
echo "qBittorrent enable_multi_connections_from_same_ip = ${multi} (PBH turns it off)"
[ "$multi" = False ] || { echo "qBittorrent still accepts several connections from one IP: PBH cannot tell peers from its own connections"; fail=1; }

# ---- the opt-in apps (apps-values.yaml) ---------------------------------------
# Each serves itself under its own base path, with no rewrite. qui and Maintainerr keep
# no login here -- the edge is the login, as the Arrs run External -- while autobrr does,
# because this install gives the release no provider. So the HTML must arrive without a
# redirect, and the asset URLs it builds must resolve through the same prefix.
for app in qui autobrr maintainerr lidarr bazarr filebrowser; do
  : >"$WORK/${app}-headers"
  code=$(gw_get -s -D "$WORK/${app}-headers" -o "$WORK/${app}.html" -w '%{http_code}' \
    "https://${DOMAIN}/${app}/" || true)
  loc=$(grep -i '^location:' "$WORK/${app}-headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
  echo "GET /${app}/ via gateway -> ${code} ${loc}"
  [ "$code" = 200 ] || { echo "/${app}/ did not serve its UI: ${code}"; fail=1; }
  [ -z "$loc" ] || { echo "/${app}/ redirected to ${loc}: the app is asking for a login of its own"; fail=1; }
  grep -qi '<!doctype html' "$WORK/${app}.html" || { echo "/${app}/ answered ${code} without an HTML document"; fail=1; }
  asset=$(grep -oE '(src|href)="[^"]+\.(js|css)"' "$WORK/${app}.html" | head -1 |
    sed 's/^[a-z]*="//; s/"$//' || true)
  # A bundle can name its assets relative to its own <base href> (Bazarr does), which a
  # browser resolves against the prefix; resolve it the same way so the check holds
  # either way the bundle writes them.
  case "$asset" in ./*) asset="/${app}/${asset#./}" ;; esac
  echo "  asset the page references: ${asset}"
  case "$asset" in
    "/${app}/"*) ;;
    "") echo "/${app}/ referenced no script or stylesheet"; fail=1 ;;
    *) echo "the asset ${asset} is outside /${app}/: the base path did not reach the bundle"; fail=1 ;;
  esac
  if [ -n "$asset" ]; then
    code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}${asset}" || true)
    echo "GET ${asset} via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
  fi
  case "$app" in
    qui)
      # qui's health endpoints are on its root router, outside its base path, so the
      # Gateway must not be used for them: ask the pod, as its readinessProbe does.
      code=$($K -n "$NS" exec qui-0 -- curl -s -o /dev/null -w '%{http_code}' \
        http://127.0.0.1:7476/healthz/readiness || true)
      echo "GET /${app} in-pod /healthz/readiness -> ${code}"; [ "$code" = 200 ] || fail=1 ;;
    autobrr)
      # No provider in this install (a plain helm install sets auth.edge and nothing
      # else), so autobrr keeps its own login behind the edge's: the state a box that
      # never ran init has. Its base path covers every route.
      code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/${app}/api/healthz/readiness" || true)
      echo "GET /${app}/api/healthz/readiness via gateway -> ${code}"; [ "$code" = 200 ] || fail=1 ;;
    lidarr)
      # An Arr's health endpoint is /ping at its URL base, unauthenticated.
      code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/${app}/ping" || true)
      echo "GET /${app}/ping via gateway -> ${code}"; [ "$code" = 200 ] || fail=1 ;;
    bazarr)
      # Its health route is under the base URL the init container sealed into
      # general.base_url; the settings read proves the key it minted is accepted.
      code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/${app}/api/system/ping" || true)
      echo "GET /${app}/api/system/ping via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
      # Read here rather than through arr_key: that helper is defined below.
      key=$($K -n "$NS" get secret s5n-keys -o jsonpath="{.data.${app}-apikey}" | base64 -d)
      code=$(gw_get -s -o /dev/null -w '%{http_code}' -H "X-API-KEY: ${key}" \
        "https://${DOMAIN}/${app}/api/system/settings" || true)
      echo "GET /${app}/api/system/settings with the release Secret's key -> ${code}"
      [ "$code" = 200 ] ||
        { echo "${app} refused the API key the init container sealed into its config.yaml"; fail=1; }
      # Both sides are this box's own Arrs, read back through the app.
      settings=""
      settings=$(gw_get -s -H "X-API-KEY: ${key}" "https://${DOMAIN}/${app}/api/system/settings" || true)
      for pair in "general.use_sonarr:True" "general.use_radarr:True" "sonarr.ip:sonarr" "radarr.ip:radarr"; do
        want=${pair%%:*}; wantvalue=${pair#*:}
        got=$(printf '%s' "$settings" | python3 -c "import json,sys
d=json.load(sys.stdin); section,key=sys.argv[1].split('.')
print(d[section][key])" "$want" 2>/dev/null || true)
        echo "  ${app} ${want} = ${got} (want ${wantvalue})"
        [ "$got" = "$wantvalue" ] || fail=1
      done
      ;;
    maintainerr)
      # BASE_PATH covers every route, health included.
      code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/${app}/api/health/ready" || true)
      echo "GET /${app}/api/health/ready via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
      # DATA_DIR=/config puts its one state directory on the config subPath rather than
      # the read-only root.
      state=$($K -n "$NS" exec maintainerr-0 -- ls -1 /config || true)
      echo "  /config holds: $(echo "$state" | tr '\n' ' ')"
      grep -qx "maintainerr.sqlite" <<<"$state" ||
        { echo "maintainerr's database is not on the config mount"; fail=1; } ;;
    filebrowser)
      # Its health route is under the chart's base URL and answers without a login (no
      # provider on this box, so the no-login branch). The listing is the API's view of
      # /data's top level, which proves the source is the claim and not config.
      code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}/${app}/health" || true)
      echo "GET /${app}/health via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
      # The index is built after start, so the poll waits for the first scan.
      items=""
      for _ in $(seq 1 60); do
        items=$(gw_get -s "https://${DOMAIN}/${app}/api/resources/items?source=data&path=/" || true)
        case "$items" in *'"downloads"'*'"media"'*) break ;; esac
        sleep 2
      done
      printf '%s' "$items" >"$WORK/${app}-items.json"
      python3 - "$WORK/${app}-items.json" <<'PY' || fail=1
import json, sys
try:
    d = json.load(open(sys.argv[1], encoding="utf-8"))
except ValueError:
    print(f"the items API answered no JSON: {open(sys.argv[1]).read()[:200]!r}")
    sys.exit(1)
folders = set(d.get("folders") or [])
missing = {"downloads", "media"} - folders
if missing:
    print(f"FileBrowser Quantum's /data has no {', '.join(sorted(missing))}: {sorted(folders)}")
    sys.exit(1)
print(f"FileBrowser Quantum lists /data: {sorted(folders)}")
PY
      ;;
  esac
done

# Tautulli's prefix is its own setting: the init container seals http_root to /tautulli
# on every start because the app builds absolute redirects and asset URLs from it. Its
# index is a redirect that must stay inside the prefix, and the page it lands on must
# reference its assets under it. Nothing here has a login: the app's form is sealed off.
: >"$WORK/tautulli-headers"
code=$(gw_get -s -D "$WORK/tautulli-headers" -o /dev/null -w '%{http_code}' "https://${DOMAIN}/tautulli/" || true)
loc=$(grep -i '^location:' "$WORK/tautulli-headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
echo "GET /tautulli/ via gateway -> ${code} ${loc}"
case "$code" in
  301 | 302 | 303 | 307 | 308) : ;;
  *) echo "/tautulli/ answered ${code}, want the app's own redirect"; fail=1 ;;
esac
# The redirect is absolute and in the pod's scheme (http), so the path is what is checked
# and the page is asked for over the box's TLS.
case "$loc" in
  *"://"*/*) tpath="/${loc#*://*/}" ;;
  *"://"*) tpath="/" ;;
  *) tpath=$loc ;;
esac
case "$tpath" in
  /tautulli/*) : ;;
  *) echo "/tautulli/ redirected to ${loc}, which is outside its prefix: the app's URL root does not know it"; fail=1 ;;
esac
# Not its own login: box seals Tautulli's web login off (internal/ttlconf), and 2.19
# mints itself a password or refuses the Gateway's forwarded request unless the seal's
# two switches are set.
case "$tpath" in
  */auth/* | */auth) echo "/tautulli/ redirected to ${loc}: the app's own login is on, and nothing in front of it is box's"; fail=1 ;;
esac
if [ -n "$tpath" ]; then
  code=$(gw_get -s -o "$WORK/tautulli.html" -w '%{http_code}' "https://${DOMAIN}${tpath}" || true)
  echo "GET ${tpath} via gateway -> ${code}"
  [ "$code" = 200 ] || { echo "${tpath} did not serve the app's page: ${code}"; fail=1; }
  grep -qi '<!doctype html' "$WORK/tautulli.html" || { echo "${tpath} answered ${code} without an HTML document"; fail=1; }
  if grep -q 'id="login-form"' "$WORK/tautulli.html"; then
    echo "${tpath} served Tautulli's own login form: internal/ttlconf did not keep its login off"
    fail=1
  fi
fi
tasset=$(grep -oE '(src|href)="[^"]+\.(js|css)([?#][^"]*)?"' "$WORK/tautulli.html" | head -1 |
  sed 's/^[a-z]*="//; s/"$//' || true)
echo "  asset the page references: ${tasset}"
case "$tasset" in
  /tautulli/*) code=$(gw_get -s -o /dev/null -w '%{http_code}' "https://${DOMAIN}${tasset}" || true)
               echo "GET ${tasset} via gateway -> ${code}"; [ "$code" = 200 ] || fail=1 ;;
  "") echo "the app's page referenced no script or stylesheet"; fail=1 ;;
  *) echo "the asset ${tasset} is outside /tautulli/: the URL root did not reach the page"; fail=1 ;;
esac

# Maintainerr streams live task progress as Server-Sent Events, which Envoy's 15s route
# timeout would silently cut, so the chart gives that one path no request timeout. Held
# open for 30s: exit 28 is curl's own --max-time, meaning the stream outlived the window.
sse_exit=0
sse_secs=$(gw_get -sN --max-time 30 -w '%{time_total}' -o "$WORK/maintainerr-sse.txt" \
  "https://${DOMAIN}/maintainerr/api/events/stream") || sse_exit=$?
echo "GET /maintainerr/api/events/stream: curl exit ${sse_exit} after ${sse_secs}s (28 = still open at the 30s cap), $(wc -c <"$WORK/maintainerr-sse.txt") bytes"
[ "$sse_exit" = 28 ] || { echo "the event stream closed before 30s (curl exit ${sse_exit}): something is timing it out"; fail=1; }

# The peers Service is a NodePort on the node's own address -- how a single-IP box runs,
# the Gateway holding the LoadBalancer address -- so a TCP connect to <node>:30080 works.
peer_type=$($K -n "$NS" get svc qbittorrent-peers -o jsonpath='{.spec.type}/{.spec.ports[0].nodePort}')
echo "peers Service: ${peer_type}"; [ "$peer_type" = NodePort/30080 ] || fail=1
PEER_ADDR=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${CLUSTER}-control-plane")
echo "node address: ${PEER_ADDR}"
[ -n "$PEER_ADDR" ] || fail=1
if python3 - "$PEER_ADDR" <<'PY'
import socket, sys
s = socket.create_connection((sys.argv[1], 30080), timeout=10)
s.close()
print("TCP", sys.argv[1], "30080 -> connected")
PY
then
  :
else
  echo "TCP ${PEER_ADDR}:30080 -> REFUSED/TIMEOUT"; fail=1
fi

# Hardlink invariant: downloads/ and media/ are one filesystem, as an Arr pod sees them.
$K -n "$NS" exec sonarr-0 -- sh -c '
  set -e
  f=/data/downloads/tv/.s5n-linkcheck; l=/data/media/tv/.s5n-linkcheck
  rm -f "$f" "$l"; echo x > "$f"; ln "$f" "$l"
  n=$(stat -c %h "$l"); d1=$(stat -c %d "$f"); d2=$(stat -c %d "$l")
  rm -f "$f" "$l"
  echo "hardlink nlink=$n dev=$d1/$d2"; [ "$n" = 2 ] && [ "$d1" = "$d2" ]' || fail=1

# The categories the Job made are on the data path and owned by the release's uid:gid.
# The nested category's parent (4K) must carry the leaf's owner too: a root-owned
# intermediate lets an Arr create a leaf under it and then fail to write there.
for d in /srv/s5n/data/downloads/tv /srv/s5n/data/media/tv \
         /srv/s5n/data/downloads/4K /srv/s5n/data/media/4K \
         /srv/s5n/data/downloads/4K/Movies /srv/s5n/data/media/4K/Movies \
         /srv/s5n/data/downloads/lidarr /srv/s5n/data/media/music; do
  got=$(docker exec "${CLUSTER}-control-plane" stat -c '%u:%g %a' "$d" 2>/dev/null || echo missing)
  echo "  ${d}: ${got}"
  case "$got" in 1000:1000\ *) ;; *) echo "want 1000:1000, got ${got}"; fail=1 ;; esac
done

# NetworkPolicy: the release policies exist, prowlarr may reach sonarr, and a pod outside
# the release may not.
for p in s5n-default-deny s5n-egress s5n-wiring-egress; do
  if $K -n "$NS" get networkpolicy "$p" >/dev/null 2>&1; then
    echo "policy $p: present"
  else
    echo "policy $p: MISSING"; fail=1
  fi
done
$K -n "$NS" exec prowlarr-0 -- curl -s -o /dev/null -w 'prowlarr->sonarr %{http_code}\n' --max-time 5 http://sonarr:8989/sonarr/ping || fail=1
if $K -n default run np-probe --rm -i --restart=Never --image="${CURL_IMAGE}" --quiet -- \
     curl -s -o /dev/null --max-time 5 "http://sonarr.${NS}:8989/sonarr/ping"; then
  echo "outsider->sonarr: REACHABLE (policy not enforced)"; fail=1
else
  echo "outsider->sonarr: blocked"
fi

# ---- NetworkPolicy: every declared edge, and one that is not ----------------
# The peers come from the chart's catalog (`calls`), which is what the release's policies
# derive from: a list typed here could agree with the script and still disagree with the
# release -- which is how a shipped chart came to refuse Seerr's requests while every test
# passed. A probe pod must run in the release namespace, since a podSelector peer matches
# only its own namespace; that puts it behind Pod Security restricted and the image
# policy, so its image is the release's blessed Sonarr image (which has curl). It carries
# the caller's labels and nothing else: the callee's ingress decides.
enabled_apps=$($K -n "$NS" get pods -l 's5n.io/app' \
  -o jsonpath='{range .items[*]}{.metadata.labels.s5n\.io/app}{"\n"}{end}' 2>/dev/null | sort -u | tr '\n' ' ' || true)
[ -n "${enabled_apps// /}" ] || { echo "no app pods found: the release's edges cannot be derived"; fail=1; }
PROBE_IMAGE=$($K -n "$NS" get statefulset sonarr -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)
[ -n "$PROBE_IMAGE" ] || { echo "no Sonarr image to run the probes with"; fail=1; }
pairs=$(go run ./hack/appcalls charts/s5n $enabled_apps 2>"$WORK/appcalls.err" || true)
[ -n "$pairs" ] || { echo "appcalls printed no edges: $(sed 's/^/    /' "$WORK/appcalls.err")"; fail=1; }
echo "apps with pods: ${enabled_apps}"
echo "declared edges ($(grep -c . <<<"$pairs")):"
sed 's/^/  /' <<<"$pairs"

# probe_pod <name> <caller> <callee>...: one pod per caller, dialling each callee and
# printing "<callee> <http code>"; 000 is the timeout the policy produces.
probe_pod() {
  local name=$1 caller=$2 entries="" callee url
  shift 2
  for callee in "$@"; do
    url="http://${callee}.${NS}:$($K -n "$NS" get svc "$callee" -o jsonpath='{.spec.ports[0].port}')/"
    entries="${entries} ${callee}=${url}"
  done
  $K -n "$NS" delete pod "$name" --ignore-not-found >/dev/null 2>&1
  $K -n "$NS" create -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  labels:
    app.kubernetes.io/instance: s5n
    s5n.io/app: ${caller}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    runAsNonRoot: true
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
      command: ["/bin/sh", "-c"]
      args:
        - |
          for e in${entries}; do
            printf '%s %s\n' "\${e%%=*}" "\$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "\${e#*=}")"
          done
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: ["ALL"]}
EOF
}

probe_wait_read() { # probe_wait_read <name> <log file>
  local name=$1 out=$2 phase=""
  for _ in $(seq 1 90); do
    phase=$($K -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$phase" in Succeeded|Failed) break ;; esac
    sleep 1
  done
  $K -n "$NS" logs "$name" >"$out" 2>/dev/null || true
  [ -s "$out" ] || { echo "${name}: printed nothing (phase ${phase:-unknown})"; fail=1; }
}

# One pod per caller, all at once: a caller with six callees costs the same wall time.
declare -A callees
order=""
while read -r caller callee; do
  [ -n "$callee" ] || continue
  [ -n "${callees[$caller]:-}" ] || order="${order} ${caller}"
  callees[$caller]="${callees[$caller]:-} ${callee}"
done <<<"$pairs"
for caller in $order; do
  probe_pod "np-${caller}" "$caller" ${callees[$caller]}
done
for caller in $order; do
  probe_wait_read "np-${caller}" "$WORK/np-${caller}.txt"
  while read -r callee code; do
    [ -n "$callee" ] || continue
    echo "  ${caller} -> ${callee}: ${code}"
    case "$code" in
      ""|000) echo "  ${caller} -> ${callee} was refused by the callee's ingress; the catalog declares it"; fail=1 ;;
    esac
  done <"$WORK/np-${caller}.txt"
  $K -n "$NS" delete pod "np-${caller}" --ignore-not-found >/dev/null 2>&1
done

# The negative half: an edge nothing declares must time out. If the catalog ever declares
# it, say so rather than asserting the opposite of the release.
if grep -qx 'tautulli qbittorrent' <<<"$pairs"; then
  echo "the catalog declares tautulli -> qbittorrent now: pick another pair for the negative probe"
  fail=1
else
  probe_pod np-undeclared tautulli qbittorrent
  probe_wait_read np-undeclared "$WORK/np-undeclared.txt"
  undeclared=$(awk '{print $2}' "$WORK/np-undeclared.txt" 2>/dev/null || true)
  echo "  tautulli -> qbittorrent (undeclared): ${undeclared:-nothing printed}"
  [ "$undeclared" = 000 ] || { echo "an edge no app declares answered ${undeclared:-nothing}: the ingress is not enforced"; fail=1; }
  $K -n "$NS" delete pod np-undeclared --ignore-not-found >/dev/null 2>&1
fi

# ---- wiring -------------------------------------------------------------------
status_field() { # status_field <python expression over `d`>
  $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.status\.json}' |
    python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

arr_port() { $K -n "$NS" get svc "$1" -o jsonpath='{.spec.ports[0].port}'; }
arr_key() { $K -n "$NS" get secret s5n-keys -o jsonpath="{.data.$1-apikey}" | base64 -d; }

# ---- qui and autobrr: the account the chart owns -----------------------------
# Both hand an API key only to a signed-in session, so the chart generates a password and
# each init container writes it in. No OIDC provider here, which is the other half: qui
# runs with its login off (its API open inside the release) and autobrr keeps the login
# it cannot turn off, with that account behind it.

# app_snapshot: what one app holds through its own API, so a second run can be compared
# field for field (state-diff.py).
app_snapshot() { # app_snapshot <dir> <app>
  local dir=$1 app=$2 base port jar
  case "$app" in
    qui)
      base=/qui
      port=$(arr_port qui)
      port_forward qui-0 "$port" || return 1
      curl -sS "http://127.0.0.1:${PF_PORT}${base}/api/instances/" >"$dir/qui-instances.json" 2>/dev/null || true
      ;;
    autobrr)
      base=/autobrr
      port=$(arr_port autobrr)
      jar="$dir/autobrr-jar.txt"
      port_forward autobrr-0 "$port" || return 1
      curl -sS -c "$jar" -o /dev/null \
        -X POST "http://127.0.0.1:${PF_PORT}${base}/api/auth/login" -H 'Content-Type: application/json' \
        --data "{\"username\":\"${ACCOUNT}\",\"password\":\"${AUTOBRR_PASSWORD}\"}" 2>/dev/null || true
      curl -sS -b "$jar" "http://127.0.0.1:${PF_PORT}${base}/api/downloaders" >"$dir/autobrr-downloaders.json" 2>/dev/null || true
      rm -f "$jar"
      ;;
    bazarr | bazarr-4k)
      # Its settings document: a second run changed no setting, on an app whose whole
      # connection is settings rather than rows.
      key=$(arr_key "$app")
      port=$(arr_port "$app")
      port_forward "${app}-0" "$port" || return 1
      curl -sS -H "X-API-KEY: ${key}" "http://127.0.0.1:${PF_PORT}/${app}/api/system/settings" \
        >"$dir/${app}-settings.json" 2>/dev/null || true
      ;;
    pbh)
      # The downloader the Job owns. The status endpoint is left out: its live counters
      # move for reasons that are not configuration.
      port=$(arr_port pbh)
      port_forward pbh-0 "$port" || return 1
      curl -sS "http://127.0.0.1:${PF_PORT}/api/downloaders" >"$dir/pbh-downloaders.json" 2>/dev/null || true
      ;;
  esac
  port_forward_stop
}

# Everything the Job may change, so a second run is compared rather than trusted; free
# space is scrubbed by state-diff.py because it moves for other reasons.
snapshot() { # snapshot <dir>
  local dir=$1 key port app
  mkdir -p "$dir"
  for app in sonarr radarr radarr-4k lidarr; do
    key=$(arr_key "$app"); port=$(arr_port "$app")
    api=api/v3
    # Lidarr's API is /api/v1; the rest of the shape is the same.
    [ "$app" = lidarr ] && api=api/v1
    for path in downloadclient rootfolder config/mediamanagement; do
      $K -n "$NS" exec "${app}-0" -- curl -s -H "X-Api-Key: $key" \
        "http://localhost:${port}/${app}/${api}/${path}" >"$dir/${app}-${path//\//-}.json"
    done
  done
  key=$(arr_key prowlarr); port=$(arr_port prowlarr)
  $K -n "$NS" exec prowlarr-0 -- curl -s -H "X-Api-Key: $key" \
    "http://localhost:${port}/prowlarr/api/v1/applications" >"$dir/prowlarr-applications.json"
  # Categories, with the seeded WebUI credential -- also the proof the init container's
  # hash is accepted. The image has no curl, so this reads through a tunnel.
  local qpw
  qpw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' | base64 -d)
  port_forward qbittorrent-0 8080 || { echo "could not reach qBittorrent"; fail=1; }
  curl -s -c "$dir/qbittorrent-jar.txt" -o /dev/null -X POST \
    --data-urlencode 'username=admin' --data-urlencode "password=${qpw}" \
    "http://127.0.0.1:${PF_PORT}/api/v2/auth/login" 2>/dev/null || true
  curl -s -b "$dir/qbittorrent-jar.txt" "http://127.0.0.1:${PF_PORT}/api/v2/torrents/categories" \
    >"$dir/qbittorrent-categories.json" 2>/dev/null || true
  grep -q '"tv"' "$dir/qbittorrent-categories.json" ||
    { echo "qBittorrent answered no categories: $(head -c 200 "$dir/qbittorrent-categories.json")"; fail=1; }
  rm -f "$dir/qbittorrent-jar.txt"
  port_forward_stop
  # SABnzbd's data contract and categories with the Secret's key: a second run rewrote no
  # path, and the sealed key is one the app accepts.
  local sabkey
  sabkey=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.sabnzbd-apikey}' | base64 -d)
  for key in complete_dir download_dir; do
    $K -n "$NS" exec sabnzbd-0 -- curl -s \
      "http://localhost:8080/sabnzbd/api?mode=get_config&section=misc&keyword=${key}&output=json&apikey=${sabkey}" \
      >"$dir/sabnzbd-${key}.json" || true
  done
  $K -n "$NS" exec sabnzbd-0 -- curl -s \
    "http://localhost:8080/sabnzbd/api?mode=get_config&section=categories&output=json&apikey=${sabkey}" \
    >"$dir/sabnzbd-categories.json" || true
  # Tautulli's settings with the seeded api_key, at the prefix the same container seals.
  # PMS is legitimately empty: no Plex token is supplied, so the link stays pending.
  local tpw
  tpw=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.tautulli-apikey}' | base64 -d)
  $K -n "$NS" exec tautulli-0 -- curl -s \
    "http://localhost:8181/tautulli/api/v2?apikey=${tpw}&cmd=get_settings" >"$dir/tautulli-settings.json"
  # The two apps with their own user table: a duplicate row or a rewritten client shows up
  # in the diff like any Arr's.
  for app in qui autobrr pbh bazarr bazarr-4k; do
    app_snapshot "$dir" "$app" || { echo "could not snapshot ${app}"; fail=1; }
  done
}


# The account the chart owns, from the release Secret: both init containers wrote it in.
wiring_cfg=$($K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.wiring\.json}')
ACCOUNT=$(printf '%s' "$wiring_cfg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["qui"]["username"])')
QUI_PASSWORD=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qui-password}' | base64 -d)
AUTOBRR_PASSWORD=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.autobrr-password}' | base64 -d)
QBIT_PASSWORD=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' | base64 -d)
echo "the automation account: ${ACCOUNT} (qui ${#QUI_PASSWORD} char password)"

log "wiring run 1: asserts the release is actually connected"
$K -n "$NS" wait --for=condition=complete job/s5n-wiring --timeout=600s || fail=1
first_status=$(status_field 'd["status"]')
echo "wiring status = ${first_status}"
[ "$first_status" = ok ] || { fail=1; $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.summary\.txt}'; }

for expected in \
  data/dirs/downloads/tv data/dirs/media/tv \
  data/dirs/downloads/movies data/dirs/media/movies \
  data/dirs/downloads/lidarr data/dirs/media/music \
  data/dirs/downloads/4K/Movies data/dirs/media/4K/Movies \
  sonarr/downloadclient sonarr/rootfolder sonarr/hardlinks \
  radarr/downloadclient radarr/rootfolder radarr/hardlinks \
  radarr-4k/downloadclient radarr-4k/rootfolder radarr-4k/hardlinks \
  lidarr/downloadclient lidarr/rootfolder lidarr/hardlinks \
  prowlarr/application/sonarr prowlarr/application/radarr prowlarr/application/radarr-4k \
  prowlarr/application/lidarr \
  bazarr/ready bazarr/sonarr bazarr/radarr \
  bazarr-4k/ready bazarr-4k/sonarr bazarr-4k/radarr \
  sabnzbd/ready sabnzbd/data-contract \
  sabnzbd/category/tv sabnzbd/category/movies sabnzbd/category/4K/Movies sabnzbd/category/lidarr \
  sonarr/sabnzbd radarr/sabnzbd radarr-4k/sabnzbd lidarr/sabnzbd \
  qbittorrent/data-contract qbittorrent/category/tv qbittorrent/category/movies \
  qbittorrent/category/4K/Movies qbittorrent/category/lidarr \
  data/hardlink/tv data/hardlink/movies data/hardlink/4K/Movies data/hardlink/music \
  qui/ready qui/login qui/instance \
  autobrr/ready autobrr/login autobrr/downloader/qbittorrent \
  autobrr/downloader/sonarr autobrr/downloader/radarr autobrr/downloader/radarr-4k \
  autobrr/downloader/lidarr \
  pbh/ready pbh/downloader \
  plex/ready tautulli/ready seerr/ready; do
  res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$expected'][0]" 2>/dev/null || echo missing)
  echo "  ${expected}: ${res}"; [ "$res" = ok ] || fail=1
done

# Each Bazarr is wired to its own pair: the 4K one reads radarr-4k and no sonarr, because
# this install switches on the 4K Radarr only. The Job turns the missing side's switch off.
assert_bazarr_pair() { # assert_bazarr_pair <instance> <sonarr|-> <radarr|->
  local inst=$1 sonarr=$2 radarr=$3 key got
  key=$(arr_key "$inst")
  port_forward "${inst}-0" "$(arr_port "$inst")" || return 1
  got="$WORK/${inst}-settings.json"
  curl -sS -H "X-API-KEY: ${key}" "http://127.0.0.1:${PF_PORT}/${inst}/api/system/settings" \
    >"$got" 2>/dev/null || true
  port_forward_stop
  python3 - "$got" "$inst" "$sonarr" "$radarr" <<'PY' || fail=1
import json, sys
path, inst, sonarr, radarr = sys.argv[1:5]
d = json.load(open(path))
def want(flag):
    return flag != "-"
problems = []
if d["general"]["use_sonarr"] != want(sonarr):
    problems.append(f"general.use_sonarr = {d['general']['use_sonarr']}, want {want(sonarr)}")
if d["general"]["use_radarr"] != want(radarr):
    problems.append(f"general.use_radarr = {d['general']['use_radarr']}, want {want(radarr)}")
for side, name in (("sonarr", sonarr), ("radarr", radarr)):
    if not want(name):
        continue
    if d[side]["ip"] != name:
        problems.append(f"{side}.ip = {d[side]['ip']}, want {name}")
    if d[side]["base_url"].rstrip("/") not in ("/" + name, ""):
        problems.append(f"{side}.base_url = {d[side]['base_url']}, want /{name}")
if problems:
    print(f"{inst}: " + "; ".join(problems))
    sys.exit(1)
print(f"{inst}: wired to its own pair")
PY
}
assert_bazarr_pair bazarr sonarr radarr
assert_bazarr_pair bazarr-4k - radarr-4k

# SABnzbd's UI and API through the Gateway. The UI is its own page (no login of its own),
# and a 200 means both 403 switches are right: the Host is whitelisted and the Gateway's
# X-Forwarded-For no longer marks the request external.
: >"$WORK/sabnzbd-headers"
# With no news server yet its root answers a 303 to the wizard, both under the prefix,
# so the check follows the redirect.
sab_code=$(gw_get -s -L -D "$WORK/sabnzbd-headers" -o "$WORK/sabnzbd.html" -w '%{http_code}' \
  "https://${DOMAIN}/sabnzbd/" || true)
sab_loc=$(grep -i '^location:' "$WORK/sabnzbd-headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
echo "GET /sabnzbd/ via gateway -> ${sab_code} ${sab_loc}"
[ "$sab_code" = 200 ] || { echo "SABnzbd's UI did not serve through the Gateway (${sab_code})"; fail=1; }
case "$sab_loc" in
  "" | /sabnzbd/* | "https://${DOMAIN}/sabnzbd/"*) ;;
  *) echo "/sabnzbd/ redirected to ${sab_loc}, which is outside the prefix: the app is asking for a login of its own"; fail=1 ;;
esac
grep -qi '<html' "$WORK/sabnzbd.html" || { echo "/sabnzbd/ answered without an HTML document"; fail=1; }
sab_key=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.sabnzbd-apikey}' | base64 -d)
sab_version=$(gw_get -s "https://${DOMAIN}/sabnzbd/api?mode=version&output=json" || true)
sab_config=$(gw_get -s "https://${DOMAIN}/sabnzbd/api?mode=get_config&section=misc&keyword=complete_dir&output=json&apikey=${sab_key}" || true)
sab_cats=$(gw_get -s "https://${DOMAIN}/sabnzbd/api?mode=get_config&section=categories&output=json&apikey=${sab_key}" || true)
printf '%s' "$sab_version" >"$WORK/sabnzbd-version.json"
printf '%s' "$sab_config" >"$WORK/sabnzbd-complete-dir.json"
printf '%s' "$sab_cats" >"$WORK/sabnzbd-categories.json"
python3 - "$WORK/sabnzbd-version.json" "$WORK/sabnzbd-complete-dir.json" "$WORK/sabnzbd-categories.json" <<'PY' || fail=1
import json, sys
version = json.load(open(sys.argv[1], encoding="utf-8"))
if not version.get("version"):
    print(f"mode=version answered {version!r}")
    sys.exit(1)
complete = json.load(open(sys.argv[2], encoding="utf-8"))["config"]["misc"]["complete_dir"]
if complete.rstrip("/") != "/data/downloads":
    print(f"complete_dir = {complete!r}, want the data claim")
    sys.exit(1)
cats = {c["name"]: c["dir"] for c in json.load(open(sys.argv[3], encoding="utf-8"))["config"]["categories"]}
want = {"tv": "tv", "movies": "movies", "lidarr": "lidarr", "4k/movies": "4K/Movies"}
problems = [f"{n}: dir {cats.get(n)!r}, want {d!r}" for n, d in want.items() if cats.get(n) != d]
if problems:
    print("; ".join(problems))
    sys.exit(1)
print(f"SABnzbd {version['version']}: complete_dir {complete}, categories {sorted(want)}")
PY

# Each Arr holds the release's SABnzbd as its usenet client with that instance's
# category -- the 4K Radarr's nested 4K/Movies. SAB folds category names and the Arr
# refuses one the app does not list, so the client carries 4k/movies while the directory
# keeps 4K/Movies. The row is read back; its key is masked, and the Job's own test
# proved the key before it wrote.
for app in sonarr radarr radarr-4k lidarr; do
  case "$app" in
    sonarr) category=tv ;;
    radarr) category=movies ;;
    radarr-4k) category=4k/movies ;;
    lidarr) category=lidarr ;;
  esac
  key=$(arr_key "$app"); port=$(arr_port "$app"); api=api/v3
  [ "$app" = lidarr ] && api=api/v1
  $K -n "$NS" exec "${app}-0" -- curl -s -H "X-Api-Key: $key" \
    "http://localhost:${port}/${app}/${api}/downloadclient" >"$WORK/${app}-clients.json"
  python3 - "$WORK/${app}-clients.json" "$app" "$category" <<'PY' || fail=1
import json, sys
clients = json.load(open(sys.argv[1], encoding="utf-8"))
app, category = sys.argv[2], sys.argv[3]
found = [c for c in clients if c.get("implementation") == "Sabnzbd"]
if len(found) != 1:
    print(f"{app}: {len(found)} SABnzbd clients, want exactly one")
    sys.exit(1)
c = found[0]
fields = {f["name"]: f.get("value") for f in c.get("fields", [])}
category_field = {"sonarr": "tvCategory", "radarr": "movieCategory", "lidarr": "musicCategory"}
field = "movieCategory" if app.startswith("radarr") else category_field[app]
problems = []
if fields.get("host") != "sabnzbd":
    problems.append(f"host = {fields.get('host')!r}, want sabnzbd")
if fields.get("port") != 8080:
    problems.append(f"port = {fields.get('port')!r}, want 8080")
if (fields.get("urlBase") or "").rstrip("/") != "/sabnzbd":
    problems.append(f"urlBase = {fields.get('urlBase')!r}, want /sabnzbd")
if fields.get(field) != category:
    problems.append(f"{field} = {fields.get(field)!r}, want {category!r}")
if problems:
    print(f"{app}: " + "; ".join(problems))
    sys.exit(1)
print(f"{app}: SABnzbd client, category {category}")
PY
done

# Read back through the apps: exactly one qui instance pointed at qBittorrent with local
# filesystem access, and one autobrr client per Arr instance (the 4K one's host carries
# its base path) plus qBittorrent.
port_forward qui-0 "$(arr_port qui)" || fail=1
curl -sS "http://127.0.0.1:${PF_PORT}/qui/api/instances/" >"$WORK/qui-live.json" 2>/dev/null || true
port_forward_stop
port_forward autobrr-0 "$(arr_port autobrr)" || fail=1
autobrr_login=$(curl -sS -c "$WORK/autobrr-live.jar" -o /dev/null -w '%{http_code}' \
  -X POST "http://127.0.0.1:${PF_PORT}/autobrr/api/auth/login" -H 'Content-Type: application/json' \
  --data "{\"username\":\"${ACCOUNT}\",\"password\":\"${AUTOBRR_PASSWORD}\"}" 2>/dev/null || true)
curl -sS -b "$WORK/autobrr-live.jar" "http://127.0.0.1:${PF_PORT}/autobrr/api/downloaders" >"$WORK/autobrr-live.json" 2>/dev/null || true
qbit_url=$(printf '%s' "$wiring_cfg" | python3 -c 'import json,sys; print(json.load(sys.stdin)["qbittorrent"]["url"])')
autobrr_test=$(curl -sS -o /dev/null -w '%{http_code}' -b "$WORK/autobrr-live.jar" \
  -X POST "http://127.0.0.1:${PF_PORT}/autobrr/api/downloaders/test" -H 'Content-Type: application/json' \
  --data "{\"name\":\"qBittorrent\",\"type\":\"QBITTORRENT\",\"enabled\":true,\"host\":\"${qbit_url}\",\"username\":\"admin\",\"password\":\"${QBIT_PASSWORD}\",\"settings\":{\"rules\":{\"enabled\":false}}}" 2>/dev/null || true)
port_forward_stop
echo "autobrr login: ${autobrr_login}, its qBittorrent client test: ${autobrr_test}"
[ "$autobrr_login" = 204 ] || { echo "autobrr refused the account the chart owns (${autobrr_login})"; fail=1; }
[ "$autobrr_test" = 204 ] || { echo "autobrr could not reach qBittorrent with the client the Job registered (${autobrr_test})"; fail=1; }
python3 - "$WORK/qui-live.json" "$WORK/autobrr-live.json" "$wiring_cfg" <<'PY' || fail=1
import json, sys
qui = json.load(open(sys.argv[1], encoding="utf-8"))
clients = json.load(open(sys.argv[2], encoding="utf-8"))
cfg = json.loads(sys.argv[3])
ok = True
want_qbit = cfg["qbittorrent"]["url"]
if len(qui) != 1:
    print("qui holds %d instances, want exactly 1" % len(qui))
    ok = False
else:
    row = qui[0]
    print("qui instance: %s -> %s (local filesystem access %s)" %
          (row.get("name"), row.get("host"), row.get("hasLocalFilesystemAccess")))
    if row.get("host") != want_qbit:
        print("  it points at %r, want %r" % (row.get("host"), want_qbit))
        ok = False
    if not row.get("hasLocalFilesystemAccess"):
        print("  it has no local filesystem access: /data is the same mount in both apps")
        ok = False
want = {i["name"]: i["url"].rstrip("/") + i["urlBase"] for i in cfg["instances"]}
print("autobrr holds: %s" % ", ".join("%s(%s)" % (c.get("name"), c.get("type")) for c in clients))
for name, host in want.items():
    got = [c for c in clients if c.get("name") == name]
    if len(got) != 1:
        print("  %s: %d clients, want 1" % (name, len(got)))
        ok = False
    elif got[0].get("host") != host:
        print("  %s dials %r, want the Arr's own address %r" % (name, got[0].get("host"), host))
        ok = False
qbit = [c for c in clients if c.get("type") == "QBITTORRENT"]
if len(qbit) != 1 or qbit[0].get("host") != want_qbit:
    print("  qBittorrent: %r, want exactly one client at %r" % (qbit, want_qbit))
    ok = False
if len(clients) != len(want) + 1:
    print("  %d clients in total, want %d (qBittorrent + one per Arr instance)" % (len(clients), len(want) + 1))
    ok = False
sys.exit(0 if ok else 1)
PY

# No claim token and no Plex server token anywhere in this e2e, so Plex stays unclaimed,
# Tautulli has nothing to link with and Seerr has no admin: all three rows must say so in
# the receipt rather than failing the release.
for pending in plex/claim tautulli/plex seerr/plex; do
  res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$pending'][0]" 2>/dev/null || echo missing)
  echo "  ${pending}: ${res} (expected pending: no Plex token in this e2e)"
  [ "$res" = pending ] || fail=1
done
# Seerr is served at its own host, so that is the address the row asks a human to sign in at.
seerr_row=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='seerr/plex'][0]" 2>/dev/null || echo missing)
echo "  seerr/plex detail: ${seerr_row}"
case "$seerr_row" in
  *" sign in once at seerr.${DOMAIN} "*) ;;
  *) echo "the seerr/plex row does not name seerr.${DOMAIN}: ${seerr_row}"; fail=1 ;;
esac
claimed=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='plex/ready'][0]")
echo "  plex/ready detail: ${claimed}"
case "$claimed" in *claimed=false*) ;; *) echo "expected an unclaimed Plex in this e2e"; fail=1 ;; esac

snapshot "$WORK/before"

# qBittorrent's own view of the nested category: the name was accepted and its save path
# is the downloads tree. 5.3 creates the parent itself, so 4K is listed too.
python3 - "$WORK/before/qbittorrent-categories.json" <<'PY' || fail=1
import json, sys
cats = json.load(open(sys.argv[1], encoding="utf-8"))
cat = cats.get("4K/Movies") or {}
path = cat.get("savePath", "")
print(f"  qbittorrent 4K/Movies savePath = {path!r}")
sys.exit(0 if path.rstrip("/") == "/data/downloads/4K/Movies" else 1)
PY

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
# Live objects are edited through the app APIs as a human would: a rename and a non-owned
# field survive, an owned field drifts and is restored (with the receipt saying so), a
# deleted owned object is recreated (naming the old id), and an object the Job never
# created is left alone.
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
# The same last line hack/e2e-box.sh prints, after the assertions and before the cluster
# goes, so a run leaves a number behind.
echo "wall time (kind): ${SECONDS}s"
[ "$KEEP" = --keep ] || $KIND delete cluster --name "$CLUSTER"
exit "$fail"
