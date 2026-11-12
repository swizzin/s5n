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
# The login is part of every install: bootstrap puts one Authelia in
# s5n-system and one SecurityPolicy on the Gateway, root's `box init` creates
# the one account, and the fresh scenario then proves the whole flow with a
# cookie jar -- an app path redirects to the portal, the portal takes the
# password, the jar gets in, and the Arrs' own API keys still answer. The
# preinstalled scenario proves the same redirect on its own host name.
#
# The fresh scenario runs Pebble, Let's Encrypt's test ACME server, inside the
# cluster and points the production issuer at it, so HTTP-01 through the
# Gateway is proven on every run: Certificate Ready, HTTPS with Pebble's CA, an
# HTTP redirect for app paths, and a challenge path that is not redirected --
# including on a renewal issued with the redirect already in place. The
# preinstalled scenario's name does not resolve, which proves the self-signed
# half.
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
# The one account the login admits. A generic name: the repo carries no real
# one, and the e2e's password is generated per run below.
LOGIN=alice
IMAGE=ghcr.io/swizzin/s5n:e2e
# The platform versions box installs are not this script's: they come from
# charts/s5n/platform.yaml, the file the chart release carries, printed by
# hack/versions through the parser box uses, so the plan box prints is asserted
# against the pin the release it pulled names.
# MetalLB's upstream manifest is tagged with a leading "v" while its chart pin
# has none, so the tag is derived from the one pin.
pins=$(go run ./hack/versions)
pin() { printf '%s\n' "$pins" | sed -n "s/^$1=//p"; }
v_tag() { case "$1" in v*) printf '%s' "$1" ;; *) printf 'v%s' "$1" ;; esac; }
for pin_key in authelia k0s envoyGateway metallb certManager; do
  [ -n "$(pin "$pin_key")" ] || {
    echo "charts/s5n/platform.yaml has no ${pin_key} pin"
    exit 1
  }
done
ENVOY_GATEWAY_VERSION=$(pin envoyGateway)
AUTHELIA_IMAGE=$(pin authelia)
METALLB_CHART_VERSION=$(pin metallb)
CERT_MANAGER_VERSION=$(pin certManager)
METALLB_VERSION=${METALLB_VERSION:-$(v_tag "$(pin metallb)")}
# The third scenario's Gateway API implementation. NGINX Gateway Fabric is one
# Deployment plus one data-plane pod per Gateway, and its Gateway class is
# created by its own chart. It is here because it is an implementation box does
# not support: the scenario proves the refusal, not a working Gateway. Pinned,
# like every other platform version here.
GATEWAY_API_VERSION=${GATEWAY_API_VERSION:-v1.6.3}
NGF_VERSION=${NGF_VERSION:-2.8.1}
# Pebble 2.10.1, the ACME test server the fresh scenario proves HTTP-01
# against, pinned by tag and digest like every other test image here.
PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:2.10.1@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199}  # renovate: datasource=docker
# The image the NetworkPolicy probes use: curl, pinned like every other test
# image here. It is what asks the login directly, which must time out.
CURL_IMAGE=${CURL_IMAGE:-docker.io/curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69}  # renovate: datasource=docker

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
  fresh) CLUSTER=s5n-box; DOMAIN=box.example.test ;;
  preinstalled) CLUSTER=s5n-box-lb; DOMAIN=box.internal.example ;;
  foreign-gateway) CLUSTER=s5n-box-gw; DOMAIN=box.example.com ;;
esac
# The names the catalog serves at a host of their own (Plex and Seerr): the
# chart renders their routes there, and box resolves and certifies them.
PLEX_HOST="plex.${DOMAIN}"
SEERR_HOST="seerr.${DOMAIN}"
# The fresh scenario points the production issuer at Pebble; every other
# scenario keeps Let's Encrypt's directory and never issues from it.
ACME_ARGS=()
# The development-only --dns-file override: the e2e's stand-in for the test
# host's own DNS, which an unprivileged run cannot edit. Empty for every
# scenario until the one that moves a name from "no record" to a public one.
DNS_ARGS=()
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
# distribution v3.1.2, pinned by tag and digest like every other test image.
REGISTRY_IMAGE=registry:3@sha256:38907510915c3f1604c7e83d6ab602ab3020d092f73bdaed8696773693ff768c  # renovate: datasource=docker
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

# The login's password: generated per run, written where only `box init`
# reads it (--password-file), and asserted never to appear in box's output.
# The node's users database is seeded from the same file, because
# --paths-in-node means the host path is inside the test node and box cannot
# write there: hack/authelia-users is the same internal/authelia code box uses.
AUTHELIA_PASSWORD=$(openssl rand -hex 16)
printf '%s\n' "$AUTHELIA_PASSWORD" >"$WORK/authelia-password"
chmod 600 "$WORK/authelia-password"

# Only what `box init` creates on a real host, and only because --paths-in-node
# keeps it from touching this node itself: the two volume roots and one
# /config/<instance> directory per enabled app (setup.settingsDirs). The data
# categories are deliberately absent — downloads/<category> and media/<category>
# are the wiring Job's, and pre-creating them would hide a first install that
# cannot make the root folders its Arr steps need.
log "preparing host paths inside the node (what box init creates on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,plex,tautulli,seerr,qui,maintainerr}
  chown -R 1000:1000 /srv/s5n'

# The login's own directory, which box init creates on a real host: 0700, owned
# by the uid the container runs as (65532). The users database goes in before
# the install because Authelia will not start without it, and box cannot write
# into the node -- hack/authelia-users writes the same file from the same
# package, so the row the login verifies is the row box would have written.
log "seeding the login's state directory in the node (0700, uid 65532)"
go run ./hack/authelia-users --login "$LOGIN" --password-file "$WORK/authelia-password" --out "$WORK/users_database.yml"
docker exec -i "${CLUSTER}-control-plane" bash -c '
  mkdir -p /var/lib/s5n/authelia
  cat >/var/lib/s5n/authelia/users_database.yml
  chown -R 65532:65532 /var/lib/s5n/authelia
  chmod 700 /var/lib/s5n/authelia
  chmod 600 /var/lib/s5n/authelia/users_database.yml
' <"$WORK/users_database.yml"

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
# The fresh scenario's ACME server is Pebble, not Let's Encrypt: the values
# file forces the production issuer, so the Certificate names s5n-letsencrypt
# and the test can prove the HTTP-01 path without the internet.
if [ "$SCENARIO" = fresh ]; then
  cat >>"$WORK/values.yaml" <<'EOF'

tls:
  issuer: letsencrypt
EOF
fi

bootstrap() { # bootstrap <output file>
  # The platform and root's seedbox are one command now. --record points the
  # cluster decision at this work directory: on a real host it is
  # /etc/swizzin/cluster.yaml, written by root, and the e2e is not root.
  #
  # No --chart-version: bootstrap takes the default-version path, so the e2e
  # exercises the newest-release selection against a real registry. The chart
  # is the one pushed above; box pulls it over loopback.
  "$WORK/box" bootstrap --yes --chart-repo "$CHART_REPO" ${ACME_ARGS[@]+"${ACME_ARGS[@]}"} ${DNS_ARGS[@]+"${DNS_ARGS[@]}"} \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --namespace "$NS" --domain "$DOMAIN" \
    --data-path /srv/s5n/data --config-path /srv/s5n/config \
    --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr,qui,maintainerr \
    --puid 1000 --pgid 1000 \
    --login "$LOGIN" --password-file "$WORK/authelia-password" \
    --paths-in-node --record "$WORK/cluster.yaml" \
    --kubeconfig "$WORK/kubeconfig" >"$1" 2>&1
}

init() { # init <output file> [extra dev flags]
  # --paths-in-node: the paths are in the kind node's filesystem, not this
  # host's, so init neither reports nor creates them, and apply skips its stat
  # the same way. On a real host init creates them (hack/e2e-box.sh cannot:
  # this host is not the node).
  #
  # --chart-version pins the release to the package pushed above: init reads
  # one chart, and the apply it hands over to installs that same value.
  "$WORK/box" init --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" ${DNS_ARGS[@]+"${DNS_ARGS[@]}"} \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --namespace "$NS" --domain "$DOMAIN" \
    --data-path /srv/s5n/data --config-path /srv/s5n/config \
    --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr,qui,maintainerr \
    --puid 1000 --pgid 1000 \
    --login "$LOGIN" --password-file "$WORK/authelia-password" \
    --paths-in-node "${@:2}" \
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


# https_get retries a TLS request while the Gateway's data plane converges.
# Replacing a Certificate's Secret briefly makes the listener's certificateRef
# unresolved, so Envoy withdraws and restores port 443; a client retries, and
# the e2e does the same. It prints the last status code, and -k skips
# verification for the self-signed scenario.
https_get() { # https_get <address> <path> <ca-file|-> [requests]
  local addr=$1 path=$2 ca=$3 requests=${4:-60} code="" i
  for i in $(seq 1 "$requests"); do
    if [ "$ca" = - ]; then
      code=$(curl -k -sS -o /dev/null -w '%{http_code}'         --resolve "${DOMAIN}:443:${addr}" "https://${DOMAIN}${path}" 2>"$WORK/https.err" || true)
    else
      code=$(curl -sS -o /dev/null -w '%{http_code}'         --resolve "${DOMAIN}:443:${addr}" --cacert "$ca" "https://${DOMAIN}${path}" 2>"$WORK/https.err" || true)
    fi
    [ "$code" = 200 ] && break
    sleep 1
  done
  printf '%s' "$code"
}

# same_cert is "these two PEM documents hold the same certificate", by
# fingerprint: box re-encodes the anchor's certificate before it writes it, so
# a byte comparison would be about whitespace as much as about the certificate.
same_cert() { # same_cert <pem-a> <pem-b>
  local a b
  a=$(printf '%s' "$1" | openssl x509 -noout -fingerprint -sha256 2>/dev/null || true)
  b=$(printf '%s' "$2" | openssl x509 -noout -fingerprint -sha256 2>/dev/null || true)
  [ -n "$a" ] && [ "$a" = "$b" ]
}

# tls_serial prints the serial number of the certificate in a cert-manager
# Secret. A renewal is a new serial; the Secret's UID alone only says it was
# rewritten.
tls_serial() { # tls_serial <secret-name>
  $K -n s5n-gateway get secret "$1" -o jsonpath='{.data.tls\.crt}' 2>/dev/null |
    base64 -d 2>/dev/null |
    openssl x509 -noout -serial 2>/dev/null |
    sed 's/^serial=//'
}

# ---- the ACME test server ----------------------------------------------------
# Pebble is Let's Encrypt's test ACME server: same protocol, no rate limits, and
# a CA it generates at startup (fetched from its management API). cert-manager
# reaches its directory through the issuer override box takes for development
# runs. Pebble validates HTTP-01 on port 80, through the Gateway, exactly like
# production, and the certificate the app serves chains to that CA.
install_pebble() {
  log "Pebble 2.10.1 (the ACME test server) in the cluster, pinned by digest"
  node_ip=$($K get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
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
    - {name: acme, port: 14000, targetPort: 14000}
    - {name: management, port: 15000, targetPort: 15000, nodePort: 31500}
EOF
  $K -n pebble rollout status deploy/pebble --timeout=180s || { echo "Pebble never rolled out"; fail=1; return 0; }
  for i in $(seq 1 60); do
    if curl -k -fsS "https://${node_ip}:31500/roots/0" -o "$WORK/pebble-root.pem" 2>/dev/null; then
      break
    fi
    if [ "$i" = 60 ]; then
      echo "Pebble never served its root CA"; fail=1; return 0
    fi
    sleep 2
  done
  echo "  Pebble root CA in $WORK/pebble-root.pem"
}

# cert-manager's HTTP-01 self-check and Pebble both resolve the test name
# themselves, so the smallest change that makes it point at the Gateway is a
# CoreDNS hosts entry. Nothing in the chart or box depends on it: this is the
# test's own DNS.
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
  # The ACME server and the DNS entry come first: bootstrap issues the host's
  # certificate inside this run, so both have to answer before it starts.
  install_pebble
  [ "$fail" = 0 ] || return 0
  # Every name the box serves: its own and each app's own host. init resolves
  # them and refuses an app name that does not answer where the domain does, so
  # the app hosts have to be here before bootstrap or it stops on purpose.
  patch_coredns "$node_ip" "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" ||
    { echo "could not patch CoreDNS for the box's names"; fail=1; return 0; }
  ACME_ARGS=(--acme-server "https://pebble.pebble.svc:14000/dir")

  log "box bootstrap: the platform layer, then root's first install"
  if bootstrap "$WORK/bootstrap1.txt"; then
    cat "$WORK/bootstrap1.txt"
  else
    cat "$WORK/bootstrap1.txt"; fail=1
  fi
  grep -q "^platform plan:" "$WORK/bootstrap1.txt" || { echo "bootstrap printed no platform plan"; fail=1; }
  grep -qF "+ envoy-gateway ${ENVOY_GATEWAY_VERSION} (Helm, envoy-gateway-system)" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not install Envoy Gateway at its pinned version"; fail=1; }
  grep -qF "+ GatewayClass eg and Gateway s5n-gateway/s5n" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not apply the Gateway before the address layer"; fail=1; }
  grep -qF "? Gateway s5n-gateway/s5n getting an address" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print the Gateway's address as the question"; fail=1; }
  grep -qF "? metallb ${METALLB_CHART_VERSION} (Helm, metallb-system), unless the Gateway gets an address without it" "$WORK/bootstrap1.txt" ||
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
  # cert-manager is part of the platform layer now: the plan names it at its
  # pin, and the install waits for its controller, webhook and cainjector.
  grep -qF "+ cert-manager ${CERT_MANAGER_VERSION} (Helm, cert-manager)" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not install cert-manager at its pinned version"; fail=1; }
  $K -n cert-manager rollout status deploy/cert-manager --timeout=180s >/dev/null ||
    { echo "cert-manager's controller never rolled out"; fail=1; }
  $K -n cert-manager rollout status deploy/cert-manager-webhook --timeout=180s >/dev/null ||
    { echo "cert-manager's webhook never rolled out"; fail=1; }
  cm_args=$($K -n cert-manager get deploy cert-manager -o jsonpath='{.spec.template.spec.containers[0].args}')
  case "$cm_args" in
    *--config=*) ;;
    *) echo "cert-manager was installed without its controller configuration"; fail=1 ;;
  esac
  cm_config=$($K -n cert-manager get cm cert-manager -o jsonpath='{.data.config\.yaml}')
  case "$cm_config" in
    *"gatewayAPI:"*"enabled: true"*) ;;
    *) echo "cert-manager was installed without Gateway API support"; fail=1 ;;
  esac
  # Root's two issuers are applied after the Gateway they solve through. The
  # production one keeps Let's Encrypt's directory: the override is the e2e's
  # own Pebble run, not this scenario.
  grep -qF "+ ClusterIssuers s5n-letsencrypt and s5n-selfsigned" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not apply root's ClusterIssuers"; fail=1; }
  for issuer in s5n-letsencrypt s5n-selfsigned; do
    $K get clusterissuer "$issuer" >/dev/null 2>&1 ||
      { echo "ClusterIssuer ${issuer} was not applied"; fail=1; }
  done
  # This scenario's issuer is pointed at Pebble below; the production URL is
  # the preinstalled scenario's assertion.
  acme_server=$($K get clusterissuer s5n-letsencrypt -o jsonpath='{.spec.acme.server}')
  [ "$acme_server" = "https://pebble.pebble.svc:14000/dir" ] ||
    { echo "s5n-letsencrypt points at ${acme_server}"; fail=1; }
  solver_ref=$($K get clusterissuer s5n-letsencrypt -o jsonpath='{.spec.acme.solvers[0].http01.gatewayHTTPRoute.parentRefs[0].sectionName}' || true)
  [ -z "$solver_ref" ] ||
    { echo "the HTTP-01 solver pins sectionName '${solver_ref}': it must attach to the generic http listener and each host's http-<host> listener"; fail=1; }

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

  # ---- TLS: Pebble proved HTTP-01 through the Gateway ------------------------
  # The Certificate is real: cert-manager created an order, Pebble fetched the
  # challenge through the Gateway on port 80, and the issued chain is signed by
  # Pebble's own CA. https with --cacert is the proof the listener serves it;
  # the redirect and the challenge path are the proof of which route wins.
  cert=s5n-tls-box-example-test
  grep -qF "tls: tls.issuer=letsencrypt forces Let's Encrypt" "$WORK/bootstrap1.txt" ||
    { echo "init did not say the issuer was forced"; fail=1; }
  grep -qF "applied Certificate s5n-gateway/${cert} (issuer s5n-letsencrypt)" "$WORK/bootstrap1.txt" ||
    { echo "box did not apply the certificate with the production issuer"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s ||
    { echo "the Certificate never became Ready"; fail=1; }
  cert_issuer=$($K -n s5n-gateway get certificate "$cert" -o jsonpath='{.spec.issuerRef.name}')
  [ "$cert_issuer" = s5n-letsencrypt ] || { echo "the Certificate names issuer ${cert_issuer}"; fail=1; }
  acme_server=$($K get clusterissuer s5n-letsencrypt -o jsonpath='{.spec.acme.server}')
  [ "$acme_server" = "https://pebble.pebble.svc:14000/dir" ] ||
    { echo "s5n-letsencrypt points at ${acme_server}, not the test server"; fail=1; }
  # The test override accepts Pebble's self-signed ACME endpoint; a production
  # run leaves verification on, which the second scenario's issuer asserts.
  [ "$($K get clusterissuer s5n-letsencrypt -o jsonpath='{.spec.acme.skipTLSVerify}')" = true ] ||
    { echo "the test issuer does not accept Pebble's certificate"; fail=1; }

  # The listener references the Certificate's Secret. The shared generic http
  # listener is untouched (from All, so hosts without a certificate keep
  # serving); this host has its own port-80 listener, hostname-matched and
  # narrowed to s5n-gateway, which listener isolation prefers for its Host.
  # That is what makes the redirect win over an app route's longer prefix
  # without closing port 80 for any other host.
  listener_port=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="https-box-example-test")].port}')
  listener_secret=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="https-box-example-test")].tls.certificateRefs[0].name}')
  listener_routes=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="https-box-example-test")].allowedRoutes.namespaces.from}')
  http_routes=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http")].allowedRoutes.namespaces.from}')
  http_host_port=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http-box-example-test")].port}')
  http_host_hostname=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http-box-example-test")].hostname}')
  http_host_routes=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http-box-example-test")].allowedRoutes.namespaces.from}')
  http_host_selector=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http-box-example-test")].allowedRoutes.namespaces.selector.matchLabels.kubernetes\.io/metadata\.name}')
  echo "  listener https-box-example-test: ${listener_port} -> ${listener_secret}, 443 routes ${listener_routes}"
  echo "  listener http-box-example-test: ${http_host_port} ${http_host_hostname}, 80 routes ${http_host_routes} (${http_host_selector}); generic http routes ${http_routes}"
  [ "$listener_port" = 443 ] || { echo "no HTTPS listener on 443"; fail=1; }
  [ "$listener_secret" = "$cert" ] || { echo "the listener does not reference ${cert}"; fail=1; }
  [ "$listener_routes" = All ] || { echo "the HTTPS listener does not admit the seedbox's routes"; fail=1; }
  [ "$http_routes" = All ] || { echo "the shared http listener was narrowed (from ${http_routes}): it must keep serving every host"; fail=1; }
  [ "$http_host_port" = 80 ] || { echo "the host's own port-80 listener is on ${http_host_port}"; fail=1; }
  [ "$http_host_hostname" = "$DOMAIN" ] || { echo "the host's port-80 listener matches ${http_host_hostname}, want ${DOMAIN}"; fail=1; }
  [ "$http_host_routes" = Selector ] || { echo "the host's port-80 listener was not narrowed to s5n-gateway"; fail=1; }
  [ "$http_host_selector" = s5n-gateway ] || { echo "the host's port-80 listener selects ${http_host_selector}, want s5n-gateway"; fail=1; }

  # The redirect attaches to the host's own port-80 listener, not the shared
  # one. The chart's app routes attach with no sectionName at all, so under
  # isolation they land on the HTTPS listener and the generic http listener --
  # never on the host's port-80 listener.
  redirect_section=$($K -n s5n-gateway get httproute s5n-tls-box-example-test-redirect -o jsonpath='{.spec.parentRefs[0].sectionName}')
  echo "  redirect route sectionName: ${redirect_section}"
  [ "$redirect_section" = "http-box-example-test" ] || { echo "the redirect does not attach to http-box-example-test"; fail=1; }
  $K -n "$NS" get httproute -o json | python3 -c '
import json, sys
items = json.load(sys.stdin).get("items", [])
bad = []
for r in items:
    for ref in r["spec"]["parentRefs"]:
        if ref.get("sectionName"):
            bad.append(r["metadata"]["name"] + " -> " + ref["sectionName"])
if bad:
    print("app routes pin a listener: " + ", ".join(bad))
    sys.exit(1)
print("  app routes: %d HTTPRoutes, none pins a sectionName" % len(items))
' || { echo "the chart's app routes must attach without a sectionName"; fail=1; }

  https_code=$(https_get "$gw_addr" /sonarr/ping "$WORK/pebble-root.pem")
  echo "  HTTPS ${DOMAIN}/sonarr/ping through the Gateway: ${https_code}"
  [ "$https_code" = 200 ] || { echo "HTTPS did not answer 200 with Pebble's CA"; fail=1; }

  http_code=$(curl -sS -D "$WORK/http-headers" -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:80:${gw_addr}" "http://${DOMAIN}/sonarr/ping" || true)
  echo "  HTTP ${DOMAIN}/sonarr/ping: ${http_code}"
  [ "$http_code" = 301 ] || { echo "an app path on port 80 was not redirected"; fail=1; }
  grep -qi "^location: https://${DOMAIN}/sonarr/ping" "$WORK/http-headers" ||
    { echo "the redirect does not point at HTTPS"; fail=1; }

  # ---- the login: root's seedbox is behind Authelia --------------------------
  # The SecurityPolicy targets the Gateway itself, so every listener and every
  # route asks Authelia first -- including the shared port-80 listener, which is
  # what keeps an app from ever being served unauthenticated over plain HTTP.
  # These assertions are the whole flow: redirect, portal, password, cookie,
  # and the paths that carry their own credentials.
  log "the login: an unauthenticated app path redirects to the portal"
  authelia_pod=$($K -n s5n-system get pod -l app.kubernetes.io/name=authelia -o jsonpath='{.items[0].metadata.name}' || true)
  authelia_uid=$($K -n s5n-system get pod -l app.kubernetes.io/name=authelia -o jsonpath='{.items[0].metadata.uid}' || true)
  authelia_runas=$($K -n s5n-system get deploy authelia -o jsonpath='{.spec.template.spec.securityContext.runAsUser}' || true)
  authelia_image=$($K -n s5n-system get deploy authelia -o jsonpath='{.spec.template.spec.containers[0].image}' || true)
  policy_target=$($K -n s5n-gateway get securitypolicy s5n-authelia -o jsonpath='{.spec.targetRefs[0].kind}/{.spec.targetRefs[0].name}' || true)
  policy_accepted=$($K -n s5n-gateway get securitypolicy s5n-authelia -o jsonpath='{.status.ancestors[?(@.ancestorRef.name=="s5n")].conditions[?(@.type=="Accepted")].status}' || true)
  policy_failopen=$($K -n s5n-gateway get securitypolicy s5n-authelia -o jsonpath='{.spec.extAuth.failOpen}' || true)
  policy_headers=$($K -n s5n-gateway get securitypolicy s5n-authelia -o jsonpath='{.spec.extAuth.headersToExtAuth[*]}' || true)
  echo "  pod ${authelia_pod} (uid ${authelia_uid}), runAsUser ${authelia_runas}, image ${authelia_image}"
  echo "  SecurityPolicy -> ${policy_target}, Accepted ${policy_accepted}, failOpen ${policy_failopen}, headers [${policy_headers}]"
  [ -n "$authelia_pod" ] || { echo "the login has no pod"; fail=1; }
  [ "$authelia_runas" = 65532 ] || { echo "the login runs as ${authelia_runas}, want 65532"; fail=1; }
  [ "$authelia_image" = "$AUTHELIA_IMAGE" ] ||
    { echo "the login runs ${authelia_image}, want the pinned ${AUTHELIA_IMAGE}"; fail=1; }
  [ "$policy_target" = Gateway/s5n ] ||
    { echo "the SecurityPolicy targets ${policy_target}, want the Gateway itself"; fail=1; }
  [ "$policy_accepted" = True ] || { echo "Envoy Gateway never accepted the SecurityPolicy"; fail=1; }
  [ "$policy_failopen" = false ] || { echo "the SecurityPolicy fails open"; fail=1; }
  case " ${policy_headers} " in
    *" cookie "*) ;;
    *) echo "headersToExtAuth = [${policy_headers}]: without cookie every request is anonymous"; fail=1 ;;
  esac
  # The claim and the file: the state is on the host, owned by the login's uid,
  # and the digest is the one the portal will accept below.
  for kind in authelia; do
    pv_phase=$($K get pv s5n-${kind} -o jsonpath='{.status.phase}' || true)
    pvc_phase=$($K -n s5n-system get pvc s5n-${kind} -o jsonpath='{.status.phase}' || true)
    pv_path=$($K get pv s5n-${kind} -o jsonpath='{.spec.hostPath.path}' || true)
    echo "  s5n-${kind}: ${pv_phase} -> ${pvc_phase} (path ${pv_path})"
    [ "$pv_phase" = Bound ] || fail=1
    [ "$pvc_phase" = Bound ] || fail=1
    [ "$pv_path" = /var/lib/s5n/authelia ] || { echo "the login's volume backs ${pv_path}"; fail=1; }
  done
  node_owner=$(docker exec "${CLUSTER}-control-plane" stat -c '%u:%g' /var/lib/s5n/authelia 2>/dev/null || echo missing)
  node_mode=$(docker exec "${CLUSTER}-control-plane" stat -c '%a' /var/lib/s5n/authelia 2>/dev/null || echo missing)
  echo "  /var/lib/s5n/authelia: owner ${node_owner}, mode ${node_mode}"
  [ "$node_owner" = 65532:65532 ] || { echo "the login's directory is not owned by its uid"; fail=1; }
  [ "$node_mode" = 700 ] || { echo "the login's directory is mode ${node_mode}, want 700"; fail=1; }
  $K -n s5n-system get secret s5n-authelia -o jsonpath='{.data.storage-encryption-key}' >/dev/null 2>&1 ||
    { echo "the login's storage encryption key is not in s5n-authelia"; fail=1; }
  # box's own words: it installed the login and reported the policy accepted,
  # both before the release was written. The step's Label ("installing root's
  # login") is TUI-only; the plain path prints the Done line, so that is what
  # the log carries.
  grep -qF "login ready: s5n-system/authelia, SecurityPolicy s5n-gateway/s5n-authelia accepted" "$WORK/bootstrap1.txt" ||
    { echo "box did not say it installed the login and accepted its SecurityPolicy"; fail=1; }
  grep -qF "login: ${LOGIN} at https://${DOMAIN}/auth/" "$WORK/bootstrap1.txt" ||
    { echo "the final card did not print the login"; fail=1; }
  if grep -qF "$AUTHELIA_PASSWORD" "$WORK/bootstrap1.txt"; then
    echo "box printed the password: it is read from --password-file, and the card must not repeat it"
    fail=1
  fi
  # The login is installed before the release: its Done line comes before the
  # release apply in the transcript. (The step's Label is TUI-only, and the
  # "first install" line is part of init's plan, printed before any write, so
  # neither is the thing to look for.)
  login_line=$(grep -n "login ready:" "$WORK/bootstrap1.txt" | head -1 | cut -d: -f1)
  release_line=$(grep -n "applied: s5n revision" "$WORK/bootstrap1.txt" | head -1 | cut -d: -f1)
  echo "  transcript: login at line ${login_line}, release at line ${release_line}"
  if [ -z "$login_line" ] || [ -z "$release_line" ]; then
    echo "the transcript does not show both the login and the release"
    fail=1
  elif [ "$login_line" -ge "$release_line" ]; then
    echo "the login was not installed before the release"
    fail=1
  fi

  JAR="$WORK/authelia-jar.txt"
  rm -f "$JAR"
  unauth_headers="$WORK/unauth-headers.txt"
  unauth_code=$(curl -sS -o /dev/null -D "$unauth_headers" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/" 2>"$WORK/unauth.err" || true)
  unauth_location=$(grep -i '^location:' "$unauth_headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
  echo "  GET https://${DOMAIN}/sonarr/ without a cookie: ${unauth_code} -> ${unauth_location}"
  [ "$unauth_code" = 302 ] ||
    { echo "an unauthenticated app path answered ${unauth_code}, not a redirect to the login (sonarr was served open)"; fail=1; }
  case "$unauth_location" in
    "https://${DOMAIN}/auth/"*) ;;
    *) echo "the redirect does not point at the portal: ${unauth_location}"; fail=1 ;;
  esac
  case "$unauth_location" in
    *"rd="*) ;;
    *) echo "the redirect carries no rd=, so the portal cannot send the browser back"; fail=1 ;;
  esac

  # Every opt-in app is behind the same login, and none of them keeps one of its
  # own: an unauthenticated request must reach the edge before the app does.
  for app in qui maintainerr; do
    app_headers="$WORK/unauth-${app}-headers.txt"
    app_code=$(curl -sS -o /dev/null -D "$app_headers" -w '%{http_code}' \
      --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
      "https://${DOMAIN}/${app}/" 2>/dev/null || true)
    app_location=$(grep -i '^location:' "$app_headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
    echo "  GET https://${DOMAIN}/${app}/ without a cookie: ${app_code} -> ${app_location}"
    [ "$app_code" = 302 ] ||
      { echo "an unauthenticated /${app}/ answered ${app_code}, not a redirect to the login"; fail=1; }
    case "$app_location" in
      "https://${DOMAIN}/auth/"*) ;;
      *) echo "the /${app}/ redirect does not point at the portal: ${app_location}"; fail=1 ;;
    esac
  done

  # Port 80: this host's own listener carries no login (the Allow box writes
  # with it), so an app URL gets the 301 to HTTPS -- the behaviour a browser,
  # a bookmark and a renewal need -- and never Sonarr's UI.
  nl_section=$($K -n s5n-gateway get securitypolicy s5n-no-login-box-example-test -o jsonpath='{.spec.targetRefs[0].sectionName}' || true)
  nl_action=$($K -n s5n-gateway get securitypolicy s5n-no-login-box-example-test -o jsonpath='{.spec.authorization.defaultAction}' || true)
  nl_extauth=$($K -n s5n-gateway get securitypolicy s5n-no-login-box-example-test -o jsonpath='{.spec.extAuth}' || true)
  nl_accepted=$($K -n s5n-gateway get securitypolicy s5n-no-login-box-example-test -o jsonpath='{.status.ancestors[?(@.ancestorRef.sectionName=="http-box-example-test")].conditions[?(@.type=="Accepted")].status}' || true)
  echo "  port-80 Allow policy: sectionName ${nl_section}, defaultAction ${nl_action}, extAuth [${nl_extauth}], Accepted ${nl_accepted}"
  [ "$nl_section" = http-box-example-test ] ||
    { echo "the port-80 Allow targets ${nl_section}, want this host's own listener"; fail=1; }
  [ "$nl_action" = Allow ] || { echo "the port-80 policy does not allow: ${nl_action}"; fail=1; }
  [ -z "$nl_extauth" ] || { echo "the port-80 policy still carries extAuth"; fail=1; }
  [ "$nl_accepted" = True ] || { echo "Envoy Gateway never accepted the port-80 Allow"; fail=1; }

  # Envoy Gateway's own account of the two policies is the proof that the login
  # was carved out of exactly one listener rather than switched off for the
  # host: the Gateway-wide policy stays Accepted, and its Overridden condition
  # names this host's port-80 listener and no HTTPS one.
  gw_overridden=$($K -n s5n-gateway get securitypolicy s5n-authelia -o jsonpath='{.status.ancestors[?(@.ancestorRef.name=="s5n")].conditions[?(@.type=="Overridden")].status}' || true)
  gw_override_msg=$($K -n s5n-gateway get securitypolicy s5n-authelia -o jsonpath='{.status.ancestors[?(@.ancestorRef.name=="s5n")].conditions[?(@.type=="Overridden")].message}' || true)
  echo "  the Gateway-wide login is overridden on: ${gw_override_msg}"
  [ "$gw_overridden" = True ] ||
    { echo "Envoy Gateway does not report the Gateway-wide login overridden, so the port-80 Allow is not taking effect"; fail=1; }
  case "$gw_override_msg" in
    *"s5n-gateway/s5n/http-box-example-test"*) ;;
    *) echo "the login is not overridden on s5n-gateway/s5n/http-box-example-test: ${gw_override_msg}"; fail=1 ;;
  esac
  case "$gw_override_msg" in
    *"https-box-example-test"*) echo "the login is overridden on an HTTPS listener too: ${gw_override_msg}"; fail=1 ;;
  esac
  http_unauth=$(curl -sS -o "$WORK/port80-body" -D "$WORK/port80-headers" -w '%{http_code}' \
    --resolve "${DOMAIN}:80:${gw_addr}" "http://${DOMAIN}/sonarr/" 2>/dev/null || true)
  http_unauth_location=$(grep -i '^location:' "$WORK/port80-headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
  echo "  GET http://${DOMAIN}/sonarr/ (port 80) without a cookie: ${http_unauth} -> ${http_unauth_location}"
  [ "$http_unauth" = 301 ] ||
    { echo "port 80 answered ${http_unauth} for an app path: it must redirect to HTTPS, not ask for a login or serve Sonarr"; fail=1; }
  [ "$http_unauth_location" = "https://${DOMAIN}/sonarr/" ] ||
    { echo "port 80 redirected to ${http_unauth_location}"; fail=1; }
  if grep -qi '<title>Sonarr' "$WORK/port80-body"; then
    echo "port 80 served Sonarr's UI in plaintext"
    fail=1
  fi

  # The other half: the shared generic http listener keeps the login, so a host
  # with no certificate of its own is not served and is not redirected into
  # someone else's name either. The shared listener has no hostname, so any
  # Host that this host's listener does not match lands on it. With no route at
  # all for that Host the router answers 404 -- Envoy Gateway attaches ext_authz
  # per route, so there is nothing for the login to guard, and nothing of
  # Sonarr's is served. The assertion below this one adds a route for a
  # certificate-less host and gets the login's own refusal, which is the half
  # that matters.
  shared_code=$(curl -sS -o "$WORK/shared-body" -w '%{http_code}' \
    --resolve "other.example.test:80:${gw_addr}" "http://other.example.test/sonarr/" 2>/dev/null || true)
  echo "  GET http://other.example.test/sonarr/ (the shared listener, no certificate): ${shared_code}"
  case "$shared_code" in
    404) ;;
    *) echo "the shared port-80 listener answered ${shared_code} for a host with no route and no certificate: want the router's 404, not an app"; fail=1 ;;
  esac
  if grep -qi '<title>Sonarr' "$WORK/shared-body"; then
    echo "the shared listener served Sonarr to a host with no certificate"
    fail=1
  fi

  # The portal is the login page, and its assets resolve under /auth/ (the
  # server mounts the configured path and the root; the browser only ever uses
  # the former).
  portal_code=$(curl -sS -o "$WORK/portal.html" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/auth/" 2>"$WORK/portal.err" || true)
  echo "  GET https://${DOMAIN}/auth/: ${portal_code} ($(wc -c <"$WORK/portal.html" 2>/dev/null || echo 0) bytes)"
  [ "$portal_code" = 200 ] || { echo "the portal did not answer 200"; fail=1; }
  grep -q 'manifest.json' "$WORK/portal.html" || { echo "the portal is not Authelia's HTML"; fail=1; }
  asset_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/auth/manifest.json" 2>/dev/null || true)
  echo "  GET https://${DOMAIN}/auth/manifest.json: ${asset_code}"
  [ "$asset_code" = 200 ] || { echo "the portal's assets do not resolve under /auth/"; fail=1; }

  # The password: the digest in the node's users database is the one this
  # password hashes to, and the portal sets a session cookie for it.
  login_json=$(printf '{"username":"%s","password":"%s","targetURL":"https://%s/sonarr/"}' \
    "$LOGIN" "$AUTHELIA_PASSWORD" "$DOMAIN")
  ff_headers="$WORK/firstfactor-headers.txt"
  ff_code=$(curl -sS -o "$WORK/firstfactor.json" -D "$ff_headers" -w '%{http_code}' \
    -c "$JAR" -X POST "https://${DOMAIN}/auth/api/firstfactor" \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    -H 'Content-Type: application/json' --data "$login_json" 2>"$WORK/firstfactor.err" || true)
  echo "  POST /auth/api/firstfactor: ${ff_code}"
  [ "$ff_code" = 200 ] || { echo "the login did not accept the password (${ff_code})"; sed -n 1,3p "$WORK/firstfactor.json" || true; fail=1; }
  grep -qi 'set-cookie: authelia_session=' "$ff_headers" ||
    { echo "no authelia_session cookie was set"; fail=1; }

  # A wrong password is refused, once: never a retry loop (Authelia bans an
  # address after a handful of failures, which would fail the rest of this run
  # for the wrong reason).
  wrong_code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "https://${DOMAIN}/auth/api/firstfactor" \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    -H 'Content-Type: application/json' \
    --data "$(printf '{"username":"%s","password":"not-the-password","targetURL":"https://%s/sonarr/"}' "$LOGIN" "$DOMAIN")" \
    2>/dev/null || true)
  echo "  POST /auth/api/firstfactor with a wrong password: ${wrong_code}"
  [ "$wrong_code" = 401 ] || { echo "a wrong password answered ${wrong_code}, want 401 from the portal"; fail=1; }

  # With the jar: the app itself, and the two prefixes that carry their own
  # credentials and must never be redirected.
  authed_code=$(curl -sS -o "$WORK/sonarr-authed.html" -w '%{http_code}' \
    -b "$JAR" --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/" 2>/dev/null || true)
  echo "  GET https://${DOMAIN}/sonarr/ with the session: ${authed_code}"
  [ "$authed_code" = 200 ] || { echo "the login did not give access to Sonarr (${authed_code})"; fail=1; }
  grep -qi '<title>Sonarr' "$WORK/sonarr-authed.html" ||
    { echo "the 200 was not Sonarr's page: the cookie may not be in scope"; fail=1; }
  api_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/api/v3/system/status" 2>/dev/null || true)
  ping_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/ping" 2>/dev/null || true)
  echo "  without a cookie: /sonarr/api/v3/system/status ${api_code}, /sonarr/ping ${ping_code}"
  [ "$api_code" = 401 ] ||
    { echo "the API without a key answered ${api_code}: it must be Sonarr's own 401, not the login's redirect (the API bypass is missing)"; fail=1; }
  [ "$ping_code" = 200 ] || { echo "/sonarr/ping answered ${ping_code}: the ping bypass is missing"; fail=1; }

  # ---- the login is reachable only through the Gateway ----------------------
  # Authelia believes X-Forwarded-Proto and X-Forwarded-Host, so a pod that can
  # call 9091 directly can answer for any host and any scheme -- that was the
  # measured hole: s5n-system had no NetworkPolicy at all. The probe carries no
  # release label, so the release's own egress policy does not apply to it: what
  # it proves is the login's ingress policy, not the release's.
  log "the login refuses every caller but the Gateway"
  probe_out=$(timeout 90 $K -n "$NS" run oidc-probe --rm -i --restart=Never \
    --labels=s5n.io/probe=oidc --image="$CURL_IMAGE" --quiet -- \
    curl -s -o /dev/null --max-time 5 -w '%{http_code}' \
    "http://authelia.s5n-system:9091/auth/api/health" 2>&1 || true)
  echo "  probe (no release label) -> authelia.s5n-system:9091/auth/api/health: ${probe_out:-timed out}"
  case "$probe_out" in
    *000*) : ;;
    200* | *200*) echo "the login answered a pod directly: its ingress policy is missing or admits the wrong peer"; fail=1 ;;
    "") echo "the probe produced no answer at all, so it proves nothing"; fail=1 ;;
    *) echo "the probe answered ${probe_out}, which is not a timeout"; fail=1 ;;
  esac
  # And the proxy itself is still admitted: the cookie-jar block above logged in
  # through the Gateway in this same run, so a 000 here with a working login is
  # the two halves of the policy -- one way in, and that way open.
  $K -n "$NS" delete pod oidc-probe --ignore-not-found >/dev/null 2>&1 || true

  # ---- the apps with a host of their own ------------------------------------
  # Seerr and Plex are served at seerr.<domain> and plex.<domain>: their own
  # certificate from Pebble, their own listeners, and a route by Host. The
  # final card and the grant are what init decided; these assertions are what
  # cluster got.
  log "the app hosts: a certificate and a listener each, and Seerr on its own name"
  for app_host in "plex:${PLEX_HOST}" "seerr:${SEERR_HOST}"; do
    app=${app_host%%:*}; host=${app_host##*:}
    label=${host//./-}
    cert=s5n-tls-${label}
    grep -qF "applied Certificate s5n-gateway/${cert} (issuer s5n-letsencrypt)" "$WORK/bootstrap1.txt" ||
      { echo "box did not apply ${cert} with the production issuer"; fail=1; }
    $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s ||
      { echo "the ${app} host's Certificate never became Ready"; fail=1; }
    got_issuer=$($K -n s5n-gateway get certificate "$cert" -o jsonpath='{.spec.issuerRef.name}' || true)
    [ "$got_issuer" = s5n-letsencrypt ] || { echo "${cert} names issuer ${got_issuer}"; fail=1; }
    got_secret=$($K -n s5n-gateway get gateway s5n \
      -o jsonpath="{.spec.listeners[?(@.name==\"https-${label}\")].tls.certificateRefs[0].name}" || true)
    got_http=$($K -n s5n-gateway get gateway s5n \
      -o jsonpath="{.spec.listeners[?(@.name==\"http-${label}\")].hostname}" || true)
    echo "  ${host}: certificate ${cert}, https listener -> ${got_secret}, http listener on ${got_http}"
    [ "$got_secret" = "$cert" ] || { echo "the ${app} host's HTTPS listener references ${got_secret}"; fail=1; }
    [ "$got_http" = "$host" ] || { echo "the ${app} host's port-80 listener matches ${got_http}"; fail=1; }
  done

  # Seerr's page on its own name. The edge posture is the one it has on a path
  # (a bypass: it signs users in through Plex itself), so an unauthenticated
  # request gets Seerr's own page and login, not Authelia's portal -- and with
  # the session cookie from the portal in the jar it still gets the same page,
  # which is the cookie's domain covering the subdomain.
  seerr_unauth=$WORK/seerr-unauth.html
  # Seerr answers its root with a redirect of its own (to its login or its
  # first-run setup), so the page is fetched the way a browser gets it.
  seerr_code=$(curl -sS -L -o "$seerr_unauth" -w '%{http_code}' \
    --resolve "${SEERR_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${SEERR_HOST}/" 2>/dev/null || true)
  echo "  GET https://${SEERR_HOST}/ without a session (following Seerr's redirect): ${seerr_code}"
  [ "$seerr_code" = 200 ] || { echo "Seerr's own host answered ${seerr_code}"; fail=1; }
  grep -qi '<!doctype html' "$seerr_unauth" || { echo "Seerr's host did not serve an HTML page"; fail=1; }
  seerr_asset=$(grep -oE '(src|href)="/[^"]+\.(js|css)"' "$seerr_unauth" | head -1 | sed 's/^[a-z]*="//; s/"$//' || true)
  echo "  asset the page references: ${seerr_asset}"
  case "$seerr_asset" in
    "/"*) ;;
    "") echo "Seerr's page referenced no script or stylesheet"; fail=1 ;;
    *) echo "the asset ${seerr_asset} is not absolute: Seerr is not served from its own root"; fail=1 ;;
  esac
  if [ -n "$seerr_asset" ]; then
    asset_code=$(curl -sS -o /dev/null -w '%{http_code}' \
      --resolve "${SEERR_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
      "https://${SEERR_HOST}${seerr_asset}" 2>/dev/null || true)
    echo "  GET https://${SEERR_HOST}${seerr_asset}: ${asset_code}"
    [ "$asset_code" = 200 ] || { echo "Seerr's asset does not resolve on its own host"; fail=1; }
  fi
  seerr_authed=$(curl -sS -L -o "$WORK/seerr-authed.html" -w '%{http_code}' -b "$JAR" \
    --resolve "${SEERR_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${SEERR_HOST}/" 2>/dev/null || true)
  echo "  GET https://${SEERR_HOST}/ with the portal's session cookie: ${seerr_authed}"
  [ "$seerr_authed" = 200 ] || { echo "Seerr's own host answered ${seerr_authed} with a session cookie"; fail=1; }
  # The cookie's domain is the box's name, which a browser sends to a subdomain
  # too; Authelia names one cookie per host, and the app hosts are under it.
  cookie_domain=$($K -n s5n-system get cm s5n-authelia -o jsonpath='{.data.configuration\.yml}' |
    python3 -c 'import sys, re; print(re.search(r"- domain: \x27([^\x27]+)\x27", sys.stdin.read()).group(1))' 2>/dev/null || true)
  echo "  the login's session cookie domain: ${cookie_domain}"
  [ "$cookie_domain" = "$DOMAIN" ] || { echo "the cookie domain is ${cookie_domain}, want ${DOMAIN} (it covers its subdomains)"; fail=1; }
  # The old path is gone: no route, so the box's own host answers 404 there.
  old_path=$(curl -sS -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/seerr/" 2>/dev/null || true)
  echo "  GET https://${DOMAIN}/seerr/ (the old path): ${old_path}"
  [ "$old_path" = 404 ] || { echo "/seerr still answers ${old_path}: a path route was left behind"; fail=1; }
  # The card says nothing about missing records: every name answered here.
  if grep -q "has no DNS record" "$WORK/bootstrap1.txt"; then
    echo "init printed the no-DNS card line on a host whose names all resolve"
    fail=1
  fi

  # Path normalisation: Envoy normalises the request path before its filters
  # run (normalize_path is on by default and Envoy Gateway sets it explicitly),
  # so /sonarr/api/../ is a request for / -- not the API, and not a bypass.
  # An escaped dot is not a path separator: %2e%2e stays on the wire and the
  # API prefix matches. What matters either way is that no app page is served
  # without the login, so both are asserted against the UI, and the first
  # against the redirect.
  dotdot_code=$(curl -sS --path-as-is -o "$WORK/dotdot.html" -D "$WORK/dotdot-headers" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/api/../" 2>/dev/null || true)
  escaped_code=$(curl -sS --path-as-is -o "$WORK/escaped.html" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/api/%2e%2e/" 2>/dev/null || true)
  echo "  --path-as-is /sonarr/api/../: ${dotdot_code}; /sonarr/api/%2e%2e/: ${escaped_code}"
  [ "$dotdot_code" = 302 ] ||
    { echo "/sonarr/api/../ answered ${dotdot_code}: Envoy did not normalise it to /, so the login did not run"; fail=1; }
  if grep -qi '<title>Sonarr' "$WORK/dotdot.html"; then
    echo "/sonarr/api/../ served Sonarr's UI"
    fail=1
  fi
  if [ "$escaped_code" = 200 ] && grep -qi '<title>Sonarr' "$WORK/escaped.html"; then
    echo "/sonarr/api/%2e%2e/ served Sonarr's UI through the API bypass"
    fail=1
  fi

  # The challenge path is not asserted here: cert-manager's solver HTTPRoute
  # exists only while an order is pending, so after issuance that path is the
  # redirect's. The renewal below observes the solver route while it exists.

  # ---- a renewal, with the redirect and the narrowed listener in place -------
  # The renewal is triggered the way `cmctl renew` does: the Certificate's
  # Issuing condition is set to True/ManuallyTriggered, cert-manager orders a
  # new certificate and updates the existing Secret in place. Deleting the
  # Secret would be a different event -- Envoy Gateway would drop :443 while
  # the certificateRef was unresolved, which no real renewal does.
  #
  # HTTPS is polled through the whole renewal: it must answer 200 every time,
  # and the certificate must end with a new serial. The solver's HTTPRoute is
  # also caught while it exists, when the poll sees it: an exact path served by
  # the solver, with the redirect on the same host and port, is the precedence
  # the renewals depend on.
  log "renewal: a new order through the same port 80, with the redirect in place"
  old_serial=$(tls_serial "$cert")
  [ -n "$old_serial" ] || { echo "could not read the first certificate's serial"; fail=1; }
  stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  if ! $K -n s5n-gateway patch certificate "$cert" --subresource=status --type=merge \
    -p "{\"status\":{\"conditions\":[{\"type\":\"Issuing\",\"status\":\"True\",\"reason\":\"ManuallyTriggered\",\"message\":\"e2e renewal\",\"lastTransitionTime\":\"${stamp}\"}]}}" >/dev/null; then
    echo "could not trigger the renewal"
    fail=1
  fi
  solver_seen=""
  new_serial=""
  https_ok=0
  https_bad=0
  for i in $(seq 1 240); do
    if [ -z "$solver_seen" ]; then
      # The solver route under renewal is this host's: with more than one
      # certificate on the box, another host's solver route can be in the same
      # namespace at the same moment, and its exact path says nothing about
      # this host's port-80 listeners.
      solver=$($K -n s5n-gateway get httproute -o json 2>/dev/null | python3 -c '
import json, sys
domain = sys.argv[1]
items = json.load(sys.stdin).get("items", [])
for r in items:
    name = r["metadata"]["name"]
    if not name.startswith("cm-acme-http-solver"):
        continue
    if domain not in (r["spec"].get("hostnames") or []):
        continue
    ref = r["spec"]["parentRefs"][0]
    for rule in r["spec"].get("rules", []):
        for m in rule.get("matches", []):
            path = m.get("path", {})
            if path.get("type") == "Exact" and path.get("value", "").startswith("/.well-known/acme-challenge/"):
                print(name + " " + path["value"] + " " + str(ref.get("sectionName", "")))
                sys.exit(0)
' "$DOMAIN" || true)
      if [ -n "$solver" ]; then
        solver_seen=$(echo "$solver" | awk '{print $2}')
        solver_section=$(echo "$solver" | awk '{print $3}')
        [ -z "$solver_section" ] || { echo "the solver route pins the ${solver_section} listener instead of attaching to every admitting listener"; fail=1; }
        solver_code=$(curl -sS -o /dev/null -w '%{http_code}' \
          --resolve "${DOMAIN}:80:${gw_addr}" "http://${DOMAIN}${solver_seen}" || true)
        echo "  solver path ${solver_seen}: ${solver_code}"
        case "$solver_code" in
          301 | 302 | 307 | 308) echo "the solver's path was redirected"; fail=1 ;;
        esac
      fi
    fi
    code=$(curl -sS -o /dev/null -w '%{http_code}' \
      --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
      "https://${DOMAIN}/sonarr/ping" 2>"$WORK/https.err" || true)
    if [ "$code" = 200 ]; then
      https_ok=$((https_ok + 1))
    else
      https_bad=$((https_bad + 1))
    fi
    new_serial=$(tls_serial "$cert")
    if [ -n "$new_serial" ] && [ "$new_serial" != "$old_serial" ]; then
      break
    fi
    sleep 1
  done
  echo "  HTTPS during the renewal: ${https_ok} answers 200, ${https_bad} did not"
  if [ "$https_bad" != 0 ]; then
    echo "HTTPS did not answer throughout the renewal:"
    sed -n '1,5p' "$WORK/https.err" 2>/dev/null || true
    fail=1
  fi
  [ "$https_ok" -gt 0 ] || { echo "HTTPS was never checked successfully during the renewal"; fail=1; }
  if [ -z "$solver_seen" ]; then
    # The route lives about as long as Pebble's validation sleep (5s). Missing
    # it changes nothing: the new serial below is the proof the solver won.
    echo "  the solver route was not observed (it exists only during the challenge)"
  fi
  echo "  certificate serial ${old_serial} -> ${new_serial}"
  [ -n "$new_serial" ] && [ "$new_serial" != "$old_serial" ] ||
    { echo "the renewal did not issue a new certificate"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s ||
    { echo "the renewed Certificate never became Ready"; fail=1; }
  # Pebble generates its CA at startup; re-fetch it in case the pod restarted.
  re=$(curl -k -fsS "https://${gw_addr}:31500/roots/0" 2>/dev/null || true)
  if [ -n "$re" ]; then
    printf '%s\n' "$re" >"$WORK/pebble-root.pem"
  fi
  https_code=$(https_get "$gw_addr" /sonarr/ping "$WORK/pebble-root.pem")
  echo "  renewed HTTPS: ${https_code}"
  [ "$https_code" = 200 ] || { echo "HTTPS did not answer after the renewal"; fail=1; }

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
  # ---- admission: a seedbox namespace may not touch the login ----------------
  # The login lives in s5n-system and the policy on the Gateway, but a
  # namespace-scoped kubeconfig can still write a SecurityPolicy in its own
  # namespace -- and Envoy Gateway lets a route-targeted policy override the
  # Gateway-level one, which is a working bypass. The bindings root creates at
  # adduser are what refuse it; hack/render-grant is the same renderer box uses.
  log "admission: the login cannot be removed or overridden from ${NS}"
  go run ./hack/render-grant charts/s5n "$NS" 31111 NodePort | $K apply -f - >/dev/null
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
  denied=""
  # The object has to be one the API server would otherwise accept: the CRD's
  # schema is checked before any admission policy, so a fixture missing extAuth's
  # http block is refused for being malformed and never reaches the policy that
  # is under test. This one is a working bypass (extAuth to the login, failing
  # open); only the grant's admission policy stands between a seedbox user and
  # the login.
  for i in $(seq 1 30); do
    if ! $K --as="$SA" -n "$NS" create --dry-run=server -f - >"$WORK/login-deny.txt" 2>&1 <<'EOF'
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: e2e-bypass
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: sonarr
  extAuth:
    failOpen: true
    http:
      backendRefs:
        - name: authelia
          namespace: s5n-system
          port: 9091
      path: /api/authz/ext-authz/
EOF
    then
      grep -q "the login in front of the Gateway is root's" "$WORK/login-deny.txt" && { denied=yes; break; }
    fi
    sleep 1
  done
  echo "  a SecurityPolicy create in ${NS} as the user: ${denied:-allowed}"
  [ "$denied" = yes ] || {
    cat "$WORK/login-deny.txt" 2>/dev/null || true
    echo "a seedbox user could write a SecurityPolicy over the login"
    fail=1
  }

  # ---- admission: a route may not reach the sign-in path ---------------------
  # Authelia serves ^/auth without a login and Gateway API gives the request to
  # the most specific route, so a user's own HTTPRoute under /auth on their
  # host would be served unauthenticated. The binding refuses the write.
  log "admission: a route under /auth cannot be created from ${NS}"
  # The fixture is a route the API server would otherwise accept: a parentRef
  # to the Gateway and one backend. The path is the only thing under test.
  httproute() { # <name> <type> <path>
    cat <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $1
spec:
  parentRefs:
    - name: s5n
      namespace: s5n-gateway
  rules:
    - matches:
        - path:
            type: $2
            value: "$3"
      backendRefs:
        - name: sonarr
          port: 8989
EOF
  }
  # The binding lands a moment after the grant is applied: a dry-run admitted
  # while it is still missing is retried, not reported as a pass.
  deny_route() { # <name> <type> <path> <message> <out>
    local name=$1 type=$2 path=$3 want=$4 out=$5
    : >"$out"
    for _ in $(seq 1 30); do
      if httproute "$name" "$type" "$path" |
        $K --as="$SA" -n "$NS" create --dry-run=server -f - >"$out" 2>&1; then
        sleep 1
        continue
      fi
      grep -q "$want" "$out"
      return
    done
    return 1
  }
  allow_route() { # <namespace> <name> <type> <path> <out>
    httproute "$2" "$3" "$4" |
      $K --as="$SA" -n "$1" create --dry-run=server -f - >"$5" 2>&1
  }
  auth_case() { # <label> <name> <type> <path> <message>
    if deny_route "$2" "$3" "$4" "$5" "$WORK/auth-$2.txt"; then
      echo "  $1: denied ($(sed -n 's/.*denied request: //p' "$WORK/auth-$2.txt" | head -1))"
    else
      echo "  $1: admitted, or refused for another reason"
      cat "$WORK/auth-$2.txt" || true
      fail=1
    fi
  }
  auth_case "PathPrefix /auth/x" e2e-auth-prefix PathPrefix /auth/x \
    "s5n: /auth is root's sign-in path; a seedbox route may not match it"
  auth_case "Exact /auth" e2e-auth-exact Exact /auth \
    "s5n: /auth is root's sign-in path; a seedbox route may not match it"
  auth_case "RegularExpression /a.th/.*" e2e-auth-regex RegularExpression "/a.th/.*" \
    "may not use a RegularExpression path"
  # The CRD allows %61 (it refuses only %2f and dot segments), so this one
  # reaches the policy; if a future CRD refuses it first, this case says which
  # layer refused instead of asserting the policy's message.
  auth_case "PathPrefix /%61uth/x" e2e-auth-percent PathPrefix "/%61uth/x" \
    "may not use a percent-encoded path"
  if allow_route "$NS" e2e-auth-neighbour PathPrefix /authx "$WORK/auth-neighbour.txt"; then
    echo "  PathPrefix /authx: admitted"
  else
    echo "  PathPrefix /authx: refused"
    cat "$WORK/auth-neighbour.txt" || true
    fail=1
  fi
  # The policy is bound to one namespace, not installed cluster-wide: the same
  # route in a namespace with no binding is admitted.
  $K create namespace s5n-e2e-unbound --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: box-user
  namespace: s5n-e2e-unbound
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["httproutes"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: box-user
  namespace: s5n-e2e-unbound
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: box-user
subjects:
  - kind: ServiceAccount
    name: box-user
    namespace: ${NS}
EOF
  if allow_route s5n-e2e-unbound e2e-auth-unbound PathPrefix /auth/x "$WORK/auth-unbound.txt"; then
    echo "  PathPrefix /auth/x in an unbound namespace: admitted"
  else
    echo "  an unbound namespace refused /auth/x: the policy is not scoped"
    cat "$WORK/auth-unbound.txt" || true
    fail=1
  fi

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
    echo "issuer-le=$($K get clusterissuer s5n-letsencrypt -o json | platform_spec)"
    echo "issuer-ss=$($K get clusterissuer s5n-selfsigned -o json | platform_spec)"
    for p in s5n-blessed-images s5n-granted-service s5n-no-login-changes s5n-no-auth-routes; do
      echo "vap-${p}=$($K get validatingadmissionpolicy "$p" -o json | platform_spec)"
    done
    echo "s5n-system=$($K get namespace s5n-system -o json | platform_spec)"
  }
  platform_before=$(platform_state)
  grant_before2=$(sha256sum "$WORK/grant.yaml" | cut -d' ' -f1)
  values_before2=$(sha256sum "$WORK/values.yaml" | cut -d' ' -f1)

  # The second bootstrap re-applies gateway.yaml under manager box while the
  # Certificate is already Ready, so the TLS half re-applies this host's two
  # listeners and their Allow. Under the old design that re-opened the shared
  # listener between the two writes and served /sonarr/ in plaintext. Poll the
  # app path through the whole run: every sample is the 301 to HTTPS, and none
  # may ever be the app.
  port80_window="$WORK/port80-window.log"
  : >"$port80_window"
  rm -f "$WORK/port80-stop"
  (
    while [ ! -e "$WORK/port80-stop" ]; do
      window_code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 \
        --resolve "${DOMAIN}:80:${gw_addr}" "http://${DOMAIN}/sonarr/" 2>/dev/null || true)
      printf '%s\n' "$window_code" >>"$port80_window"
      sleep 0.2
    done
  ) &
  port80_poll=$!

  log "box bootstrap: a second run must install nothing and change nothing"
  if bootstrap "$WORK/bootstrap2.txt"; then
    cat "$WORK/bootstrap2.txt"
  else
    cat "$WORK/bootstrap2.txt"; fail=1
  fi
  touch "$WORK/port80-stop"
  wait "$port80_poll" || true
  port80_samples=$(grep -c . "$port80_window" || true)
  port80_200=$(grep -cx 200 "$port80_window" || true)
  port80_redirect=$(grep -cx 301 "$port80_window" || true)
  port80_other=$(grep -vxE '301|000' "$port80_window" | grep -c . || true)
  echo "  port 80 during the second bootstrap: ${port80_samples} samples, ${port80_redirect} x 301, ${port80_200} x 200, ${port80_other} other"
  [ "$port80_samples" -gt 0 ] || { echo "the second bootstrap was too quick to sample port 80"; fail=1; }
  [ "$port80_redirect" -gt 0 ] || { echo "port 80 never redirected an app path during the second bootstrap"; fail=1; }
  if [ "$port80_200" != 0 ]; then
    echo "port 80 served the app in plaintext during the second bootstrap:"
    grep -n '^200$' "$port80_window" | head -5 || true
    fail=1
  fi
  if [ "$port80_other" != 0 ]; then
    echo "port 80 answered something other than the redirect during the second bootstrap:"
    grep -vxE '301|000' "$port80_window" | sort | uniq -c | head -5 || true
    fail=1
  fi
  grep -qF "= metallb: already at ${METALLB_CHART_VERSION}" "$WORK/bootstrap2.txt" ||
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
  # The cert-manager box installed is, by then, a working install its own
  # probe uses, so a re-run leaves it alone exactly like Envoy Gateway.
  if grep -qE '^\s+[+~] cert-manager ' "$WORK/bootstrap2.txt"; then
    echo "the second run took the cert-manager release back over"; fail=1
  fi
  if grep -qE "^\s+[+?] (envoy-gateway|metallb) " "$WORK/bootstrap2.txt"; then
    echo "the second run planned an install on an installed platform"; fail=1
  fi
  grep -q "already says this" "$WORK/bootstrap2.txt" || { echo "the second init did not say the grant was already right"; fail=1; }
  grep -q "nothing to change" "$WORK/bootstrap2.txt" || { echo "the second init did not say nothing to change"; fail=1; }
  no_probe_service
  # The login is a second object set box owns, and it must be as still as the
  # platform: the second run re-renders the same configuration, the same pod
  # template annotation, and the same SecurityPolicy. A restarted pod would be
  # a changed hash, so its uid is the assertion; the session cookie from before
  # the run is the user-visible half of the same fact.
  authelia_uid_after=$($K -n s5n-system get pod -l app.kubernetes.io/name=authelia -o jsonpath='{.items[0].metadata.uid}' || true)
  echo "  login pod uid: ${authelia_uid} -> ${authelia_uid_after}"
  [ "$authelia_uid_after" = "$authelia_uid" ] ||
    { echo "the second bootstrap restarted the login (a changed configuration rolls the pod)"; fail=1; }
  session_after=$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/sonarr/" 2>/dev/null || true)
  echo "  the session from before the second bootstrap: ${session_after}"
  [ "$session_after" = 200 ] || { echo "the second bootstrap logged everyone out"; fail=1; }

  platform_after=$(platform_state)
  if [ "$platform_after" != "$platform_before" ]; then
    echo "the second bootstrap touched the platform"
    echo "  before: ${platform_before}"
    echo "  after:  ${platform_after}"
    fail=1
  else
    echo "platform untouched: ${platform_after}"
  fi
  # A re-run re-applies gateway.yaml, which only carries the generic http
  # listener. That listener belongs to manager box; each host's two listeners
  # belong to the host's manager, so neither apply can remove the other's.
  https_after=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="https-box-example-test")].port}')
  http_host_after=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http-box-example-test")].port}')
  http_after=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.spec.listeners[?(@.name=="http")].allowedRoutes.namespaces.from}')
  [ "$https_after" = 443 ] ||
    { echo "the second bootstrap dropped the HTTPS listener (port ${https_after})"; fail=1; }
  [ "$http_host_after" = 80 ] ||
    { echo "the second bootstrap dropped the host's port-80 listener (port ${http_host_after})"; fail=1; }
  [ "$http_after" = All ] ||
    { echo "the second bootstrap narrowed the shared http listener (from ${http_after})"; fail=1; }

  # Which manager owns which listener, read from the Gateway's own
  # managedFields: box owns the generic http listener and nothing else; this
  # host's port-80 listener and its HTTPS one have a manager each, so an apply
  # that names one can never remove the other. No field is owned by two box
  # managers, so no bootstrap can flip one under the other.
  $K -n s5n-gateway get gateway s5n -o json --show-managed-fields | python3 -c '
import json, sys
g = json.load(sys.stdin)
want = {
    "box": {"http"},
    "box-tls-box-example-test-http": {"http-box-example-test"},
    "box-tls-box-example-test-https": {"https-box-example-test"},
}
got = {}
for entry in g["metadata"].get("managedFields") or []:
    manager = entry.get("manager", "")
    listeners = (entry.get("fieldsV1") or {}).get("f:spec", {}).get("f:listeners", {})
    names = set()
    for key in listeners:
        if key.startswith("k:"):
            names.add(json.loads(key[2:])["name"])
    if names:
        got.setdefault(manager, set()).update(names)
print("  managedFields listeners:", {m: sorted(v) for m, v in sorted(got.items())})
for manager, names in want.items():
    if got.get(manager) != names:
        print("manager %s owns %s, want %s" % (manager, sorted(got.get(manager, [])), sorted(names)))
        sys.exit(1)
owner = {}
for manager, names in got.items():
    for name in names:
        if name in owner:
            print("listener %s is owned by both %s and %s" % (name, owner[name], manager))
            sys.exit(1)
        owner[name] = manager
' || { echo "the Gateway's listeners are not owned one-manager-each"; fail=1; }

  # After the re-apply, the app path on this host's port 80 is still the
  # redirect, and the Allow that makes it work is still there. A different host
  # name with no certificate still reaches the generic listener, where the login
  # is: its route attaches there, and Authelia refuses the plain-HTTP target
  # outright (400, before it reads any rule) -- so not even /ping answers, and
  # certainly no app. That is what the shared listener is for: a host without a
  # certificate stays closed on port 80. (This box's app routes are host-scoped
  # to its own domain, so the test writes the route a second, certificate-less
  # box would own.)
  http_code=$(curl -sS -D "$WORK/http-headers2" -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:80:${gw_addr}" "http://${DOMAIN}/sonarr/" || true)
  echo "  HTTP ${DOMAIN}/sonarr/ after the second bootstrap: ${http_code}"
  [ "$http_code" = 301 ] || { echo "an app path on port 80 answered ${http_code} after the second bootstrap, want the 301"; fail=1; }
  grep -qi "^location: https://${DOMAIN}/sonarr/" "$WORK/http-headers2" ||
    { echo "the redirect after the second bootstrap does not point at HTTPS"; fail=1; }
  nl_after=$($K -n s5n-gateway get securitypolicy s5n-no-login-box-example-test -o jsonpath='{.spec.authorization.defaultAction}' 2>/dev/null || true)
  [ "$nl_after" = Allow ] || { echo "the second bootstrap removed the port-80 Allow"; fail=1; }
  $K apply -f - >/dev/null <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: e2e-other-host
  namespace: seedbox
spec:
  parentRefs:
    - name: s5n
      namespace: s5n-gateway
  hostnames:
    - other.example.test
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /sonarr
      backendRefs:
        - name: sonarr
          port: 8989
EOF
  other_code=""
  for i in $(seq 1 30); do
    other_code=$(curl -sS -o /dev/null -w '%{http_code}' \
      --resolve "other.example.test:80:${gw_addr}" "http://other.example.test/sonarr/ping" || true)
    [ "$other_code" = 400 ] && break
    sleep 1
  done
  echo "  HTTP other.example.test/sonarr/ping (no certificate): ${other_code}"
  [ "$other_code" = 400 ] ||
    { echo "a host with no certificate answered ${other_code} on plain HTTP: the login must refuse its route (400, an http target) rather than serve it"; fail=1; }
  $K -n "$NS" delete httproute e2e-other-host --ignore-not-found >/dev/null || fail=1
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

  # ---- a host that has none of the three objects it needs ---------------------
  # The case the promise is about: the release renders a route on seerr.<domain>
  # while nothing answers there, so the app is unreachable and nothing says so
  # (docs/box.md, "No run leaves an app host without its listener"). The box is
  # put in the state an install without Seerr leaves: the app off, and all three
  # of root's per-host objects gone -- the Certificate, both listeners, and the
  # login's access_control rule for the name. apply has to write the rule as
  # well: a host with a listener and a Certificate but no rule answers the
  # login's default deny, which is a 403 nobody can get past.
  log "turning Seerr off, then removing everything root had written for its host"
  python3 - "$WORK/grant.yaml" <<'PY' || { echo "could not turn Seerr off in the grant"; fail=1; }
import re, sys
path = sys.argv[1]
text = open(path).read()
updated, count = re.subn(r"(?m)^(  seerr:\n    enabled: )true$", r"\1false", text)
if count != 1:
    sys.exit("the grant does not say apps.seerr.enabled: true (%d matches)" % count)
open(path, "w").write(updated)
PY
  if apply "$WORK/apply-off.txt"; then
    cat "$WORK/apply-off.txt"
  else
    cat "$WORK/apply-off.txt"; fail=1
  fi
  seerr_label=${SEERR_HOST//./-}
  seerr_cert=s5n-tls-${seerr_label}
  $K -n s5n-gateway delete certificate "$seerr_cert" --ignore-not-found >/dev/null || fail=1
  # Both listeners, by index from the back so the earlier ones keep theirs.
  listener_patch=$(python3 - "$($K -n s5n-gateway get gateway s5n -o json)" "$seerr_label" <<'PY'
import json, sys
gateway = json.loads(sys.argv[1])
label = sys.argv[2]
gone = {"https-" + label, "http-" + label}
print(json.dumps([{"op": "remove", "path": "/spec/listeners/%d" % i}
                  for i, l in reversed(list(enumerate(gateway["spec"]["listeners"])))
                  if l["name"] in gone]))
PY
) || { echo "could not read the Gateway's listeners"; fail=1; }
  $K -n s5n-gateway patch gateway s5n --type=json -p "$listener_patch" >/dev/null ||
    { echo "could not remove ${SEERR_HOST}'s listeners"; fail=1; }
  if $K -n s5n-gateway get certificate "$seerr_cert" >/dev/null 2>&1; then
    echo "the Certificate ${seerr_cert} is still there"
    fail=1
  fi
  for gone in "https-${seerr_label}" "http-${seerr_label}"; do
    still=$($K -n s5n-gateway get gateway s5n \
      -o jsonpath="{.spec.listeners[?(@.name==\"${gone}\")].protocol}" 2>/dev/null || true)
    [ -z "$still" ] || { echo "the listener ${gone} is still on the Gateway"; fail=1; }
  done

  # The login's rule went with the app: root's apply renders the login's whole
  # configuration from the values on every run, so turning Seerr off removed its
  # access_control rule and rolled the login onto the rule-less file. The login
  # that is answering has never heard of the name -- which is what a box
  # installed without Seerr looks like -- and box got it there, not this script.
  log "the apply that turned Seerr off also removed the login's rule for ${SEERR_HOST}"
  $K -n s5n-system rollout status deploy/authelia --timeout=180s >/dev/null ||
    { echo "the login did not come back on the rule-less configuration"; fail=1; }
  authelia_uid_before=$($K -n s5n-system get pod -l app.kubernetes.io/name=authelia -o jsonpath='{.items[0].metadata.uid}' || true)
  still_ruled=$($K -n s5n-system get cm s5n-authelia -o jsonpath='{.data.configuration\.yml}' | grep -cF "'${SEERR_HOST}'" || true)
  echo "  rules for ${SEERR_HOST} in the login's configuration: ${still_ruled}"
  [ "$still_ruled" = 0 ] || { echo "the login still has a rule for ${SEERR_HOST}"; fail=1; }

  # Root turns the app back on in the grant. There is no `box install` yet, so
  # this is the only way an app is added to a seedbox.
  log "box apply: an app host with none of its objects gets all three before the release moves"
  python3 - "$WORK/grant.yaml" <<'PY' || { echo "could not turn Seerr on in the grant"; fail=1; }
import re, sys
path = sys.argv[1]
text = open(path).read()
updated, count = re.subn(r"(?m)^(  seerr:\n    enabled: )false$", r"\1true", text)
if count != 1:
    sys.exit("the grant does not say apps.seerr.enabled: false (%d matches)" % count)
open(path, "w").write(updated)
PY
  if apply "$WORK/apply-seerr.txt"; then
    cat "$WORK/apply-seerr.txt"
  else
    cat "$WORK/apply-seerr.txt"; fail=1
  fi
  grep -qF "Certificate, listener and login rule for ${SEERR_HOST}" "$WORK/apply-seerr.txt" ||
    { echo "apply did not show the login rule it would write for ${SEERR_HOST}"; fail=1; }
  grep -qF "applied Certificate s5n-gateway/${seerr_cert} (issuer s5n-letsencrypt)" "$WORK/apply-seerr.txt" ||
    { echo "apply did not write ${seerr_cert} with the production issuer"; fail=1; }
  grep -qF "login: the rule for ${SEERR_HOST} is in place" "$WORK/apply-seerr.txt" ||
    { echo "apply did not write the login's rule for ${SEERR_HOST}"; fail=1; }
  # The order is the promise: the host is served before the release that renders
  # its route moves, so there is never a route on a name nothing answers on.
  seerr_cert_line=$(grep -n "applied Certificate s5n-gateway/${seerr_cert}" "$WORK/apply-seerr.txt" | head -1 | cut -d: -f1 || true)
  seerr_rule_line=$(grep -n "login: the rule for ${SEERR_HOST} is in place" "$WORK/apply-seerr.txt" | head -1 | cut -d: -f1 || true)
  seerr_helm_line=$(grep -n "applied: s5n revision" "$WORK/apply-seerr.txt" | head -1 | cut -d: -f1 || true)
  echo "  login rule at line ${seerr_rule_line}, certificate at line ${seerr_cert_line}, release at line ${seerr_helm_line}"
  if [ -z "$seerr_rule_line" ] || [ -z "$seerr_cert_line" ] || [ -z "$seerr_helm_line" ] ||
     [ "$seerr_rule_line" -ge "$seerr_helm_line" ] || [ "$seerr_cert_line" -ge "$seerr_helm_line" ]; then
    echo "the per-host step did not run before the release moved"
    fail=1
  fi
  seerr_port=$($K -n s5n-gateway get gateway s5n \
    -o jsonpath="{.spec.listeners[?(@.name==\"https-${seerr_label}\")].port}" 2>/dev/null || true)
  echo "  the listener https-${seerr_label} is on port ${seerr_port}"
  [ "$seerr_port" = 443 ] || { echo "apply did not add the HTTPS listener for ${SEERR_HOST}"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/${seerr_cert}" --timeout=180s ||
    { echo "the Certificate ${seerr_cert} never became Ready"; fail=1; }
  # The rule is in the file the login starts with, and the pod that is answering
  # is the one that read it: a pod left on the old hash would answer with the old
  # rules however right the ConfigMap looked.
  ruled_after=$($K -n s5n-system get cm s5n-authelia -o jsonpath='{.data.configuration\.yml}' | grep -cF -- "- domain: ['${SEERR_HOST}']" || true)
  echo "  rules for ${SEERR_HOST} in the login's configuration: ${ruled_after}"
  [ "$ruled_after" = 1 ] || { echo "the login has ${ruled_after} rules for ${SEERR_HOST}, want one"; fail=1; }
  config_hash=$($K -n s5n-system get cm s5n-authelia -o jsonpath='{.data.configuration\.yml}' | sha256sum | cut -d' ' -f1)
  pod_hash=$($K -n s5n-system get deploy authelia -o jsonpath='{.spec.template.metadata.annotations.s5n\.io/config-hash}' || true)
  echo "  configuration hash ${config_hash}, the login's pod template has ${pod_hash}"
  [ "$pod_hash" = "$config_hash" ] || { echo "the login's pod template hash is not its configuration's: the rule is written but not read"; fail=1; }
  authelia_uid_after=$($K -n s5n-system get pod -l app.kubernetes.io/name=authelia -o jsonpath='{.items[0].metadata.uid}' || true)
  [ -n "$authelia_uid_after" ] && [ "$authelia_uid_after" != "$authelia_uid_before" ] ||
    { echo "the login's pod did not restart onto its new configuration"; fail=1; }
  # And the app answers through Envoy by Host: the route the release rendered is
  # now attached to a listener that serves the name, behind a login that admits
  # it. Before the rule this was a 403 from the login, which is the whole bug.
  seerr_code=$(curl -sS -L -o "$WORK/seerr-again.html" -w '%{http_code}' \
    --resolve "${SEERR_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${SEERR_HOST}/" 2>/dev/null || true)
  echo "  GET https://${SEERR_HOST}/ after the apply: ${seerr_code}"
  [ "$seerr_code" = 200 ] || { echo "the app host did not answer after apply served it: ${seerr_code}"; fail=1; }
  grep -qi '<!doctype html' "$WORK/seerr-again.html" || { echo "the app host served no HTML after apply"; fail=1; }

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
  grep -qF "? metallb ${METALLB_CHART_VERSION} (Helm, metallb-system), unless the Gateway gets an address without it" "$WORK/bootstrap1.txt" ||
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

  # ---- TLS: a name that does not resolve gets the self-signed issuer ---------
  # No CoreDNS entry and a reserved TLD: init resolves nothing, picks
  # s5n-selfsigned, and the certificate is still a real TLS certificate. -k is
  # the honest client behaviour for a box nobody can reach from the internet.
  cert=s5n-tls-box-internal-example
  grep -qF "tls: ${DOMAIN} does not resolve" "$WORK/bootstrap1.txt" ||
    { echo "init did not report that the domain does not resolve"; fail=1; }
  grep -qF "using a self-signed certificate" "$WORK/bootstrap1.txt" ||
    { echo "init did not choose the self-signed issuer"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=120s ||
    { echo "the self-signed Certificate never became Ready"; fail=1; }
  cert_issuer=$($K -n s5n-gateway get certificate "$cert" -o jsonpath='{.spec.issuerRef.name}')
  [ "$cert_issuer" = s5n-selfsigned ] || { echo "the self-signed Certificate names issuer ${cert_issuer}"; fail=1; }
  # The production issuer was not touched by this scenario: no Pebble override.
  acme_server=$($K get clusterissuer s5n-letsencrypt -o jsonpath='{.spec.acme.server}')
  [ "$acme_server" = "https://acme-v02.api.letsencrypt.org/directory" ] ||
    { echo "s5n-letsencrypt was pointed away from Let's Encrypt"; fail=1; }
  [ -z "$($K get clusterissuer s5n-letsencrypt -o jsonpath='{.spec.acme.skipTLSVerify}')" ] ||
    { echo "the production issuer skips ACME endpoint verification"; fail=1; }

  # ---- the anchor: the box root travels into the seedbox's namespace ---------
  # A self-signed host's certificate chains to one root box owns, so root's run
  # copies that root into the seedbox's namespace: it is what an app which
  # fetches the login's OIDC documents from its own pod has to trust. The
  # ConfigMap carries the certificate and nothing else -- never a key.
  root_crt=$($K -n cert-manager get secret s5n-root-ca -o jsonpath='{.data.tls\.crt}' | base64 -d || true)
  [ -n "$root_crt" ] || { echo "the box root's Secret cert-manager/s5n-root-ca is empty or missing"; fail=1; }
  # The issuer the certificate names is a CA over the root, not the bare
  # self-signed one 0.3.0 shipped: same name, different object.
  [ "$($K get clusterissuer s5n-selfsigned -o jsonpath='{.spec.ca.secretName}' || true)" = s5n-root-ca ] ||
    { echo "s5n-selfsigned is not a CA issuer over s5n-root-ca"; fail=1; }
  anchor=$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)
  [ -n "$anchor" ] || { echo "root's run did not write ConfigMap ${NS}/s5n-host-ca for a self-signed host"; fail=1; }
  same_cert "$anchor" "$root_crt" ||
    { echo "s5n-host-ca does not hold the box root's certificate"; fail=1; }
  anchor_keys=$($K -n "$NS" get configmap s5n-host-ca -o json 2>/dev/null |
    python3 -c 'import json,sys;print(",".join(sorted(json.load(sys.stdin).get("data",{}).keys())))' || true)
  [ "$anchor_keys" = ca.crt ] || { echo "s5n-host-ca carries ${anchor_keys}, want the key ca.crt alone"; fail=1; }
  printf '%s\n' "$anchor" >"$WORK/anchor-ca.pem"

  # The certificate the Gateway serves is the root's child, verified against
  # that same ConfigMap: the chain is captured the way a client sees it, and
  # `openssl verify` is the assertion. 0.3.0's shape would pass a naive check --
  # the leaf was its own CA -- so the leaf is also compared with the root.
  echo | openssl s_client -connect "${lb_addr}:443" -servername "${DOMAIN}" -showcerts \
    >"$WORK/served-chain.pem" 2>/dev/null || true
  awk '/BEGIN CERTIFICATE/{n++} n==1' "$WORK/served-chain.pem" >"$WORK/served-leaf.pem"
  if [ ! -s "$WORK/served-leaf.pem" ]; then
    echo "openssl s_client through the Gateway returned no certificate"
    fail=1
  else
    echo "  served: $(openssl x509 -in "$WORK/served-leaf.pem" -noout -subject -issuer 2>/dev/null | tr '\n' ' ')"
    if openssl verify -CAfile "$WORK/anchor-ca.pem" "$WORK/served-leaf.pem" >"$WORK/verify.txt" 2>&1; then
      echo "  verify: $(cat "$WORK/verify.txt")"
    else
      echo "the certificate ${DOMAIN} serves does not verify against s5n-host-ca"
      cat "$WORK/verify.txt"
      fail=1
    fi
    if same_cert "$(cat "$WORK/served-leaf.pem")" "$root_crt"; then
      echo "the served certificate is the root itself, not a leaf signed by it"
      fail=1
    fi
  fi
  # A client that trusts nothing but the anchor gets the app's own answer.
  verified_code=$(https_get "$lb_addr" /sonarr/ping "$WORK/anchor-ca.pem" || true)
  echo "  HTTPS ${DOMAIN}/sonarr/ping with s5n-host-ca as the only anchor: ${verified_code}"
  [ "$verified_code" = 200 ] || { echo "the host did not answer 200 with s5n-host-ca as the only anchor"; fail=1; }

  https_code=$(https_get "$lb_addr" /sonarr/ping -)
  echo "  HTTPS ${DOMAIN}/sonarr/ping (self-signed): ${https_code}"
  [ "$https_code" = 200 ] || { echo "HTTPS did not answer 200 with the self-signed certificate"; fail=1; }
  http_code=$(curl -sS -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:80:${lb_addr}" "http://${DOMAIN}/sonarr/ping" || true)
  echo "  HTTP ${DOMAIN}/sonarr/ping: ${http_code}"
  [ "$http_code" = 301 ] || { echo "an app path on port 80 was not redirected"; fail=1; }
  # The same on the app root: the host's port-80 listener carries no login, so
  # the UI path gets the 301 rather than the portal's refusal.
  http_ui=$(curl -sS -D "$WORK/preinstalled-http-headers" -o /dev/null -w '%{http_code}' \
    --resolve "${DOMAIN}:80:${lb_addr}" "http://${DOMAIN}/sonarr/" || true)
  echo "  HTTP ${DOMAIN}/sonarr/: ${http_ui}"
  [ "$http_ui" = 301 ] || { echo "an app UI path on port 80 answered ${http_ui}, want the 301"; fail=1; }
  grep -qi "^location: https://${DOMAIN}/sonarr/" "$WORK/preinstalled-http-headers" ||
    { echo "the port-80 redirect on this host does not point at HTTPS"; fail=1; }

  # Every scenario installs the login, so the app UI is never served to an
  # unauthenticated request: no cookie means the portal, on this host's own
  # name and its self-signed certificate.
  unauth_headers="$WORK/preinstalled-unauth-headers.txt"
  unauth_code=$(curl -sS -k -o /dev/null -D "$unauth_headers" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${lb_addr}" "https://${DOMAIN}/sonarr/" 2>/dev/null || true)
  unauth_location=$(grep -i '^location:' "$unauth_headers" | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
  echo "  GET https://${DOMAIN}/sonarr/ without a cookie: ${unauth_code} -> ${unauth_location}"
  [ "$unauth_code" = 302 ] || { echo "an unauthenticated app path answered ${unauth_code}"; fail=1; }
  case "$unauth_location" in
    "https://${DOMAIN}/auth/"*) ;;
    *) echo "the redirect does not point at the portal: ${unauth_location}"; fail=1 ;;
  esac

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
    echo "issuer-le=$($K get clusterissuer s5n-letsencrypt -o json | platform_spec)"
    echo "issuer-ss=$($K get clusterissuer s5n-selfsigned -o json | platform_spec)"
    for p in s5n-blessed-images s5n-granted-service s5n-no-login-changes s5n-no-auth-routes; do
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


  # ---- a renewal keeps the anchor -------------------------------------------
  # Force a re-issue with the trigger box itself uses -- the Issuing condition
  # `cmctl renew` sets -- and prove the anchor did not move. That is the whole
  # reason for a root: a new leaf, one anchor, and no pod restarted.
  serial_before_renewal=$(tls_serial "$cert" || true)
  anchor_rv_before=$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.metadata.resourceVersion}' || true)
  $K -n s5n-gateway get certificate "$cert" -o json | python3 -c '
import json, sys
crt = json.load(sys.stdin)
conds = [c for c in crt["status"].get("conditions", []) if c["type"] != "Issuing"]
conds.append({"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
              "message": "Certificate re-issuance manually triggered",
              "observedGeneration": crt["metadata"]["generation"],
              "lastTransitionTime": "2026-01-01T00:00:00Z"})
print(json.dumps({"status": {"conditions": conds}}))' >"$WORK/cert-status.json"
  $K -n s5n-gateway patch certificate "$cert" --subresource=status --type=merge \
    -p "$(cat "$WORK/cert-status.json")" >/dev/null ||
    { echo "could not set the Issuing condition on ${cert}"; fail=1; }
  serial_after_renewal=""
  for i in $(seq 1 60); do
    serial_after_renewal=$(tls_serial "$cert" || true)
    [ -n "$serial_after_renewal" ] && [ "$serial_after_renewal" != "$serial_before_renewal" ] && break
    sleep 2
  done
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=120s || fail=1
  [ -n "$serial_after_renewal" ] && [ "$serial_after_renewal" != "$serial_before_renewal" ] ||
    { echo "the forced renewal did not produce a new serial (was ${serial_before_renewal})"; fail=1; }
  echo "  renewal serial: ${serial_before_renewal} -> ${serial_after_renewal}"
  same_cert "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' || true)" "$anchor" ||
    { echo "the anchor's certificate changed with the renewal"; fail=1; }
  [ "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.metadata.resourceVersion}' || true)" = "$anchor_rv_before" ] ||
    { echo "the anchor ConfigMap was rewritten by a renewal"; fail=1; }
  # And the Gateway serves the new leaf, still verifying against that anchor.
  served_serial=""
  for i in $(seq 1 30); do
    echo | openssl s_client -connect "${lb_addr}:443" -servername "${DOMAIN}" -showcerts \
      >"$WORK/renewed-chain.pem" 2>/dev/null || true
    awk '/BEGIN CERTIFICATE/{n++} n==1' "$WORK/renewed-chain.pem" >"$WORK/renewed-leaf.pem"
    served_serial=$(openssl x509 -in "$WORK/renewed-leaf.pem" -noout -serial 2>/dev/null | sed 's/^serial=//' || true)
    [ "$served_serial" = "$serial_after_renewal" ] && break
    sleep 2
  done
  [ "$served_serial" = "$serial_after_renewal" ] ||
    { echo "the Gateway still serves serial ${served_serial}, want the renewed ${serial_after_renewal}"; fail=1; }
  openssl verify -CAfile "$WORK/anchor-ca.pem" "$WORK/renewed-leaf.pem" >"$WORK/verify2.txt" 2>&1 ||
    { echo "the renewed certificate does not verify against the unchanged anchor"; cat "$WORK/verify2.txt"; fail=1; }

  # ---- a record appears before the certificate moves: the anchor stays -------
  # The window this pins: the operator adds the DNS record and runs `box apply`
  # before `box init`. The host still serves the certificate init last wrote --
  # only init's per-host step moves it to Let's Encrypt -- so the anchor has to
  # stay, or every app that restarts while the certificate is still box's own
  # stops trusting the login. The anchor followed DNS once: this apply would
  # have taken it away.
  log "a public record and an apply: the anchor stays until the certificate moves"
  cat >"$WORK/dns-window" <<EOF
192.0.2.10 ${DOMAIN}
EOF
  anchor_rv_window=$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.metadata.resourceVersion}')
  if "$WORK/box" apply --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --dns-file "$WORK/dns-window" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/apply-dns-window.txt" 2>&1; then
    cat "$WORK/apply-dns-window.txt"
  else
    cat "$WORK/apply-dns-window.txt"; fail=1
  fi
  grep -qF "nothing to change" "$WORK/apply-dns-window.txt" ||
    { echo "the apply over a self-signed host had work to do"; fail=1; }
  [ "$($K -n s5n-gateway get certificate "$cert" -o jsonpath='{.spec.issuerRef.name}')" = s5n-selfsigned ] ||
    { echo "the host's Certificate moved to another issuer without init"; fail=1; }
  same_cert "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' || true)" "$anchor" ||
    { echo "a public DNS answer moved the anchor while the host still served box's own certificate"; fail=1; }
  [ "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.metadata.resourceVersion}')" = "$anchor_rv_window" ] ||
    { echo "the anchor ConfigMap was rewritten by an apply that saw a public record"; fail=1; }
  echo "  s5n-host-ca untouched: the certificate is still self-signed"

  # ---- an upgraded box: a leaf from 0.3.0's bare self-signed issuer ----------
  # 0.3.0 shipped s5n-selfsigned as `selfSigned: {}`, so every host certificate
  # was its own CA. This block recreates that state BY HAND and nothing in the
  # product does it: it is the pre-existing box the next bootstrap has to
  # repair. It is applied under box's own field manager on purpose -- an
  # installed 0.3.0 box has exactly that, and it is what lets the next apply
  # take the field away.
  if $K apply --server-side --field-manager=box --force-conflicts -f - >/dev/null <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: s5n-selfsigned
  labels:
    app.kubernetes.io/part-of: s5n
    app.kubernetes.io/managed-by: box
spec:
  selfSigned: {}
EOF
  then :; else echo "could not restore the 0.3.0 self-signed issuer"; fail=1; fi
  [ -z "$($K get clusterissuer s5n-selfsigned -o jsonpath='{.spec.ca}' || true)" ] ||
    { echo "the hand-made 0.3.0 issuer still names a CA"; fail=1; }
  $K -n s5n-gateway delete secret "$cert" --ignore-not-found >/dev/null ||
    { echo "could not delete the certificate's Secret for the 0.3.0 shape"; fail=1; }
  serial_old_shape=""
  for i in $(seq 1 60); do
    serial_old_shape=$(tls_serial "$cert" || true)
    [ -n "$serial_old_shape" ] && break
    sleep 2
  done
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s ||
    { echo "the 0.3.0-shaped certificate never came back"; fail=1; }
  old_shape_ca=$($K -n s5n-gateway get secret "$cert" -o jsonpath='{.data.ca\.crt}' | base64 -d || true)
  old_shape_leaf=$($K -n s5n-gateway get secret "$cert" -o jsonpath='{.data.tls\.crt}' | base64 -d || true)
  same_cert "$old_shape_ca" "$old_shape_leaf" ||
    { echo "the hand-made certificate is not its own CA, so this is not the 0.3.0 shape"; fail=1; }
  echo "  pre-upgrade leaf is its own CA (serial ${serial_old_shape})"

  log "box bootstrap: an upgraded box's host certificate is re-issued from the root"
  if bootstrap "$WORK/bootstrap-upgrade.txt"; then
    cat "$WORK/bootstrap-upgrade.txt"
  else
    cat "$WORK/bootstrap-upgrade.txt"; fail=1
  fi
  grep -qF "re-issued from the box root" "$WORK/bootstrap-upgrade.txt" ||
    { echo "box did not report re-issuing the host certificate"; fail=1; }
  [ "$($K get clusterissuer s5n-selfsigned -o jsonpath='{.spec.ca.secretName}' || true)" = s5n-root-ca ] ||
    { echo "bootstrap did not restore s5n-selfsigned as a CA issuer"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s || fail=1
  serial_after_upgrade=$(tls_serial "$cert" || true)
  [ -n "$serial_after_upgrade" ] && [ "$serial_after_upgrade" != "$serial_old_shape" ] ||
    { echo "the host certificate was not re-issued after the upgrade (still ${serial_after_upgrade})"; fail=1; }
  same_cert "$($K -n s5n-gateway get secret "$cert" -o jsonpath='{.data.ca\.crt}' | base64 -d || true)" "$root_crt" ||
    { echo "the re-issued certificate does not chain to the box root"; fail=1; }
  echo "  upgraded serial: ${serial_old_shape} -> ${serial_after_upgrade}, chained to s5n-root-ca"

  # ---- the no-DNS card, then the move to an issued certificate --------------
  # Run 1 had no DNS answers at all. init installed self-signed certificates
  # for the domain and for each app host, and its final card said, per host,
  # that the name has no record and that a re-run is what moves it. This is the
  # state a box with a made-up name is in, and it is not a failure.
  log "the no-DNS final card names every host and its record"
  cert=s5n-tls-box-internal-example
  app_cert=s5n-tls-seerr-box-internal-example
  grep -qF "${DOMAIN} has no DNS record, so its certificate is self-signed; add one, then run box init again to move it to Let's Encrypt" "$WORK/bootstrap1.txt" ||
    { echo "the final card does not name the domain's missing record"; fail=1; }
  for host in "$PLEX_HOST" "$SEERR_HOST"; do
    grep -qF "${host} has no DNS record, so its certificate is self-signed; add either a wildcard *.${DOMAIN} or a record for ${host}, then run box init again to move it to Let's Encrypt" "$WORK/bootstrap1.txt" ||
      { echo "the final card does not name ${host}'s missing record"; fail=1; }
  done
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$app_cert" --timeout=120s ||
    { echo "the app host's self-signed Certificate never became Ready"; fail=1; }
  [ "$($K -n s5n-gateway get certificate "$app_cert" -o jsonpath='{.spec.issuerRef.name}')" = s5n-selfsigned ] ||
    { echo "the app host's Certificate is not self-signed on a box with no records"; fail=1; }
  old_serial=$(tls_serial "$cert")
  old_app_serial=$(tls_serial "$app_cert")
  echo "  self-signed serials: ${DOMAIN} ${old_serial}, ${SEERR_HOST} ${old_app_serial}"

  # The records exist now. --dns-file is the test's own answer for the host's
  # resolver: the same file for every name, a public address, so init's rule
  # (public answer -> Let's Encrypt) has something real to decide on. Pebble
  # and cert-manager resolve the names through CoreDNS, which points at the
  # Gateway, so the HTTP-01 challenge still arrives on port 80.
  log "a record exists: init again moves the same hosts to Let's Encrypt"
  cat >"$WORK/dns" <<EOF
192.0.2.10 ${DOMAIN} ${PLEX_HOST} ${SEERR_HOST}
EOF
  install_pebble
  [ "$fail" = 0 ] || return 0
  patch_coredns "$lb_addr" "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" ||
    { echo "could not patch CoreDNS for the box's names"; fail=1; }
  ACME_ARGS=(--acme-server "https://pebble.pebble.svc:14000/dir")
  DNS_ARGS=(--dns-file "$WORK/dns")
  log "box bootstrap: the names answer now, so the same certificates are issued"
  if bootstrap "$WORK/bootstrap3.txt"; then
    cat "$WORK/bootstrap3.txt"
  else
    cat "$WORK/bootstrap3.txt"; fail=1
  fi
  for host in "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST"; do
    grep -qF "tls: ${host} resolves to 192.0.2.10: using Let's Encrypt" "$WORK/bootstrap3.txt" ||
      { echo "init did not move ${host} to Let's Encrypt"; fail=1; }
  done
  if grep -q "has no DNS record" "$WORK/bootstrap3.txt"; then
    echo "the card still says a name has no record after the record was added"
    fail=1
  fi
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$cert" --timeout=180s ||
    { echo "the domain's Certificate never became Ready against Pebble"; fail=1; }
  $K -n s5n-gateway wait --for=condition=Ready "certificate/$app_cert" --timeout=180s ||
    { echo "the app host's Certificate never became Ready against Pebble"; fail=1; }
  for name in "$cert" "$app_cert"; do
    got=$($K -n s5n-gateway get certificate "$name" -o jsonpath='{.spec.issuerRef.name}')
    [ "$got" = s5n-letsencrypt ] || { echo "${name} still names issuer ${got}"; fail=1; }
  done
  new_serial=$(tls_serial "$cert")
  new_app_serial=$(tls_serial "$app_cert")
  echo "  issued serials: ${DOMAIN} ${new_serial}, ${SEERR_HOST} ${new_app_serial}"
  [ -n "$new_serial" ] && [ "$new_serial" != "$old_serial" ] ||
    { echo "the domain's certificate was not re-issued"; fail=1; }
  [ -n "$new_app_serial" ] && [ "$new_app_serial" != "$old_app_serial" ] ||
    { echo "the app host's certificate was not re-issued"; fail=1; }

  # The host is publicly trusted now, so there is nothing for a pod to add:
  # root's run removed the anchor it wrote while the certificate was box's own.
  if $K -n "$NS" get configmap s5n-host-ca >/dev/null 2>&1; then
    echo "s5n-host-ca is still in ${NS} after the move to Let's Encrypt"; fail=1
  else
    echo "  s5n-host-ca removed: a public root needs no anchor"
  fi

  # --ca-file is the same writer with another source, for the one mode with
  # nothing to copy: an ACME certificate has no ca.crt at any point. The e2e
  # hands box Pebble's root on init -- the run that writes the host's
  # Certificate, and so the run the anchor follows -- and asserts the ConfigMap,
  # then an init without the flag takes it away again, so the flag feeds the
  # product path instead of adding a test-only one. The scenario passes
  # --ca-file nowhere else: the move above proves the removal, this the file.
  log "box init --ca-file: an ACME host's anchor comes from the file"
  if init "$WORK/init-ca.txt" --ca-file "$WORK/pebble-root.pem"; then
    grep -qF "wrote ConfigMap ${NS}/s5n-host-ca" "$WORK/init-ca.txt" ||
      { echo "box did not report writing the anchor from --ca-file"; cat "$WORK/init-ca.txt"; fail=1; }
  else
    cat "$WORK/init-ca.txt"; fail=1
  fi
  same_cert "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)" \
    "$(cat "$WORK/pebble-root.pem")" ||
    { echo "--ca-file did not reach ${NS}/s5n-host-ca"; fail=1; }
  log "box init: an ACME host's anchor goes again"
  if init "$WORK/init-noca.txt"; then :; else cat "$WORK/init-noca.txt"; fail=1; fi
  if $K -n "$NS" get configmap s5n-host-ca >/dev/null 2>&1; then
    echo "the anchor survived an init without --ca-file on an ACME host"; fail=1
  fi
  https_code=$(https_get "$lb_addr" /sonarr/ping "$WORK/pebble-root.pem")
  echo "  HTTPS ${DOMAIN}/sonarr/ping with Pebble's CA: ${https_code}"
  [ "$https_code" = 200 ] || { echo "HTTPS did not answer 200 with Pebble's CA after the move"; fail=1; }
  seerr_https=$(curl -sS -L -o /dev/null -w '%{http_code}' \
    --resolve "${SEERR_HOST}:443:${lb_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${SEERR_HOST}/" 2>/dev/null || true)
  echo "  HTTPS ${SEERR_HOST}/ with Pebble's CA (following Seerr's redirect): ${seerr_https}"
  [ "$seerr_https" = 200 ] || { echo "the app host did not serve the issued certificate"; fail=1; }
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
