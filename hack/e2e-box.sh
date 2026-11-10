#!/usr/bin/env bash
# The box-driven install path, on kind.
#
# hack/e2e-kind.sh installs the chart with `helm upgrade --install` and asserts
# the result. This script installs the same release the other way: `box
# bootstrap` decides the cluster, installs the platform layer (Envoy Gateway's
# Gateway API CRDs, MetalLB, the GatewayClass and Gateway, the admission
# policy), then runs init for root, which writes root's grant and values file
# and hands over to `box apply`.
#
# The chart is installed the way a user installs one: `helm package` from this
# working tree, `helm push` to a distribution registry container bound to 127.0.0.1, and
# box pulls it from there with --chart-repo (and --chart-version). box never
# reads `charts/` from its own tree; the kind cluster never pulls the chart.
#
# Three scenarios, three clusters:
#
#   fresh         a cluster with nothing installed. The Gateway box applies
#                 gets no address, so MetalLB and its pool are installed, and
#                 the address is then checked from the host. Then:
#                 a second bootstrap installs nothing (no Helm revision, no
#                 changed spec or label on anything box owns, byte-identical
#                 seedbox files), one edited value is printed as a diff and
#                 applied, and the claims outlive `helm uninstall`.
#   preinstalled  MetalLB installed the way a user would -- the upstream
#                 manifest and an IPAddressPool, not through box -- before
#                 `box bootstrap` ever runs. The probe answers, so box installs
#                 no MetalLB release and no s5n-pool, removes its probe
#                 Service, and the Gateway still becomes Programmed on an
#                 address from the pool that was already there.
#   foreign-gateway  a different Gateway API implementation -- NGINX Gateway
#                 Fabric -- installed before box runs, on Gateway API's
#                 standard channel. Envoy Gateway is the only implementation
#                 box supports (logins are checked at the edge by one provider
#                 through Envoy's SecurityPolicy), so box must refuse that
#                 cluster, naming the class and its controller, and write
#                 nothing: no namespace, no GatewayClass, no record.
#
# Why one script and three clusters instead of a flag inside hack/e2e-kind.sh:
# the existing e2e is the chart's acceptance test, and its helm install is what
# proves the chart still installs the way users install it today. These
# scenarios are about box's decision, share the bootstrap command line, and
# disagree about what is on the cluster before it runs -- so they cannot share
# a cluster. `all` runs this script once per scenario, which gives each its own
# work directory, cluster name and exit code.
#
# Usage: hack/e2e-box.sh [--keep] [fresh|preinstalled|foreign-gateway|all]   (default: all)
set -euo pipefail
cd "$(dirname "$0")/.."

KIND=${KIND:-kind}
NS=seedbox
IMAGE=ghcr.io/swizzin/s5n:e2e
METALLB_VERSION=${METALLB_VERSION:-v0.16.1}
# The third scenario's Gateway API implementation. NGINX Gateway Fabric is one
# Deployment plus one data-plane pod per Gateway, and its Gateway class is
# created by its own chart. It is here because it is an implementation box does
# not support: the scenario proves the refusal, not a working Gateway. Pinned,
# like every other platform version here.
GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-v1.6.3}
NGF_VERSION=${NGF_VERSION:-2.8.1}

KEEP=""
SCENARIO=all
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=--keep ;;
    fresh | preinstalled | foreign-gateway | all) SCENARIO=$arg ;;
    -h | --help)
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $arg (want fresh, preinstalled, foreign-gateway or all)" >&2
      exit 2
      ;;
  esac
done

if [ "$SCENARIO" = all ]; then
  status=0
  "$0" fresh $KEEP || status=1
  "$0" preinstalled $KEEP || status=1
  "$0" foreign-gateway $KEEP || status=1
  exit "$status"
fi

case "$SCENARIO" in
  fresh) CLUSTER=s5n-box ;;
  preinstalled) CLUSTER=s5n-box-lb ;;
  foreign-gateway) CLUSTER=s5n-box-gw ;;
esac
CTX="kind-${CLUSTER}"
K="kubectl --context ${CTX}"
HELM="helm --kube-context ${CTX}"
WORK=$(mktemp -d)
START=$(date +%s)
REGISTRY=""
# The registry is a container on this host: it must not outlive the run, even
# when a scenario fails before its own cleanup.
cleanup() {
  if [ -n "$REGISTRY" ]; then
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

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
log "scenario ${SCENARIO}: kubeconfig ${KUBECONFIG} (~/.kube/config is not used)"

# ---- the chart a user would get -------------------------------------------
# box only installs published charts (docs/box.md), so the e2e publishes one:
# `helm package` from this working tree, `helm push` to a registry on this
# host, and box pulls it by repository (and version) like any release. The
# registry is a distribution container published on 127.0.0.1 only, which is
# also the only host box speaks plain HTTP to (loopback registries). The kind
# cluster never pulls the chart: box runs on the host.
# distribution v3.1.2, pinned by digest like every other image the e2e runs.
REGISTRY_IMAGE=registry:3@sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c
REGISTRY=s5n-e2e-registry-${SCENARIO}
log "OCI registry (distribution 3.1.2) on 127.0.0.1 only (container ${REGISTRY})"
docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
docker run -d --rm --name "$REGISTRY" -p 127.0.0.1::5000 "$REGISTRY_IMAGE" >/dev/null
REGISTRY_PORT=$(docker port "$REGISTRY" 5000/tcp | head -1 | sed 's/.*://')
[ -n "$REGISTRY_PORT" ] || {
  echo "the local registry published no port"
  exit 1
}
for i in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:${REGISTRY_PORT}/v2/" >/dev/null 2>&1 && break
  [ "$i" = 30 ] && {
    echo "the local registry never answered on 127.0.0.1:${REGISTRY_PORT}"
    exit 1
  }
  sleep 1
done

# The version the packaged chart declares is the version box is built as and
# the one it asks the registry for. It is the chart's own version, so the e2e
# cannot drift from what `helm package` writes.
CHART_VERSION=$(helm show chart charts/s5n | awk '$1 == "version:" {print $2}')
[ -n "$CHART_VERSION" ] || {
  echo "charts/s5n/Chart.yaml declares no version"
  exit 1
}
CHART_REPO="oci://127.0.0.1:${REGISTRY_PORT}/charts/s5n"
log "packaging charts/s5n ${CHART_VERSION} from this commit and pushing it to ${CHART_REPO}"
mkdir -p "$WORK/charts"
helm package charts/s5n --destination "$WORK/charts" >/dev/null
helm push "$WORK/charts/s5n-${CHART_VERSION}.tgz" "oci://127.0.0.1:${REGISTRY_PORT}/charts" --plain-http >/dev/null

# Each scenario's premise is a cluster in one known state, so a leftover from an
# interrupted run is deleted rather than reused: reusing one would leave the
# platform (or MetalLB) in place and the probes would say "use", which is not
# the path under test.
if $KIND get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "deleting the leftover kind cluster (this test needs a fresh one)"
  $KIND delete cluster --name "$CLUSTER"
fi

log "creating kind cluster ${CLUSTER}"
$KIND create cluster --name "$CLUSTER" --config hack/kind.yaml --wait 120s

log "building the s5n image (the wiring Job and the qBittorrent credential seed)"
docker build -q -t "$IMAGE" .
$KIND load docker-image --name "$CLUSTER" "$IMAGE"

log "building box ${CHART_VERSION} (the version it pulls the chart as)"
CGO_ENABLED=0 go build -ldflags "-X main.version=${CHART_VERSION}" -o "$WORK/box" ./cmd/box
built=$("$WORK/box" version)
[ "$built" = "$CHART_VERSION" ] || {
  echo "box reports version '${built}', want the chart version ${CHART_VERSION}"
  fail=1
}
$KIND get kubeconfig --name "$CLUSTER" >"$WORK/kubeconfig"

# Only what `box init` creates on a real host, and only because --paths-in-node
# keeps it from touching this node itself: the two volume roots and one
# /config/<instance> directory per enabled app (setup.settingsDirs). The data
# categories are deliberately absent — downloads/<category> and media/<category>
# are the wiring Job's, and pre-creating them would hide a first install that
# cannot make the root folders its Arr steps need.
log "preparing host paths inside the node (what box init creates on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,plex,tautulli,seerr}
  chown -R 1000:1000 /srv/s5n'

# Nothing below may prepare the data categories: the wiring Job owns them, and a
# first install that cannot create its own root folders is the failure this e2e
# is here to catch. A guard rather than a comment, so a pre-create cannot creep
# back in. Every scenario installs through box, so this holds for all three.
for d in /srv/s5n/data/downloads /srv/s5n/data/media; do
  if docker exec "${CLUSTER}-control-plane" test -e "$d"; then
    echo "${d} already exists before the install: the e2e prepared a path the wiring Job must create"; fail=1
  fi
done

# Root's values file, written by hand: the tuning `box init` must never touch.
# The wiring image is the locally built one, under the repository the chart's
# s5n variant names (the override hack/e2e-kind.sh passes as --set), and it is
# not a catalog question, so init has no opinion about it. It has to exist
# before bootstrap: bootstrap hands over to init, which reads it.
log "root's values file: the tuning init must leave alone"
cat >"$WORK/values.yaml" <<'EOF'
wiring:
  image:
    # The locally built image, under the repository the chart's s5n variant
    # names: the same override hack/e2e-kind.sh passes as --set.
    tag: e2e
EOF

bootstrap() { # bootstrap <output file>
  # The platform and root's seedbox are one command now. --record points the
  # cluster decision at this work directory: on a real host it is
  # /etc/swizzin/cluster.yaml, written by root, and the e2e is not root.
  #
  # No --chart-version: bootstrap takes the default-version path, so the e2e
  # exercises the newest-release selection against a real registry. The chart
  # is the one pushed above; box pulls it over loopback.
  "$WORK/box" bootstrap --yes --chart-repo "$CHART_REPO" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --namespace "$NS" --domain box.example.com \
    --data-path /srv/s5n/data --config-path /srv/s5n/config \
    --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr \
    --puid 1000 --pgid 1000 \
    --paths-in-node --record "$WORK/cluster.yaml" \
    --kubeconfig "$WORK/kubeconfig" >"$1" 2>&1
}

init() { # init <output file>
  # --paths-in-node: the paths are in the kind node's filesystem, not this
  # host's, so init neither reports nor creates them, and apply skips its stat
  # the same way. On a real host init creates them (hack/e2e-box.sh cannot:
  # this host is not the node).
  #
  # --chart-version pins the release to the package pushed above: init reads
  # one chart, and the apply it hands over to installs that same value.
  "$WORK/box" init --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
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
  "$WORK/box" apply --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
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

helm_revision() { # helm_revision <namespace>
  $HELM list -n "$1" -o json |
    python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["revision"] if r else 0)'
}

# What box owns, not the whole object: a controller rewriting a status or a
# managedFields timestamp bumps resourceVersion without box having written
# anything, so comparing resourceVersions makes this assert flaky. A spec or
# label change is a write; status churn is not.
platform_spec() {
  python3 -c 'import json,sys; o=json.load(sys.stdin); print(json.dumps({"spec": o.get("spec"), "labels": o.get("metadata", {}).get("labels")}, sort_keys=True))'
}

# The probe is the Gateway's own status now. A throwaway Service would be the
# old design coming back, so its name is a tripwire rather than a leftover to
# clean: nothing may create it.
no_probe_service() {
  if $K get service -A --no-headers -o custom-columns=NAME:.metadata.name 2>/dev/null |
    grep -qx box-loadbalancer-probe; then
    echo "something created a box-loadbalancer-probe Service: the Gateway is the probe"
    fail=1
  fi
}

# ---- scenario: fresh --------------------------------------------------------
fresh() {
  # ---- bootstrap 1: the platform and root's first install -------------------
  # The cluster has nothing: no CRDs, no LoadBalancer, no Gateway, no policy. So
  # the decision says "install" for each, and bootstrap must apply all of it
  # before init writes root's files. The plan is the review; --yes is the answer,
  # and the init half is the same `first install` `box init` prints on its own.
  #
  # MetalLB is the one step the plan cannot promise: whether it is needed is
  # settled by the Gateway's own status after the confirm. On a bare kind
  # cluster nothing assigns an address, so the question runs its 60 seconds and
  # MetalLB follows.
  log "box bootstrap: the platform layer, then root's first install"
  if bootstrap "$WORK/bootstrap1.txt"; then
    cat "$WORK/bootstrap1.txt"
  else
    cat "$WORK/bootstrap1.txt"; fail=1
  fi
  grep -q "^platform plan:" "$WORK/bootstrap1.txt" || { echo "bootstrap printed no platform plan"; fail=1; }
  grep -qF "+ envoy-gateway 1.9.3 (Helm, envoy-gateway-system)" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not install Envoy Gateway at its pinned version"; fail=1; }
  grep -qF "+ GatewayClass eg and Gateway s5n-gateway/s5n" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not apply the Gateway before the address layer"; fail=1; }
  grep -qF "? Gateway s5n-gateway/s5n getting an address" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print the Gateway's address as the question"; fail=1; }
  grep -qF "? metallb 0.16.1 (Helm, metallb-system), unless the Gateway gets an address without it" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print MetalLB as conditional"; fail=1; }
  grep -qF "? IPAddressPool s5n-pool" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print the MetalLB pool as conditional"; fail=1; }
  grep -qF "the Gateway has no address (AddressNotAssigned) after waiting up to 15s; installing MetalLB" "$WORK/bootstrap1.txt" ||
    { echo "the Gateway's own status did not decide the install"; fail=1; }
  grep -qF "the Gateway answers on " "$WORK/bootstrap1.txt" ||
    { echo "box never checked that the Gateway's address answers from the host"; fail=1; }
  # The first run installs, it does not verify: the cluster had none of it.
  if grep -q "^  = envoy-gateway" "$WORK/bootstrap1.txt"; then
    echo "bootstrap thought Envoy Gateway was already installed on a fresh cluster"; fail=1
  fi
  # cert-manager is the one component this lane does not install. Silence would
  # read as "nothing to do"; the line is asserted so it cannot regress into a
  # skip.
  grep -q "cert-manager: box does not install it yet" "$WORK/bootstrap1.txt" ||
    { echo "an unhandled cert-manager install was not reported"; fail=1; }

  # The init half ran in the same process: root's first release, and the
  # namespace box created because the cluster had none.
  # The chart was pulled from the registry the e2e pushed it to, not read from
  # this checkout: box only installs published charts. Bootstrap used the
  # default version, so the ref names the version box selected.
  grep -qF "chart: s5n ${CHART_VERSION} (${CHART_REPO}:${CHART_VERSION})" "$WORK/bootstrap1.txt" ||
    { echo "box did not pull ${CHART_REPO}:${CHART_VERSION}"; fail=1; }
  grep -q "first install" "$WORK/bootstrap1.txt" || { echo "no 'first install' line"; fail=1; }
  grep -q "^wiring ok" "$WORK/bootstrap1.txt" || { echo "box did not report a healthy receipt"; fail=1; }
  # The data path was empty before this run: these steps are the wiring Job
  # making both trees itself, before the Arr root-folder steps that need them.
  for d in downloads/tv media/tv downloads/movies media/movies; do
    got=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='data/dirs/$d'][0]" 2>/dev/null || echo missing)
    echo "  data/dirs/${d}: ${got}"
    [ "$got" = created ] || { echo "the wiring Job did not create data/dirs/${d}"; fail=1; }
  done
  # Created by the Job, so owned by the release's uid:gid — the owner init
  # promises, and the one the apps themselves run as.
  for d in /srv/s5n/data/downloads/tv /srv/s5n/data/media/tv; do
    got=$(docker exec "${CLUSTER}-control-plane" stat -c '%u:%g' "$d" 2>/dev/null || echo missing)
    echo "  ${d}: owner ${got}"
    [ "$got" = 1000:1000 ] || { echo "${d} is not owned by the release's uid:gid"; fail=1; }
  done
  # The namespace was box's first write, and the plan said so: the cluster had
  # none of it before this run.
  grep -qF '+ Namespace seedbox (Pod Security restricted)' "$WORK/bootstrap1.txt" ||
    { echo "the plan did not show the namespace it creates"; fail=1; }

  # What the install did: MetalLB is a release, its pool is the node's own
  # address as a /32, the Gateway reports that address, and the check box
  # printed passed on it.
  no_probe_service
  [ "$(helm_revision metallb-system)" != 0 ] || { echo "MetalLB is not a Helm release after the install"; fail=1; }
  # The release is box's because box labelled it, not because of its name.
  $HELM get metadata metallb -n metallb-system -o json |
    python3 -c 'import json,sys; d=json.load(sys.stdin); m=d["labels"].get("app.kubernetes.io/managed-by"); print("  metallb release managed-by:", m); sys.exit(0 if m=="box" else 1)' || fail=1
  node_ip=$($K get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  pool_addr=$($K -n metallb-system get ipaddresspool s5n-pool -o jsonpath='{.spec.addresses[0]}')
  gw_addr=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.addresses[0].value}')
  gw_programmed=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}')
  echo "  node ${node_ip}, MetalLB pool ${pool_addr}, Gateway ${gw_addr} (Programmed ${gw_programmed})"
  [ "$pool_addr" = "${node_ip}/32" ] || { echo "box's pool is ${pool_addr}, want ${node_ip}/32"; fail=1; }
  [ "$gw_addr" = "$node_ip" ] || { echo "the Gateway reports ${gw_addr}, want the pool's ${node_ip}"; fail=1; }
  [ "$gw_programmed" = True ] || { echo "the Gateway is not Programmed"; fail=1; }
  grep -qF "the Gateway answers on ${gw_addr}:80 (HTTP " "$WORK/bootstrap1.txt" ||
    { echo "box did not report the reachability check on ${gw_addr}"; fail=1; }

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

  # ---- bootstrap 2: nothing installed, nothing to install -------------------
  # The idempotence contract of the platform half: the probes now find the CRDs
  # and an Accepted GatewayClass, the releases are at their pinned versions, and
  # box's objects are server-side applies it already owns. A second bootstrap may
  # only verify: no Helm revision (box never calls Helm for a release at its
  # pinned version), no changed resourceVersion on anything it applied, and both
  # seedbox files byte-identical.
  #
  # MetalLB is box's own release at the pinned version, which is itself the
  # answer the question would look for: the second run verifies it and never
  # asks about the Gateway at all.
  platform_state() {
    echo "releases=$(helm_revision envoy-gateway-system),$(helm_revision metallb-system),$(helm_revision "$NS")"
    echo "gatewayclass=$($K get gatewayclass eg -o json | platform_spec)"
    echo "gateway=$($K -n s5n-gateway get gateway s5n -o json | platform_spec)"
    echo "pool=$($K -n metallb-system get ipaddresspool s5n-pool -o json | platform_spec)"
    echo "l2=$($K -n metallb-system get l2advertisement s5n-l2 -o json | platform_spec)"
    for p in s5n-blessed-images s5n-granted-service; do
      echo "vap-${p}=$($K get validatingadmissionpolicy "$p" -o json | platform_spec)"
    done
    echo "s5n-system=$($K get namespace s5n-system -o json | platform_spec)"
  }
  platform_before=$(platform_state)
  grant_before2=$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)
  values_before2=$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)

  log "box bootstrap: a second run must install nothing and change nothing"
  if bootstrap "$WORK/bootstrap2.txt"; then
    cat "$WORK/bootstrap2.txt"
  else
    cat "$WORK/bootstrap2.txt"; fail=1
  fi
  grep -qF "= metallb: already at 0.16.1" "$WORK/bootstrap2.txt" ||
    { echo "the second run did not verify MetalLB"; fail=1; }
  # box's own MetalLB is the answer the question would look for, so the second
  # run does not ask it and does not wait for a Gateway it did not write.
  if grep -q "getting an address" "$WORK/bootstrap2.txt"; then
    echo "the second run asked whether the Gateway needed MetalLB it already owns"; fail=1
  fi
  # Envoy Gateway is used, not managed, once its CRDs and an Accepted
  # GatewayClass are there: the probes verify the capability, so the second run
  # must not mention the release at all.
  if grep -q "envoy-gateway" "$WORK/bootstrap2.txt"; then
    echo "the second run took the Envoy Gateway release back over"; fail=1
  fi
  if grep -qE "^\s+[+?] (envoy-gateway|metallb) " "$WORK/bootstrap2.txt"; then
    echo "the second run planned an install on an installed platform"; fail=1
  fi
  grep -q "already says this" "$WORK/bootstrap2.txt" || { echo "the second init did not say the grant was already right"; fail=1; }
  grep -q "nothing to change" "$WORK/bootstrap2.txt" || { echo "the second init did not say nothing to change"; fail=1; }
  no_probe_service

  platform_after=$(platform_state)
  if [ "$platform_after" != "$platform_before" ]; then
    echo "the second bootstrap touched the platform"
    echo "  before: ${platform_before}"
    echo "  after:  ${platform_after}"
    fail=1
  else
    echo "platform untouched: ${platform_after}"
  fi
  [ "$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)" = "$grant_before2" ] ||
    { echo "the second bootstrap rewrote the grant"; fail=1; }
  [ "$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)" = "$values_before2" ] ||
    { echo "the second bootstrap rewrote the values file"; fail=1; }

  # ---- init 2: the same answers ---------------------------------------------
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

  # ---- apply 3: one key edited -----------------------------------------------
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

  # ---- the claims outlive the release ----------------------------------------
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
}

# install_metallb puts MetalLB on the cluster the way a user would: the
# upstream manifest and an L2 pool carved from the bottom of the kind docker
# network, not through box. It sets MANUAL_POOL for the assertions that follow.
# The preinstalled scenario needs this exact state: an address layer box did
# not install and must not touch.
install_metallb() {
  log "MetalLB ${METALLB_VERSION} from the upstream manifest, not through box"
  $K apply -f "https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml" >/dev/null
  $K -n metallb-system rollout status deploy/controller --timeout=180s
  $K -n metallb-system rollout status daemonset/speaker --timeout=180s
  # The pool below passes MetalLB's validating webhook, which the controller
  # serves; wait for its endpoints so the apply does not race it, and retry the
  # apply itself because an endpoint address only says the pod is Ready (the
  # webhook can still be loading its certificate). Both are hack/e2e-kind.sh's
  # experience, repeated here rather than trusted.
  for i in $(seq 1 30); do
    ep=$($K -n metallb-system get endpoints metallb-webhook-service -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null || true)
    [ -n "$ep" ] && break
    sleep 2
  done

  # Carve a small L2 pool from the top of the kind docker network's IPv4 subnet,
  # the same trick hack/e2e-kind.sh uses. The name is deliberately not box's:
  # after bootstrap, s5n-pool existing at all would mean box had installed its
  # own pool over this one.
  kind_subnet=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -v ':' | head -1)
  pool=$(python3 - "$kind_subnet" <<'PY'
import ipaddress, sys
net = ipaddress.ip_network(sys.argv[1], strict=False)
ips = list(net.hosts())[-16:]
print(f"{ips[0]}-{ips[-1]}")
PY
)
  echo "  address pool: ${pool}"
  preinstalled_pool() {
    $K apply -f - >/dev/null <<EOF
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: pre-existing-pool
  namespace: metallb-system
spec:
  addresses:
    - ${pool}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: pre-existing-l2
  namespace: metallb-system
spec:
  ipAddressPools:
    - pre-existing-pool
EOF
  }
  for i in $(seq 1 30); do
    preinstalled_pool 2>"$WORK/metallb.err" && break
    [ "$i" = 30 ] && { cat "$WORK/metallb.err" >&2; fail=1; }
    sleep 2
  done
  MANUAL_POOL=$pool
}

# ---- scenario: preinstalled --------------------------------------------------
# The same cluster state a box on k3s (servicelb), on a cloud provider, or with
# MetalLB installed by hand already has: LoadBalancer Services get addresses
# before box runs. box must find out, by asking, that it does not need to
# install anything -- and must leave the operator's address layer alone.
preinstalled() {
  install_metallb
  [ "$fail" = 0 ] || return 0
  pool=$MANUAL_POOL

  # The proof the cluster is in the state under test: a LoadBalancer Service
  # gets an address before box has done anything.
  $K create namespace lb-check --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K -n lb-check apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: before-box
spec:
  type: LoadBalancer
  ports:
    - port: 80
EOF
  before_addr=""
  for i in $(seq 1 30); do
    before_addr=$($K -n lb-check get service before-box -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    [ -n "$before_addr" ] && break
    sleep 1
  done
  $K delete namespace lb-check --wait=false >/dev/null
  [ -n "$before_addr" ] || { echo "the pre-installed MetalLB did not answer a LoadBalancer Service"; fail=1; }
  echo "  a LoadBalancer answers before box runs: ${before_addr}"

  # ---- bootstrap: box must use what answers ---------------------------------
  log "box bootstrap: the Gateway gets an address without MetalLB, so none is installed"
  if bootstrap "$WORK/bootstrap1.txt"; then
    cat "$WORK/bootstrap1.txt"
  else
    cat "$WORK/bootstrap1.txt"; fail=1
  fi
  grep -qF "? metallb 0.16.1 (Helm, metallb-system), unless the Gateway gets an address without it" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print MetalLB as conditional"; fail=1; }
  # The plan prints the question on every run -- that is the point of it -- so
  # what must not happen is the answer: no install, and no verification of a
  # release box does not own.
  if grep -q "installing metallb" "$WORK/bootstrap1.txt"; then
    echo "box installed MetalLB beside the one that was already there"; fail=1
  fi
  if grep -q "= metallb" "$WORK/bootstrap1.txt"; then
    echo "box treated the pre-installed MetalLB as its own release"; fail=1
  fi
  grep -q "the Gateway already has an address: " "$WORK/bootstrap1.txt" ||
    { echo "the Gateway's own address did not answer the question"; fail=1; }
  grep -q "^wiring ok" "$WORK/bootstrap1.txt" || { echo "box did not report a healthy receipt"; fail=1; }

  # Nothing box owns was installed: no release, no pool, no advertisement, and
  # no probe Service (the Gateway's own status is the probe).
  [ "$(helm_revision metallb-system)" = 0 ] ||
    { echo "box created a metallb Helm release beside the one that was already there"; fail=1; }
  if $K -n metallb-system get ipaddresspool s5n-pool >/dev/null 2>&1; then
    echo "box created its own IPAddressPool beside the one that was already there"; fail=1
  fi
  if $K -n metallb-system get l2advertisement s5n-l2 >/dev/null 2>&1; then
    echo "box created its own L2Advertisement beside the one that was already there"; fail=1
  fi
  no_probe_service
  # The operator's pool is untouched: still the range it was given, not the
  # node's /32 box would have written.
  pool_now=$($K -n metallb-system get ipaddresspool pre-existing-pool -o jsonpath='{.spec.addresses[0]}')
  [ "$pool_now" = "$pool" ] || { echo "box rewrote the pool it did not own: ${pool_now}"; fail=1; }

  # The Gateway is served by the address layer that was already there: its
  # address has to come out of the pre-existing pool, not the node's address,
  # and box has to have said so.
  lb_addr=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.addresses[0].value}')
  gw_programmed=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}')
  echo "  Gateway address: ${lb_addr} (Programmed ${gw_programmed})"
  [ -n "$lb_addr" ] || { echo "the Gateway has no address"; fail=1; }
  [ "$gw_programmed" = True ] || { echo "the Gateway is not Programmed"; fail=1; }
  grep -qF "the Gateway already has an address: ${lb_addr}; MetalLB and its address pool are not installed" "$WORK/bootstrap1.txt" ||
    { echo "box did not say the Gateway's own address meant no MetalLB"; fail=1; }
  grep -qF "the Gateway answers on ${lb_addr}:80 (HTTP " "$WORK/bootstrap1.txt" ||
    { echo "box did not report the reachability check on ${lb_addr}"; fail=1; }
  python3 - "$lb_addr" "$pool" <<'PY' || fail=1
import ipaddress, sys
addr, pool = sys.argv[1], sys.argv[2]
first, last = pool.split("-")
if not (ipaddress.ip_address(first) <= ipaddress.ip_address(addr) <= ipaddress.ip_address(last)):
    print(f"the Gateway's address {addr} is not from the pool that was already there ({pool})")
    sys.exit(1)
PY

  $K -n "$NS" wait --for=condition=Ready pod -l 'app.kubernetes.io/instance=s5n,s5n.io/app' --timeout=300s || fail=1

  # ---- a second bootstrap: still nothing installed -------------------------
  # box's MetalLB is still not a release it can see, so the second run asks the
  # same question again and takes the same answer -- without installing
  # anything, and without a new revision of root's seedbox.
  platform_state() {
    echo "releases=$(helm_revision envoy-gateway-system),$(helm_revision metallb-system),$(helm_revision "$NS")"
    echo "gatewayclass=$($K get gatewayclass eg -o json | platform_spec)"
    echo "gateway=$($K -n s5n-gateway get gateway s5n -o json | platform_spec)"
    echo "pool=$($K -n metallb-system get ipaddresspool pre-existing-pool -o json | platform_spec)"
    echo "l2=$($K -n metallb-system get l2advertisement pre-existing-l2 -o json | platform_spec)"
    for p in s5n-blessed-images s5n-granted-service; do
      echo "vap-${p}=$($K get validatingadmissionpolicy "$p" -o json | platform_spec)"
    done
    echo "s5n-system=$($K get namespace s5n-system -o json | platform_spec)"
  }
  platform_before=$(platform_state)
  grant_before=$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)
  values_before=$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)
  rev1=$(revision)

  log "box bootstrap: a second run installs nothing either"
  if bootstrap "$WORK/bootstrap2.txt"; then
    cat "$WORK/bootstrap2.txt"
  else
    cat "$WORK/bootstrap2.txt"; fail=1
  fi
  grep -qF "the Gateway already has an address: " "$WORK/bootstrap2.txt" ||
    { echo "the second run did not ask the same question"; fail=1; }
  if grep -q "installing metallb" "$WORK/bootstrap2.txt"; then
    echo "the second run installed MetalLB"; fail=1
  fi
  grep -q "nothing to change" "$WORK/bootstrap2.txt" || { echo "the second init did not say nothing to change"; fail=1; }
  [ "$(helm_revision metallb-system)" = 0 ] ||
    { echo "the second run created a metallb Helm release"; fail=1; }
  if $K -n metallb-system get ipaddresspool s5n-pool >/dev/null 2>&1; then
    echo "the second run created box's pool"; fail=1
  fi
  no_probe_service
  platform_after=$(platform_state)
  if [ "$platform_after" != "$platform_before" ]; then
    echo "the second bootstrap touched the platform"
    echo "  before: ${platform_before}"
    echo "  after:  ${platform_after}"
    fail=1
  else
    echo "platform untouched: ${platform_after}"
  fi
  [ "$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)" = "$grant_before" ] ||
    { echo "the second bootstrap rewrote the grant"; fail=1; }
  [ "$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)" = "$values_before" ] ||
    { echo "the second bootstrap rewrote the values file"; fail=1; }
  [ "$(revision)" = "$rev1" ] || { echo "the second bootstrap created a release revision"; fail=1; }
}

# ---- scenario: foreign-gateway ----------------------------------------------
# A Gateway API implementation box does not support. NGINX Gateway Fabric v2 is
# one Deployment plus a data-plane pod per Gateway; its chart creates the
# GatewayClass `nginx` and its controller accepts it. Envoy Gateway is the only
# implementation box supports (logins are checked at the edge by one provider
# through Envoy's SecurityPolicy), so an Accepted foreign class is a refusal:
# box names the class and its controller and writes nothing at all -- no
# namespace, no GatewayClass eg, no Envoy Gateway, no record.
foreign_gateway() {
  log "Gateway API ${GATEWAY_API_VERSION} (the standard channel NGINX Gateway Fabric is built against)"
  $K apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null

  log "NGINX Gateway Fabric ${NGF_VERSION}, not through box"
  $HELM upgrade --install ngf oci://ghcr.io/nginx/charts/nginx-gateway-fabric \
    --version "${NGF_VERSION}" -n nginx-gateway --create-namespace --wait --timeout 5m >/dev/null
  accepted=""
  for i in $(seq 1 60); do
    accepted=$($K get gatewayclass nginx -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || true)
    [ "$accepted" = True ] && break
    sleep 2
  done
  [ "$accepted" = True ] || { echo "the NGINX Gateway Fabric GatewayClass was never Accepted, so the scenario proves nothing"; fail=1; return 0; }

  # ---- bootstrap: box refuses the cluster ----------------------------------
  # The refused class is the one piece of evidence; the sentence asserted below
  # is the contract, so it is matched exactly. A non-zero exit is part of it.
  log "box bootstrap: a foreign Gateway implementation is refused, and nothing is written"
  if bootstrap "$WORK/bootstrap1.txt"; then
    cat "$WORK/bootstrap1.txt"
    echo "box bootstrap accepted a cluster whose only GatewayClass is NGINX Gateway Fabric's"
    fail=1
  else
    cat "$WORK/bootstrap1.txt"
  fi
  grep -qF 'GatewayClass "nginx" (controller gateway.nginx.org/nginx-gateway-controller) is Accepted, but box supports only Envoy Gateway' "$WORK/bootstrap1.txt" ||
    { echo "box did not refuse the foreign class with the decided wording"; fail=1; }

  # Nothing was written: no record, no namespace of box's, no GatewayClass eg,
  # no Envoy Gateway release, no MetalLB install, no Gateway.
  [ ! -e "$WORK/cluster.yaml" ] || { echo "box wrote the cluster record on a refused cluster"; fail=1; }
  for ns in "$NS" s5n-system s5n-gateway envoy-gateway-system metallb-system; do
    if $K get namespace "$ns" >/dev/null 2>&1; then
      echo "box created namespace ${ns} on a refused cluster"; fail=1
    fi
  done
  if $K get gatewayclass eg >/dev/null 2>&1; then
    echo "box created GatewayClass eg beside the foreign implementation"; fail=1
  fi
  releases=$($HELM list -A -o json 2>/dev/null || echo '[]')
  echo "  helm releases: ${releases}"
  python3 - "$releases" <<'PY' || fail=1
import json, sys
rels = json.loads(sys.argv[1]) if sys.argv[1].strip() else []
names = {r["name"] for r in rels}
for want in ("envoy-gateway", "metallb"):
    if want in names:
        print(f"box installed the {want} release on a refused cluster")
        sys.exit(1)
PY

  # The operator's implementation is untouched: same controller, still its own
  # class, and no Gateway box would have written.
  class_controller=$($K get gatewayclass nginx -o jsonpath='{.spec.controllerName}')
  class_manager=$($K get gatewayclass nginx -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}')
  [ "$class_controller" = gateway.nginx.org/nginx-gateway-controller ] || { echo "box rewrote the class controller"; fail=1; }
  [ "$class_manager" != box ] || { echo "box labelled the operator's GatewayClass as its own"; fail=1; }

  # ---- a second bootstrap: the same refusal, still nothing written ----------
  # The refusal is not a first-run state machine: a re-run reaches the same
  # decision from the cluster, because no record was written to remember it.
  log "box bootstrap: the refusal is the same on a re-run"
  if bootstrap "$WORK/bootstrap2.txt"; then
    cat "$WORK/bootstrap2.txt"
    echo "a second bootstrap accepted the foreign implementation"; fail=1
  else
    cat "$WORK/bootstrap2.txt"
  fi
  grep -qF 'box supports only Envoy Gateway' "$WORK/bootstrap2.txt" ||
    { echo "the second run did not refuse the foreign class"; fail=1; }
  [ ! -e "$WORK/cluster.yaml" ] || { echo "the second run wrote the cluster record"; fail=1; }
}

# box is the whole interface: a client-go/klog line such as
# "I1107 ... warnings.go:107] ..." must never appear in what the operator reads.
no_klog() {
  for f in "$WORK"/apply*.txt "$WORK"/bootstrap*.txt; do
    [ -e "$f" ] || continue
    if grep -Eq '^[IWE][0-9]{4} ' "$f"; then
      echo "client-go/klog noise in $(basename "$f"):"
      grep -E '^[IWE][0-9]{4} ' "$f"
      fail=1
    fi
  done
}

# A box built with -X main.version is what a release is. Without it box says it
# is skipping the chart's min-box check, which would quietly make the e2e pass
# a floor a real box could not.
no_dev_notice() {
  for f in "$WORK"/*.txt; do
    [ -e "$f" ] || continue
    if grep -q "built without a version" "$f"; then
      echo "box skipped the min-box check: $(basename "$f") says it was built without a version"
      fail=1
    fi
  done
}

if [ "$SCENARIO" = fresh ]; then
  fresh
elif [ "$SCENARIO" = preinstalled ]; then
  preinstalled
else
  foreign_gateway
fi
no_klog
no_dev_notice

if [ "$fail" != 0 ]; then
  log "wiring Job logs"
  $K -n "$NS" logs job/s5n-wiring --tail=80 || true
  log "box bootstrap"
  cat "$WORK/bootstrap1.txt" 2>/dev/null || true
fi

elapsed=$(( $(date +%s) - START ))
if [ "$fail" = 0 ]; then log "PASS (${SCENARIO})"; else log "FAIL (${SCENARIO})"; fi
echo "wall time (${SCENARIO}): ${elapsed}s"
[ "$KEEP" = --keep ] || $KIND delete cluster --name "$CLUSTER"
exit "$fail"
