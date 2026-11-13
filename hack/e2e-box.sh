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
# The OIDC clients are proved the same way, by signing in: the app's own start
# hands over an authorization URL, Authelia's authorization endpoint answers
# with a code, the app's callback exchanges it, and an endpoint only the app's
# session opens names the account. A jar with no cookie, one with the portal's
# cookie alone, and a second account are the refusals around it.
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
# one, and the e2e's password is generated per run below. The second one exists
# only to be refused: no rule and no client names it.
LOGIN=alice
SECOND_LOGIN=bob
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
# PeerBanHelper's page calls /api/... from the site root, so the catalog serves it
# at a host of its own as well.
PBH_HOST="pbh.${DOMAIN}"
# The fresh scenario points the production issuer at Pebble; every other
# scenario keeps Let's Encrypt's directory and never issues from it.
ACME_ARGS=()
# The development-only --dns-file override: the e2e's stand-in for the test
# host's own DNS, which an unprivileged run cannot edit. Empty for every
# scenario until the one that moves a name from "no record" to a public one.
DNS_ARGS=()
# The development-only --ca-file override: Pebble's root, which an ACME
# certificate's Secret does not carry, so the anchor ConfigMap box writes for an
# app that signs in through the login has nowhere else to come from. This is the
# same writer as a self-signed box's (which reads the box root out of
# cert-manager), not a second code path.
CA_ARGS=()
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
# The torrent port the grant records, as the last assert_torrent_port read it:
# the binding below admits exactly that number.
TORRENT_PORT=""

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
#
# A second account exists for the refusal: the provider's authorization policy
# is one per seedbox and admits user:<LOGIN> alone, so a second row is what
# proves a client cannot be authorized by someone the box did not name. It
# never gets a listener, a rule or a client of its own.
AUTHELIA_PASSWORD=$(openssl rand -hex 16)
printf '%s\n' "$AUTHELIA_PASSWORD" >"$WORK/authelia-password"
chmod 600 "$WORK/authelia-password"
SECOND_PASSWORD=$(openssl rand -hex 16)
printf '%s\n' "$SECOND_PASSWORD" >"$WORK/second-password"
chmod 600 "$WORK/second-password"

# Only what `box init` creates on a real host, and only because --paths-in-node
# keeps it from touching this node itself: the two volume roots and one
# /config/<instance> directory per enabled app (setup.settingsDirs). The data
# categories are deliberately absent — downloads/<category> and media/<category>
# are the wiring Job's, and pre-creating them would hide a first install that
# cannot make the root folders its Arr steps need.
log "preparing host paths inside the node (what box init creates on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c '
  mkdir -p /srv/s5n/data \
           /srv/s5n/config/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,plex,tautulli,seerr,qui,autobrr,maintainerr,pbh}
  chown -R 1000:1000 /srv/s5n'

# The login's own directory, which box init creates on a real host: 0700, owned
# by the uid the container runs as (65532). The users database goes in before
# the install because Authelia will not start without it, and box cannot write
# into the node -- hack/authelia-users writes the same file from the same
# package, so the row the login verifies is the row box would have written.
log "seeding the login's state directory in the node (0700, uid 65532)"
go run ./hack/authelia-users --login "$LOGIN" --password-file "$WORK/authelia-password" \
  --login "$SECOND_LOGIN" --password-file "$WORK/second-password" --out "$WORK/users_database.yml"
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
  "$WORK/box" bootstrap --yes --chart-repo "$CHART_REPO" ${ACME_ARGS[@]+"${ACME_ARGS[@]}"} ${DNS_ARGS[@]+"${DNS_ARGS[@]}"} ${CA_ARGS[@]+"${CA_ARGS[@]}"} \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --namespace "$NS" --domain "$DOMAIN" \
    --data-path /srv/s5n/data --config-path /srv/s5n/config \
    --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr,qui,autobrr,maintainerr,pbh \
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
  "$WORK/box" init --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" ${DNS_ARGS[@]+"${DNS_ARGS[@]}"} ${CA_ARGS[@]+"${CA_ARGS[@]}"} \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --namespace "$NS" --domain "$DOMAIN" \
    --data-path /srv/s5n/data --config-path /srv/s5n/config \
    --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr,qui,autobrr,maintainerr,pbh \
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

# ---- the apps that own their user table --------------------------------------
# qui and autobrr hand out an API key only to a signed-in session, so the chart
# generates a password in the release Secret and each app's init container
# writes it into the app on every start; the wiring Job signs in with it. What
# that account did is the receipt, but a receipt is the Job's own word, so these
# read the apps back through their own APIs with the Secret's password -- which
# is also the proof that a local account and the edge's OIDC provider coexist.

# port_forward opens an API-server tunnel to one app pod and leaves it in
# PF_PORT (and its process in PF_PID). This is the only way to ask these apps
# anything directly: their ingress policy admits the Gateway and the wiring Job
# alone, and the Job's image is distroless (no shell to drive a client with).
# kubelet's own connection is not a pod's, so no policy applies to it -- and the
# tunnel arrives on 127.0.0.1, which curl treats as a secure context, so qui's
# Secure session cookie is replayed here exactly as the Job replays it.
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

# assert_torrent_port <step> [the port a re-run must still have]
#
# box picks the torrent port at init from the band the API server does not
# assign by itself (Kubernetes' ServiceNodePortStaticSubrange: dynamic
# allocation starts at the top of the range and only spills into this band once
# that is full), records it in the grant, and the chart hands the same number to
# the peers Service and to qBittorrent. A fixed port in the upper part of the
# range -- the chart's own 31111 -- is one any Service asking for "any free
# port" can win first, and then every apply of this seedbox fails on the peers
# Service. So the four things asserted here are: the grant has a port in the
# band, the Service forwards it, nothing else holds it, and the app announces it.
assert_torrent_port() { # assert_torrent_port <step> [expected]
  local step=$1 want=${2:-} port svc_port listen_port others password code
  port=$(python3 - "$WORK/grant.yaml" <<'PY'
import sys, yaml
grant = yaml.safe_load(open(sys.argv[1])) or {}
torrent = (grant.get("apps") or {}).get("qbittorrent", {}).get("torrent") or {}
print(torrent.get("port", ""))
PY
  ) || true
  echo "  ${step}: the grant's torrent port is ${port:-absent}"
  case "$port" in
    ''|*[!0-9]*) echo "the grant records no torrent port"; fail=1; return 0 ;;
  esac
  if [ "$port" -lt 30000 ] || [ "$port" -gt 30085 ]; then
    echo "the torrent port ${port} is outside the band box picks from (30000-30085)"; fail=1
  fi
  if [ -n "$want" ] && [ "$port" != "$want" ]; then
    echo "the torrent port moved from ${want} to ${port}"; fail=1
  fi
  TORRENT_PORT=$port
  svc_port=$($K -n "$NS" get service qbittorrent-peers -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)
  echo "  ${step}: the peers Service is on ${svc_port}"
  [ "$svc_port" = "$port" ] ||
    { echo "the peers Service is on ${svc_port}, the grant says ${port}"; fail=1; }
  # Nothing else in the cluster holds it: that is what "free when box picked
  # it" means, and the collision this whole decision exists for.
  others=$($K get services --all-namespaces -o json | python3 -c '
import json, sys
port, ns, name = sys.argv[1], sys.argv[2], sys.argv[3]
for svc in json.load(sys.stdin)["items"]:
    meta = svc["metadata"]
    if meta["namespace"] == ns and meta["name"] == name:
        continue
    if any(str(p.get("nodePort", "")) == port for p in svc["spec"].get("ports") or []):
        print(meta["namespace"] + "/" + meta["name"])
' "$port" "$NS" qbittorrent-peers || true)
  [ -z "$others" ] ||
    { echo "the torrent port ${port} is also held by ${others}"; fail=1; }
  # And the app itself announces it. The chart hands the value to both sides,
  # but only qBittorrent's own answer proves it took: its listen port is the
  # number peers dial, and the Service is only pointing at it.
  password=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' 2>/dev/null | base64 -d || true)
  if [ -z "$password" ]; then
    echo "the release Secret holds no qBittorrent password"; fail=1; return 0
  fi
  port_forward qbittorrent-0 8080 || { fail=1; return 0; }
  : >"$WORK/qbt-port.jar"
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -c "$WORK/qbt-port.jar" \
    --data-urlencode "username=admin" --data-urlencode "password=${password}" \
    "http://127.0.0.1:${PF_PORT}/api/v2/auth/login" || true)
  listen_port=""
  # Any 2xx is a sign-in: qBittorrent 5 answers 204 with no body.
  case "$code" in
    2??)
      listen_port=$(curl -sS --max-time 15 -b "$WORK/qbt-port.jar" \
        "http://127.0.0.1:${PF_PORT}/api/v2/app/preferences" 2>/dev/null |
        python3 -c 'import json,sys; print(json.load(sys.stdin).get("listen_port",""))' 2>/dev/null || true)
      ;;
    *)
      echo "qBittorrent's API answered ${code:-nothing} to its own password"
      ;;
  esac
  port_forward_stop
  echo "  ${step}: qBittorrent listens on ${listen_port:-unknown}"
  [ "$listen_port" = "$port" ] ||
    { echo "qBittorrent listens on ${listen_port:-unknown}, the grant says ${port}"; fail=1; }
}

# qui_session signs in over the tunnel and leaves the code in QUI_LOGIN_CODE and
# the raw cookie in QUI_COOKIE. The cookie is captured from Set-Cookie because
# qui's is Secure: a jar would refuse it over this http:// socket.
qui_session() { # qui_session <base-url>
  local base=$1
  : >"$WORK/qui-headers"
  QUI_LOGIN_CODE=$(curl -sS -o "$WORK/qui-login.json" -D "$WORK/qui-headers" -w '%{http_code}' \
    -X POST "http://127.0.0.1:${PF_PORT}${base}/api/auth/login" \
    -H 'Content-Type: application/json' \
    --data "{\"username\":\"${QUI_USER}\",\"password\":\"${QUI_PASSWORD}\"}" 2>/dev/null || true)
  QUI_COOKIE=$(sed -n 's/^[Ss]et-[Cc]ookie: *\([^;]*\);.*/\1/p' "$WORK/qui-headers" | head -1 || true)
  QUI_LOGIN_BODY=$(head -c 300 "$WORK/qui-login.json" 2>/dev/null || true)
}

# qui_change_password turns a fresh session into a password change, the way a
# person would do it in qui's settings: the old password proves the session, the
# new one is what the app stores until its pod restarts. qui routes this as a
# PUT (internal/api/server.go:502), not a POST.
qui_change_password() { # qui_change_password <base-url> <current> <new>
  local base=$1 current=$2 next=$3
  curl -sS -o "$WORK/qui-change.json" -w '%{http_code}' \
    -X PUT "http://127.0.0.1:${PF_PORT}${base}/api/auth/change-password" \
    -H "Cookie: ${QUI_COOKIE}" -H 'X-Requested-With: XMLHttpRequest' \
    -H 'Content-Type: application/json' \
    --data "{\"currentPassword\":\"${current}\",\"newPassword\":\"${next}\"}" 2>/dev/null || true
}

# autobrr_session signs in over the tunnel with a cookie jar (autobrr's cookie
# is not Secure) and leaves the code in AUTOBRR_LOGIN_CODE.
autobrr_session() { # autobrr_session <base-url> <jar>
  local base=$1 jar=$2
  AUTOBRR_LOGIN_CODE=$(curl -sS -c "$jar" -o /dev/null -w '%{http_code}' \
    -X POST "http://127.0.0.1:${PF_PORT}${base}/api/auth/login" \
    -H 'Content-Type: application/json' \
    --data "{\"username\":\"${AUTOBRR_USER}\",\"password\":\"${AUTOBRR_PASSWORD}\"}" 2>/dev/null || true)
}

# rerun_wiring runs the release's wiring Job again without moving the release:
# the hook Job's own spec, under a name of its own. A helm upgrade would do the
# same thing but a failed hook leaves the release "failed", which is what the
# changed-password case is about to make it -- and box refuses to touch a
# release in that state, which would hide the rest of the run behind it.
rerun_wiring() { # rerun_wiring <output file>
  $K -n "$NS" delete job s5n-wiring-rerun --ignore-not-found >/dev/null 2>&1 || true
  $K -n "$NS" get job s5n-wiring -o json |
    python3 -c 'import json, sys
job = json.loads(sys.stdin.read())
meta = {"name": "s5n-wiring-rerun", "namespace": job["metadata"]["namespace"]}
if job["metadata"].get("labels"):
    meta["labels"] = job["metadata"]["labels"]
out = {"apiVersion": "batch/v1", "kind": "Job", "metadata": meta, "spec": job["spec"]}
# The selector belongs to the controller: a Job created without one has it
# filled in, and a copied one would pin the pod template hash it came from.
out["spec"].pop("selector", None)
# The controller writes its own identity into the pod template labels, and a
# Job refuses a template that carries another Job identity.
for k in ("batch.kubernetes.io/controller-uid", "controller-uid",
          "batch.kubernetes.io/job-name", "job-name"):
    out["spec"]["template"]["metadata"]["labels"].pop(k, None)
print(json.dumps(out))' 2>/dev/null >"$1.json" || true
  if ! $K -n "$NS" apply -f "$1.json" >>"$1" 2>&1; then
    echo "could not create the re-run Job:"; tail -5 "$1"
    return 1
  fi
  # Complete *or* Failed: the changed-password case is deliberately a failing
  # run, and `kubectl wait --for=condition=complete` would sit there for its
  # whole timeout waiting for a condition that is never coming.
  local i state
  for i in $(seq 1 150); do
    state=$($K -n "$NS" get job s5n-wiring-rerun \
      -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}{.status.conditions[?(@.type=="Failed")].status}' \
      2>/dev/null || true)
    case "$state" in
      True* | *True) break ;;
    esac
    sleep 2
  done
  echo "s5n-wiring-rerun finished: ${state:-no condition}" >>"$1"
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

# ---- the OIDC provider ------------------------------------------------------
# An app that signs in through the login dials the issuer itself, and the issuer
# is the URL the browser used: https://<host>/auth. That name resolves nowhere
# inside a cluster, and the certificate is the box's, so three things have to be
# right at once -- the pod's alias for the name, the trust root it verifies with,
# and a NetworkPolicy that lets it reach the proxy.
#
# What a client does when one of those is wrong differs, and it is worth knowing
# which proof you have. qui refuses to start until it can read the discovery
# document, so Ready is the three of them together. autobrr starts and retries
# discovery for about four minutes in the background (50 attempts, five seconds
# apart: internal/auth/oidc.go at v1.90.0), so its Ready says nothing about the
# issuer -- the sign-in e2e does.
#
# The client secret is one Secret in this namespace, written once by root; the
# login keeps only its digest. Both are asserted here against each other.
assert_oidc() { # assert_oidc <domain> <app> <environment prefix>
  local domain=$1 app=$2 prefix=$3
  local secret_name proxy_ip alias_ip alias_host issuer client redirect ssl_dir disc
  local grant_ip cm_digest stored_digest plaintext policy provider has_hmac anchor eg_port
  # The app's own readiness is the alias, the trust root and the egress rule
  # proved together -- for the client that will not start without them, and as
  # a first sign that nothing is wrong for the one that starts anyway.
  $K -n "$NS" wait --for=condition=Ready "pod/${app}-0" --timeout=300s ||
    { echo "${app}-0 never became Ready"; fail=1; }
  secret_name=$($K -n "$NS" get statefulset "$app" \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="oidc")].secret.secretName}' 2>/dev/null || true)
  echo "  ${app} mounts client secret: ${secret_name:-nothing}"
  [ "$secret_name" = "s5n-oidc-${app}" ] ||
    { echo "${app} does not mount the client secret box wrote"; fail=1; }

  proxy_ip=$($K -n envoy-gateway-system get service \
    -l 'app.kubernetes.io/name=envoy,gateway.envoyproxy.io/owning-gateway-name=s5n,gateway.envoyproxy.io/owning-gateway-namespace=s5n-gateway' \
    -o jsonpath='{.items[0].spec.clusterIP}' 2>/dev/null || true)
  alias_ip=$($K -n "$NS" get statefulset "$app" -o jsonpath='{.spec.template.spec.hostAliases[0].ip}' 2>/dev/null || true)
  alias_host=$($K -n "$NS" get statefulset "$app" -o jsonpath='{.spec.template.spec.hostAliases[0].hostnames[0]}' 2>/dev/null || true)
  echo "  ${app} alias: ${alias_host} -> ${alias_ip} (proxy Service ${proxy_ip})"
  [ -n "$proxy_ip" ] || { echo "the Gateway's proxy Service has no ClusterIP to alias"; fail=1; }
  [ "$alias_ip" = "$proxy_ip" ] ||
    { echo "${app} aliases ${alias_ip}, want the proxy Service's ${proxy_ip}"; fail=1; }
  [ "$alias_host" = "$domain" ] ||
    { echo "${app} aliases ${alias_host}, want the box's own name ${domain}"; fail=1; }

  # box wrote the address into the grant at init: a user's apply renders it and
  # never reads the Gateway's namespace.
  grant_ip=$(sed -n 's/^ *gatewayIP: *//p' "$WORK/grant.yaml" | head -1 || true)
  echo "  grant's gatewayIP: ${grant_ip:-absent}"
  [ "$grant_ip" = "$proxy_ip" ] ||
    { echo "the grant records ${grant_ip:-no address}, want the proxy Service's ${proxy_ip}"; fail=1; }

  issuer=$($K -n "$NS" get statefulset "$app" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"${prefix}__OIDC_ISSUER\")].value}" 2>/dev/null || true)
  client=$($K -n "$NS" get statefulset "$app" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"${prefix}__OIDC_CLIENT_ID\")].value}" 2>/dev/null || true)
  redirect=$($K -n "$NS" get statefulset "$app" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"${prefix}__OIDC_REDIRECT_URL\")].value}" 2>/dev/null || true)
  ssl_dir=$($K -n "$NS" get statefulset "$app" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SSL_CERT_DIR")].value}' 2>/dev/null || true)
  echo "  ${app}: issuer ${issuer}, client ${client}, redirect ${redirect}, SSL_CERT_DIR ${ssl_dir}"
  [ "$issuer" = "https://${domain}/auth" ] || { echo "${app}'s issuer is ${issuer}"; fail=1; }
  [ "$client" = "${NS}-${app}" ] || { echo "${app}'s client id is ${client}"; fail=1; }
  [ "$redirect" = "https://${domain}/${app}/api/auth/oidc/callback" ] ||
    { echo "${app}'s redirect URI is ${redirect}: it must be the callback path it builds from its base URL"; fail=1; }
  [ "$ssl_dir" = /etc/s5n-host-ca ] ||
    { echo "SSL_CERT_DIR is ${ssl_dir}: the anchor is added to the bundle, never substituted for it"; fail=1; }

  # The two halves agree: the client's digest in the login's configuration is the
  # one box stored beside the signing material, and the plaintext is in this
  # namespace. The digest is looked up by client id, because a box with two
  # clients has two of them in the same file.
  policy=$($K -n s5n-system get configmap s5n-authelia -o jsonpath='{.data.configuration\.yml}' 2>/dev/null || true)
  cm_digest=$(printf '%s' "$policy" | python3 -c 'import sys, yaml
conf = yaml.safe_load(sys.stdin.read()) or {}
for c in conf.get("identity_providers", {}).get("oidc", {}).get("clients", []):
    if c.get("client_id") == sys.argv[1]:
        print(c.get("client_secret", ""))
' "${NS}-${app}" || true)
  stored_digest=$($K -n s5n-system get secret s5n-authelia -o jsonpath="{.data.oidc-client-${NS}-${app}}" 2>/dev/null | base64 -d || true)
  plaintext=$($K -n "$NS" get secret "s5n-oidc-${app}" -o jsonpath='{.data.client-secret}' 2>/dev/null | base64 -d || true)
  echo "  ${app} client secret: ${#plaintext} characters in ${NS}/s5n-oidc-${app}, digest ${cm_digest:0:24}..."
  case "$cm_digest" in
    '$pbkdf2-sha512$310000$'*) : ;;
    *) echo "the login's client_secret for ${app} is not a pbkdf2-sha512 digest: ${cm_digest}"; fail=1 ;;
  esac
  [ "$cm_digest" = "$stored_digest" ] ||
    { echo "the configuration's digest and the one stored in the login's Secret differ for ${app}"; fail=1; }
  [ "${#plaintext}" -ge 40 ] ||
    { echo "the client secret in ${NS} is ${#plaintext} characters, want the generated 48"; fail=1; }

  # The signing material is a file in the same Secret, and the configuration
  # does not carry it.
  provider=$($K -n s5n-system get secret s5n-authelia -o jsonpath='{.data.oidc\.yml}' 2>/dev/null | base64 -d || true)
  has_hmac=$(printf '%s' "$provider" | grep -c "hmac_secret:" || true)
  echo "  provider file: ${#provider} bytes, hmac_secret lines ${has_hmac}"
  [ "$has_hmac" = 1 ] || { echo "the login's Secret carries no oidc.yml with an hmac_secret"; fail=1; }
  case "$provider" in
    *"BEGIN RSA PRIVATE KEY"*) : ;;
    *) echo "the login's Secret carries no RSA signing key"; fail=1 ;;
  esac

  # The policy is this seedbox's: the client points at it, and it admits only
  # this box's account.
  printf '%s' "$policy" | grep -q "authorization_policy: '${NS}'" ||
    { echo "${app}'s client does not name this box's authorization policy"; fail=1; }
  printf '%s' "$policy" | grep -q "user:${LOGIN}" ||
    { echo "the policy admits no ${LOGIN}: the rule and the login's account disagree"; fail=1; }

  # The trust anchor, when the box's certificate is not publicly trusted: box
  # copied the root into this namespace, and the pod mounts it.
  anchor=$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)
  if [ -n "$anchor" ]; then
    printf '%s' "$anchor" | grep -q "BEGIN CERTIFICATE" ||
      { echo "the anchor ConfigMap carries no certificate"; fail=1; }
    printf '%s' "$anchor" | grep -q "PRIVATE KEY" &&
      { echo "the anchor ConfigMap carries a private key"; fail=1; }
    echo "  anchor: $(printf '%s' "$anchor" | grep -c 'BEGIN CERTIFICATE') certificate(s) in ${NS}/s5n-host-ca"
  else
    echo "  anchor: none, and the box's certificate is publicly trusted"
  fi

  # And the receipt: discovery, from inside the app's own pod, over the box's
  # own name, trusted by exactly what that pod was given.
  #
  # curl here is OpenSSL, and OpenSSL's directory lookup only finds certificates
  # under their subject-hash names (<hash>.0) -- not ca.crt. So SSL_CERT_DIR,
  # which the pod inherits and which is exactly right for a Go client (it reads
  # every file in it), trusts nothing in curl: point it at the file instead. A
  # Let's Encrypt box has no anchor and curl's system bundle is what trusts the
  # issuer.
  local ca_args=()
  if [ -n "$anchor" ]; then
    ca_args=(--cacert /etc/s5n-host-ca/ca.crt)
  fi
  disc=$($K -n "$NS" exec "${app}-0" -- curl -sS --max-time 15 "${ca_args[@]}" \
    "https://${domain}/auth/.well-known/openid-configuration" 2>"$WORK/oidc-discovery.err" || true)
  if [ -z "$disc" ]; then
    echo "${app}'s pod got no discovery document from https://${domain}/auth: the alias, the anchor or the egress rule is wrong"
    sed 's/^/    curl: /' "$WORK/oidc-discovery.err" 2>/dev/null || true
    fail=1
  else
    printf '%s' "$disc" >"$WORK/oidc-discovery.json"
    if python3 -c 'import json, sys
domain, path = sys.argv[1], sys.argv[2]
d = json.load(open(path))
want = "https://%s/auth" % domain
if d.get("issuer") != want:
    print("  discovery issuer is %r, want %r" % (d.get("issuer"), want)); sys.exit(1)
for key in ("authorization_endpoint", "token_endpoint", "jwks_uri", "userinfo_endpoint"):
    if not d.get(key, "").startswith(want + "/"):
        print("  %s is %r, want a URL under %r" % (key, d.get(key), want)); sys.exit(1)
print("  discovery: issuer %s, token %s, jwks %s" % (d["issuer"], d["token_endpoint"], d["jwks_uri"]))' "$domain" "$WORK/oidc-discovery.json"; then
      :
    else
      echo "the discovery document is not the one this box's issuer advertises"
      fail=1
    fi
  fi

  # The egress rule that lets it: the proxy's pods, on the container port Envoy
  # listens on. 443 here would be a rule that admits nothing (NetworkPolicy is
  # evaluated after the Service's DNAT).
  eg_port=$($K -n "$NS" get networkpolicy "${app}-oidc-egress" -o jsonpath='{.spec.egress[0].ports[0].port}' 2>/dev/null || true)
  echo "  ${app}-oidc-egress port: ${eg_port:-missing}"
  [ "$eg_port" = 10443 ] || { echo "the ${app} OIDC egress rule names ${eg_port:-no port}, want 10443"; fail=1; }
}

# ---- an app's own sign-in: the OIDC round trip, without a browser ----------
# An app the catalog marks `oidc: true` is a client of the login's own
# provider, and the round trip belongs to the app: it hands the browser an
# authorization URL and takes the code back on a callback of its own. The
# app's own OIDC routes are behind the edge's login like every other path on
# the host, so a person's visit is two sign-ins that look like one -- the
# portal first, then the app's silent round trip.
#
# This is that visit, with a cookie jar:
#
#   1. the app's page without a cookie: the edge redirects to the portal;
#   2. the portal's first factor, with the target the edge passed;
#   3. the app's own start, GET <base>/api/auth/oidc/config, whose JSON carries
#      the authorization URL and which sets the app's state in its session;
#   4. Authelia's authorization endpoint, and the code it answers with;
#   5. the callback, which exchanges the code and sets the app's session;
#   6. an endpoint only that session opens -- and the same endpoint refused
#      without it, so a 200 cannot come from the edge's login alone.
#
# Authelia is asked twice, because the flow has two arms and a browser meets
# both. With the portal's cookie, the authorization endpoint answers at once:
# the client's consent is implicit, so no consent page appears, and the code
# comes back in the redirect that follows. With that one cookie gone -- a
# browser whose portal session expired while the app page was open -- it
# answers 303 to its own portal carrying flow=openid_connect&flow_id=<id>, and
# the first factor is asked with that flow id (internal/handlers/types.go:111,
# Flow/FlowID; response.go:155-313 is what answers with the URL that resumes
# the flow). Neither arm is a reachability check.
#
# OIDC_JAR is the signed-in jar, for a caller that wants one more thing from
# the app without signing in again.

# portal_login: one password login at the portal, the way the browser's form
# does it. <jar> <login> <password> <target url> <address> <ca-file|->; sets
# PORTAL_CODE and PORTAL_BODY.
portal_login() {
  local jar=$1 login=$2 password=$3 target=$4 addr=$5 ca=$6
  local ca_args=(--cacert "$ca") body
  [ "$ca" = - ] && ca_args=(-k)
  body=$(python3 -c 'import json, sys
print(json.dumps({"username": sys.argv[1], "password": sys.argv[2], "targetURL": sys.argv[3]}))' \
    "$login" "$password" "$target")
  PORTAL_CODE=$(curl -sS -o "$WORK/portal-login.json" -w '%{http_code}' \
    -b "$jar" -c "$jar" -X POST "https://${DOMAIN}/auth/api/firstfactor" \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    -H 'Content-Type: application/json' --data "$body" 2>"$WORK/portal-login.err" || true)
  PORTAL_BODY=$(cat "$WORK/portal-login.json" 2>/dev/null || true)
}

# json_field: one top-level field of a JSON document in a file, or "" -- a body
# that is not JSON (an HTML error page) answers "" rather than failing the run
# twice, and an empty value is an assertion the caller makes.
json_field() { # json_field <file> <dotted.path>
  python3 -c 'import json, sys
try:
    doc = json.load(open(sys.argv[1], encoding="utf-8"))
    for part in sys.argv[2].split("."):
        doc = doc[part]
    print(doc if isinstance(doc, str) else "")
except Exception:
    print("")' "$1" "$2" 2>/dev/null || true
}

# oidc_login <app> <base path> <api path> <address> <ca-file|->
#
# The app's whole sign-in as one function, so a second client is one call: the
# paths differ by the app's base and the endpoint it protects, and nothing else
# does. It asserts as it goes; fail=1 is the e2e's.
oidc_login() {
  local app=$1 base=$2 api=$3 addr=$4 ca=$5
  local ca_args=(--cacert "$ca")
  [ "$ca" = - ] && ca_args=(-k)
  local jar="$WORK/oidc-${app}-jar.txt"
  local expired="$WORK/oidc-${app}-expired.txt"
  local edge="$WORK/oidc-${app}-edge.txt"
  local hdrs="$WORK/oidc-${app}-headers.txt"
  local url="https://${DOMAIN}${base}"
  local code location rd auth_url flow_id redirect callback body
  OIDC_JAR=$jar
  rm -f "$jar" "$expired" "$edge"

  # 1. The app's page, no cookie: the edge sends the browser to the portal, and
  #    the portal carries where to come back to.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "$url/" 2>"$WORK/oidc.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  ${app}: GET ${url}/ without a cookie: ${code} -> ${location}"
  [ "$code" = 302 ] || { echo "the unauthenticated ${base}/ answered ${code}, not the portal's redirect"; fail=1; }
  case "$location" in
    "https://${DOMAIN}/auth/"*) : ;;
    *) echo "the redirect does not point at the portal: ${location}"; fail=1 ;;
  esac
  rd=$(printf '%s' "$location" | python3 -c 'import sys, urllib.parse as u
print(u.parse_qs(u.urlparse(sys.stdin.read()).query).get("rd", [""])[0])' || true)
  # A portal redirect always carries rd; the app's own page is where init's
  # target would be if one ever did not.
  [ -n "$rd" ] || rd="https://${DOMAIN}${base}/"

  # 2. The portal, with the password the run generated.
  portal_login "$jar" "$LOGIN" "$AUTHELIA_PASSWORD" "$rd" "$addr" "$ca"
  echo "  ${app}: POST /auth/api/firstfactor: ${PORTAL_CODE}"
  [ "$PORTAL_CODE" = 200 ] || { echo "the portal refused the box's own account (${PORTAL_CODE})"; fail=1; }

  # 3. The app's own start. Its state lives in the app's session, so the same
  #    jar has to make the callback.
  code=$(curl -sS -o "$WORK/oidc-${app}-config.json" -w '%{http_code}' \
    -b "$jar" -c "$jar" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "$url/api/auth/oidc/config" 2>"$WORK/oidc.err" || true)
  body=$(cat "$WORK/oidc-${app}-config.json" 2>/dev/null || true)
  auth_url=$(json_field "$WORK/oidc-${app}-config.json" authorizationUrl)
  echo "  ${app}: GET ${base}/api/auth/oidc/config: ${code}, authorization URL ${auth_url%%\?*}"
  [ "$code" = 200 ] || { echo "the app's OIDC start answered ${code}"; fail=1; }
  case "$auth_url" in
    "https://${DOMAIN}/auth/"*) : ;;
    *) echo "the authorization URL is not this box's issuer: ${auth_url}"; fail=1 ;;
  esac

  # 4a. Signed in at the edge already: the authorization endpoint answers with
  #     the callback and a code, and nothing in between. A consent page here
  #     would be a page a person has to click, and the client's consent is
  #     implicit.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    -b "$jar" -c "$jar" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "$auth_url" 2>"$WORK/oidc.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  ${app}: the authorization endpoint, signed in: ${code} -> ${location%%\?*}"
  case "$code" in
    302 | 303) : ;;
    *) echo "the authorization endpoint answered ${code} to a signed-in request"; fail=1 ;;
  esac
  case "$location" in
    "https://${DOMAIN}${base}/api/auth/oidc/callback?"*) : ;;
    *) echo "the authorization endpoint sent the browser to ${location}, not the app's callback"; fail=1 ;;
  esac
  case "$location" in
    *"code="*) : ;;
    *) echo "the redirect to the callback carries no code"; fail=1 ;;
  esac

  # 4b. The other arm: the same authorization request from a browser whose
  #     portal session is gone. The app's session (its state) stays; only the
  #     login's cookie is dropped.
  cp "$jar" "$expired"
  python3 - "$expired" <<'PY'
import sys
path = sys.argv[1]
lines = [l for l in open(path, encoding="utf-8") if "authelia_session" not in l]
open(path, "w", encoding="utf-8").writelines(lines)
PY
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    -b "$expired" -c "$expired" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "$auth_url" 2>"$WORK/oidc.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  ${app}: the authorization endpoint without the portal's cookie: ${code} -> ${location}"
  [ "$code" = 303 ] ||
    { echo "an unauthenticated authorization request answered ${code}, want the portal's 303"; fail=1; }
  case "$location" in
    *"flow=openid_connect"*) : ;;
    *) echo "the portal's redirect carries no openid_connect flow: ${location}"; fail=1 ;;
  esac
  flow_id=$(printf '%s' "$location" | python3 -c 'import sys, urllib.parse as u
print(u.parse_qs(u.urlparse(sys.stdin.read()).query).get("flow_id", [""])[0])' || true)
  echo "  ${app}: portal flow id ${flow_id:-absent}"
  [ -n "$flow_id" ] || fail=1

  # The portal's first factor for that flow: the flow id is what makes the
  # answer a URL that resumes the OIDC request instead of the app's page.
  body=$(python3 -c 'import json, sys
print(json.dumps({"username": sys.argv[1], "password": sys.argv[2],
                  "flow": "openid_connect", "flowID": sys.argv[3]}))' \
    "$LOGIN" "$AUTHELIA_PASSWORD" "$flow_id")
  code=$(curl -sS -o "$WORK/oidc-${app}-flow.json" -w '%{http_code}' \
    -b "$expired" -c "$expired" -X POST "https://${DOMAIN}/auth/api/firstfactor" \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    -H 'Content-Type: application/json' --data "$body" 2>"$WORK/oidc.err" || true)
  redirect=$(json_field "$WORK/oidc-${app}-flow.json" data.redirect)
  echo "  ${app}: POST /auth/api/firstfactor with the flow id: ${code}, next ${redirect%%\?*}"
  [ "$code" = 200 ] || { echo "the portal refused the flow's first factor (${code})"; fail=1; }
  [ -n "$redirect" ] ||
    { echo "the portal's first factor answered with no next step: $(head -c 200 "$WORK/oidc-${app}-flow.json" 2>/dev/null)"; fail=1; }
  case "$redirect" in
    "https://${DOMAIN}/auth/api/oidc/authorization?"*) : ;;
    *) echo "the flow's next step is ${redirect}, not the authorization endpoint"; fail=1 ;;
  esac
  # A consent step would be the page implicit consent exists to remove.
  case "$redirect" in
    *"/oidc/consent"*) echo "the flow asked for consent: ${redirect}"; fail=1 ;;
  esac

  # 5. Resume the flow: the authorization endpoint issues the code, the app's
  #    callback redeems it at the token endpoint and opens a session.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    -b "$expired" -c "$expired" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "$redirect" 2>"$WORK/oidc.err" || true)
  callback=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  ${app}: the authorization endpoint, resumed: ${code} -> ${callback%%\?*}"
  case "$code" in
    302 | 303) : ;;
    *) echo "resuming the flow answered ${code}"; fail=1 ;;
  esac
  case "$callback" in
    "https://${DOMAIN}${base}/api/auth/oidc/callback?"*) : ;;
    *) echo "the resumed flow sent the browser to ${callback}, not the app's callback"; fail=1 ;;
  esac
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    -b "$expired" -c "$expired" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "$callback" 2>"$WORK/oidc.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  ${app}: GET the callback: ${code} -> ${location}"
  [ "$code" = 302 ] ||
    { echo "the callback answered ${code}: the code exchange did not complete"; fail=1; }
  # Where the app sends the browser after a successful exchange differs: qui
  # builds it from its redirect URL and answers an absolute one, autobrr
  # redirects to its own baseUrl, which is a path. Both are inside the app.
  case "$location" in
    "https://${DOMAIN}${base}"* | "${base}"*) : ;;
    *) echo "the callback sent the browser to ${location}, not back into the app"; fail=1 ;;
  esac

  # 6. An endpoint only the app's own session opens. It answers with the
  #    account the provider issued the token for, and with the method that
  #    proves the session came from OIDC and not from a password row.
  code=$(curl -sS -o "$WORK/oidc-${app}-me.json" -w '%{http_code}' \
    -b "$expired" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "https://${DOMAIN}${api}" 2>"$WORK/oidc.err" || true)
  body=$(cat "$WORK/oidc-${app}-me.json" 2>/dev/null || true)
  echo "  ${app}: GET ${api} with the session: ${code} ${body}"
  [ "$code" = 200 ] || { echo "${api} answered ${code} for a signed-in user"; fail=1; }
  if ! printf '%s' "$body" | python3 -c 'import json, sys
d = json.load(sys.stdin)
want = sys.argv[1]
bad = [f for f in ("username", "auth_method") if d.get(f) != {"username": want, "auth_method": "oidc"}[f]]
if bad:
    print("  the session says %s" % {f: d.get(f) for f in ("username", "auth_method")})
    sys.exit(1)' "$LOGIN"; then
    echo "${api} does not identify ${LOGIN} as an OIDC session"
    fail=1
  fi

  # And without that session: the edge's login alone is not the app's. This is
  # the assertion that keeps the 200 above from passing on the portal's cookie.
  portal_login "$edge" "$LOGIN" "$AUTHELIA_PASSWORD" "https://${DOMAIN}/" "$addr" "$ca"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -b "$edge" \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "https://${DOMAIN}${api}" \
    2>"$WORK/oidc.err" || true)
  echo "  ${app}: GET ${api} with the portal's cookie and no app session: ${code}"
  case "$code" in
    401 | 403) : ;;
    *) echo "${api} answered ${code} without the app's own session, want 401 or 403"; fail=1 ;;
  esac
  # No cookie at all is the edge's redirect, which is a different refusal: the
  # app is never reached.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "https://${DOMAIN}${api}" \
    2>"$WORK/oidc.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  ${app}: GET ${api} with no cookie: ${code} -> ${location%%\?*}"
  [ "$code" = 302 ] ||
    { echo "${api} answered ${code} with no cookie: the edge's login did not stand in front of it"; fail=1; }
}

# oidc_refuses_another_login <app> <base> <address> <ca-file|->
#
# The client's own authorization policy, which is per seedbox: it admits
# user:<login> and denies everything else. A second account signs in at the
# portal -- /auth is the bypass, so anyone in the users database gets that far
# -- and the authorization endpoint refuses it, before any code exists.
#
# The authorization URL is taken with the box's own account because the app's
# OIDC start sits behind the edge's login: a second account cannot read it, and
# what it can reach is the portal and the authorization endpoint, which is
# where the refusal is.
oidc_refuses_another_login() {
  local app=$1 base=$2 addr=$3 ca=$4
  local ca_args=(--cacert "$ca")
  [ "$ca" = - ] && ca_args=(-k)
  local jar="$WORK/oidc-${app}-other.txt"
  local hdrs="$WORK/oidc-${app}-other-headers.txt"
  local code auth_url location
  rm -f "$jar"
  auth_url=$(json_field "$WORK/oidc-${app}-config.json" authorizationUrl)
  [ -n "$auth_url" ] || { echo "no authorization URL to test the policy with"; fail=1; return 0; }
  portal_login "$jar" "$SECOND_LOGIN" "$SECOND_PASSWORD" "https://${DOMAIN}/" "$addr" "$ca"
  echo "  ${app}: ${SECOND_LOGIN} signs in at the portal: ${PORTAL_CODE}"
  [ "$PORTAL_CODE" = 200 ] ||
    { echo "the portal refused ${SECOND_LOGIN}'s first factor (${PORTAL_CODE}), so the client's policy was never asked"; fail=1; }
  code=$(curl -sS -o "$WORK/oidc-${app}-other.json" -D "$hdrs" -w '%{http_code}' \
    -b "$jar" -c "$jar" --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" \
    "$auth_url" 2>"$WORK/oidc.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  detail=$(printf '%s' "$location" | python3 -c 'import sys, urllib.parse as u
q = u.parse_qs(u.urlparse(sys.stdin.read()).query)
print(q.get("error", [""])[0], "-", q.get("error_description", [""])[0])' || true)
  echo "  ${app}: ${SECOND_LOGIN} at the authorization endpoint: ${code} (${detail:-no error parameter})"
  # Refused the OAuth way: the client's callback carries the error, because a
  # valid redirect URI is where an authorization error belongs. A bare status is
  # the other shape (a request Authelia will not answer), and either is a
  # refusal as long as no code exists.
  case "$code" in
    302 | 303)
      case "$location" in
        "https://${DOMAIN}${base}/api/auth/oidc/callback?"*) : ;;
        *) echo "${SECOND_LOGIN} was redirected to ${location}, not the app's callback"; fail=1 ;;
      esac
      case "$location" in
        *"error=access_denied"*) : ;;
        *) echo "${SECOND_LOGIN} was not refused by the client's policy: ${location}"; fail=1 ;;
      esac
      ;;
    400 | 401 | 403) : ;;
    *) echo "${SECOND_LOGIN} got ${code} from the authorization endpoint, want a refusal"; fail=1 ;;
  esac
  case "$location" in
    *"code="*) echo "${SECOND_LOGIN} was issued a code: the client's policy admits another login"; fail=1 ;;
  esac
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
  # Pebble's root is this scenario's anchor: an ACME-issued certificate has no
  # ca.crt in its Secret, so box is given the root the login's own certificate
  # chains to. On a real Let's Encrypt box there is nothing to give: the image's
  # bundle covers it.
  CA_ARGS=(--ca-file "$WORK/pebble-root.pem")
  # Every name the box serves: its own and each app's own host. init resolves
  # them and refuses an app name that does not answer where the domain does, so
  # the app hosts have to be here before bootstrap or it stops on purpose.
  patch_coredns "$node_ip" "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" "$PBH_HOST" ||
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
  # The peer port box picked for this cluster, and the three places it has to
  # agree: the grant, the Service and the app's own preference.
  assert_torrent_port "first install"
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
  # qui and autobrr keep their own user table, so the wiring Job signs in with
  # the account the chart created and registers their connections: qui's one
  # instance pointed at qBittorrent, and autobrr's qBittorrent client plus one
  # per Arr instance. A fresh install creates every one of them; the receipt is
  # read back through the apps themselves later in this scenario.
  # PeerBanHelper holds one downloader, and the receipt says so only after PBH's
  # own status reported it HEALTHY: that is the app's proof it logged in to
  # qBittorrent with the password the Job wrote.
  for step in qui/ready qui/login qui/instance autobrr/ready autobrr/login \
              autobrr/downloader/qbittorrent autobrr/downloader/sonarr autobrr/downloader/radarr \
              pbh/ready pbh/downloader; do
    res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$step'][0]" 2>/dev/null || echo missing)
    detail=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='$step'][0]" 2>/dev/null || echo missing)
    echo "  ${step}: ${res} ${detail}"
    [ "$res" = ok ] || { echo "${step} is ${res}: the Job did not register this connection"; fail=1; }
    case "$step" in
      qui/ready | autobrr/ready | qui/login | autobrr/login | pbh/ready) ;;
      *) [ "$detail" = created ] || { echo "${step} was ${res} ${detail}, want created on a fresh install"; fail=1; } ;;
    esac
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
  for app in qui autobrr maintainerr; do
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

  # qBittorrent has no bypass at the edge, and its API is no exception: unlike
  # an Arr's /api, the whole app -- API included -- is behind the login. That is
  # the property its own bypass rests on, because what reaches the WebUI
  # unauthenticated is then only Envoy forwarding an admitted request or a pod
  # the release's NetworkPolicy lets in.
  for path in /qbittorrent/ /qbittorrent/api/v2/app/version; do
    qb_headers="$WORK/unauth-qbittorrent-headers.txt"
    : >"$qb_headers"
    qb_code=$(curl -sS -o /dev/null -D "$qb_headers" -w '%{http_code}' \
      --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
      "https://${DOMAIN}${path}" 2>/dev/null || true)
    qb_location=$(sed -n 's/^[Ll]ocation: *//p' "$qb_headers" | tr -d '\r' | head -1 || true)
    echo "  GET https://${DOMAIN}${path} without a cookie: ${qb_code} -> ${qb_location}"
    [ "$qb_code" = 302 ] ||
      { echo "an unauthenticated ${path} answered ${qb_code}: qBittorrent is reachable through the edge without a login"; fail=1; }
    case "$qb_location" in
      "https://${DOMAIN}/auth/"*) ;;
      *) echo "the ${path} redirect does not point at the portal: ${qb_location}"; fail=1 ;;
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

  # And with the session, qBittorrent's API answers with no qBittorrent login at
  # all: the only credential this request carried is the edge's. That is the
  # whole decision -- one login, and the app behind it needs none of its own.
  qb_version=$(curl -sS -o "$WORK/qbittorrent-version.txt" -w '%{http_code}' \
    -b "$JAR" --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${DOMAIN}/qbittorrent/api/v2/app/version" 2>/dev/null || true)
  echo "  with the session: /qbittorrent/api/v2/app/version ${qb_version} ($(cat "$WORK/qbittorrent-version.txt" 2>/dev/null || true))"
  [ "$qb_version" = 200 ] ||
    { echo "qBittorrent's API answered ${qb_version} through the edge with a session: it is asking for a second login"; fail=1; }
  case "$(cat "$WORK/qbittorrent-version.txt" 2>/dev/null || true)" in
    v5.*) ;;
    *) echo "the 200 from qBittorrent's API is not its version string: the WebUI answered a login form instead"; fail=1 ;;
  esac

  # ---- the login is reachable only through the Gateway ----------------------
  # Authelia believes X-Forwarded-Proto and X-Forwarded-Host, so a pod that can
  # call 9091 directly can answer for any host and any scheme -- that was the
  # measured hole: s5n-system had no NetworkPolicy at all. The probe carries no
  # release label, so the release's own egress policy does not apply to it: what
  # it proves is the login's ingress policy, not the release's.
  log "the login refuses every caller but the Gateway"
  # The pod runs under the namespace's Pod Security: a bare `kubectl run` is
  # refused there (restricted wants runAsNonRoot, no capabilities and a seccomp
  # profile), and a refusal is not the timeout this probe is about -- so it says
  # which one happened rather than passing as "no answer".
  probe_out=$(timeout 90 $K -n "$NS" run oidc-probe --rm -i --restart=Never \
    --labels=s5n.io/probe=oidc --image="$CURL_IMAGE" --quiet \
    --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65534,"runAsGroup":65534,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"oidc-probe","image":"'"$CURL_IMAGE"'","command":["curl","-s","-o","/dev/null","--max-time","5","-w","%{http_code}","http://authelia.s5n-system:9091/auth/api/health"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"runAsNonRoot":true,"runAsUser":65534}}]}}' \
    2>&1 || true)
  echo "  probe (no release label) -> authelia.s5n-system:9091/auth/api/health: ${probe_out:-timed out}"
  case "$probe_out" in
    *000*) : ;;
    200* | *200*) echo "the login answered a pod directly: its ingress policy is missing or admits the wrong peer"; fail=1 ;;
    "") echo "the probe produced no answer at all, so it proves nothing"; fail=1 ;;
    *Forbidden* | *violates* | *Invalid*)
      echo "the probe pod was refused before it ran, so the login was never asked: ${probe_out}"; fail=1 ;;
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
  for app_host in "plex:${PLEX_HOST}" "seerr:${SEERR_HOST}" "pbh:${PBH_HOST}"; do
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
  # ---- PeerBanHelper: one login, and its API behind it -----------------------
  # The chart switches the app's own WebUI login off: its page sends /api/...
  # with a token only the browser could hold, so a second password there would be
  # a form with no way to fill it. The edge's login is therefore the only one,
  # and these two requests are that decision, measured: without the portal's
  # cookie the app's page is a redirect to the portal, and with it the app's own
  # API answers without any credential of PBH's.
  pbh_headers="$WORK/pbh-unauth-headers.txt"
  : >"$pbh_headers"
  pbh_unauth=$(curl -sS -o /dev/null -D "$pbh_headers" -w '%{http_code}' \
    --resolve "${PBH_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${PBH_HOST}/" 2>/dev/null || true)
  pbh_location=$(sed -n 's/^[Ll]ocation: *//p' "$pbh_headers" | tr -d '\r' | head -1)
  echo "  GET https://${PBH_HOST}/ without a session: ${pbh_unauth} -> ${pbh_location}"
  [ "$pbh_unauth" = 302 ] ||
    { echo "PBH's own host answered ${pbh_unauth} without a session: the app is served open"; fail=1; }
  case "$pbh_location" in
    "https://${DOMAIN}/auth/"*) ;;
    *) echo "PBH's redirect does not point at the portal: ${pbh_location}"; fail=1 ;;
  esac
  pbh_api=$(curl -sS -o "$WORK/pbh-downloaders.json" -w '%{http_code}' -b "$JAR" \
    --resolve "${PBH_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${PBH_HOST}/api/downloaders" 2>/dev/null || true)
  echo "  GET https://${PBH_HOST}/api/downloaders with the portal's session: ${pbh_api}"
  [ "$pbh_api" = 200 ] ||
    { echo "PBH answered the app's own API with ${pbh_api}: it is asking for a second login"; fail=1; }
  pbh_id=$(python3 -c 'import json,sys
rows = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
print(rows[0]["id"] if len(rows) == 1 else "")' "$WORK/pbh-downloaders.json" 2>/dev/null || true)
  echo "  PBH holds one downloader: ${pbh_id}"
  [ "$pbh_id" = qbittorrent ] || { echo "PBH holds ${pbh_id}, want the one downloader the Job registers"; fail=1; }
  # Its config carries no credential either: HEALTHY is PBH's own verdict that
  # it polls qBittorrent, and this is what it polls with.
  pbh_status=$(curl -sS -o "$WORK/pbh-status.json" -w '%{http_code}' -b "$JAR" \
    --resolve "${PBH_HOST}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    "https://${PBH_HOST}/api/downloaders/qbittorrent/status" 2>/dev/null || true)
  echo "  GET https://${PBH_HOST}/api/downloaders/qbittorrent/status: ${pbh_status}"
  [ "$pbh_status" = 200 ] || { echo "PBH did not answer its own status endpoint (${pbh_status})"; fail=1; }
  python3 - "$WORK/pbh-status.json" <<'PYPBH' || fail=1
import json, sys
status = json.load(open(sys.argv[1], encoding="utf-8"))["data"]
cfg = status.get("config") or {}
print("  PBH's downloader: %s, username %r, password %d characters"
      % (status.get("lastStatus"), cfg.get("username"), len(cfg.get("password") or "")))
ok = True
if status.get("lastStatus") != "HEALTHY":
    print("  PBH is not healthy on qBittorrent: %s" % status.get("lastStatusMessage"))
    ok = False
if cfg.get("username") or cfg.get("password"):
    print("  PBH holds a qBittorrent credential: HEALTHY proves it needs none")
    ok = False
sys.exit(0 if ok else 1)
PYPBH

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
  # ---- the OIDC provider: both clients are Ready on the first install --------
  log "the provider: both clients, the issuer they dial, and the anchor"
  assert_oidc "$DOMAIN" qui QUI
  assert_oidc "$DOMAIN" autobrr AUTOBRR
  # ---- the sign-in itself ----------------------------------------------------
  # Ready proves a pod read the discovery document -- for the client that will
  # not start without it. It does not prove anyone can sign in, which is the
  # thing an operator does, so the flow is run here as a browser runs it, for
  # both clients; a second account then tries the same flow to show the client's
  # policy is this seedbox's alone.
  log "the sign-in: both clients' OIDC round trips, and the client that refuses another login"
  oidc_login qui /qui /qui/api/auth/me "$gw_addr" "$WORK/pebble-root.pem"
  oidc_refuses_another_login qui /qui "$gw_addr" "$WORK/pebble-root.pem"
  oidc_login autobrr /autobrr /autobrr/api/auth/validate "$gw_addr" "$WORK/pebble-root.pem"
  oidc_refuses_another_login autobrr /autobrr "$gw_addr" "$WORK/pebble-root.pem"
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
  # The grant's own port, not a number this script picked: the binding has to
  # admit the peers Service box actually wrote, or every later apply in this
  # scenario is refused by the policy under test.
  log "admission: the login cannot be removed or overridden from ${NS}"
  go run ./hack/render-grant charts/s5n "$NS" "${TORRENT_PORT:-}" NodePort | $K apply -f - >/dev/null
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
  # And the peer port is where the first run left it: a re-run keeps the
  # grant's port instead of picking a second one peers are not announcing.
  assert_torrent_port "after the second init" "$TORRENT_PORT"
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

  # ---- qui and autobrr: one account, and what the Job did with it -----------
  # Both apps keep their own user table and mint an API key only for a session,
  # so the chart generates a password in the release Secret and each app's init
  # container writes it into the app on every start. The receipt says the Job
  # signed in; these assertions ask the apps. Reading them needs the Secret's
  # password, which is also the proof that the local account and the edge's OIDC
  # provider coexist: the person signs in through the provider, the Job through
  # the account.
  log "qui and autobrr: the account the chart owns, read back through both APIs"
  read -r QUI_URL QUI_BASE QUI_USER AUTOBRR_URL AUTOBRR_BASE AUTOBRR_USER QBIT_URL <<<"$(
    $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.wiring\.json}' |
      python3 -c 'import json, sys
cfg = json.load(sys.stdin)
print(cfg["qui"]["url"], cfg["qui"]["urlBase"], cfg["qui"]["username"],
      cfg["autobrr"]["url"], cfg["autobrr"]["urlBase"], cfg["autobrr"]["username"],
      cfg["qbittorrent"]["url"])' 2>/dev/null || true)"
  [ -n "$QUI_USER" ] && [ -n "$AUTOBRR_USER" ] ||
    { echo "the wiring config names no qui/autobrr account: the render did not reach the Job"; fail=1; }
  QUI_PASSWORD=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qui-password}' 2>/dev/null | base64 -d || true)
  AUTOBRR_PASSWORD=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.autobrr-password}' 2>/dev/null | base64 -d || true)
  QBIT_PASSWORD=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' 2>/dev/null | base64 -d || true)
  [ ${#QUI_PASSWORD} -ge 8 ] && [ -n "$AUTOBRR_PASSWORD" ] ||
    { echo "the release Secret carries no account passwords"; fail=1; }
  echo "  account ${QUI_USER} (${#QUI_PASSWORD} char password), qui ${QUI_URL}${QUI_BASE}, autobrr ${AUTOBRR_URL}${AUTOBRR_BASE}"

  # owned_ids is the record the Job keeps: qui's instance, autobrr's clients by
  # name and PeerBanHelper's downloader id. The ids are the whole identity, so a
  # duplicate would show up here.
  owned_ids() {
    $K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.owned\.json}' |
      python3 -c 'import json, sys
rec = json.load(sys.stdin)
print("qui=%s" % rec["qui"]["instance"],
      " ".join("%s=%s" % (k, v) for k, v in sorted(rec["autobrr"]["clients"].items())),
      "pbh=%s" % (rec.get("pbh") or {}).get("downloader", ""))' 2>/dev/null || true
  }
  ids_before=$(owned_ids)
  echo "  owned.json: ${ids_before:-none}"
  case "$ids_before" in
    qui=0* | "" | *"=0"*) echo "owned.json records no qui instance or autobrr client: ${ids_before}"; fail=1 ;;
  esac
  case "$ids_before" in
    *"pbh=qbittorrent"*) ;;
    *) echo "owned.json records no PeerBanHelper downloader: ${ids_before}"; fail=1 ;;
  esac

  # A second run of the same Job must change nothing at all: no created, no
  # updated, and the same ids -- which is also the proof that neither app grew a
  # duplicate row (qui has no uniqueness on name or host, autobrr none on name).
  rerun_wiring "$WORK/rerun-quiet.txt" || fail=1
  created=$(status_field 'd["writes"]["created"]' 2>/dev/null || echo missing)
  updated=$(status_field 'd["writes"]["updated"]' 2>/dev/null || echo missing)
  ids_after=$(owned_ids)
  echo "  second run: ${created} created, ${updated} updated; ids ${ids_before} -> ${ids_after}"
  [ "$created" = 0 ] && [ "$updated" = 0 ] || { echo "an unchanged release wrote ${created} created / ${updated} updated"; fail=1; }
  [ "$ids_before" = "$ids_after" ] || { echo "the second run recorded different ids"; fail=1; }

  # ---- what qui holds, through its API ---------------------------------------
  port_forward qui-0 7476 || fail=1
  qui_session "$QUI_BASE"
  echo "  qui: POST ${QUI_BASE}/api/auth/login with the Secret's password: ${QUI_LOGIN_CODE}"
  [ "$QUI_LOGIN_CODE" = 200 ] || { echo "qui refused the account the chart owns: ${QUI_LOGIN_BODY}"; fail=1; }
  curl -sS -H "Cookie: ${QUI_COOKIE}" "http://127.0.0.1:${PF_PORT}${QUI_BASE}/api/instances/" >"$WORK/qui-instances.json" 2>/dev/null || true
  qui_id=$(python3 -c 'import json, sys
try:
    rows = json.load(open(sys.argv[1]))
except Exception:
    rows = []
print(rows[0]["id"] if len(rows) == 1 else "")' "$WORK/qui-instances.json" 2>/dev/null || true)
  qui_test=$(curl -sS -X POST -H "Cookie: ${QUI_COOKIE}" -H 'X-Requested-With: XMLHttpRequest' \
    "http://127.0.0.1:${PF_PORT}${QUI_BASE}/api/instances/${qui_id}/test" 2>/dev/null || true)
  port_forward_stop
  echo "  qui instance ${qui_id:-none}, test: ${qui_test:-no answer}"
  python3 - "$WORK/qui-instances.json" "$QBIT_URL" "$qui_test" <<'PY' || fail=1
import json, sys
rows = json.load(open(sys.argv[1], encoding="utf-8"))
want_host, test_body = sys.argv[2], sys.argv[3]
if len(rows) != 1:
    print("  qui holds %d instances, want exactly 1" % len(rows))
    sys.exit(1)
row = rows[0]
print("  qui instance: %s -> %s (local filesystem access %s)" %
      (row.get("name"), row.get("host"), row.get("hasLocalFilesystemAccess")))
ok = True
if row.get("host") != want_host:
    print("  the instance points at %r, want qBittorrent's %r" % (row.get("host"), want_host))
    ok = False
if not row.get("hasLocalFilesystemAccess"):
    print("  the instance has no local filesystem access: /data is the same mount in both apps")
    ok = False
# qBittorrent skips its login for the release's addresses, so the instance the
# Job registers carries no credential: qui's own test above is the proof it
# reaches qBittorrent without one.
if row.get("username") or row.get("password"):
    print("  the instance holds a qBittorrent credential (%r/%r): none should"
          % (row.get("username"), row.get("password")))
    ok = False
try:
    tested = json.loads(test_body)
except Exception:
    tested = {}
if not tested.get("connected"):
    print("  qui's own test does not report a connection: %r" % (test_body,))
    ok = False
sys.exit(0 if ok else 1)
PY

  # ---- what autobrr holds, through its API -----------------------------------
  port_forward autobrr-0 7474 || fail=1
  autobrr_session "$AUTOBRR_BASE" "$WORK/autobrr-live.jar"
  echo "  autobrr: POST ${AUTOBRR_BASE}/api/auth/login with the Secret's password: ${AUTOBRR_LOGIN_CODE}"
  [ "$AUTOBRR_LOGIN_CODE" = 204 ] || { echo "autobrr refused the account the chart owns (${AUTOBRR_LOGIN_CODE})"; fail=1; }
  curl -sS -b "$WORK/autobrr-live.jar" "http://127.0.0.1:${PF_PORT}${AUTOBRR_BASE}/api/downloaders" >"$WORK/autobrr-downloaders.json" 2>/dev/null || true
  autobrr_test=$(curl -sS -o /dev/null -w '%{http_code}' -b "$WORK/autobrr-live.jar" \
    -X POST "http://127.0.0.1:${PF_PORT}${AUTOBRR_BASE}/api/downloaders/test" \
    -H 'Content-Type: application/json' \
    --data "{\"name\":\"qBittorrent\",\"type\":\"QBITTORRENT\",\"enabled\":true,\"host\":\"${QBIT_URL}\",\"username\":\"\",\"password\":\"\",\"settings\":{\"rules\":{\"enabled\":false}}}" 2>/dev/null || true)
  port_forward_stop
  echo "  autobrr's qBittorrent client test: ${autobrr_test}"
  [ "$autobrr_test" = 204 ] || { echo "autobrr could not reach qBittorrent with the client the Job registers (${autobrr_test})"; fail=1; }
  python3 - "$WORK/autobrr-downloaders.json" "$QBIT_URL" \
    "$($K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.wiring\.json}')" <<'PY' || fail=1
import json, sys
rows = json.load(open(sys.argv[1], encoding="utf-8"))
qbit_url = sys.argv[2]
# Every enabled Arr instance is one client, and its host is the address the Arr
# itself answers on -- the base path included.
cfg = json.loads(sys.argv[3])
want = {i["name"]: i["url"].rstrip("/") + i["urlBase"] for i in cfg["instances"]}
by_name = {}
for row in rows:
    by_name.setdefault(row.get("name"), []).append(row)
print("  autobrr holds: %s" % ", ".join("%s(%s)" % (r["name"], r["type"]) for r in rows))
ok = True
qbit = [r for r in rows if r.get("type") == "QBITTORRENT"]
if len(qbit) != 1:
    print("  autobrr holds %d qBittorrent clients, want 1" % len(qbit))
    ok = False
elif qbit[0].get("host") != qbit_url:
    print("  the qBittorrent client dials %r, want %r" % (qbit[0].get("host"), qbit_url))
    ok = False
if qbit and qbit[0].get("username"):
    print("  the qBittorrent client holds username %r: qBittorrent needs none" % qbit[0].get("username"))
    ok = False
if len(rows) != len(want) + 1:
    print("  autobrr holds %d clients, want %d (qBittorrent + one per Arr)" % (len(rows), len(want) + 1))
    ok = False
for name in want:
    if len(by_name.get(name, [])) != 1:
        print("  autobrr holds %d clients named %s, want 1" % (len(by_name.get(name, [])), name))
        ok = False
for name, host in want.items():
    if not any(r.get("name") == name and r.get("host") == host for r in rows):
        print("  no client for %s dials %r: the Arr's base path is part of the host" % (name, host))
        ok = False
sys.exit(0 if ok else 1)
PY

  # ---- rotating the qBittorrent password: nothing else holds it -------------
  # The credential is sealed into qBittorrent.conf and given to no app, so a
  # rotation is qBittorrent's alone: its init container re-seals the Secret's
  # value at the next start, and the wiring Job -- which reads no qBittorrent
  # password at all -- rewrites nothing. That is the property this block pins,
  # and it is why no app has to be stopped for the rotation: nothing polls with
  # a stale password, so there is no window of refused logins to race.
  log "rotating the qBittorrent password: nothing but qBittorrent itself holds it"
  QBIT_PASSWORD=$(openssl rand -hex 12)
  $K -n "$NS" patch secret s5n-keys --type=merge \
    -p "{\"data\":{\"qbittorrent-password\":\"$(printf '%s' "$QBIT_PASSWORD" | base64 -w0)\"}}" >/dev/null ||
    { echo "could not rotate the qBittorrent password in the release Secret"; fail=1; }
  $K -n "$NS" rollout restart statefulset/qbittorrent >/dev/null
  $K -n "$NS" rollout status statefulset/qbittorrent --timeout=300s >/dev/null ||
    { echo "qBittorrent did not come back on the rotated password"; fail=1; }
  # The init container is what carried the new value into the file, and the
  # login endpoint is the proof it took (a rotation that never reached the file
  # would leave the old hash in place and be invisible to every app).
  port_forward qbittorrent-0 8080 || fail=1
  : >"$WORK/qbt-rotated.jar"
  rotated_login=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -c "$WORK/qbt-rotated.jar" \
    --data-urlencode "username=admin" --data-urlencode "password=${QBIT_PASSWORD}" \
    "http://127.0.0.1:${PF_PORT}/api/v2/auth/login" 2>/dev/null || true)
  port_forward_stop
  case "$rotated_login" in
    2??) ;;
    *) echo "qBittorrent refused the rotated password (${rotated_login:-no answer}): the init container did not seal the Secret's new value"; fail=1 ;;
  esac

  rerun_wiring "$WORK/rerun-rotate.txt" || fail=1
  created=$(status_field 'd["writes"]["created"]' 2>/dev/null || echo missing)
  updated=$(status_field 'd["writes"]["updated"]' 2>/dev/null || echo missing)
  echo "  after the rotation: ${created} created, ${updated} updated"
  [ "$created" = 0 ] && [ "$updated" = 0 ] ||
    { echo "the rotation wrote ${created} created / ${updated} updated: something still holds the qBittorrent password"; fail=1; }
  for step in qui/instance autobrr/downloader/qbittorrent pbh/downloader sonarr/downloadclient radarr/downloadclient; do
    detail=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='$step'][0]" 2>/dev/null || echo missing)
    echo "  ${step}: ${detail}"
    [ "$detail" = unchanged ] || { echo "${step} changed after a qBittorrent password rotation: it holds a copy"; fail=1; }
  done

  # And every caller still reaches qBittorrent, with the credential it stored
  # (none) rather than the one the Secret now holds.
  port_forward qui-0 7476 || fail=1
  qui_session "$QUI_BASE"
  curl -sS -H "Cookie: ${QUI_COOKIE}" "http://127.0.0.1:${PF_PORT}${QUI_BASE}/api/instances/" >"$WORK/qui-instances-rotated.json" 2>/dev/null || true
  qui_id=$(python3 -c 'import json, sys
rows = json.load(open(sys.argv[1]))
print(rows[0]["id"] if len(rows) == 1 else "")' "$WORK/qui-instances-rotated.json" 2>/dev/null || true)
  qui_test=$(curl -sS -X POST -H "Cookie: ${QUI_COOKIE}" -H 'X-Requested-With: XMLHttpRequest' \
    "http://127.0.0.1:${PF_PORT}${QUI_BASE}/api/instances/${qui_id}/test" 2>/dev/null || true)
  port_forward_stop
  case "$qui_test" in
    *'"connected":true'*) echo "  qui still reaches qBittorrent on the rotated password: ok" ;;
    *) echo "qui lost qBittorrent on the rotated password: ${qui_test}"; fail=1 ;;
  esac

  port_forward autobrr-0 7474 || fail=1
  autobrr_session "$AUTOBRR_BASE" "$WORK/autobrr-rotated.jar"
  autobrr_test=$(curl -sS -o /dev/null -w '%{http_code}' -b "$WORK/autobrr-rotated.jar" \
    -X POST "http://127.0.0.1:${PF_PORT}${AUTOBRR_BASE}/api/downloaders/test" \
    -H 'Content-Type: application/json' \
    --data "{\"name\":\"qBittorrent\",\"type\":\"QBITTORRENT\",\"enabled\":true,\"host\":\"${QBIT_URL}\",\"username\":\"\",\"password\":\"\",\"settings\":{\"rules\":{\"enabled\":false}}}" 2>/dev/null || true)
  port_forward_stop
  echo "  autobrr's qBittorrent client on the rotated password: ${autobrr_test}"
  [ "$autobrr_test" = 204 ] || { echo "autobrr lost qBittorrent on the rotated password (${autobrr_test})"; fail=1; }

  # The Arrs' copies are the pair that would have gone stale first if anything
  # still held the password: the client is fetched as the Job wrote it -- empty
  # credentials -- and Sonarr's own test endpoint decides whether qBittorrent
  # accepts it.
  sonarr_key=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.sonarr-apikey}' 2>/dev/null | base64 -d || true)
  [ -n "$sonarr_key" ] || { echo "the release Secret has no sonarr API key"; fail=1; }
  $K -n "$NS" exec sonarr-0 -- curl -s -H "X-Api-Key: ${sonarr_key}" \
    "http://localhost:8989/sonarr/api/v3/downloadclient" >"$WORK/sonarr-clients-rotated.json" 2>/dev/null || true
  if python3 - "$WORK/sonarr-clients-rotated.json" >"$WORK/sonarr-client-test.json" <<'PYSONARR'
import json, sys
clients = json.load(open(sys.argv[1], encoding="utf-8"))
qbit = [c for c in clients if str(c.get("implementation", "")).lower() == "qbittorrent"]
if len(qbit) != 1:
    sys.exit("sonarr holds %d qBittorrent clients, want 1" % len(qbit))
body = qbit[0]
fields = {f.get("name"): f.get("value") for f in body.get("fields") or []}
if fields.get("username") or fields.get("password"):
    sys.exit("sonarr's download client still holds a credential (%r/%r)"
             % (fields.get("username"), fields.get("password")))
print(json.dumps(body))
PYSONARR
  then
    sonarr_test=$($K -n "$NS" exec -i sonarr-0 -- curl -s -o /dev/null -w '%{http_code}' \
      -X POST -H "X-Api-Key: ${sonarr_key}" -H 'Content-Type: application/json' \
      --data-binary @- "http://localhost:8989/sonarr/api/v3/downloadclient/test" \
      2>/dev/null <"$WORK/sonarr-client-test.json" || true)
    echo "  sonarr's qBittorrent client on the rotated password: ${sonarr_test}"
    case "$sonarr_test" in
      200 | 201 | 204) ;;
      *) echo "sonarr could not reach qBittorrent with the client the Job wrote (${sonarr_test})"; fail=1 ;;
    esac
  else
    echo "could not read Sonarr's download clients back: $(head -c 300 "$WORK/sonarr-clients-rotated.json" 2>/dev/null)"
    fail=1
  fi

  # autobrr's init container, on a restart: its second create-user run fails
  # with the one message that means "already there", which is why the script
  # matches that sentence instead of ignoring the exit code, and the password is
  # set again after it. Restarted here so the re-run is real.
  $K -n "$NS" rollout restart statefulset/autobrr >/dev/null
  $K -n "$NS" rollout status statefulset/autobrr --timeout=300s >/dev/null 2>&1 ||
    { echo "autobrr did not come back after its restart"; fail=1; }
  autobrr_init=$($K -n "$NS" logs autobrr-0 -c account 2>/dev/null || true)
  echo "  autobrr's init container: $(printf '%s' "$autobrr_init" | tr '\n' ' ' | cut -c1-220)"
  case "$autobrr_init" in
    *"account already exists (autobrrctl: only 1 user account is supported)"*) ;;
    *) echo "autobrr's init container did not meet the existing account as expected"; fail=1 ;;
  esac
  case "$autobrr_init" in
    *"successfully updated password for user"*) ;;
    *) echo "autobrr's init container did not set the Secret's password again"; fail=1 ;;
  esac

  # ---- a password changed inside qui ----------------------------------------
  # The account belongs to the app, so an operator can change it there; the Job
  # then cannot sign in until the pod restarts, because that is when the init
  # container writes the Secret's password back. The receipt must say that in
  # words and name the command -- not report an HTTP status.
  log "a password changed inside qui is reported, and a restart puts it back"
  port_forward qui-0 7476 || fail=1
  qui_session "$QUI_BASE"
  [ "$QUI_LOGIN_CODE" = 200 ] || { echo "qui refused the Secret's password before the change (${QUI_LOGIN_CODE})"; fail=1; }
  QUI_INSIDE_PASSWORD="inside-$(openssl rand -hex 8)"
  change_code=$(qui_change_password "$QUI_BASE" "$QUI_PASSWORD" "$QUI_INSIDE_PASSWORD")
  echo "  changed qui's password through its API: ${change_code} $(head -c 120 "$WORK/qui-change.json" 2>/dev/null)"
  # The change took, and the Secret's password no longer opens a session: that
  # is the state the Job has to report, so the assertion below is not vacuous.
  qui_session "$QUI_BASE"
  echo "  the Secret's password after the change: ${QUI_LOGIN_CODE} ${QUI_LOGIN_BODY}"
  port_forward_stop
  [ "$change_code" = 200 ] || { echo "qui refused the password change (${change_code})"; fail=1; }
  [ "$QUI_LOGIN_CODE" = 401 ] || { echo "the Secret's password still opens a session after the change (${QUI_LOGIN_CODE})"; fail=1; }

  rerun_wiring "$WORK/rerun-changed.txt" || fail=1
  login_result=$(status_field "[s['result'] for s in d['steps'] if s['name']=='qui/login'][0]" 2>/dev/null || echo missing)
  login_detail=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='qui/login'][0]" 2>/dev/null || echo missing)
  echo "  qui/login: ${login_result}: ${login_detail}"
  [ "$login_result" = failed ] ||
    { echo "the Job signed in although the password was changed inside qui"; fail=1; }
  case "$login_detail" in
    *"changed inside the app"*"kubectl rollout restart statefulset/qui"*) ;;
    *) echo "the receipt does not name the cause and the fix: ${login_detail}"; fail=1 ;;
  esac

  $K -n "$NS" rollout restart statefulset/qui >/dev/null
  $K -n "$NS" rollout status statefulset/qui --timeout=300s >/dev/null ||
    { echo "qui did not come back after the restart"; fail=1; }
  # The init container is what puts the password back, so its log is the
  # first-hand account: on a re-run it says the account exists and that the
  # password was set again.
  qui_init=$($K -n "$NS" logs qui-0 -c account 2>/dev/null || true)
  echo "  qui's init container: $(printf '%s' "$qui_init" | tr '\n' ' ' | cut -c1-220)"
  case "$qui_init" in
    *"User account already exists"*) ;;
    *) echo "qui's init container did not report the account already existing"; fail=1 ;;
  esac
  case "$qui_init" in
    *"Password changed successfully for user '${QUI_USER}'"*) ;;
    *) echo "qui's init container did not set the Secret's password again"; fail=1 ;;
  esac
  rerun_wiring "$WORK/rerun-restored.txt" || fail=1
  for step in qui/login qui/instance; do
    res=$(status_field "[s['result'] for s in d['steps'] if s['name']=='$step'][0]" 2>/dev/null || echo missing)
    echo "  ${step} after the restart: ${res}"
    [ "$res" = ok ] || { echo "${step} is ${res} after the restart that put the Secret's password back"; fail=1; }
  done
  port_forward qui-0 7476 || fail=1
  qui_session "$QUI_BASE"
  echo "  qui after the restart: POST login with the Secret's password: ${QUI_LOGIN_CODE}"
  port_forward_stop
  [ "$QUI_LOGIN_CODE" = 200 ] ||
    { echo "the account did not come back to the Secret's password (${QUI_LOGIN_CODE})"; fail=1; }

  # ---- autobrr's onboarding endpoint ----------------------------------------
  # It was open while autobrr's user table was empty, which is how a stranger
  # could take the app over; the account the init container writes closes it.
  # Asked through the Gateway with the edge's session, because that is the only
  # way in for a person.
  log "autobrr's onboarding endpoint is refused now that the account exists"
  portal_login "$WORK/edge-onboard-jar" "$LOGIN" "$AUTHELIA_PASSWORD" "https://${DOMAIN}/autobrr/" "$gw_addr" "$WORK/pebble-root.pem"
  echo "  portal sign-in for the request: ${PORTAL_CODE}"
  [ "$PORTAL_CODE" = 200 ] || { echo "the portal refused the box's own account (${PORTAL_CODE})"; fail=1; }
  onboard_code=$(curl -sS -o "$WORK/onboard.json" -w '%{http_code}' -b "$WORK/edge-onboard-jar" \
    --resolve "${DOMAIN}:443:${gw_addr}" --cacert "$WORK/pebble-root.pem" \
    -X POST "https://${DOMAIN}/autobrr/api/auth/onboard" -H 'Content-Type: application/json' \
    --data '{"username":"intruder","password":"intruder-pass-123"}' 2>/dev/null || true)
  echo "  POST /autobrr/api/auth/onboard with the edge session: ${onboard_code} $(head -c 160 "$WORK/onboard.json" 2>/dev/null)"
  [ "$onboard_code" = 503 ] ||
    { echo "autobrr's onboarding endpoint answered ${onboard_code}: the account that closes it is missing"; fail=1; }
  grep -q 'onboarding unavailable' "$WORK/onboard.json" ||
    { echo "the refusal is not autobrr's own onboarding one"; fail=1; }

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

  # ---- a port in box's band, held by someone else -------------------------
  # The band is where the API server does *not* put its own allocations, so the
  # only way a port there is taken is a Service that asked for it by number.
  # box has to see it -- that LIST across namespaces is the whole mechanism --
  # and pick around it.
  $K create namespace port-holder --dry-run=client -o yaml | $K apply -f - >/dev/null
  $K -n port-holder apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: holder
spec:
  type: NodePort
  ports:
    - port: 80
      nodePort: 30000
EOF
  held_port=$($K -n port-holder get service holder -o jsonpath='{.spec.ports[0].nodePort}' || true)
  [ "$held_port" = 30000 ] ||
    { echo "could not hold nodePort 30000 in another namespace (got ${held_port:-nothing})"; fail=1; }
  echo "  another namespace holds nodePort 30000 before box runs"

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
  # A port in the band was already held by an unrelated Service when box ran,
  # and box must not have picked it -- nor anything else someone else holds.
  assert_torrent_port "the port another Service already held"
  picked=$TORRENT_PORT
  [ "$picked" != 30000 ] ||
    { echo "box picked the nodePort the holder Service was created with"; fail=1; }

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

  # ---- the provider, on a self-signed box ----------------------------------
  # The same app, the same issuer, and this time the anchor is the box's own
  # root: nothing public signs this host, so root's init copied the root into
  # the namespace and the pod trusts it with SSL_CERT_DIR. Ready on the first
  # install is the whole proof -- qui does not start until it can read the
  # discovery document.
  log "the provider on a self-signed host: the box root is the anchor"
  assert_oidc "$DOMAIN" qui QUI
  assert_oidc "$DOMAIN" autobrr AUTOBRR
  # The same sign-in on a host nothing public signs: the browser's side of it
  # trusts s5n-host-ca alone, which is what a person's browser does after
  # importing the box's root.
  log "the sign-in on a self-signed host: both clients' OIDC round trips"
  oidc_login qui /qui /qui/api/auth/me "$lb_addr" "$WORK/anchor-ca.pem"
  oidc_refuses_another_login qui /qui "$lb_addr" "$WORK/anchor-ca.pem"
  oidc_login autobrr /autobrr /autobrr/api/auth/validate "$lb_addr" "$WORK/anchor-ca.pem"
  oidc_refuses_another_login autobrr /autobrr "$lb_addr" "$WORK/anchor-ca.pem"

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
  # The second run keeps the port the first one recorded.
  assert_torrent_port "after the second bootstrap" "$TORRENT_PORT"
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
192.0.2.10 ${DOMAIN} ${PLEX_HOST} ${SEERR_HOST} ${PBH_HOST}
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

  # ---- an upgraded box: the Arr's stored qBittorrent password is cleared -----
  # The previous release's wiring wrote its own copy of the qBittorrent password
  # into every Arr's download client. Both credential fields are owned, and now
  # owned as empty, so the next run of the release's own Job clears the stored
  # copy instead of needing it. The upgrade is measured the only way an API
  # allows: put a password back through Sonarr's own endpoint (what that install
  # left behind), run the Job, and read the client again.
  log "an upgraded box: the Arr's stored qBittorrent password is cleared"
  sonarr_key=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.sonarr-apikey}' 2>/dev/null | base64 -d || true)
  [ -n "$sonarr_key" ] || { echo "the release Secret has no sonarr API key"; fail=1; }
  $K -n "$NS" exec sonarr-0 -- curl -s -H "X-Api-Key: ${sonarr_key}" \
    "http://localhost:8989/sonarr/api/v3/downloadclient" >"$WORK/sonarr-before-upgrade.json" 2>/dev/null || true
  sonarr_client_id=$(python3 - "$WORK/sonarr-before-upgrade.json" "$WORK/sonarr-with-password.json" <<'PYSEED'
import json, sys
clients = json.load(open(sys.argv[1], encoding="utf-8"))
qbit = [c for c in clients if str(c.get("implementation", "")).lower() == "qbittorrent"]
if len(qbit) != 1:
    sys.exit("sonarr holds %d qBittorrent clients, want 1" % len(qbit))
body = qbit[0]
for field in body.get("fields") or []:
    if field.get("name") == "username":
        field["value"] = "admin"
    if field.get("name") == "password":
        field["value"] = "left-over-password"
with open(sys.argv[2], "w", encoding="utf-8") as out:
    json.dump(body, out)
print(body.get("id", ""))
PYSEED
  ) || true
  [ -n "$sonarr_client_id" ] || { echo "could not read Sonarr's qBittorrent client id"; fail=1; }
  seeded=$($K -n "$NS" exec -i sonarr-0 -- curl -s -o /dev/null -w '%{http_code}' \
    -X PUT -H "X-Api-Key: ${sonarr_key}" -H 'Content-Type: application/json' \
    --data-binary @- "http://localhost:8989/sonarr/api/v3/downloadclient/${sonarr_client_id}?forceSave=true" \
    2>/dev/null <"$WORK/sonarr-with-password.json" || true)
  echo "  PUT the previous release's credentials back into Sonarr's client ${sonarr_client_id}: ${seeded}"
  # Sonarr answers 202 Accepted on a download client PUT, not 200.
  case "$seeded" in
    200 | 201 | 202 | 204) ;;
    *) echo "Sonarr refused the seeded download client: ${seeded}"; fail=1 ;;
  esac

  # What the Arr answers for a *set* privacy field, for the record and for the
  # assertion below: a set password comes back as its mask. Reading it here is
  # what makes the empty form after the Job run mean something.
  $K -n "$NS" exec sonarr-0 -- curl -s -H "X-Api-Key: ${sonarr_key}" \
    "http://localhost:8989/sonarr/api/v3/downloadclient" >"$WORK/sonarr-seeded.json" 2>/dev/null || true
  python3 - "$WORK/sonarr-seeded.json" <<'PYSEEDREAD' || fail=1
import json, sys
clients = json.load(open(sys.argv[1], encoding="utf-8"))
qbit = [c for c in clients if str(c.get("implementation", "")).lower() == "qbittorrent"]
fields = {f.get("name"): f.get("value") for f in (qbit[0].get("fields") or [])} if qbit else {}
print("  Sonarr's client with the password set: username %r, password %r"
      % (fields.get("username"), fields.get("password")))
if not fields.get("password"):
    sys.exit("the seeded password did not take: the Arr answers %r" % (fields.get("password"),))
PYSEEDREAD

  rerun_wiring "$WORK/rerun-upgrade.txt" || fail=1
  upgrade_detail=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='sonarr/downloadclient'][0]" 2>/dev/null || echo missing)
  echo "  sonarr/downloadclient after the upgrade run: ${upgrade_detail}"
  [ "$upgrade_detail" = updated ] ||
    { echo "the Job did not rewrite the Arr's download client: the stored password was left in place"; fail=1; }

  $K -n "$NS" exec sonarr-0 -- curl -s -H "X-Api-Key: ${sonarr_key}" \
    "http://localhost:8989/sonarr/api/v3/downloadclient" >"$WORK/sonarr-after-upgrade.json" 2>/dev/null || true
  # What the Arr answers for a privacy field is the point of reading it back: a
  # set password comes back as its mask, and an empty one as an empty value, so
  # asserting the empty form is asserting that no plaintext is left.
  if python3 - "$WORK/sonarr-after-upgrade.json" <<'PYCHECK'
import json, sys
clients = json.load(open(sys.argv[1], encoding="utf-8"))
qbit = [c for c in clients if str(c.get("implementation", "")).lower() == "qbittorrent"]
if len(qbit) != 1:
    sys.exit("sonarr holds %d qBittorrent clients, want 1" % len(qbit))
fields = {f.get("name"): f.get("value") for f in qbit[0].get("fields") or []}
password = fields.get("password")
print("  Sonarr's client after the upgrade: username %r, password %r"
      % (fields.get("username"), password))
if fields.get("username") or password not in (None, ""):
    print("  Sonarr still holds a credential: the Job's empty owned fields did not reach it")
    sys.exit(1)
sys.exit(0)
PYCHECK
  then :; else fail=1; fi

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
  for host in "$PLEX_HOST" "$SEERR_HOST" "$PBH_HOST"; do
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
192.0.2.10 ${DOMAIN} ${PLEX_HOST} ${SEERR_HOST} ${PBH_HOST}
EOF
  install_pebble
  [ "$fail" = 0 ] || return 0
  # Pebble's root is this scenario's anchor: an ACME-issued certificate has no
  # ca.crt in its Secret, so box is given the root the login's own certificate
  # chains to. On a real Let's Encrypt box there is nothing to give: the image's
  # bundle covers it.
  CA_ARGS=(--ca-file "$WORK/pebble-root.pem")
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
  for host in "$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" "$PBH_HOST"; do
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

  # The move is the run that writes the host's Certificate with a public
  # issuer, and --ca-file is what an ACME certificate's anchor has to come
  # from: its Secret carries no ca.crt. So the anchor is the file's root, not
  # the box's own -- the root under a certificate that is no longer served
  # would be an anchor nothing chains to.
  same_cert "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)" \
    "$(cat "$WORK/pebble-root.pem")" ||
    { echo "the anchor after the move is not the one --ca-file gave"; fail=1; }

  # --ca-file is the same writer with another source, for the one mode with
  # nothing to copy: an ACME certificate has no ca.crt at any point. The e2e
  # hands box Pebble's root on init -- the run that writes the host's
  # Certificate, and so the run the anchor follows -- and asserts the ConfigMap,
  # then an init without the flag takes it away again, so the flag feeds the
  # product path instead of adding a test-only one. The ConfigMap is removed
  # first here, because the move above already wrote this very certificate:
  # this block is the writer with no anchor to compare against.
  $K -n "$NS" delete configmap s5n-host-ca --ignore-not-found >/dev/null
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
  # Without the dev flag: the move above is the only run that passes it, and
  # this is the assertion that a public host carries no anchor.
  CA_ARGS=()
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

  # The holder Service was this scenario's setup, not part of the install.
  $K delete namespace port-holder --wait=false >/dev/null 2>&1 || true
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
