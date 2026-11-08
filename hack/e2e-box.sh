#!/usr/bin/env bash
# The box-driven install path, on kind.
#
# hack/e2e-kind.sh installs the chart with `helm upgrade --install` and asserts
# the result. This script installs the same release the other way: root's grant
# and values file, then `box apply`, which reads both, checks them against the
# chart's schema, diffs, confirms and calls the Helm SDK itself. It then proves
# the three properties the apply path has to have:
#
#   1. a first apply installs the stack and its wiring receipt says ok;
#   2. a second apply with nothing edited says so and makes zero writes;
#   3. one edited value is printed as a diff, applied, and visible in the
#      release afterwards.
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

if ! $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
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

log "namespace ${NS} with Pod Security restricted (enforced)"
$K create namespace "$NS" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K label --overwrite namespace "$NS" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted >/dev/null

log "Gateway API CRDs and a Gateway (the chart's parentRef target)"
$HELM upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version "$ENVOY_GATEWAY_CHART_VERSION" -n envoy-gateway-system --create-namespace \
  --wait --timeout 5m >/dev/null
$K create namespace s5n-gateway --dry-run=client -o yaml | $K apply -f - >/dev/null
$K apply -f hack/envoy-gateway.yaml >/dev/null

# Root's two files, written exactly as `box adduser` would for root's own
# seedbox. The grant carries every decision hack/e2e-kind.sh leaves to chart
# defaults (every app, every path, the domain); the values file carries the
# tuning that helm run passes as --set: the claim sizes and the locally built
# image tag. Phase 3 appends one app setting to this file.
log "writing root's grant and values file"
cat >"$WORK/grant.yaml" <<'EOF'
namespace: seedbox

global:
  puid: 1000
  pgid: 1000

storage:
  data:
    hostPath: /srv/s5n/data
  config:
    hostPath: /srv/s5n/config

gateway:
  host: box.example.com
  parentRef:
    name: s5n
    namespace: s5n-gateway

networkPolicy:
  enabled: true

apps:
  qbittorrent:
    enabled: true
  sonarr:
    enabled: true
  radarr:
    enabled: true
  prowlarr:
    enabled: true
  plex:
    enabled: true
  tautulli:
    enabled: true
  seerr:
    enabled: true
EOF

grant_sum=$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)

cat >"$WORK/values.yaml" <<'EOF'
storage:
  data:
    size: 10Gi
  config:
    size: 1Gi

wiring:
  image:
    # The locally built image, under the repository the chart's s5n variant
    # names: the same override hack/e2e-kind.sh passes as --set.
    tag: e2e
EOF

apply() { # apply <output file>
  "$WORK/box" apply --yes --chart charts/s5n \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --kubeconfig "$WORK/kubeconfig" >"$1" 2>&1
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

# ---- apply 1: install -------------------------------------------------------
log "box apply: first install"
if apply "$WORK/apply1.txt"; then
  cat "$WORK/apply1.txt"
else
  cat "$WORK/apply1.txt"; fail=1
fi
grep -q "first install" "$WORK/apply1.txt" || { echo "no 'first install' line"; fail=1; }
grep -q "^wiring ok" "$WORK/apply1.txt" || { echo "box did not report a healthy receipt"; fail=1; }

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
rev1=$(revision); secrets1=$(release_secrets)
echo "release revision ${rev1} (${secrets1} release secrets)"
[ "$rev1" = 1 ] || { echo "first apply is not revision 1"; fail=1; }

# ---- apply 2: nothing edited -------------------------------------------------
log "box apply: an unchanged seedbox must be zero writes"
if apply "$WORK/apply2.txt"; then
  cat "$WORK/apply2.txt"
else
  cat "$WORK/apply2.txt"; fail=1
fi
grep -q "nothing to change" "$WORK/apply2.txt" || { echo "box did not say nothing to change"; fail=1; }
rev2=$(revision); secrets2=$(release_secrets)
echo "release revision ${rev2} (${secrets2} release secrets)"
# Zero writes means no new Helm revision, no new release secret (`box apply`
# does not call Helm at all on this path) and no duplicated app state: the
# receipt cannot have been rewritten either, because the wiring Job never ran.
[ "$rev2" = "$rev1" ] || { echo "the unchanged apply created revision ${rev2}"; fail=1; }
[ "$secrets2" = "$secrets1" ] || { echo "the unchanged apply wrote a release secret"; fail=1; }

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
# and box never writes it on apply.
[ "$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)" = "$grant_sum" ] ||
  { echo "box rewrote the grant"; fail=1; }

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
