#!/usr/bin/env bash
# The box-driven install path, on kind.
#
# hack/e2e-kind.sh installs the chart with `helm upgrade --install` and asserts
# the result. This script installs the same release the other way: `box init`
# asks a seedbox's questions, writes root's grant and values file (the values
# file is written first, by hand, with the locally built image tag, and init
# must leave it alone), then hands over to `box apply`, which reads both files,
# checks them against the chart's schema, diffs, confirms and calls the Helm SDK
# itself. It then proves the four properties the init/apply path has to have:
#
#   1. a first init on a cluster with no namespace creates it (Pod Security
#      restricted) and installs the stack, and its wiring receipt says ok;
#   2. a second init with identical answers splices nothing (both files are
#      byte-identical) and its apply is a no-op;
#   3. one edited value is printed as a diff, applied, and visible in the
#      release afterwards;
#   4. the claims outlive `helm uninstall`.
#
# Why a separate script instead of a mode of hack/e2e-kind.sh: the existing e2e
# is the chart's acceptance test, and its helm install is what proves the chart
# still installs the way users install it today. A flag inside that script would
# put two install paths, two sets of assertions and two failure modes in one
# 600-line file. This one stands alone. The cluster setup it repeats is the
# obvious candidate for a shared helper once `box apply` stops being a proof of
# concept.
#
# Usage: hack/e2e-box.sh [--keep]
set -euo pipefail
cd "$(dirname "$0")/.."

KIND=${KIND:-kind}
CLUSTER=s5n-box
NS=seedbox
# Envoy Gateway's chart ships the Gateway API CRDs the chart's HTTPRoutes need,
# at the same pinned version hack/e2e-kind.sh uses. MetalLB is not installed:
# the box path renders the peers Service as a NodePort and never asks for a
# LoadBalancer address, so the address layer is out of scope here.
ENVOY_GATEWAY_CHART_VERSION=${ENVOY_GATEWAY_CHART_VERSION:-1.9.3}
KEEP=${1:-}
CTX="kind-${CLUSTER}"
K="kubectl --context ${CTX}"
HELM="helm --kube-context ${CTX}"
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
  $KIND create cluster --name "$CLUSTER" --config hack/kind.yaml --wait 120s
fi

log "building the s5n image (the wiring Job and the qBittorrent credential seed)"
docker build -q -t "$IMAGE" .
$KIND load docker-image --name "$CLUSTER" "$IMAGE"

log "building box"
go build -o "$WORK/box" ./cmd/box
$KIND get kubeconfig --name "$CLUSTER" >"$WORK/kubeconfig"

log "preparing host paths inside the node (the installer does this on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data/downloads/{tv,movies,tv-4k,movies-4k,misc} \
           /srv/s5n/data/media/{tv,movies,tv-4k,movies-4k} \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,plex,tautulli,seerr}
  chown -R 1000:1000 /srv/s5n'

log "Gateway API CRDs and a Gateway (the chart's parentRef target)"
$HELM upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version "$ENVOY_GATEWAY_CHART_VERSION" -n envoy-gateway-system --create-namespace \
  --wait --timeout 5m >/dev/null
$K create namespace s5n-gateway --dry-run=client -o yaml | $K apply -f - >/dev/null
$K apply -f hack/envoy-gateway.yaml >/dev/null

# Root's values file, written by hand: the tuning `box init` must never touch.
# The wiring image is the locally built one, under the repository the chart's
# s5n variant names (the override hack/e2e-kind.sh passes as --set), and it is
# not a catalog question, so init has no opinion about it.
log "root's values file: the tuning init must leave alone"
cat >"$WORK/values.yaml" <<'EOF'
wiring:
  image:
    # The locally built image, under the repository the chart's s5n variant
    # names: the same override hack/e2e-kind.sh passes as --set.
    tag: e2e
EOF

init() { # init <output file>
  # --paths-in-node: the paths are in the kind node's filesystem, not this
  # host's, so init neither reports nor creates them, and apply skips its stat
  # the same way. On a real host init creates them (hack/e2e-box.sh cannot:
  # this host is not the node).
  "$WORK/box" init --yes --chart charts/s5n \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --namespace "$NS" --domain box.example.com \
    --data-path /srv/s5n/data --config-path /srv/s5n/config \
    --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr \
    --puid 1000 --pgid 1000 \
    --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$1" 2>&1
}

apply() { # apply <output file>
  # --paths-in-node: the grant's directories are in the kind node's filesystem,
  # not this host's, so box cannot stat them. It is the one place the check is
  # skipped, and only a script that wrote those directories itself passes it.
  "$WORK/box" apply --yes --chart charts/s5n \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$1" 2>&1
}

# storage_state is the two claims and two volumes, by resourceVersion: a second
# apply that touches any of them shows up as a changed number, and the claims
# are not release objects so Helm's revision cannot prove anything about them.
storage_state() {
  $K get pv seedbox-s5n-data seedbox-s5n-config -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion} {end}'
  $K -n "$NS" get pvc seedbox-s5n-data seedbox-s5n-config -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion} {end}'
}

revision() {
  $HELM list -n "$NS" -o json |
    python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["revision"] if r else 0)'
}
release_secrets() {
  $K -n "$NS" get secret -l owner=helm,name=s5n -o name | wc -l
}
status_field() { # status_field <python expression over `d`>
  $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.status\.json}' |
    python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

# ---- init 1: the install -----------------------------------------------------
log "box init: first install"
if init "$WORK/apply1.txt"; then
  cat "$WORK/apply1.txt"
else
  cat "$WORK/apply1.txt"; fail=1
fi
grep -q "first install" "$WORK/apply1.txt" || { echo "no 'first install' line"; fail=1; }
grep -q "^wiring ok" "$WORK/apply1.txt" || { echo "box did not report a healthy receipt"; fail=1; }
# The namespace was box's first write, and the plan said so: the cluster had
# none of it before this run.
grep -qF '+ Namespace seedbox (Pod Security restricted)' "$WORK/apply1.txt" ||
  { echo "the plan did not show the namespace it creates"; fail=1; }

# The receipt is the same object the helm path asserts, so the same checks
# apply: every wiring step ok, and the three that need a Plex token pending.
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
for pending in plex/claim tautulli/plex seerr/plex; do
  res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$pending'][0]" 2>/dev/null || echo missing)
  echo "  ${pending}: ${res} (expected pending: no Plex token in this e2e)"
  [ "$res" = pending ] || fail=1
done

$K -n "$NS" wait --for=condition=Ready pod -l 'app.kubernetes.io/instance=s5n,s5n.io/app' --timeout=300s || fail=1
# ---- storage: box created the claims, and they are bound ----
for kind in data config; do
  pv=seedbox-s5n-${kind}
  pvc=$pv
  pv_phase=$($K get pv "$pv" -o jsonpath='{.status.phase}')
  pvc_phase=$($K -n "$NS" get pvc "$pvc" -o jsonpath='{.status.phase}')
  pv_manager=$($K get pv "$pv" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')
  pv_path=$($K get pv "$pv" -o jsonpath='{.spec.hostPath.path}')
  pv_claim=$($K get pv "$pv" -o jsonpath='{.spec.claimRef.name}')
  pv_reclaim=$($K get pv "$pv" -o jsonpath='{.spec.persistentVolumeReclaimPolicy}')
  pvc_volume=$($K -n "$NS" get pvc "$pvc" -o jsonpath='{.spec.volumeName}')
  pvc_class=$($K -n "$NS" get pvc "$pvc" -o jsonpath='{.spec.storageClassName}')
  echo "  ${pv}: ${pv_phase} -> ${pvc}/${pvc_phase} (path ${pv_path}, claimRef ${pv_claim}, reclaim ${pv_reclaim}, managed-by ${pv_manager}, volumeName ${pvc_volume}, class '${pvc_class}')"
  [ "$pv_phase" = Bound ] || fail=1
  [ "$pvc_phase" = Bound ] || fail=1
  [ "$pv_manager" = box ] || fail=1
  [ "$pv_claim" = "$pvc" ] || fail=1
  [ "$pv_reclaim" = Retain ] || fail=1
  [ "$pvc_volume" = "$pv" ] || fail=1
  [ -z "$pvc_class" ] || fail=1
done
[ "$($K get pv seedbox-s5n-data -o jsonpath='{.spec.hostPath.path}')" = /srv/s5n/data ] || fail=1
[ "$($K get pv seedbox-s5n-config -o jsonpath='{.spec.hostPath.path}')" = /srv/s5n/config ] || fail=1

# ---- the namespace: box created it, at restricted ----
# init owns the namespace on a fresh cluster now, so it is what proves the
# claims could be written at all. The labels are the ones root's `adduser`
# sets: enforce is what admits a pod, audit and warn say the same level.
ns_label() { $K get namespace "$NS" -o jsonpath="{.metadata.labels.pod-security\.kubernetes\.io/$1}"; }
ns_enforce=$(ns_label enforce); ns_audit=$(ns_label audit); ns_warn=$(ns_label warn)
ns_manager=$($K get namespace "$NS" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')
echo "  namespace ${NS}: enforce=${ns_enforce} audit=${ns_audit} warn=${ns_warn} managed-by=${ns_manager}"
[ "$ns_enforce" = restricted ] || { echo "the namespace does not enforce restricted"; fail=1; }
[ "$ns_audit" = restricted ] || { echo "the namespace does not audit restricted"; fail=1; }
[ "$ns_warn" = restricted ] || { echo "the namespace does not warn restricted"; fail=1; }
[ "$ns_manager" = box ] || { echo "the namespace is not labelled managed-by box"; fail=1; }

# Zero writes includes the storage: the same resourceVersions after apply 2.
storage_before=$(storage_state)
ns_before=$($K get namespace "$NS" -o jsonpath='{.metadata.resourceVersion}')

rev1=$(revision); secrets1=$(release_secrets)
echo "release revision ${rev1} (${secrets1} release secrets)"
[ "$rev1" = 1 ] || { echo "first apply is not revision 1"; fail=1; }

# ---- init 2: the same answers ----------------------------------------------
# Re-running init is the other half of its contract: every question is prefilled
# from the files, so identical answers splice nothing, and the files must come
# out byte for byte. Its apply then has nothing to change, exactly like a second
# `box apply`.
log "box init: the same answers must splice nothing and apply nothing"
grant_before=$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)
values_before=$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)
if init "$WORK/apply2.txt"; then
  cat "$WORK/apply2.txt"
else
  cat "$WORK/apply2.txt"; fail=1
fi
grant_after=$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)
values_after=$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)
echo "grant   ${grant_before} -> ${grant_after}"
echo "values  ${values_before} -> ${values_after}"
[ "$grant_before" = "$grant_after" ] || { echo "the second init spliced into the grant"; fail=1; }
[ "$values_before" = "$values_after" ] || { echo "the second init spliced into the values file"; fail=1; }
grep -q "already says this" "$WORK/apply2.txt" || { echo "init did not say the grant was already right"; fail=1; }
grep -q "nothing to change" "$WORK/apply2.txt" || { echo "box did not say nothing to change"; fail=1; }
rev2=$(revision); secrets2=$(release_secrets)
echo "release revision ${rev2} (${secrets2} release secrets)"
# Zero writes means no new Helm revision, no new release secret (`box apply`
# does not call Helm at all on this path) and no duplicated app state: the
# receipt cannot have been rewritten either, because the wiring Job never ran.
[ "$rev2" = "$rev1" ] || { echo "the unchanged apply created revision ${rev2}"; fail=1; }
[ "$secrets2" = "$secrets1" ] || { echo "the unchanged apply wrote a release secret"; fail=1; }
storage_after=$(storage_state)
if [ "$storage_after" != "$storage_before" ]; then
  echo "the unchanged apply touched storage"
  echo "  before: ${storage_before}"
  echo "  after:  ${storage_after}"
  fail=1
else
  echo "storage untouched: ${storage_after}"
fi
ns_after=$($K get namespace "$NS" -o jsonpath='{.metadata.resourceVersion}')
if [ "$ns_after" != "$ns_before" ]; then
  echo "the unchanged apply touched the namespace"
  echo "  before: ${ns_before}"
  echo "  after:  ${ns_after}"
  fail=1
else
  echo "namespace untouched: ${ns_after}"
fi

# ---- apply 3: one key edited -------------------------------------------------
log "box apply: one edited value is diffed and applied"
# A resources limit, not a claim size: kind's local-path StorageClass cannot
# resize a PVC, so a size edit would fail inside the release rather than test
# the apply path. A limit is mutable on a running StatefulSet and lands in the
# workload spec, so this also proves a real rollout, not only stored values.
cat >>"$WORK/values.yaml" <<'EOF'

apps:
  sonarr:
    resources:
      limits:
        memory: 1Gi
EOF
if apply "$WORK/apply3.txt"; then
  cat "$WORK/apply3.txt"
else
  cat "$WORK/apply3.txt"; fail=1
fi
grep -qF '+ apps.sonarr.resources.limits.memory: "1Gi"' "$WORK/apply3.txt" ||
  { echo "the diff did not name the edited key"; fail=1; }
changes=$(grep -cE '^[+~] ' "$WORK/apply3.txt" || true)
echo "changed keys printed: ${changes}"
[ "$changes" = 1 ] || { echo "expected exactly one changed key, got ${changes}"; fail=1; }
rev3=$(revision)
echo "release revision ${rev3}"
[ "$rev3" = "$((rev1 + 1))" ] || { echo "the edited apply did not create one new revision"; fail=1; }
$HELM get values s5n -n "$NS" -o json |
  python3 -c 'import json,sys; d=json.load(sys.stdin); v=d["apps"]["sonarr"]["resources"]["limits"]["memory"]; print("release sonarr memory limit =", v); sys.exit(0 if v=="1Gi" else 1)' ||
  { echo "the release does not hold the edited value"; fail=1; }
# The rollout is the proof the value reached the workload, not only the
# release record: the container spec must carry the new limit.
$K -n "$NS" get statefulset sonarr -o jsonpath='{.spec.template.spec.containers[0].resources.limits.memory}' |
  grep -qx '1Gi' || { echo "sonarr's pod spec does not carry the new limit"; fail=1; }

# The editing itself must not have touched the second file: the grant is root's
# and neither apply nor a re-run of init writes it when nothing changed.
[ "$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)" = "$grant_before" ] ||
  { echo "box rewrote the grant"; fail=1; }

# ---- the claims outlive the release -------------------------------------
# The one destructive command an operator runs is `helm uninstall`: the data
# must not go with it. The claims and their volumes are not release objects, so
# this is not reclamation behaviour box configured -- it is what "not Helm's"
# means, and it is worth asserting rather than assuming.
log "helm uninstall keeps the claims and their volumes"
$HELM uninstall s5n -n "$NS" >/dev/null 2>&1 || { echo "helm uninstall failed"; fail=1; }
for kind in data config; do
  pv=seedbox-s5n-${kind}
  $K get pv "$pv" >/dev/null 2>&1 || { echo "helm uninstall deleted ${pv}"; fail=1; }
  $K -n "$NS" get pvc "$pv" >/dev/null 2>&1 || { echo "helm uninstall deleted claim ${pv}"; fail=1; }
  echo "  ${pv}: PV and claim still there"
done

# box is the whole interface: a client-go/klog line such as
# "I1107 ... warnings.go:107] ..." must never appear in what the operator reads.
for f in "$WORK"/apply*.txt; do
  if grep -Eq '^[IWE][0-9]{4} ' "$f"; then
    echo "client-go/klog noise in $(basename "$f"):"
    grep -E '^[IWE][0-9]{4} ' "$f"
    fail=1
  fi
done

if [ "$fail" != 0 ]; then
  log "wiring Job logs"
  $K -n "$NS" logs job/s5n-wiring --tail=80 || true
  log "box apply 3"
  cat "$WORK/apply3.txt" 2>/dev/null || true
fi

if [ "$fail" = 0 ]; then log "PASS"; else log "FAIL"; fi
[ "$KEEP" = --keep ] || $KIND delete cluster --name "$CLUSTER"
exit "$fail"
