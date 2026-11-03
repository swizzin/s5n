#!/usr/bin/env bash
# Phase-0/1 smoke test on kind: cluster -> Envoy Gateway (Gateway API) -> MetalLB (L2)
# -> s5n chart. Asserts pods Ready, routes Accepted, every app answers through the
# Gateway's LoadBalancer address, the peers NodePort accepts a TCP connect on the
# node's own address, the hardlink invariant holds on /data, and the wiring Job
# does its job twice: the first run connects everything, the second changes nothing.
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
IMAGE=s5n:e2e
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

log() { printf '\n==> %s\n' "$*"; }
fail=0

if ! $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
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
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k}
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

log "namespace ${NS} with Pod Security restricted (enforced)"
$K create namespace "$NS" --dry-run=client -o yaml | $K apply -f - >/dev/null
$K label --overwrite namespace "$NS" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/warn=restricted >/dev/null

log "GatewayClass + Gateway (the chart's default parentRef target)"
$K create namespace s5n-gateway --dry-run=client -o yaml | $K apply -f - >/dev/null
$K apply -f hack/envoy-gateway.yaml >/dev/null

# The chart under test, with the locally built image instead of the published one.
install_release() {
  $HELM upgrade --install s5n charts/s5n -n "$NS" \
    --set storage.data.size=10Gi --set storage.config.size=1Gi \
    --set wiring.image.repository=s5n \
    --set wiring.image.tag=e2e \
    --set wiring.image.digest="" \
    --wait --timeout 15m
}

log "installing s5n (the wiring Job runs as the post-install hook)"
install_release

log "asserting"
$K -n "$NS" get pods -o wide
# Only the apps: the wiring Job's pod shares the instance label and ends Completed,
# which never satisfies Ready.
$K -n "$NS" wait --for=condition=Ready pod -l 'app.kubernetes.io/instance=s5n,s5n.io/app' --timeout=300s || fail=1

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
  data/hardlink/tv data/hardlink/movies; do
  res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$expected'][0]" 2>/dev/null || echo missing)
  echo "  ${expected}: ${res}"; [ "$res" = ok ] || fail=1
done

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
