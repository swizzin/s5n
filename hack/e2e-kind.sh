#!/usr/bin/env bash
# Phase-0 smoke test on kind: cluster -> Gateway API CRDs -> Traefik Gateway -> s5n chart,
# then assert pods Ready, routes Accepted, the Arr answers through the Gateway,
# and the hardlink invariant holds on /data.
# Usage: hack/e2e-kind.sh [--keep]
set -euo pipefail
cd "$(dirname "$0")/.."

KIND=${KIND:-kind}
CLUSTER=s5n
NS=seedbox
GW_API_VERSION=${GW_API_VERSION:-v1.7.0}
TRAEFIK_CHART_VERSION=${TRAEFIK_CHART_VERSION:-42.2.0}
KEEP=${1:-}
CTX="kind-${CLUSTER}"
K="kubectl --context ${CTX}"

log() { printf '\n==> %s\n' "$*"; }

if ! $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "creating kind cluster"
  $KIND create cluster --config hack/kind.yaml --wait 120s
fi

log "preparing host paths inside the node (the installer does this on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data/torrents/{tv,movies,tv-4k,movies-4k,misc} \
           /srv/s5n/data/media/{tv,movies,tv-4k,movies-4k} \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k}
  chown -R 1000:1000 /srv/s5n'

log "Gateway API CRDs ${GW_API_VERSION}"
$K apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GW_API_VERSION}/standard-install.yaml" >/dev/null

log "Traefik ${TRAEFIK_CHART_VERSION} as the Gateway"
helm repo add traefik https://traefik.github.io/charts >/dev/null 2>&1 || true
helm repo update traefik >/dev/null
helm --kube-context "$CTX" upgrade --install traefik traefik/traefik \
  --version "$TRAEFIK_CHART_VERSION" -n s5n-gateway --create-namespace \
  -f hack/traefik-values.yaml --wait --timeout 5m >/dev/null

log "namespace ${NS} with Pod Security restricted (enforced)"
$K create namespace "$NS" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K label --overwrite namespace "$NS" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted >/dev/null

log "installing s5n"
helm --kube-context "$CTX" upgrade --install s5n charts/s5n -n "$NS" \
  --set storage.data.size=10Gi --set storage.config.size=1Gi \
  --wait --timeout 10m

log "asserting"
fail=0
$K -n "$NS" get pods -o wide
$K -n "$NS" wait --for=condition=Ready pod -l app.kubernetes.io/instance=s5n --timeout=300s || fail=1

for r in $($K -n "$NS" get httproute -o name); do
  acc=$($K -n "$NS" get "$r" -o jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}')
  echo "$r Accepted=$acc"; [ "$acc" = True ] || fail=1
done

# Through the Gateway from the host (kind maps node 30080 -> host 18080).
for path in /sonarr/ping /radarr/ping /prowlarr/ping; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:18080${path}")
  echo "GET ${path} via gateway -> ${code}"; [ "$code" = 200 ] || fail=1
done
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:18080/qbittorrent/")
echo "GET /qbittorrent/ via gateway -> ${code}"; [ "$code" = 200 ] || fail=1

# Hardlink invariant: torrents/ and media/ are one filesystem, as seen by an Arr pod.
$K -n "$NS" exec sonarr-0 -- sh -c '
  set -e
  f=/data/torrents/tv/.s5n-linkcheck; l=/data/media/tv/.s5n-linkcheck
  rm -f "$f" "$l"; echo x > "$f"; ln "$f" "$l"
  n=$(stat -c %h "$l"); d1=$(stat -c %d "$f"); d2=$(stat -c %d "$l")
  rm -f "$f" "$l"
  echo "hardlink nlink=$n dev=$d1/$d2"; [ "$n" = 2 ] && [ "$d1" = "$d2" ]' || fail=1

# NetworkPolicy: prowlarr may reach sonarr; a pod outside the release may not.
$K -n "$NS" exec prowlarr-0 -- curl -s -o /dev/null -w 'prowlarr->sonarr %{http_code}\n' --max-time 5 http://sonarr:8989/sonarr/ping || fail=1
if $K -n default run np-probe --rm -i --restart=Never --image=curlimages/curl:8.11.1 --quiet -- \
     curl -s -o /dev/null --max-time 5 "http://sonarr.${NS}:8989/sonarr/ping"; then
  echo "outsider->sonarr: REACHABLE (policy not enforced)"; fail=1
else
  echo "outsider->sonarr: blocked"
fi

if [ "$fail" = 0 ]; then log "PASS"; else log "FAIL"; fi
[ "$KEEP" = --keep ] || $KIND delete cluster --name "$CLUSTER"
exit "$fail"
