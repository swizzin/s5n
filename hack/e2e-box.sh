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
# Five scenarios, five clusters:
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
#   update        `box update`, on a real cluster, from an installed release to
#                 one whose new image cannot start. The step takes a copy of
#                 sonarr's config, moves the release, and puts sonarr back on the
#                 image it ran when the new one keeps exiting -- while the app
#                 the same step moved elsewhere stays moved and the release stays
#                 at the new version. Then a re-apply keeps the hold, a release
#                 that pins another image lapses it, and `box rollback --pin`
#                 restores the newest copy into an ordinary values pin. Then
#                 `box apply` refuses to move the release, copies sonarr before
#                 a values pin moves it, and a rollback restores that copy. This is
#                 the only scenario box needs to reach the config it copies, so
#                 it, migrate and migrate-keep are the three whose volume roots
#                 are a directory on this host, mounted into the node (see
#                 VOLUME ROOTS below).
#   migrate-keep  the same source cut over the other way: box installs onto the
#                 cluster the source runs on and adopts in place, with the source's
#                 workloads left at 0 replicas.
#   migrate       an install box did not make -- qBittorrent, Sonarr, Radarr and
#                 Plex over plain host directories, seeded through their own
#                 APIs (and, for Plex, through its own SQLite on the database it
#                 built), plus four apps whose own code writes what the migration
#                 reads (autobrr, qui, pbh, and Tautulli behind a URL root) --
#                 taken over by `box migrate discover` and
#                 `box migrate apply` after the source cluster is thrown away.
#                 The apps must come back healthy on the chart's images with
#                 their paths under /data, their API keys unchanged, their
#                 download clients adopted by the ids the source had, and the
#                 data renamed rather than moved. Plex is the one app whose
#                 database is rewritten by a one-shot Job running its own image
#                 (see SCENARIO: MIGRATE below).
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
# Why one script and five clusters instead of a flag inside hack/e2e-kind.sh:
# the existing e2e is the chart's acceptance test, and its helm install is what
# proves the chart still installs the way users install it today. These
# scenarios are about box's decision, share the bootstrap command line, and
# disagree about what is on the cluster before it runs -- so they cannot share a
# cluster. `all` runs this script once per scenario, which gives each its own
# work directory, cluster name and exit code.
#
# Usage: hack/e2e-box.sh [--keep] [fresh|preinstalled|foreign-gateway|update|migrate|migrate-keep|all]   (default: all)
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
    fresh | preinstalled | foreign-gateway | update | migrate | migrate-keep | all) SCENARIO=$arg ;;
    -h | --help)
      sed -n '2,/^set -euo pipefail/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $arg (want fresh, preinstalled, foreign-gateway, update, migrate, migrate-keep or all)" >&2
      exit 2
      ;;
  esac
done

if [ "$SCENARIO" = all ]; then
  status=0
  "$0" fresh $KEEP || status=1
  "$0" preinstalled $KEEP || status=1
  "$0" foreign-gateway $KEEP || status=1
  "$0" update $KEEP || status=1
  "$0" migrate $KEEP || status=1
  "$0" migrate-keep $KEEP || status=1
  exit "$status"
fi

case "$SCENARIO" in
  fresh) CLUSTER=s5n-box; DOMAIN=box.example.test ;;
  preinstalled) CLUSTER=s5n-box-lb; DOMAIN=box.internal.example ;;
  foreign-gateway) CLUSTER=s5n-box-gw; DOMAIN=box.example.com ;;
  update) CLUSTER=s5n-box-update; DOMAIN=box.update.example.test ;;
  migrate) CLUSTER=s5n-box-migrate; DOMAIN=box.migrate.example.test ;;
  migrate-keep) CLUSTER=s5n-box-migrate-keep; DOMAIN=box.keep.example.test ;;
esac
# The fresh scenario is the one that proves the namespace a first run derives
# from its login: it passes no --namespace, and the box has to land in
# s5n-<login>. The other scenarios keep an explicit namespace, which still wins
# (and keeps their assertions on the namespace they have always used).
if [ "$SCENARIO" = fresh ]; then
  NS="s5n-${LOGIN}"
fi
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

# ---- the seedbox's volume roots --------------------------------------------
# The two host directories the grant names, and the one thing that differs
# between the scenarios. Every scenario but update keeps them inside the node,
# where box cannot reach them: --paths-in-node is how box skips the stat a real
# host would have passed, and nothing box does touches an app's config.
#
# update, migrate and migrate-keep are the ones that need the opposite. A step copies an app's
# config, restores it and writes the hold beside the copies -- all of it on the
# filesystem the seedbox's /config comes from -- so box and the pods have to
# see one tree; and migrate reads a source install's configs and renames its
# data, from a box that runs on the host. Here the roots are a directory on this
# host, bind-mounted into the node at the same absolute path (with the whole
# work directory around them, below): the PV's hostPath, the pod's /config and
# box's snapshot directory all name it, which is what a real host has.
DATA_PATH=/srv/s5n/data
CONFIG_PATH=/srv/s5n/config
KIND_CONFIG=hack/kind.yaml
if [ "$SCENARIO" = update ] || [ "$SCENARIO" = migrate ] || [ "$SCENARIO" = migrate-keep ]; then
  DATA_PATH="$WORK/hostfs/data"
  CONFIG_PATH="$WORK/hostfs/config"
  KIND_CONFIG="$WORK/kind.yaml"
  mkdir -p "$DATA_PATH" "$CONFIG_PATH"
  # box runs as whoever started the e2e, and the apps run as the grant's
  # puid:pgid: on a real host box is root and everything below the roots is the
  # seedbox user's. Here the two are different users, so the roots are writable
  # by both and a restore can replace a file either of them wrote. Nothing in
  # the scenario depends on the mode; an unwritable directory would fail the
  # step with a permission error instead of the behaviour under test.
  chmod 0777 "$WORK/hostfs" "$DATA_PATH" "$CONFIG_PATH"
  # The whole work directory is mounted at its own path, not just the two roots:
  # the node's root is the one user that can read a tree the migration left
  # owned by the seedbox's user (see migrate_box), and it is how this script
  # hands box's own files back after box ran as root.
  awk -v mount="$WORK" '
    { print }
    /^  - role: control-plane$/ {
      print "    extraMounts:"
      print "      - hostPath: " mount
      print "        containerPath: " mount
      print "        propagation: Bidirectional"
    }
  ' hack/kind.yaml >"$KIND_CONFIG"
  grep -qF "containerPath: ${WORK}" "$KIND_CONFIG" || {
    echo "the kind config has no mount for ${WORK}"
    exit 1
  }
fi
# The registry is a container on this host: it must not outlive the run, even
# when a scenario fails before its own cleanup.
cleanup() {
  # The exit status before anything here runs: the line every log ends with has
  # to be the scenario's own answer, not the last cleanup command's.
  local code=$?
  if [ -n "$REGISTRY" ]; then
    docker rm -f "$REGISTRY" >/dev/null 2>&1 || true
  fi
  # The update scenario's notice receiver, by pid: a run that fails before its
  # own kill must not leave a listener behind.
  if [ -n "${NOTICE_PID:-}" ]; then
    kill "$NOTICE_PID" >/dev/null 2>&1 || true
  fi
  # --keep is for reading what a run did: each init's transcript, the bootstrap
  # transcripts, the box binary that wrote them. The cluster it leaves behind is
  # worth nothing without those, so the work directory stays with it and its
  # path is printed. A run without --keep still cleans up after itself.
  if [ "$KEEP" = --keep ]; then
    printf '\n==> kept work dir: %s\n' "$WORK"
  else
    # The migrate scenarios run box as root and hand the config tree to the
    # seedbox's user, neither of them this script's user, so part of the work
    # directory is not this user's to delete. The registry image is already
    # here and has a shell's rm; root in it removes what this user cannot.
    if [ -n "${REGISTRY_IMAGE:-}" ] && [ -d "$WORK" ]; then
      docker run --rm -u 0 -v "$(dirname "$WORK"):/work-parent" --entrypoint /bin/rm \
        "$REGISTRY_IMAGE" -rf "/work-parent/$(basename "$WORK")" >/dev/null 2>&1 || true
    fi
    rm -rf "$WORK"
  fi
  printf 'E2E EXIT: %s\n' "$code"
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
$KIND create cluster --name "$CLUSTER" --config "$KIND_CONFIG" --wait 120s

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
log "preparing host paths (what box init creates on real hosts)"
docker exec "${CLUSTER}-control-plane" bash -c "
  mkdir -p '$DATA_PATH' \\
           '$CONFIG_PATH'/{qbittorrent,prowlarr,sonarr,sonarr-4k,radarr,radarr-4k,plex,tautulli,seerr,qui,autobrr,maintainerr,pbh}
  chown -R 1000:1000 '$DATA_PATH' '$CONFIG_PATH'"

# The update scenario's extra step. Its roots are a host directory mounted into
# the node, so box -- which runs as whoever started the e2e -- writes its
# snapshots and its hold into directories the apps own. Group and other get the
# same rights as the owner: box has to be able to replace a file an app wrote,
# and an app has to be able to write the config a restore put back.
if [ "$SCENARIO" = update ]; then
  docker exec "${CLUSTER}-control-plane" chmod -R a+rwX "$CONFIG_PATH"
fi

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
for d in "$DATA_PATH/downloads" "$DATA_PATH/media"; do
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

# APPS is the app list bootstrap and init write into the grant. Every run wants
# the whole catalog in front of it; the one step that proves a box with nothing
# on sets it to `none`, the word for the empty list.
APPS="qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr,qui,autobrr,maintainerr,pbh"

bootstrap() { # bootstrap <output file>
  # The platform and root's seedbox are one command now. --record points the
  # cluster decision at this work directory: on a real host it is
  # /etc/s5n/cluster.yaml, written by root, and the e2e is not root.
  #
  # No --chart-version: bootstrap takes the default-version path, so the e2e
  # exercises the newest-release selection against a real registry. The chart
  # is the one pushed above; box pulls it over loopback.
  #
  # The fresh scenario passes no --namespace, so box derives it from the login
  # (s5n-<login>); every other scenario names its own.
  local ns_args=()
  [ "$SCENARIO" = fresh ] || ns_args=(--namespace "$NS")
  "$WORK/box" bootstrap --yes --chart-repo "$CHART_REPO" ${ACME_ARGS[@]+"${ACME_ARGS[@]}"} ${DNS_ARGS[@]+"${DNS_ARGS[@]}"} ${CA_ARGS[@]+"${CA_ARGS[@]}"} ${UNIT_ARGS[@]+"${UNIT_ARGS[@]}"} \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    ${ns_args[@]+"${ns_args[@]}"} --domain "$DOMAIN" \
    --data-path "$DATA_PATH" --config-path "$CONFIG_PATH" \
    --apps "$APPS" \
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
  local ns_args=()
  [ "$SCENARIO" = fresh ] || ns_args=(--namespace "$NS")
  "$WORK/box" init --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" ${DNS_ARGS[@]+"${DNS_ARGS[@]}"} ${CA_ARGS[@]+"${CA_ARGS[@]}"} \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    ${ns_args[@]+"${ns_args[@]}"} --domain "$DOMAIN" \
    --data-path "$DATA_PATH" --config-path "$CONFIG_PATH" \
    --apps "$APPS" \
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
  $K get pv ${NS}-s5n-data ${NS}-s5n-config -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion} {end}'
  $K -n "$NS" get pvc ${NS}-s5n-data ${NS}-s5n-config -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion} {end}'
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

# host_cert is the Certificate (and Secret) name box gives one host: the DNS
# name flattened into a label, the way internal/platform's CertificateName
# makes it.
host_cert() { printf 's5n-tls-%s' "$(printf '%s' "$1" | tr '.' '-')"; }

# retag_pinned <repository> <digest> <tag>: give the kind node the image a
# release pins (by digest) under another tag, for a release the e2e publishes
# itself. The tag is made in the node's containerd, which already holds the
# image because the cluster ran it, rather than on the host and loaded with
# kind: on a docker that keeps images in containerd (the default for a fresh
# Docker 29, as on CI runners) a pulled image is a multi-platform index whose
# other platforms were never fetched, and kind's import of it fails on the
# first missing one. The pull is for a node that has lost the image.
retag_pinned() {
  local node="${CLUSTER}-control-plane"
  if ! docker exec "$node" ctr --namespace k8s.io images ls -q | grep -qxF "$1@$2"; then
    if ! docker exec "$node" crictl pull "$1@$2" >/dev/null; then
      echo "pulling $1@$2 into the node failed"
      return 1
    fi
  fi
  if ! docker exec "$node" ctr --namespace k8s.io images tag --force "$1@$2" "$1:$3" >/dev/null; then
    echo "tagging $1@$2 as $1:$3 in the node failed"
    return 1
  fi
}

# A re-run of init on a box whose certificates are already issued is seconds of
# work: every Certificate it re-applies is Ready for its current generation, so
# every wait answers on its first read. A step that takes minutes instead means
# a wait ran out on a name this cluster cannot issue for, which is exactly how
# this scenario lost 183 s twice while its CoreDNS patch left one name
# unanswered -- the run stayed green on wall time nobody had explained. The
# budget is well under CertificateTimeout (3m), so a wait that runs out fails
# the round instead of passing it.
INIT_BUDGET=${INIT_BUDGET:-120}
init_took() { # init_took <label> <seconds>
  echo "  init ${1}: ${2}s (budget ${INIT_BUDGET}s)"
  [ "$2" -lt "$INIT_BUDGET" ] ||
    { echo "init ${1} took ${2}s, over its ${INIT_BUDGET}s budget: a wait ran out on a name that could not issue"; fail=1; }
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
    # Both are pinned: a NodePort Service with one explicit and one random
    # nodePort can have the random one allocated on 31500, and the API server
    # then refuses the second ("provided port is already allocated") -- a
    # failure that depends on what else the cluster has handed out.
    - {name: acme, port: 14000, targetPort: 14000, nodePort: 31400}
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

# location_path <location>: the path (and query) of a Location header, whether
# the app built it absolute or relative. Tautulli's are absolute and carry the
# scheme it is reached by inside the cluster -- http, since the pod's side of
# the Gateway is plain HTTP -- which is not the scheme a browser uses to reach
# the box. Only the path is the edge's and the app's shared business.
location_path() {
  case "$1" in
    *"://"*/*) printf '/%s\n' "${1#*://*/}" ;;
    *"://"*) printf '/\n' ;;
    *) printf '%s\n' "$1" ;;
  esac
}

# ---- the front door a browser meets -----------------------------------------
# What an API call never meets, because a browser is not an API client: an app
# whose redirects leave the prefix it is served under, an app whose own login
# stands in front of the box's, and a request that carries a header credential
# of its own while its session cookie is valid.
#
# assert_front_door <address> <ca-file|->
#
# Tautulli is the app under all three. It is served under /tautulli, its own
# password form is sealed off (internal/ttlconf: a non-empty http_password is
# what makes it ask), and every redirect it builds is absolute, from its own
# URL root: a route that stripped the prefix sent a signed-in browser to the
# host's /home, and its logout to the login's own /auth.
#
# The header is PeerBanHelper's exact shape: its bundle sets
# "Authorization: Bearer ${authToken}" on every /api call (webui/src/service/
# utils.ts:20), and with no token in the page's storage that is "Bearer " -- an
# empty bearer value. Authelia's ext-authz used to try a header strategy first
# and fail the request outright instead of falling through to the cookie
# (authorization.go:143-165 rejects an empty bearer, handler_authz.go:303-322
# returns on the error at 4.39.28), so a signed-in person was answered 401.
assert_front_door() {
  local addr=$1 ca=$2
  local ca_args=(--cacert "$ca")
  [ "$ca" = - ] && ca_args=(-k)
  local jar="$WORK/front-door-jar.txt"
  local hdrs="$WORK/front-door-headers.txt"
  local code location final

  # A person signs in at the portal, which is how every page behind the login
  # is reached; the app path is where they were going.
  rm -f "$jar"
  portal_login "$jar" "$LOGIN" "$AUTHELIA_PASSWORD" "https://${DOMAIN}/tautulli/" "$addr" "$ca"
  echo "  front door: POST /auth/api/firstfactor: ${PORTAL_CODE}"
  [ "$PORTAL_CODE" = 200 ] ||
    { echo "the portal refused the box's own account (${PORTAL_CODE})"; fail=1; }

  # Without a cookie the answer is the portal's redirect: the app is behind the
  # login, and what stands in front of it is root's, not the app's own form.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "https://${DOMAIN}/tautulli/" \
    2>"$WORK/front-door.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  front door: /tautulli/ with no cookie: ${code} -> ${location%%\?*}"
  [ "$code" = 302 ] ||
    { echo "/tautulli/ answered ${code} with no session, want the portal's redirect"; fail=1; }
  case "$location" in
    "https://${DOMAIN}/auth/"*) : ;;
    *) echo "/tautulli/ sent the browser to ${location}, not the portal: the app is not behind the login"; fail=1 ;;
  esac

  # Signed in, the app's own redirect stays inside the prefix it is served
  # under, and the page it points at renders. Only the redirect's path is the
  # assertion: Tautulli builds an absolute one from its URL root and the Host it
  # was asked with, in the scheme it is reached by inside the cluster, so the
  # page is asked for again on the box's own TLS.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' -b "$jar" -c "$jar" \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "https://${DOMAIN}/tautulli/" \
    2>"$WORK/front-door.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  front door: /tautulli/ signed in: ${code} -> ${location}"
  case "$code" in
    301 | 302 | 303 | 307 | 308) : ;;
    *) echo "/tautulli/ answered ${code} to a signed-in browser, want the app's own redirect"; fail=1 ;;
  esac
  page=$(location_path "$location")
  case "$page" in
    /tautulli/*) : ;;
    *) echo "/tautulli/ sent the signed-in browser to ${location}: the app does not know the prefix it is served under"; fail=1 ;;
  esac
  if [ -n "$page" ]; then
    code=$(curl -sS -o "$WORK/front-door-page.html" -w '%{http_code}' -b "$jar" -c "$jar" \
      --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "https://${DOMAIN}${page}" \
      2>"$WORK/front-door.err" || true)
    echo "  front door: GET ${page} signed in: ${code}"
    [ "$code" = 200 ] || { echo "${page} did not serve the app's page: ${code}"; fail=1; }
    grep -qi '<!doctype html' "$WORK/front-door-page.html" ||
      { echo "${page} answered ${code} without an HTML document"; fail=1; }
  fi

  # And the same request carrying a header credential of its own -- an app's
  # bundle sending the token it holds, empty because nobody gave it one -- is the
  # app's redirect again, not a 401 against a valid session. The cookie is what
  # the edge checks, and it is what a browser has.
  : >"$hdrs"
  code=$(curl -sS -o /dev/null -D "$hdrs" -w '%{http_code}' -b "$jar" \
    -H 'Authorization: Bearer ' \
    --resolve "${DOMAIN}:443:${addr}" "${ca_args[@]}" "https://${DOMAIN}/tautulli/" \
    2>"$WORK/front-door.err" || true)
  location=$(sed -n 's/^[Ll]ocation: *//p' "$hdrs" | tr -d '\r' | head -1)
  echo "  front door: /tautulli/ signed in, with a header credential of its own: ${code} -> ${location}"
  case "$code" in
    401) echo "/tautulli/ refused a signed-in request because it carried an Authorization header: the edge's authz checks the header instead of the session"; fail=1 ;;
  esac
  page=$(location_path "$location")
  case "$page" in
    /tautulli/*) : ;;
    *) [ "$code" = 401 ] || { echo "/tautulli/ answered ${code} and sent the browser to ${location}, want the app's redirect"; fail=1; } ;;
  esac
}

# ---- scenario: fresh --------------------------------------------------------
fresh() {
  # The timer's two units are written into a directory this run names: the e2e
  # is not root and this host is not the box, so bootstrap must not touch
  # systemd (--unit-dir is the development switch: files in a directory, no
  # systemctl at all).
  UNIT_ARGS=(--unit-dir "$WORK/units")

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
  grep -qF "+ Envoy Gateway ${ENVOY_GATEWAY_VERSION} (the front door)" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not install Envoy Gateway at its pinned version"; fail=1; }
  grep -qF "      + the s5n Gateway, HTTP on :80" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not apply the Gateway before the address layer"; fail=1; }
  grep -qF "? Gateway s5n-gateway/s5n getting an address" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print the Gateway's address as the question"; fail=1; }
  grep -qF "? MetalLB ${METALLB_CHART_VERSION} (the gateway's address), unless the Gateway gets an address without it" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print MetalLB as conditional"; fail=1; }
  grep -qF "      ? IPAddressPool: this node's IP as a /32, over L2" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not print the MetalLB pool as conditional"; fail=1; }
  grep -qF "the Gateway has no address (AddressNotAssigned) after waiting up to 15s; installing MetalLB" "$WORK/bootstrap1.txt" ||
    { echo "the Gateway's own status did not decide the install"; fail=1; }
  grep -qF "the Gateway answers on " "$WORK/bootstrap1.txt" ||
    { echo "box never checked that the Gateway's address answers from the host"; fail=1; }
  # The first run installs, it does not verify: the cluster had none of it.
  if grep -q "^  = Envoy Gateway" "$WORK/bootstrap1.txt"; then
    echo "bootstrap thought Envoy Gateway was already installed on a fresh cluster"; fail=1
  fi
  # cert-manager is part of the platform layer now: the plan names it at its
  # pin, and the install waits for its controller, webhook and cainjector.
  grep -qF "+ cert-manager ${CERT_MANAGER_VERSION} (certificates)" "$WORK/bootstrap1.txt" ||
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
  grep -qF "      + ClusterIssuers: Let's Encrypt, plus self-signed" "$WORK/bootstrap1.txt" ||
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
  # none of it before this run. It is s5n-<login>: this scenario passed no
  # --namespace, so the derivation from the login is what put it here.
  grep -qF "+ Namespace ${NS} (Pod Security restricted)" "$WORK/bootstrap1.txt" ||
    { echo "the plan did not show the namespace it creates"; fail=1; }
  [ "$(sed -n 's/^namespace: *//p' "$WORK/grant.yaml")" = "$NS" ] ||
    { echo "the grant's namespace is not ${NS} (s5n-<login>):"; grep -n '^namespace:' "$WORK/grant.yaml"; fail=1; }
  echo "  the grant's namespace is ${NS}, which no flag named"

  # ---- root's config, and the timer that makes updates happen ---------------
  # bootstrap writes root's box.yaml once, beside the cluster record (the same
  # rule --record follows: move the record and the file moves with it), and
  # never again: the comments are its documentation and the answers below them
  # are root's. The mode is the notice address's, which can carry a token.
  conf="$WORK/box.yaml"
  [ -f "$conf" ] || { echo "bootstrap wrote no box.yaml beside the record"; fail=1; }
  cat >"$WORK/box.yaml.expected" <<'EOF'
# box's settings for this host. box never rewrites this file; edit it by hand.
update:
  timer: on        # off: no update checks and no health notices
  interval: 1h     # how often the timer checks for a newer release; 15m at least
  hold-box: false  # true: take no new box, the same as --hold-box on every run
notices:
  url: ""          # box POSTs a JSON notice here after updates and on health changes
EOF
  cmp -s "$WORK/box.yaml.expected" "$conf" ||
    { echo "box.yaml is not the file bootstrap writes:"; diff "$WORK/box.yaml.expected" "$conf" | head -5; fail=1; }
  conf_mode=$(stat -c '%a' "$conf" 2>/dev/null || true)
  [ "$conf_mode" = 600 ] ||
    { echo "box.yaml is mode ${conf_mode}, want 600: the notice address can carry a token"; fail=1; }
  conf_sha=$(sha256sum "$conf" | cut -d' ' -f1)

  # The timer is two files box owns, and box wrote them here because this run
  # named a directory for them. The service is the box under test -- this
  # process's own binary -- with the three paths this run installed, so a host
  # with two boxes or two seedboxes updates the one it bootstrapped, and no
  # timeout, because an update waits out an app's startup budget.
  units="$WORK/units"
  for unit in box-update.service box-update.timer; do
    [ -f "$units/$unit" ] || { echo "bootstrap wrote no ${unit}"; fail=1; continue; }
    unit_mode=$(stat -c '%a' "$units/$unit" 2>/dev/null || true)
    [ "$unit_mode" = 644 ] ||
      { echo "${unit} is mode ${unit_mode}, want 644: systemd reads it, not root alone"; fail=1; }
  done
  exec_start=$(sed -n 's/^ExecStart=//p' "$units/box-update.service" 2>/dev/null | sed 's/[[:space:]]*$//' || true)
  echo "  ExecStart: ${exec_start}"
  [ "$exec_start" = "${WORK}/box update --timer --record ${WORK}/cluster.yaml --grant ${WORK}/grant.yaml --values ${WORK}/values.yaml" ] ||
    { echo "box-update.service does not run this box for the files this run installed"; fail=1; }
  grep -qxF "TimeoutStartSec=0" "$units/box-update.service" 2>/dev/null ||
    { echo "box-update.service has a start timeout: an update can wait out an app's startup budget"; fail=1; }
  # The timer wakes box often and box decides what a wake is worth, so the unit
  # carries no schedule of its own beyond the wake: ten minutes after boot, then
  # every fifteen, with up to ten minutes of jitter so a fleet does not arrive
  # at the registry at once.
  for line in OnBootSec=10min OnUnitActiveSec=15min RandomizedDelaySec=10min WantedBy=timers.target; do
    grep -qxF "$line" "$units/box-update.timer" 2>/dev/null ||
      { echo "box-update.timer has no ${line}:"; cat "$units/box-update.timer" 2>/dev/null | head -12; fail=1; }
  done
  # What a re-run must not touch: the bytes, and the mtime a rewrite would move
  # even when the bytes came out the same.
  unit_state() {
    for unit in box-update.service box-update.timer; do
      printf '%s %s %s\n' "$unit" "$(sha256sum "$units/$unit" | cut -d' ' -f1)" "$(stat -c '%y' "$units/$unit")"
    done
  }
  # --unit-dir writes the files and starts nothing: the systemd these are for is
  # not this host's, and no test may touch it.
  if grep -q "systemctl" "$WORK/bootstrap1.txt"; then
    echo "--unit-dir ran systemctl: $(grep -m1 "systemctl" "$WORK/bootstrap1.txt")"; fail=1
  fi
  grep -qF "box checks for a newer release every 15 minutes: box-update.timer" "$WORK/bootstrap1.txt" ||
    { echo "bootstrap did not say what it scheduled"; fail=1; }

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
  # ---- the front door a browser meets ---------------------------------------
  # An API client never sees these: Tautulli's own login, the prefix its
  # redirects are built from, and a request that carries a header credential.
  log "the front door: signed in, an app under its prefix, and a header the edge ignores"
  assert_front_door "$gw_addr" "$WORK/pebble-root.pem"
  # ---- storage: box created the claims, and they are bound ----
  for kind in data config; do
    pv=${NS}-s5n-${kind}
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
  [ "$($K get pv ${NS}-s5n-data -o jsonpath='{.spec.hostPath.path}')" = /srv/s5n/data ] || fail=1
  [ "$($K get pv ${NS}-s5n-config -o jsonpath='{.spec.hostPath.path}')" = /srv/s5n/config ] || fail=1

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
  unit_before2=$(unit_state)

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
  grep -qF "= MetalLB ${METALLB_CHART_VERSION} (the gateway's address): installed, verifying" "$WORK/bootstrap2.txt" ||
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
  if grep -qE "^\s+[+?] (Envoy Gateway|MetalLB) " "$WORK/bootstrap2.txt"; then
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
  $K apply -f - >/dev/null <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: e2e-other-host
  namespace: ${NS}
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

  # Root's config and the timer's units are box's too, and a re-run of the same
  # answers must leave both exactly as they were: the file is root's from
  # bootstrap's write on, and units whose bytes are unchanged are not written
  # again at all (a rewrite would restart the timer for no reason).
  [ "$(sha256sum "$conf" | cut -d' ' -f1)" = "$conf_sha" ] ||
    { echo "the second bootstrap rewrote box.yaml"; fail=1; }
  unit_after=$(unit_state)
  if [ "$unit_after" != "$unit_before2" ]; then
    echo "the second bootstrap rewrote the timer's units"
    echo "  before: ${unit_before2}"
    echo "  after:  ${unit_after}"
    fail=1
  else
    echo "the timer's units are untouched: ${unit_after}"
  fi
  if grep -q "systemctl" "$WORK/bootstrap2.txt"; then
    echo "the second bootstrap ran systemctl: $(grep -m1 "systemctl" "$WORK/bootstrap2.txt")"; fail=1
  fi

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

  # ---- the timer's wake: due, then not due, then off -------------------------
  # One wake of the unit, by hand. Everything about a wake follows from
  # update.json's last check: the first one on a box that has never checked is
  # due, and on a box already on the newest release it is a registry listing and
  # nothing else. The second wake, seconds later, is not due at all.
  update_state="$WORK/update.json"
  revision_before_wake=$(revision)
  last_before=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("lastCheck") or "")' "$update_state" 2>/dev/null || true)
  echo "  last check before the wake: '${last_before}'"
  log "box update --timer: a due wake on a box that is already current"
  if "$WORK/box" update --timer --chart-repo "$CHART_REPO" --record "$WORK/cluster.yaml" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/update-wake.txt" 2>&1; then
    cat "$WORK/update-wake.txt"
  else
    cat "$WORK/update-wake.txt"; fail=1
  fi
  grep -qF "s5n ${CHART_VERSION} in ${NS} is current" "$WORK/update-wake.txt" ||
    { echo "the due wake did not check the registry and find the box current:"; grep -i "current\|chart" "$WORK/update-wake.txt" | head -5; fail=1; }
  revision_after_wake=$(revision)
  [ "$revision_after_wake" = "$revision_before_wake" ] ||
    { echo "the due wake moved the release to revision ${revision_after_wake} (was ${revision_before_wake})"; fail=1; }
  last_after=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("lastCheck") or "")' "$update_state") || true
  echo "  last check after the due wake: '${last_after}'"
  [ -n "$last_after" ] ||
    { echo "the due wake did not record when it checked: the next wake would be due again"; fail=1; }

  log "box update --timer: the next wake is not due, so it asks the registry nothing"
  if "$WORK/box" update --timer --chart-repo "$CHART_REPO" --record "$WORK/cluster.yaml" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/update-wake2.txt" 2>&1; then
    cat "$WORK/update-wake2.txt"
  else
    cat "$WORK/update-wake2.txt"; fail=1
  fi
  if grep -q "^chart: " "$WORK/update-wake2.txt"; then
    echo "a wake that is not due loaded a chart: it contacted the registry:"; grep "^chart: " "$WORK/update-wake2.txt" | head -3; fail=1
  fi
  last_notdue=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("lastCheck") or "")' "$update_state") || true
  [ "$last_notdue" = "$last_after" ] ||
    { echo "a wake that is not due moved lastCheck from ${last_after} to ${last_notdue}"; fail=1; }
  revision_after_wake2=$(revision)
  [ "$revision_after_wake2" = "$revision_before_wake" ] ||
    { echo "a wake that is not due moved the release to revision ${revision_after_wake2}"; fail=1; }

  # box status is the one line the four facts are read from. The release, the
  # last step, when box checks next, and what is held.
  log "box status: one line, from the files box already wrote"
  status_line=$("$WORK/box" status --record "$WORK/cluster.yaml" --grant "$WORK/grant.yaml" \
    --values "$WORK/values.yaml" --kubeconfig "$WORK/kubeconfig" --unit-dir "$units" 2>&1) || true
  echo "  ${status_line}"
  case "$status_line" in
    "s5n ${CHART_VERSION} · "*) ;;
    *) echo "box status does not start with the release it is on, want 's5n ${CHART_VERSION} · '"; fail=1 ;;
  esac
  case "$status_line" in
    *"next check "*) ;;
    *) echo "box status does not say when box checks next"; fail=1 ;;
  esac
  case "$status_line" in
    *"last update none"*) ;;
    *) echo "box status names a last update on a box that has never stepped"; fail=1 ;;
  esac
  # The unit is what decides whether box checks at all: no timer file in the
  # directory status looks in is "timer not installed", and it comes before any
  # clock time.
  status_none=$("$WORK/box" status --record "$WORK/cluster.yaml" --grant "$WORK/grant.yaml" \
    --values "$WORK/values.yaml" --kubeconfig "$WORK/kubeconfig" --unit-dir "$WORK/no-units" 2>&1) || true
  echo "  ${status_none}"
  case "$status_none" in
    *"timer not installed"*) ;;
    *) echo "box status did not say the timer is not installed where no unit is"; fail=1 ;;
  esac

  # `update.timer: off` is the switch, and it is an edit in root's file: the
  # wake obeys it, says where the switch is, writes nothing at all -- and
  # update.json's bytes are the proof.
  log "update.timer: off: the wake says so, exits 0, and writes nothing"
  sed -i 's/^  timer: on  /  timer: off /' "$conf"
  grep -q "^  timer: off " "$conf" || { echo "could not turn the timer off in ${conf}"; fail=1; }
  state_before_off=$(sha256sum "$update_state" | cut -d' ' -f1)
  if "$WORK/box" update --timer --chart-repo "$CHART_REPO" --record "$WORK/cluster.yaml" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/update-off.txt" 2>&1; then
    cat "$WORK/update-off.txt"
  else
    cat "$WORK/update-off.txt"; echo "a wake with the timer off exited non-zero"; fail=1
  fi
  grep -qxF "box does not check for updates by itself: ${conf} says update.timer: off" "$WORK/update-off.txt" ||
    { echo "the switched-off wake did not say the one line naming the file:"; cat "$WORK/update-off.txt"; fail=1; }
  [ "$(sha256sum "$update_state" | cut -d' ' -f1)" = "$state_before_off" ] ||
    { echo "a wake with the timer off wrote update.json"; fail=1; }
  if grep -q "^chart: " "$WORK/update-off.txt"; then
    echo "a wake with the timer off contacted the registry"; fail=1
  fi
  status_off=$("$WORK/box" status --record "$WORK/cluster.yaml" --grant "$WORK/grant.yaml" \
    --values "$WORK/values.yaml" --kubeconfig "$WORK/kubeconfig" --unit-dir "$units" 2>&1) || true
  echo "  ${status_off}"
  case "$status_off" in
    *"· timer off") ;;
    *) echo "box status does not end with 'timer off' while the file says off"; fail=1 ;;
  esac
  # Put the switch back: the rest of the scenario is the install's, and root's
  # file is bootstrap's to leave alone.
  sed -i 's/^  timer: off /  timer: on  /' "$conf"
  grep -q "^  timer: on  " "$conf" || { echo "could not put the timer back on in ${conf}"; fail=1; }

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

  # ---- a box with no apps at all --------------------------------------------
  # An empty box is a state box installs and reports, not a refusal: it is what
  # `box init --apps none` writes, what the picker leaves when nothing is
  # ticked, and the box `box migrate apply` adopts into before it switches the
  # plan's apps on. So every app goes off in one init, and what has to be true
  # afterwards is that the release is still deployed and the Job still publishes
  # an ok receipt -- the Job is the release's Helm hook, so a Job that failed on
  # a config with no app in it would make an empty box uninstallable.
  log "box init --apps none: every app off is an install, not a refusal"
  APPS=none
  if init "$WORK/init-none.txt"; then
    cat "$WORK/init-none.txt"
  else
    cat "$WORK/init-none.txt"; fail=1
  fi
  apps_on=$(python3 - "$WORK/grant.yaml" <<'PY' || true
import sys, yaml
grant = yaml.safe_load(open(sys.argv[1])) or {}
print(",".join(n for n, a in (grant.get("apps") or {}).items() if (a or {}).get("enabled")))
PY
  )
  echo "  apps on after --apps none: ${apps_on:-none}"
  [ -z "$apps_on" ] || { echo "the grant still has ${apps_on} on after --apps none"; fail=1; }
  grep -qF "nothing runs yet" "$WORK/init-none.txt" ||
    { echo "init did not say what an empty box is"; fail=1; }
  # The wiring Job ran inside that apply. Its receipt is the one `box apply`
  # waits for, and with nothing to wire it says ok and writes nothing.
  wiring_status=$(status_field 'd["status"]' 2>/dev/null || echo missing)
  created=$(status_field 'd["writes"]["created"]' 2>/dev/null || echo missing)
  updated=$(status_field 'd["writes"]["updated"]' 2>/dev/null || echo missing)
  echo "  receipt after --apps none: ${wiring_status}, created ${created}, updated ${updated}"
  [ "$wiring_status" = ok ] || { echo "the Job's receipt is ${wiring_status}, want ok: it is the release's hook"; fail=1; }
  [ "$created" = 0 ] && [ "$updated" = 0 ] ||
    { echo "a run with no apps created ${created} and updated ${updated} objects"; fail=1; }

  # And nothing of the apps is left running: a release with every app off has a
  # Secret, a ServiceAccount, the Job and the policies, and no workload.
  apps_pods() {
    $K -n "$NS" get pods -o name 2>/dev/null |
      grep -cE 'pod/(qbittorrent|sonarr|radarr|prowlarr|plex|tautulli|seerr|qui|autobrr|maintainerr|pbh)(-|$)' || true
  }
  for i in $(seq 1 120); do
    [ "$(apps_pods)" = 0 ] && break
    sleep 2
  done
  echo "  app pods after --apps none: $(apps_pods)"
  [ "$(apps_pods)" = 0 ] ||
    { echo "the apps are still running after --apps none: $($K -n "$NS" get pods -o name | tr '\n' ' ')"; fail=1; }

  # `box status` is the one line an operator reads, and an empty box is on it:
  # the release is deployed, so the line names it instead of "?".
  status_line=$("$WORK/box" status --record "$WORK/cluster.yaml" --grant "$WORK/grant.yaml" \
    --values "$WORK/values.yaml" --kubeconfig "$WORK/kubeconfig" 2>&1) || { echo "box status failed on an empty box"; fail=1; }
  echo "  box status: ${status_line}"
  case "$status_line" in
    "s5n ${CHART_VERSION} · "*) : ;;
    *) echo "box status does not report the deployed release on an empty box: ${status_line}"; fail=1 ;;
  esac

  # ---- the claims outlive the release ----------------------------------------
  # The one destructive command an operator runs is `helm uninstall`: the data
  # must not go with it. The claims and their volumes are not release objects, so
  # this is not reclamation behaviour box configured -- it is what "not Helm's"
  # means, and it is worth asserting rather than assuming.
  log "helm uninstall keeps the claims and their volumes"
  $HELM uninstall s5n -n "$NS" >/dev/null 2>&1 || { echo "helm uninstall failed"; fail=1; }
  for kind in data config; do
    pv=${NS}-s5n-${kind}
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
  grep -qF "? MetalLB ${METALLB_CHART_VERSION} (the gateway's address), unless the Gateway gets an address without it" "$WORK/bootstrap1.txt" ||
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
  # One wait for the run's Certificates, not one per host: this run decided
  # every name, and a box whose names cannot issue yet pays one
  # CertificateTimeout for the run instead of one per name. The Done line says
  # how many of them came Ready.
  cert_waits=$(grep -c 'Certificates to be Ready' "$WORK/bootstrap1.txt" || true)
  [ "$cert_waits" = 1 ] ||
    { echo "init printed ${cert_waits} Certificate wait lines, want one for the run's hosts"; fail=1; }
  grep -qE '^[0-9]+ of [0-9]+ Certificates are Ready$' "$WORK/bootstrap1.txt" ||
    { echo "init's Certificate wait did not say how many of the run's hosts came Ready"; fail=1; }
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
  log "the front door on a self-signed host: the same three answers"
  assert_front_door "$lb_addr" "$WORK/anchor-ca.pem"

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
  cert=$(host_cert "$DOMAIN")
  app_cert=$(host_cert "$SEERR_HOST")
  # Every name this scenario installs for, and the Certificate each one is. The
  # move to the ACME issuer is asserted on all of them, not only the two the
  # no-DNS card and the sign-in checks happen to name: a name left out of the
  # cluster's DNS is a certificate that can never issue, and the install stays
  # green while that host keeps serving its self-signed one.
  hosts=("$DOMAIN" "$PLEX_HOST" "$SEERR_HOST" "$PBH_HOST")
  certs=()
  for host in "${hosts[@]}"; do certs+=("$(host_cert "$host")"); done
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
  # The serial every name holds before the move, so the assertion after it is
  # per name: a Certificate that was never re-issued keeps the one it had, and
  # that is what a name with no answer in the cluster looks like.
  : >"$WORK/serials-before.txt"
  for name in "${certs[@]}"; do printf '%s %s\n' "$name" "$(tls_serial "$name")" >>"$WORK/serials-before.txt"; done

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
  # Every name the install serves, not only the two the checks below used to
  # read. cert-manager's HTTP-01 self-check and Pebble resolve the challenge's
  # name through this resolver: a name missing here never validates, so its
  # Certificate never issues and init sits out the whole CertificateTimeout on
  # it. The fresh scenario patches all of its names for the same reason.
  patch_coredns "$lb_addr" "${hosts[@]}" ||
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
  # Every name the move should have re-issued, not two of them: the Certificate
  # is Ready when its current generation is the one it issued for (a Ready left
  # from the previous certificate is what made this look done), it names the
  # ACME issuer, and the serial in its Secret is not the one it served before.
  # The serial is polled, because issuance is asynchronous: it is the evidence
  # that a new certificate exists, not that a condition says so.
  for name in "${certs[@]}"; do
    got=$($K -n s5n-gateway get certificate "$name" -o jsonpath='{.spec.issuerRef.name}')
    [ "$got" = s5n-letsencrypt ] || { echo "${name} still names issuer ${got}"; fail=1; }
    was=$(awk -v n="$name" '$1 == n {print $2}' "$WORK/serials-before.txt")
    new_serial=""
    for _ in $(seq 1 60); do
      new_serial=$(tls_serial "$name")
      [ -n "$new_serial" ] && [ "$new_serial" != "$was" ] && break
      sleep 2
    done
    echo "  ${name}: serial ${was} -> ${new_serial:-none}"
    if [ -z "$new_serial" ] || [ "$new_serial" = "$was" ]; then
      echo "the certificate ${name} serves was not re-issued: the name has no answer in the cluster's DNS, so its challenge cannot validate"
      fail=1
      continue
    fi
    $K -n s5n-gateway wait --for=condition=Ready "certificate/$name" --timeout=60s ||
      { echo "${name} was re-issued but is not Ready"; fail=1; }
  done

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
  started=$(date +%s)
  if init "$WORK/init-ca.txt" --ca-file "$WORK/pebble-root.pem"; then
    grep -qF "wrote ConfigMap ${NS}/s5n-host-ca" "$WORK/init-ca.txt" ||
      { echo "box did not report writing the anchor from --ca-file"; cat "$WORK/init-ca.txt"; fail=1; }
  else
    cat "$WORK/init-ca.txt"; fail=1
  fi
  init_took "with --ca-file" $(( $(date +%s) - started ))
  same_cert "$($K -n "$NS" get configmap s5n-host-ca -o jsonpath='{.data.ca\.crt}' 2>/dev/null || true)" \
    "$(cat "$WORK/pebble-root.pem")" ||
    { echo "--ca-file did not reach ${NS}/s5n-host-ca"; fail=1; }
  log "box init: an ACME host's anchor goes again"
  # Without the dev flag: the move above is the only run that passes it, and
  # this is the assertion that a public host carries no anchor.
  CA_ARGS=()
  started=$(date +%s)
  if init "$WORK/init-noca.txt"; then :; else cat "$WORK/init-noca.txt"; fail=1; fi
  init_took "without --ca-file" $(( $(date +%s) - started ))
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

# ---- scenario: update -------------------------------------------------------
# `box update` on a real cluster: a step that moves two apps, one of whose new
# images cannot start. box must put that app back on the image it ran -- its
# pods gone, its config restored, a hold naming it -- leave the other app moved,
# leave the release at the new version, and say all of it. Then a re-apply keeps
# the hold, a release that pins another image lapses it, and `box rollback --pin`
# restores the newest copy as an ordinary values pin. Last, `box apply` keeps
# the installed release (refusing one it was not given by `box update`), copies
# sonarr before a values pin moves it, and `box rollback --pin` restores that
# copy.
#
# Three of the images this needs are not in any chart: a sonarr that rewrites a
# file a snapshot glob names and exits (release B), prowlarr's own image under a
# new tag (B, the app that must stay moved), and sonarr's own image under a new
# tag (C, the release that lapses the hold). A built or retagged image has no
# digest, so those releases pin by tag alone -- the shape a hold and a step both
# have to read for a tag-only release to move or lapse at all.

# What the two new releases pin. The broken sonarr is built here; the other two
# are images this cluster already runs, retagged.
BROKEN_TAG=e2e-broken
MOVED_TAG=e2e-moved
RESTORED_TAG=e2e-restored
# What the operator pins sonarr to by hand after the last rollback: sonarr's
# own image under a new tag, so the app `box apply` moves comes back healthy.
APPLIED_TAG=e2e-applied
# busybox 1.37.0, the base of the image release B ships for sonarr: a shell that
# rewrites the config and then exits, which is a migration that cannot serve,
# seen from outside.
BUSYBOX_VERSION=1.37.0
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:${BUSYBOX_VERSION}@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec}  # renovate: datasource=docker
# The line release A's sonarr carries in config.xml, a file the catalog's
# snapshot globs name, and the config the failing image replaces that file with:
# after the step only the copy box took can hold the first and none of the
# second.
MARKER='<!-- s5n-e2e-update-marker -->'
BROKEN_WROTE='s5n-e2e-broken-image-rewrote-this'

# next_patch <version>: the next patch release, which is what a chart whose only
# change is an app's image is published as.
next_patch() {
  python3 -c 'import sys; v = sys.argv[1].split("."); v[-1] = str(int(v[-1]) + 1); print(".".join(v))' "$1"
}

# variant_pin <app>: "repository tag digest" for one app's default variant, read
# from this tree's variants.yaml -- the image release A runs. The scenario
# retags that image and pins the tag, so it reads the pin instead of repeating
# it: a renamed repository or a bumped tag in the chart is this scenario's input.
variant_pin() {
  python3 - "$1" <<'PY'
import re, sys
app, cur, pin = sys.argv[1], None, {}
for line in open("charts/s5n/variants.yaml"):
    head = re.match(r'^([A-Za-z0-9_.-]+):\s*$', line)
    if head:
        cur = head.group(1)
        continue
    if cur != app:
        continue
    m = re.match(r'^\s+(\w+):\s*"?([^"\s]*)"?\s*$', line)
    if m:
        pin.setdefault(m.group(1), m.group(2))
if not pin.get("repository") or not pin.get("tag"):
    sys.exit("charts/s5n/variants.yaml names no default image for " + app)
print(pin["repository"], pin["tag"], pin.get("digest", ""))
PY
}

# chart_image <app>: the one image reference the chart's own pin names, in the
# form both a pod's image and a docker run take.
chart_image() {
  variant_pin "$1" |
    awk '{ if ($3 != "") printf "%s:%s@%s\n", $1, $2, $3; else printf "%s:%s\n", $1, $2 }'
}

# repin_variant <variants.yaml> <app> <tag>: one app's default variant names a
# tag inside its own repository and no digest. The repository stays, because a
# hold puts an app back by the tag and digest a snapshot recorded and the chart
# is what supplies the repository: a release that moved an app to another
# repository could not be rolled back at all.
repin_variant() {
  python3 - "$1" "$2" "$3" <<'PY'
import re, sys
path, app, tag = sys.argv[1], sys.argv[2], sys.argv[3]
out, cur = [], None
for line in open(path):
    head = re.match(r'^([A-Za-z0-9_.-]+):\s*$', line)
    if head:
        cur = head.group(1)
    m = re.match(r'^(\s+)(tag|digest):', line)
    if cur == app and m:
        if m.group(2) == "tag":
            out.append('%stag: "%s"\n' % (m.group(1), tag))
        continue
    out.append(line)
open(path, "w").write("".join(out))
PY
}

# update_chart <dest> <version> <app> <tag> [<app> <tag> ...]: this tree's chart
# at another version, with those apps re-pinned to a tag. It is written into
# $WORK, so the scenario adds no file to the checkout, and it is published to
# the same registry the tree's own release went to.
update_chart() {
  local dest=$1 version=$2
  shift 2
  rm -rf "$dest"
  cp -R charts/s5n "$dest"
  sed -i "s/^version: .*/version: ${version}/" "$dest/Chart.yaml"
  while [ "$#" -gt 0 ]; do
    repin_variant "$dest/variants.yaml" "$1" "$2"
    shift 2
  done
  helm package "$dest" --destination "$WORK/charts" >/dev/null
  helm push "$WORK/charts/s5n-${version}.tgz" "oci://127.0.0.1:${REGISTRY_PORT}/charts" --plain-http >/dev/null
  echo "  published s5n ${version}"
}

# pod_image <pod>: the image reference the pod's app container runs.
pod_image() {
  $K -n "$NS" get pod "$1" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null || true
}

# wait_image_ready <pod> <image>: a deadline, never a bare sleep. The step that
# puts an app back re-renders the release and the StatefulSet replaces the pod,
# so both the image and the readiness arrive after the command that asked.
wait_image_ready() {
  local pod=$1 want=$2 got="" ready="" i
  for i in $(seq 1 150); do
    got=$(pod_image "$pod") || true
    ready=$($K -n "$NS" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) || true
    if [ "$got" = "$want" ] && [ "$ready" = True ]; then
      echo "  ${pod} ${got} (${ready})"
      return 0
    fi
    sleep 2
  done
  echo "  ${pod}: image ${got:-none}, Ready ${ready:-none}"
  return 1
}

# helm_chart_version: the chart version Helm has deployed, as "0.4.1".
helm_chart_version() {
  $HELM list -n "$NS" -o json |
    python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["chart"].split("-",1)[1] if r else "")'
}

# holds_assert <config root> <app> held|gone <tag> <digest>: the box's own hold
# file, read the way box reads it. gone means no hold for the app, and no file
# at all is the same answer.
holds_assert() {
  python3 - "$1" "$2" "$3" "$4" "$5" <<'PY'
import json, os, sys
root, app, state, tag, digest = sys.argv[1:6]
path = os.path.join(root, ".box", "holds.json")
if not os.path.exists(path):
    if state == "gone":
        print(f"  {path} is gone: {app} is not held")
        sys.exit(0)
    sys.exit(f"no hold file at {path}: {app} is not held")
holds = json.load(open(path)).get("holds") or []
mine = [h for h in holds if h.get("app") == app]
if state == "gone":
    if mine:
        sys.exit(f"{path} still holds {app}: {mine}")
    print(f"  {path} names no hold for {app}")
    sys.exit(0)
if len(mine) != 1:
    sys.exit(f"{path} names {app} {len(mine)} times, want once: {holds}")
h = mine[0]
if h["at"].get("tag") != tag or (h["at"].get("digest") or "") != digest:
    sys.exit(f"{app}'s hold is at {h['at']}, want {tag}@{digest}")
if h.get("by") != "update":
    sys.exit(f"{app}'s hold was written by {h.get('by')!r}, want 'update'")
print(f"  {path}: {app} held at {tag}@{digest}, avoiding {h['avoiding'].get('tag')}")
PY
}

update_box() {
  # ---- release A ------------------------------------------------------------
  # The tree's chart as it is, installed the way every scenario installs one.
  log "box bootstrap: the platform layer, then root's first install (release A, ${CHART_VERSION})"
  if bootstrap "$WORK/bootstrap1.txt"; then
    cat "$WORK/bootstrap1.txt"
  else
    cat "$WORK/bootstrap1.txt"; fail=1; return 0
  fi
  grep -qF "chart: s5n ${CHART_VERSION} (${CHART_REPO}:${CHART_VERSION})" "$WORK/bootstrap1.txt" ||
    { echo "box did not pull ${CHART_REPO}:${CHART_VERSION}"; fail=1; }
  grep -q "^wiring ok" "$WORK/bootstrap1.txt" ||
    { echo "box did not report a healthy receipt"; fail=1; }
  [ "$fail" = 0 ] || return 0

  # ---- notices: the receiver box posts to -----------------------------------
  # box on this host posts to root's address; the pods never do, so a receiver
  # here is where a step's notices land. It is a stdlib http.server that appends
  # each body as one line, bound to a port the kernel picks (written where this
  # script can read it), and it is killed by pid.
  cat >"$WORK/notice-receiver.py" <<'PY'
import http.server, os, sys
port_file, log = sys.argv[1], sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        with open(log, "a") as f:
            f.write(body.decode("utf-8", "replace") + "\n")
        self.send_response(204)
        self.end_headers()

    def log_message(self, *args):
        pass

server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(port_file + ".tmp", "w") as f:
    f.write(str(server.server_port))
os.replace(port_file + ".tmp", port_file)
server.serve_forever()
PY
  rm -f "$WORK/notice-port" "$WORK/notices.log"
  python3 "$WORK/notice-receiver.py" "$WORK/notice-port" "$WORK/notices.log" &
  NOTICE_PID=$!
  for i in $(seq 1 100); do
    [ -s "$WORK/notice-port" ] && break
    sleep 0.1
  done
  NOTICE_PORT=$(cat "$WORK/notice-port" 2>/dev/null || true)
  [ -n "$NOTICE_PORT" ] || { echo "the notice receiver never bound a port"; fail=1; return 0; }
  echo "  notice receiver on 127.0.0.1:${NOTICE_PORT} (pid ${NOTICE_PID})"
  # Root's file, edited by hand the way an operator edits it: the address is a
  # secret with a token in it, and box never writes this line.
  NOTICE_URL="http://127.0.0.1:${NOTICE_PORT}/hook/T00/s3cret?token=abc"
  sed -i "s|^  url: \"\".*|  url: \"${NOTICE_URL}\"|" "$WORK/box.yaml"
  grep -qF "${NOTICE_URL}" "$WORK/box.yaml" ||
    { echo "could not set notices.url in $WORK/box.yaml"; fail=1; return 0; }

  # ---- the marker, and what both apps run before the update ------------------
  log "sonarr's config gets a marker only the snapshot can bring back"
  sonarr_image=$(pod_image sonarr-0) || true
  prowlarr_image=$(pod_image prowlarr-0) || true
  if [ -z "$sonarr_image" ] || [ -z "$prowlarr_image" ]; then
    echo "sonarr-0/prowlarr-0 do not say what they run: '${sonarr_image}' '${prowlarr_image}'"
    fail=1; return 0
  fi
  echo "  sonarr runs ${sonarr_image}"
  echo "  prowlarr runs ${prowlarr_image}"
  # Written through the pod, so the file the failing image overwrites is the
  # file box copies: the same tree, seen from the seedbox's side.
  $K -n "$NS" exec sonarr-0 -c sonarr -- /bin/sh -c "
    set -eu
    grep -qF '$MARKER' /config/config.xml && exit 0
    sed -i 's#</Config>#    $MARKER\n</Config>#' /config/config.xml
    grep -qF '$MARKER' /config/config.xml" ||
    { echo "could not put the marker into sonarr's config through the pod"; fail=1; return 0; }
  grep -qF "$MARKER" "$CONFIG_PATH/sonarr/config.xml" ||
    { echo "the marker the pod wrote is not in ${CONFIG_PATH}/sonarr/config.xml: the pod's /config is not the tree box copies"; fail=1; return 0; }

  # ---- release B ------------------------------------------------------------
  log "release B: sonarr moves to an image that cannot start, prowlarr to another tag"
  release_b=$(next_patch "$CHART_VERSION") || true
  release_c=$(next_patch "$release_b") || true
  sonarr_pin=$(variant_pin sonarr) || true
  prowlarr_pin=$(variant_pin prowlarr) || true
  if [ -z "$release_b" ] || [ -z "$release_c" ] || [ -z "$sonarr_pin" ] || [ -z "$prowlarr_pin" ]; then
    echo "could not read the next two releases ('${release_b}', '${release_c}') or the chart's sonarr/prowlarr pins ('${sonarr_pin}', '${prowlarr_pin}')"
    fail=1; return 0
  fi
  read -r sonarr_repo sonarr_tag sonarr_digest <<<"$sonarr_pin"
  read -r prowlarr_repo prowlarr_tag prowlarr_digest <<<"$prowlarr_pin"

  # The image release B ships for sonarr: a shell that rewrites config.xml and
  # exits, so the kubelet restarts it until box calls it a crashloop. Built
  # here from a pinned busybox rather than published, and loaded into the node
  # because a kind node cannot reach this host's registry.
  mkdir -p "$WORK/image"
  cat >"$WORK/image/Dockerfile" <<EOF
# busybox ${BUSYBOX_VERSION}, the base of the image release B ships for sonarr.
FROM ${BUSYBOX_IMAGE}
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/bin/sh", "/entrypoint.sh"]
EOF
  cat >"$WORK/image/entrypoint.sh" <<EOF
#!/bin/sh
# What a migration that rewrote its config and then could not serve looks like
# from outside. config.xml is a file the catalog says an upgrade can rewrite, so
# the copy box took before the step is what holds the state this replaced.
set -eu
printf '%s\n' '<Config><System><Version>${BROKEN_WROTE}</Version></System></Config>' >/config/config.xml
echo "s5n e2e: this image cannot start" >&2
exit 1
EOF
  docker build -q -t "${sonarr_repo}:${BROKEN_TAG}" "$WORK/image" >/dev/null
  $KIND load docker-image --name "$CLUSTER" "${sonarr_repo}:${BROKEN_TAG}" >/dev/null

  # The app the same step moves and box must leave alone: prowlarr's own image,
  # retagged in the node, so the release names a tag this cluster already runs.
  retag_pinned "$prowlarr_repo" "$prowlarr_digest" "$MOVED_TAG" || { fail=1; return 0; }

  update_chart "$WORK/chart-b" "$release_b" sonarr "$BROKEN_TAG" prowlarr "$MOVED_TAG"

  # ---- the step -------------------------------------------------------------
  log "box update: the step moves both apps, and box puts sonarr back on the image it ran"
  update_b_started=$(date -u +%Y-%m-%dT%H:%M:%SZ) || true
  if "$WORK/box" update --yes --chart-repo "$CHART_REPO" --record "$WORK/cluster.yaml" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/update-b.txt" 2>&1; then
    cat "$WORK/update-b.txt"
  else
    cat "$WORK/update-b.txt"; fail=1; return 0
  fi

  # What box says, in the words the step-back and the hold write. The first
  # sentence is the announcement before anything stops; the second is what it
  # restored and what the app is now waiting on.
  grep -Eq "sonarr exited [0-9]+ times on ${BROKEN_TAG}: rolling it back to ${sonarr_tag}" "$WORK/update-b.txt" ||
    { echo "box did not announce rolling sonarr back from ${BROKEN_TAG} to ${sonarr_tag}:"; grep -i "sonarr" "$WORK/update-b.txt" | head -5; fail=1; }
  grep -qF "sonarr is back on ${sonarr_tag} with its snapshot from" "$WORK/update-b.txt" ||
    { echo "box did not say sonarr was back on ${sonarr_tag} with its copy:"; grep -i "back on" "$WORK/update-b.txt" | head -5; fail=1; }

  # What it recorded: the step passed, and the app it put back is in the record
  # `box status` reads. update.json sits beside the cluster record, which is
  # what --record names.
  python3 - "$WORK/update.json" "$release_b" <<'PY' || fail=1
import json, sys
path, want = sys.argv[1], sys.argv[2]
state = json.load(open(path))
step = state.get("lastStep") or {}
if step.get("to") != want:
    sys.exit(f"{path}: the last step went to {step.get('to')!r}, want {want!r}")
if step.get("outcome") != "ok":
    sys.exit(f"{path}: the last step ended {step.get('outcome')!r}: {step.get('error')}")
if step.get("rolledBack") != ["sonarr"]:
    sys.exit(f"{path}: rolledBack = {step.get('rolledBack')!r}, want ['sonarr']")
print(f"  update.json: {step.get('from')} -> {step.get('to')} {step.get('outcome')}, rolled back {step.get('rolledBack')}")
PY

  # Sonarr is on the image release A ran and ready: the pods the failing image
  # left behind are gone, so this is the old image serving again.
  wait_image_ready sonarr-0 "$sonarr_image" ||
    { echo "sonarr-0 is not ready on ${sonarr_image} after the step (it runs $(pod_image sonarr-0 || true))"; fail=1; }

  # Its config is the copy's: the marker exactly once, and nothing the failing
  # image wrote into the same file.
  marks=$(grep -cF "$MARKER" "$CONFIG_PATH/sonarr/config.xml" 2>/dev/null) || true
  [ "$marks" = 1 ] ||
    { echo "sonarr's config holds the marker ${marks:-no} times, want once: $(grep -c '' "$CONFIG_PATH/sonarr/config.xml" 2>/dev/null || echo unreadable) lines"; fail=1; }
  if grep -qF "$BROKEN_WROTE" "$CONFIG_PATH/sonarr/config.xml"; then
    echo "sonarr's config still holds what the failing image wrote: $(grep -F "$BROKEN_WROTE" "$CONFIG_PATH/sonarr/config.xml")"
    fail=1
  fi

  # The other app the step moved stayed moved: only the app that kept exiting
  # was put back.
  wait_image_ready prowlarr-0 "${prowlarr_repo}:${MOVED_TAG}" ||
    { echo "prowlarr-0 is not ready on ${prowlarr_repo}:${MOVED_TAG} (it runs $(pod_image prowlarr-0 || true)): the step's other app must stay moved"; fail=1; }

  # The hold: beside the snapshots, under the config root, naming the app and
  # the image it ran.
  holds_assert "$CONFIG_PATH" sonarr held "$sonarr_tag" "$sonarr_digest" || fail=1

  # The release itself was not rolled back: Helm is on B.
  release_chart=$(helm_chart_version) || true
  [ "$release_chart" = "$release_b" ] ||
    { echo "the release is on ${release_chart}, want ${release_b}: the step moves the app, not the release"; fail=1; }

  # ---- what the step told someone who was not looking -----------------------
  # The step posts a body per thing it did: the app it put back, then the step
  # itself. The sentence is the same in both fields a webhook reads (Slack's
  # text, Discord's content), and the fields carry what a script would use.
  log "notices: the step's updated and rolled-back bodies reached the receiver"
  python3 - "$WORK/notices.log" "$CHART_VERSION" "$release_b" <<'PY' || fail=1
import json, sys
path, frm, to = sys.argv[1:4]
bodies = [json.loads(line) for line in open(path) if line.strip()]
if not bodies:
    sys.exit(f"{path} is empty: the step posted no notice")
for b in bodies:
    if not b.get("text") or b.get("text") != b.get("content"):
        sys.exit("a %s notice's text and content differ: %r vs %r"
                 % (b.get("event"), b.get("text"), b.get("content")))
updated = [b for b in bodies if b.get("event") == "updated"]
if len(updated) != 1:
    sys.exit("%s holds %d updated notices, want one: %s"
             % (path, len(updated), [b.get("event") for b in bodies]))
u = updated[0]
if (u.get("from"), u.get("to")) != (frm, to):
    sys.exit("the updated notice moved %r -> %r, want %r -> %r"
             % (u.get("from"), u.get("to"), frm, to))
back = [b for b in bodies if b.get("event") == "rolled-back"]
apps = [a.get("app") for b in back for a in (b.get("apps") or [])]
if apps != ["sonarr"]:
    sys.exit("the rolled-back notice names %r, want ['sonarr']: %s" % (apps, back))
print("  notices: %d posted; updated %s -> %s, rolled back %s"
      % (len(bodies), u["from"], u["to"], ", ".join(apps)))
PY

  # The same facts in the one line an operator reads: the step, how it ended
  # (the app it put back is part of that), and the hold it left.
  status_line=$("$WORK/box" status --record "$WORK/cluster.yaml" --grant "$WORK/grant.yaml" \
    --values "$WORK/values.yaml" --kubeconfig "$WORK/kubeconfig" 2>&1) || true
  echo "  box status: ${status_line}"
  case "$status_line" in
    *"last update ${CHART_VERSION} → ${release_b} "*) ;;
    *) echo "box status does not name the step the update made"; fail=1 ;;
  esac
  case "$status_line" in
    *"ok, sonarr rolled back"*) ;;
    *) echo "box status does not say the step ended with sonarr put back"; fail=1 ;;
  esac
  case "$status_line" in
    *"held: sonarr (rolled back from ${BROKEN_TAG})"*) ;;
    *) echo "box status does not name the hold the step wrote"; fail=1 ;;
  esac

  # ---- the address is a secret ---------------------------------------------
  # The token in root's address reaches box.yaml and nothing else: no
  # transcript, no record, no notice. The one file it is allowed to be in is
  # the file root owns, and that is why it is mode 0600.
  log "the notice address is in root's file and nowhere else"
  leaks=$(grep -rlF -e 's3cret' -e 'token=abc' "$WORK" 2>/dev/null | grep -vxF "$WORK/box.yaml" || true)
  [ -z "$leaks" ] ||
    { echo "the notice address leaked into:"; echo "${leaks}"; fail=1; }

  # A receiver that is not there is one warning, never a failed update: the
  # update happened either way. The warning names the address by scheme and
  # host alone, so the token is still nowhere.
  log "notices: a closed port is one warning naming the host, and nothing else"
  kill "$NOTICE_PID" 2>/dev/null || true
  wait "$NOTICE_PID" 2>/dev/null || true
  NOTICE_PID=""

  # The wiring ran once, and the run is the rollback's: the step's first apply
  # is with the chart's hooks off, so the Job Helm created can only be the one
  # its hooks-on apply made, whose start is after this step began. The times are
  # printed because the order of the Job and the restore is the thing the step
  # exists to get right.
  job_start=$($K -n "$NS" get job s5n-wiring -o jsonpath='{.status.startTime}' 2>/dev/null) || true
  job_done=$($K -n "$NS" get job s5n-wiring -o jsonpath='{.status.completionTime}' 2>/dev/null) || true
  pod_start=$($K -n "$NS" get pod sonarr-0 -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null) || true
  [ -n "$job_start" ] && [ -n "$job_done" ] ||
    { echo "the step left no completed wiring Job (start '${job_start}', completion '${job_done}'): the hooks-on apply never ran it"; fail=1; }
  python3 - "$job_start" "$job_done" "$pod_start" "$update_b_started" <<'PY' || fail=1
import sys
from datetime import datetime
job_start, job_done, pod_start, step = sys.argv[1:5]
parse = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00"))
if parse(job_start) < parse(step):
    sys.exit(f"the wiring Job started {job_start}, before this step began at {step}: it is bootstrap's run, not the step's")
print(f"  wiring Job {job_start} -> {job_done}; sonarr-0's pod was created {pod_start}")
PY

  # ---- a re-apply keeps the hold -------------------------------------------
  # The same release, applied by hand. The hold is box's answer for this app, so
  # the chart renders the old image for it -- and because that is exactly what
  # the release is already running, the apply has nothing to write. `box apply`
  # overlays a hold exactly as a step does.
  log "box apply: sonarr stays where the rollback left it"
  if "$WORK/box" apply --yes --chart-repo "$CHART_REPO" --chart-version "$release_b" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/apply-held.txt" 2>&1; then
    cat "$WORK/apply-held.txt"
  else
    cat "$WORK/apply-held.txt"; fail=1; return 0
  fi
  grep -qF "nothing to change" "$WORK/apply-held.txt" ||
    { echo "the re-apply wrote the release again: the hold did not render the image sonarr runs:"; grep -v "nothing to change" "$WORK/apply-held.txt" | head -5; fail=1; }
  wait_image_ready sonarr-0 "$sonarr_image" ||
    { echo "sonarr-0 left ${sonarr_image} on a re-apply (it runs $(pod_image sonarr-0 || true))"; fail=1; }
  holds_assert "$CONFIG_PATH" sonarr held "$sonarr_tag" "$sonarr_digest" || fail=1

  # ---- release C: the hold lapses ------------------------------------------
  # Sonarr's next image works, and it is the image it ran in release A under
  # another tag: what the release pins is no longer what the rollback avoided,
  # so the hold is over and the app moves with the release again.
  log "release C: sonarr's image moves again, so the hold lapses"
  retag_pinned "$sonarr_repo" "$sonarr_digest" "$RESTORED_TAG" || { fail=1; return 0; }
  update_chart "$WORK/chart-c" "$release_c" sonarr "$RESTORED_TAG" prowlarr "$MOVED_TAG"
  if "$WORK/box" update --yes --chart-repo "$CHART_REPO" --record "$WORK/cluster.yaml" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/update-c.txt" 2>&1; then
    cat "$WORK/update-c.txt"
  else
    cat "$WORK/update-c.txt"; fail=1; return 0
  fi
  grep -qF "sonarr moves with the release again: s5n ${release_c} pins ${RESTORED_TAG}, not ${BROKEN_TAG}" "$WORK/update-c.txt" ||
    { echo "box did not say the hold lapsed when ${release_c} pinned ${RESTORED_TAG}:"; grep -i "moves with the release again" "$WORK/update-c.txt" | head -3; fail=1; }
  wait_image_ready sonarr-0 "${sonarr_repo}:${RESTORED_TAG}" ||
    { echo "sonarr-0 is not ready on ${sonarr_repo}:${RESTORED_TAG} (it runs $(pod_image sonarr-0 || true)): the app must move with the release"; fail=1; }
  holds_assert "$CONFIG_PATH" sonarr gone "" "" || fail=1
  release_chart=$(helm_chart_version) || true
  [ "$release_chart" = "$release_c" ] ||
    { echo "the release is on ${release_chart}, want ${release_c}"; fail=1; }

  # The receiver was killed before this step, so its notice could not be
  # delivered -- and box says so in one warning that names the address by
  # scheme and host, never the path or the token in the query.
  grep -qF "posting a notice: http://127.0.0.1:${NOTICE_PORT}" "$WORK/update-c.txt" ||
    { echo "box did not warn that the notice could not be delivered, naming the address by host:"; grep -i "notice" "$WORK/update-c.txt" | head -3; fail=1; }
  if grep -qF -e 's3cret' -e 'token=abc' "$WORK/update-c.txt"; then
    echo "the undelivered notice printed the address's path and query: $(grep -m1 -F -e 's3cret' -e 'token=abc' "$WORK/update-c.txt")"; fail=1
  fi
  leaks=$(grep -rlF -e 's3cret' -e 'token=abc' "$WORK" 2>/dev/null | grep -vxF "$WORK/box.yaml" || true)
  [ -z "$leaks" ] ||
    { echo "the notice address leaked into:"; echo "${leaks}"; fail=1; }

  # ---- box rollback sonarr --pin -------------------------------------------
  # The newest copy is the one the C step took before it moved sonarr: its
  # manifest names the image the app ran (release A's) and the one the step
  # moved it to. --pin writes that image into the values file, so the app stays
  # there until the operator takes the pin out, and no hold is left behind.
  log "box rollback sonarr --pin: the newest copy, and a pin where a hold would be"
  if "$WORK/box" rollback sonarr --pin --yes --chart-repo "$CHART_REPO" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/rollback.txt" 2>&1; then
    cat "$WORK/rollback.txt"
  else
    cat "$WORK/rollback.txt"; fail=1; return 0
  fi
  grep -Eq "sonarr's newest snapshot is [0-9]+ minutes? old \(taken .*, before ${release_b} -> ${release_c}\)" "$WORK/rollback.txt" ||
    { echo "box did not say how old the copy is and which step took it:"; grep -i "newest snapshot" "$WORK/rollback.txt" | head -3; fail=1; }
  grep -qF "sonarr is on ${sonarr_tag}, pinned in " "$WORK/rollback.txt" ||
    { echo "box did not report sonarr pinned:"; grep -i "pinned" "$WORK/rollback.txt" | head -3; fail=1; }
  wait_image_ready sonarr-0 "$sonarr_image" ||
    { echo "sonarr-0 is not ready on ${sonarr_image} after the rollback (it runs $(pod_image sonarr-0 || true))"; fail=1; }

  # An ordinary pin in the operator's file: the tag and digest the snapshot
  # recorded, under the app's image key, and no hold where one would be.
  values_pin_assert "$WORK/values.yaml" "$sonarr_tag" "$sonarr_digest" || fail=1
  holds_assert "$CONFIG_PATH" sonarr gone "" "" || fail=1
  [ "$fail" = 0 ] || return 0

  # ---- box apply keeps the installed release -------------------------------
  # Release D is published and is now the newest this box could use. apply is
  # not the command that moves a release: naming D is refused before the chart
  # is loaded, and nothing is written.
  log "release D is published: box apply refuses to move the release to it"
  release_d=$(next_patch "$release_c") || true
  [ -n "$release_d" ] || { echo "could not read the release after ${release_c}"; fail=1; return 0; }
  update_chart "$WORK/chart-d" "$release_d"
  revision_before=$(helm_revision "$NS") || true
  if "$WORK/box" apply --yes --chart-repo "$CHART_REPO" --chart-version "$release_d" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/apply-release-d.txt" 2>&1; then
    cat "$WORK/apply-release-d.txt"
    echo "box apply --chart-version ${release_d} succeeded: only box update may move the release"; fail=1; return 0
  fi
  cat "$WORK/apply-release-d.txt"
  grep -qF "apply keeps the installed release (${release_c}); \`box update\` moves it to ${release_d}" "$WORK/apply-release-d.txt" ||
    { echo "box did not refuse with the release it keeps and the command that moves it"; fail=1; }
  revision_after=$(helm_revision "$NS") || true
  [ "$revision_after" = "$revision_before" ] ||
    { echo "the refused apply wrote revision ${revision_after} (was ${revision_before})"; fail=1; }

  # ---- box apply: a values pin moves sonarr, and apply copies it first ------
  # The operator moves sonarr's pin to another tag, with no --chart-version: a
  # bare apply installs the release the box is on (C), not the newest one (D).
  # The tag is the bytes sonarr already runs under a new name, so the app comes
  # back healthy; dropping the digest is still a move (the deployed reference
  # has one and the pin does not), and a move of a running app is copied first.
  log "box apply: sonarr's pin moves to ${APPLIED_TAG}, so apply takes a copy first"
  retag_pinned "$sonarr_repo" "$sonarr_digest" "$APPLIED_TAG" || { fail=1; return 0; }
  python3 - "$WORK/values.yaml" "$APPLIED_TAG" <<'PY' || { fail=1; return 0; }
import re, sys
path, tag = sys.argv[1:3]
lines = open(path).read().splitlines(keepends=True)
start = next((i for i, line in enumerate(lines) if line.strip() == "sonarr:"), None)
if start is None:
    sys.exit(f"{path} names no sonarr")
indent = len(lines[start]) - len(lines[start].lstrip())
out, moved = lines[:start + 1], False
for i in range(start + 1, len(lines)):
    line = lines[i]
    if line.strip() and len(line) - len(line.lstrip()) <= indent:
        out.extend(lines[i:])
        break
    m = re.match(r'^(\s+)(tag|digest):', line)
    if m and m.group(2) == "digest":
        continue
    if m:
        out.append('%stag: "%s"\n' % (m.group(1), tag))
        moved = True
        continue
    out.append(line)
if not moved:
    sys.exit(f"{path}: sonarr has no tag pin to move")
open(path, "w").write("".join(out))
print(f"  values: apps.sonarr.image.tag -> {tag}, digest dropped")
PY
  grep -qF "$MARKER" "$CONFIG_PATH/sonarr/config.xml" ||
    { echo "sonarr's config lost the marker before the apply: the copy below would prove nothing"; fail=1; return 0; }
  if "$WORK/box" apply --yes --chart-repo "$CHART_REPO" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/apply-pin.txt" 2>&1; then
    cat "$WORK/apply-pin.txt"
  else
    cat "$WORK/apply-pin.txt"; fail=1; return 0
  fi
  grep -qF "sonarr: stopped and snapshotted before it moves from ${sonarr_tag} to ${APPLIED_TAG}" "$WORK/apply-pin.txt" ||
    { echo "apply's plan did not say it copies sonarr before the move:"; grep -i "sonarr" "$WORK/apply-pin.txt" | head -5; fail=1; }
  grep -qF "\`box rollback sonarr\` puts it back" "$WORK/apply-pin.txt" ||
    { echo "apply did not end by saying where the copy is and how to put it back:"; grep -i "snapshot" "$WORK/apply-pin.txt" | head -5; fail=1; }
  wait_image_ready sonarr-0 "${sonarr_repo}:${APPLIED_TAG}" ||
    { echo "sonarr-0 is not ready on ${sonarr_repo}:${APPLIED_TAG} (it runs $(pod_image sonarr-0 || true))"; fail=1; }
  release_chart=$(helm_chart_version) || true
  [ "$release_chart" = "$release_c" ] ||
    { echo "the release is on ${release_chart} after a bare apply, want ${release_c}: apply moved it"; fail=1; }
  [ "$fail" = 0 ] || return 0

  # ---- box rollback sonarr --pin: the copy the apply took ------------------
  # What the new image would have done to the config, done through the pod: the
  # marker goes. The newest copy is now the apply's, so the rollback names a step
  # that stayed on one release, and putting it back brings the marker back.
  log "box rollback sonarr --pin: the copy apply took, back under the old pin"
  $K -n "$NS" exec sonarr-0 -c sonarr -- /bin/sh -c "
    set -eu
    sed -i '\\#$MARKER#d' /config/config.xml
    ! grep -qF '$MARKER' /config/config.xml" ||
    { echo "could not take the marker out of sonarr's config through the pod"; fail=1; return 0; }
  if "$WORK/box" rollback sonarr --pin --yes --chart-repo "$CHART_REPO" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" --paths-in-node \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/rollback-apply.txt" 2>&1; then
    cat "$WORK/rollback-apply.txt"
  else
    cat "$WORK/rollback-apply.txt"; fail=1; return 0
  fi
  grep -Eq "sonarr's newest snapshot is [0-9]+ minutes? old \(taken .*, before ${release_c} -> ${release_c}\)" "$WORK/rollback-apply.txt" ||
    { echo "the rollback did not restore the copy the apply took (a step from ${release_c} to ${release_c}):"; grep -i "newest snapshot" "$WORK/rollback-apply.txt" | head -3; fail=1; }
  wait_image_ready sonarr-0 "$sonarr_image" ||
    { echo "sonarr-0 is not ready on ${sonarr_image} after the rollback (it runs $(pod_image sonarr-0 || true))"; fail=1; }
  grep -qF "$MARKER" "$CONFIG_PATH/sonarr/config.xml" ||
    { echo "the marker is not back in ${CONFIG_PATH}/sonarr/config.xml: the rollback did not restore the apply's copy"; fail=1; }
  values_pin_assert "$WORK/values.yaml" "$sonarr_tag" "$sonarr_digest" || fail=1
  holds_assert "$CONFIG_PATH" sonarr gone "" "" || fail=1
  release_chart=$(helm_chart_version) || true
  [ "$release_chart" = "$release_c" ] ||
    { echo "the release is on ${release_chart} after the rollback, want ${release_c}"; fail=1; }
}

# values_pin_assert <values file> <tag> <digest>: the operator's file pins the
# app's image to exactly that tag and digest, the way `box rollback --pin`
# writes it.
values_pin_assert() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys
path, tag, digest = sys.argv[1:4]
lines = open(path).read().splitlines()
start = next((i for i, line in enumerate(lines) if line.strip() == "sonarr:"), None)
if start is None:
    sys.exit(f"{path} names no sonarr: {lines}")
block = lines[start:start + 6]
if not any("image:" in line for line in block):
    sys.exit(f"{path}: sonarr has no image block: {block}")
for want in (tag, digest):
    if not any(want in line for line in block):
        sys.exit(f"{path}: sonarr's image block does not carry {want}: {block}")
print(f"  values: apps.sonarr.image carries {tag} and {digest}")
PY
}

# ---- scenario: migrate ------------------------------------------------------
# `box migrate` on a real cluster, end to end: an install box did not make, its
# configs, its data and its connections, and the box that adopts them.
#
# The source is written by this script into the namespace `legacy`: plain
# Deployments and Services over host directories, shaped the way a seedbox that
# predates box looks -- configs under the data tree, the library at /library in
# the Arrs and the downloads at /downloads in qBittorrent, a root folder in each
# Arr, a qBittorrent download client in each Arr pointing at the source's own
# Service, and qBittorrent categories with save paths under /downloads. The
# volumes are host directories bind-mounted into the node at the same absolute
# path (see VOLUME ROOTS above), so box on this host and the pods in the node
# see one tree: that is what makes discovery's read of the source, apply's
# staging copy and the renames under the data root one filesystem. The four apps
# whose own code writes its first config (autobrr's and qui's accounts, pbh's
# config.yml, Tautulli's config.ini) run their own image and are set up through
# their own CLI, files and API, never by this script.
#
# Then, the way an operator cuts over: `box migrate cutover` refuses while the
# source still runs (printing the stop command per app, and keeping the read as
# a survey), the operator stops it, `box migrate cutover --replace-cluster`
# writes the final snapshot, the old cluster is thrown away (box never removes
# one it did not install), and `box migrate apply <snapshot>` takes a host with
# no box to a running seedbox: bootstrap, the platform, init from the plan, then
# the adoption -- one command.
#
# The rehearsal is the other path, and this scenario takes it too: the same
# snapshot applies locked on a cluster of its own (the source's data moved aside
# for the run), with the egress policy proven from inside the namespace before
# any app starts, every torrent stopped, no data renamed, and a second box apply
# that leaves the lock in place. A rehearsal cannot apply on the host that holds
# the source's data, which is the refusal this scenario asks for while the
# source still runs. kind's kindnet enforces egress NetworkPolicy, a moment
# after a pod starts: the proof and every app's gate wait for that moment.
#
# The apps must come back healthy on the chart's own images -- which
# are much newer than the source's, so their real database migrations run on the
# copied configs -- with their paths rewritten to /data, their API keys
# unchanged, one download client each, adopted by the id the source had, and the
# data renamed to box's layout rather than moved.
#
# The box the plan is applied into runs nothing the plan names: a plan cannot
# name an app the seedbox already runs (`box migrate apply` refuses it), so the
# installer answers init with the apps off and the migration switches its own on
# from the grant. That box is also the shape the product had to learn to install
# -- no Arr and no qBittorrent is a release with nothing per-app for the wiring
# Job, and the Job accepts it and publishes an ok receipt. With no Arr on, the Job has made none
# of /data's category directories either, so the rename here starts from an
# absent target; the target a box with an Arr on has already made is the unit
# tests' (internal/migrate/apply).
#
# The login is part of what the migration has to leave right: an Arr's
# /<urlbase>/api carries its own API key, so the portal must let it through
# without a session while the Arr's UI still asks for one. The session below is
# asserted through the Gateway, on the address Envoy answers on.
#
# Plex is the one app whose database only its own binary may write (its
# full-text tables carry tokenizers compiled into Plex's libsqlite3), so the
# migration rewrites it through `Plex SQLite` in a one-shot Job in the
# seedbox's namespace. That is what this scenario runs end to end: a source
# Plex builds its own library database (an empty config is enough -- a section
# is never created, and a database Plex did not write is one it refuses to
# open: "no such table: schema_migrations"), the script seeds rows into it with
# the same shell, the migration rewrites them in a copy, and the release's Plex
# starts on the copy. What is still not covered: a library that Plex itself
# made through a plex.tv account -- its sections, its metadata and its scanned
# directories -- because a server token needs a claim token issued while signed
# in. Also absent: indexers and anything else that needs TVDB/TMDB, because
# those are the metadata services the whole e2e suite must not depend on.

# The source's images: the versions the migration code was written against
# (internal/migrate/arr names Sonarr 4.0.14.2938 and Radarr 5.20.1.9773 as the
# schema it reads; internal/migrate/qbit names qBittorrent 5.3.0). Deliberately
# old -- the chart ships Sonarr 5.0.0.2907 and Radarr 7.0.0.10815, so the box's
# images run their real database migrations -- and pinned by digest. These are
# the migration source, not versions this repository follows, so they carry no
# renovate comment: a bump would quietly stop the e2e exercising an upgrade.
MIGRATE_QBITTORRENT_IMAGE=ghcr.io/home-operations/qbittorrent:5.3.0@sha256:5a858e61f94cf2712391db32ed03556576c700858f3e62c4c6fcde262443ebd8
MIGRATE_SONARR_IMAGE=ghcr.io/home-operations/sonarr:4.0.14.2938@sha256:5126f7ae94745a8e58e6963e1f67e905766c096832eff08b0678355ad0cdecee
MIGRATE_RADARR_IMAGE=ghcr.io/home-operations/radarr:5.20.1.9773@sha256:407eb673a46134648c991bb7727b1b788897a181eb9234376105dd763c2b8406
# The source's Plex, and the image the runner Job rewrites the database with.
# Unlike the three above it is not a version of its own: it is the chart's own
# pin, read once in migrate_box from charts/s5n/variants.yaml. The same image on
# purpose -- a rewrite of the database file does not care what the source's
# version was, and the runner has to run the image the release runs.
MIGRATE_PLEX_IMAGE=""
# The source namespace. Not a seedbox: an install box knows nothing about, and
# Pod Security privileged is what such an install runs under.
MIGRATE_LEGACY_NS=legacy
# qBittorrent's WebUI port, in the source and in the chart's own qBittorrent.
MIGRATE_QBIT_PORT=8080
# The source's Radarr, as box will name it: the plain instance, which is the one
# a plan names when the source runs one Radarr. Its category is the chart's own
# for that instance.
MIGRATE_RADARR_INSTANCE=radarr
MIGRATE_RADARR_CATEGORY=movies
# Plex's library database, as both the source and the release see it: the
# image's PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR is /config/Library/
# Application Support, and the file lives in the Databases directory there.
MIGRATE_PLEX_DB="/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db"
# The account name in the source's autobrr and qui. Deliberately not the account
# box owns (s5n): those two apps allow exactly one account, so a source that ran
# them has a name of its own, and the migration's job is to make it box's.
MIGRATE_SOURCE_ACCOUNT=source-login
# The source's pbh, which is the one app whose version this fixture varies: a
# config a v7.3.0 wrote has config-version 27 and no `privacy:` block at all
# (PBH removes that block in config migration 22 and writes it back in 41), and
# the release's v9.5.4 has to start on it. Pinned by digest like the other old
# source images, and no renovate comment: a bump would stop exercising that.
MIGRATE_PBH_IMAGE=ghostchu/peerbanhelper:v7.3.0@sha256:53b3daccaa9e1dfb929540a4ad5026963a054b8af51099fa84860a8a1989e58b
# The name the source PeerBanHelper's one qBittorrent downloader is keyed by,
# which is what a v7 config does: it keys each downloader by its name, and PBH's
# own config migration 32 is what re-keys them to UUIDs and keeps the old key as
# the `name:`. Deliberately not `qBittorrent`, the name wiring's own downloader
# has: after the release's migrations the only thing that still ties this
# downloader to box's qBittorrent is the endpoint the migration pointed at it.
MIGRATE_PBH_DOWNLOADER=source-qbit
# The chart's own autobrr and qui, which the source also runs: their account
# name is what this fixture varies, not their version, so a source of the same
# version is one the migration reads the same way the release writes it.
MIGRATE_AUTOBRR_IMAGE=""
MIGRATE_QUI_IMAGE=""
# The chart's own Tautulli, which the source also runs. Its config.ini is the
# whole of what the migration reads of it, and that file is the app's own: it is
# written on the first start from _CONFIG_DEFINITIONS, with every key lower case
# (plexpy/config.py:473, `ini_key = name.lower()`), which is the shape a real
# install has and the one the rewriter has to read.
MIGRATE_TAUTULLI_IMAGE=""
# Tautulli's URL root in the source, which is what a seedbox behind a
# path-routing proxy has: the file holds it without a leading slash (the app
# normalises it when it mounts its routes, webstart.py:216). The chart serves
# Tautulli under the same prefix, with its own http_root sealed to it on every
# start (internal/ttlconf, values.yaml's rewritePath), so the migration has to
# write the chart's spelling of it -- a leading slash, this instance's name --
# and the app answers /tautulli/api/v2 on both sides.
MIGRATE_TAUTULLI_ROOT=tautulli
# Tautulli's own WebUI port, in the source and in the chart's own Tautulli.
MIGRATE_TAUTULLI_PORT=8181

# The image box runs in as root, below. The static binary on an empty base is
# the whole of it: no shell, nothing installed, and `-u 0` is what makes the
# binary run as root the way a real host runs it. Built in migrate_box from the
# binary this script already built.
BOX_ROOT_IMAGE=s5n-e2e-box-root

# box_root <node> <box arguments...>: box as root, in a container, with the work
# directory at its own path so every path box was given means the same inside
# and outside it. --network host is what reaches the local registry and the kind
# API, both on 127.0.0.1. This is not how box is normally run by the e2e: it is
# the one thing an unprivileged user cannot do, and `box migrate apply` is where
# it matters (a rewriter's atomic rename makes the file root's, and only root's
# box can then hand it to the seedbox's user).
#
# The cost is one `docker build` of a two-line image per run and one container
# start per apply (a second or two each, the image cache making the build cheap);
# the apply itself is the same work, run by root instead of by this script's user.
box_root() { # box_root <node> <box arguments...>
  local node=$1
  shift
  # The image has no /tmp, and Helm's private home, its chart tarballs and the
  # copy a rewriter's check reads all need one: a tmpfs, since what this run
  # stages is a handful of small config trees (the migration's own staging goes
  # under the config root, where the pods can see it). HOME and DOCKER_CONFIG
  # are there because Helm reads $DOCKER_CONFIG/config.json when its registry
  # client is built, and there is no home directory in the image to read one
  # from.
  # The image is empty, and box pulls the platform's charts over TLS: the
  # host's CA bundle goes in read-only, as the file it resolves to (on some
  # hosts the bundle path is a symlink into a directory the container lacks).
  local bundle="" candidate
  for candidate in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem; do
    if [ -e "$candidate" ]; then bundle=$(readlink -f "$candidate"); break; fi
  done
  [ -n "$bundle" ] || { echo "box_root: this host has no CA bundle to give box"; return 1; }
  docker run --rm -u 0 --network host -w "$WORK" \
    -v "${WORK}:${WORK}" --tmpfs /tmp:rw,size=1g \
    -v "${bundle}:/etc/ssl/certs/ca-certificates.crt:ro" \
    -e HOME=/tmp -e DOCKER_CONFIG=/tmp/.docker \
    "$BOX_ROOT_IMAGE" /box "$@"
}

# node_root <node> <shell command>: one command as root inside the node, which
# sees $WORK at its own path. The node's root is the only user here that can
# read a tree owned by the seedbox's user, and the only one that can chown it.
node_root() { # node_root <node> <command>
  docker exec "${1}-control-plane" bash -c "$2"
}

# config_read <node> <path>: one file under a seedbox's config root, read as the
# node's root. The migration leaves that tree owned by the seedbox's user and
# 0600 in places, so this script -- which runs as neither -- reads it the way
# the app does, through the same tree, rather than asking box to loosen it.
config_read() { # config_read <node> <path>
  node_root "$1" "cat '$2'"
}

# box_files_back <node> <config root>: box ran as root above, the way a real
# host runs it, so the files it wrote outside the seedbox's own tree -- the
# grant, the record, its own config, the .box record under the config root --
# are root's. The rest of this script reads those as the runner and a run
# without --keep deletes them, so they are handed back here. The seedbox's
# config tree is deliberately not in the list: whose it is, is what the
# assertion before this one is about.
box_files_back() { # box_files_back <node> <config root>
  node_root "$1" "chown $(id -u):$(id -g) '$WORK'/*.yaml '$WORK'/box.yaml 2>/dev/null || true
    chown -R $(id -u):$(id -g) '$2/.box' 2>/dev/null || true"
}

# migrate_assert_ownership <node> <config root> <puid> <pgid>: what a migration
# has to have left behind. Every path under the config root but box's own
# staging and record area belongs to the seedbox's user, because box wrote as
# root: a rewriter's atomic rename makes the file root's, and an app that cannot
# read it either crash-loops or (Seerr) starts on defaults and writes them over
# the migrated settings. The second half is the same bug from the app's side:
# every app pod is Ready, which it can only be if it read the config it was
# given.
migrate_assert_ownership() { # migrate_assert_ownership <node> <root> <puid> <pgid>
  local node=$1 root=$2 uid=$3 gid=$4 bad
  bad=$(node_root "$node" "find '$root' -mindepth 1 -not -path '$root/.box*' \( ! -uid $uid -o ! -gid $gid \) -print") || {
    echo "could not read the owner of every path under ${root}"; return 1; }
  if [ -n "$bad" ]; then
    echo "these paths under ${root} are not ${uid}:${gid}, the seedbox's user:"
    printf '%s\n' "$bad" | head -20 | sed 's/^/    /'
    return 1
  fi
  echo "  every path under ${root} but box's own .box belongs to ${uid}:${gid}"
}

# migrate_assert_ready <context> <app>...: every app pod of a migrated seedbox
# is Ready. A pod the migration handed root-owned config to fails its first
# read, so this is the app's own answer about the same bug the ownership check
# reads off the tree. Plex is not in the list its callers pass for the same
# reason it is not in the migration's own readiness assertion: its first start
# rebuilds and re-reads a library, and the scenario holds it to readiness where
# it also reads its database.
migrate_assert_ready() { # migrate_assert_ready <context> <app>...
  local ctx=$1 list
  shift
  list=$(printf '%s,' "$@")
  kubectl --context "$ctx" -n "$NS" wait --for=condition=Ready \
    pod -l "app.kubernetes.io/instance=s5n,s5n.io/app in (${list%,})" --timeout=600s || return 1
  echo "  every app pod ($*) is Ready"
}

# migrate_forward <namespace> <target> <port>: port_forward above, for a
# namespace other than the seedbox's. The tunnel's port and process land in
# MIGRATE_PF_PORT and MIGRATE_PF_PID, and a tunnel is the only way to ask either
# side anything directly: a seedbox's ingress policy admits the Gateway and the
# wiring Job alone, and kubelet's own connection is not a pod's, so no policy
# applies to it.
migrate_forward() { # migrate_forward <namespace> <svc/name|pod/name> <port>
  local ns=$1 target=$2 port=$3 i log
  log="$WORK/pf-$(printf '%s' "${ns}-${target}" | tr '/' '-').log"
  MIGRATE_PF_PORT=$(python3 -c 'import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
  $K -n "$ns" port-forward "$target" "${MIGRATE_PF_PORT}:${port}" >"$log" 2>&1 &
  MIGRATE_PF_PID=$!
  for i in $(seq 1 60); do
    if curl -sS -o /dev/null --max-time 2 "http://127.0.0.1:${MIGRATE_PF_PORT}/" 2>/dev/null; then
      return 0
    fi
    # kubectl port-forward exits on the first connection the pod refuses
    # ("lost connection to pod"): an app still starting behind a Ready pod
    # kills the tunnel, and polling a dead tunnel can never succeed. Open it
    # again on the same port.
    if ! kill -0 "$MIGRATE_PF_PID" 2>/dev/null; then
      $K -n "$ns" port-forward "$target" "${MIGRATE_PF_PORT}:${port}" >>"$log" 2>&1 &
      MIGRATE_PF_PID=$!
    fi
    sleep 1
  done
  echo "port-forward to ${ns}/${target}:${port} never answered"
  sed 's/^/    /' "$log" 2>/dev/null || true
  return 1
}

migrate_forward_stop() {
  kill "$MIGRATE_PF_PID" 2>/dev/null || true
  wait "$MIGRATE_PF_PID" 2>/dev/null || true
}

# migrate_source_manifests <file> <src> <uid> <gid> [with-accounts]: the source
# install this script did not install. One Deployment and one Service per app,
# the configs and the data on the host directories the plan will name, and the
# images from above. It runs as the uid and gid the box will, so what the apps
# write is what box can read and what a migrated copy can be owned by.
#
# with-accounts adds the four apps whose first start box's own init containers
# write into: autobrr and qui, which keep their own user table and get an account
# of the source's own name (migrate_source_account makes it with each app's own
# CLI), pbh, which writes its config.yml itself (the fixture is a v7.3.0, so
# what it writes predates the privacy block), and tautulli, whose config.ini the
# app writes itself (migrate_source_tautulli, which is the one app this fixture
# varies the settings of). The keep scenario runs the four apps this migration
# always ran: the account and config shapes are this scenario's, which owns the
# init steps they meet.
migrate_source_manifests() { # migrate_source_manifests <file> <src> <uid> <gid> [with-accounts]
  local file=$1 src=$2 uid=$3 gid=$4 more=${5:-}
  cat >"$file" <<EOF
# The source install: what box finds, not what box makes. Written by
# hack/e2e-box.sh, applied with kubectl, and thrown away with the cluster.
apiVersion: v1
kind: Namespace
metadata:
  name: ${MIGRATE_LEGACY_NS}
  labels:
    # An install box has never seen. Nothing here is held to a seedbox's rules.
    pod-security.kubernetes.io/enforce: privileged
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: qbittorrent
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: qbittorrent
  template:
    metadata:
      labels:
        app: qbittorrent
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: qbittorrent
          image: ${MIGRATE_QBITTORRENT_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: ${MIGRATE_QBIT_PORT}
          # Ready means listening: rollout status waits for it, so the seeding
          # below never meets an app that has not opened its port yet.
          readinessProbe:
            tcpSocket:
              port: http
            periodSeconds: 2
            failureThreshold: 150
          volumeMounts:
            - name: config
              mountPath: /config
            - name: downloads
              mountPath: /downloads
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/qbittorrent
            type: Directory
        - name: downloads
          hostPath:
            path: ${src}/var/data/Downloads
            type: Directory
---
apiVersion: v1
kind: Service
metadata:
  name: qbittorrent
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: qbittorrent
  ports:
    - name: http
      port: ${MIGRATE_QBIT_PORT}
      targetPort: ${MIGRATE_QBIT_PORT}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sonarr
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sonarr
  template:
    metadata:
      labels:
        app: sonarr
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: sonarr
          image: ${MIGRATE_SONARR_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: 8989
          # Ready means listening: rollout status waits for it, so the seeding
          # below never meets an app that has not opened its port yet.
          readinessProbe:
            tcpSocket:
              port: http
            periodSeconds: 2
            failureThreshold: 150
          volumeMounts:
            - name: config
              mountPath: /config
            - name: library
              mountPath: /library
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/sonarr
            type: Directory
        - name: library
          hostPath:
            path: ${src}/var/data/library
            type: Directory
---
apiVersion: v1
kind: Service
metadata:
  name: sonarr
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: sonarr
  ports:
    - name: http
      port: 8989
      targetPort: 8989
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${MIGRATE_RADARR_INSTANCE}
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${MIGRATE_RADARR_INSTANCE}
  template:
    metadata:
      labels:
        app: ${MIGRATE_RADARR_INSTANCE}
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: radarr
          image: ${MIGRATE_RADARR_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: 7878
          # Ready means listening: rollout status waits for it, so the seeding
          # below never meets an app that has not opened its port yet.
          readinessProbe:
            tcpSocket:
              port: http
            periodSeconds: 2
            failureThreshold: 150
          volumeMounts:
            - name: config
              mountPath: /config
            - name: library
              mountPath: /library
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/${MIGRATE_RADARR_INSTANCE}
            type: Directory
        - name: library
          hostPath:
            path: ${src}/var/data/library
            type: Directory
---
apiVersion: v1
kind: Service
metadata:
  name: ${MIGRATE_RADARR_INSTANCE}
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: ${MIGRATE_RADARR_INSTANCE}
  ports:
    - name: http
      port: 7878
      targetPort: 7878
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: plex
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  # Plex has to run once, which is the one thing this fixture cannot fake: an
  # empty config makes it build its own library database, and Plex refuses a
  # database it did not write ("no such table: schema_migrations"). It starts
  # unclaimed -- no plex.tv account is involved -- and stops with the others
  # before the script seeds the rows the migration rewrites.
  replicas: 1
  selector:
    matchLabels:
      app: plex
  template:
    metadata:
      labels:
        app: plex
    spec:
      # Longer than the others': Plex closes its database on SIGTERM, and the
      # migration reads it a moment later.
      terminationGracePeriodSeconds: 30
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: plex
          image: ${MIGRATE_PLEX_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: 32400
          # /identity answers without a server token. It is what says this start
          # finished building the database the migration is about: a database
          # Plex cannot open kills it before it listens at all.
          readinessProbe:
            httpGet:
              path: /identity
              port: http
            periodSeconds: 2
            failureThreshold: 150
          volumeMounts:
            - name: config
              mountPath: /config
            - name: data
              mountPath: /data
            - name: transcode
              mountPath: /transcode
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/plex
            type: Directory
        # The data root itself, which is how a real install mounts it: the
        # migration's path rules come from this mount, so the library database's
        # /data/library paths are the source's own.
        - name: data
          hostPath:
            path: ${src}/var/data
            type: Directory
        - name: transcode
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: plex
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: plex
  ports:
    - name: http
      port: 32400
      targetPort: 32400
EOF
  [ "$more" = with-accounts ] || return 0
  # The four apps box's own init containers write into. Each keeps its whole
  # state under /config, which is the source's own directory on the host, and
  # none is given a readiness probe: the script asks the app's own CLI and its
  # own files instead, which is what says the state a migration reads is real.
  # Tautulli's init container writes its API key into the config.ini the app
  # wrote, which is the file this fixture is about: no script writes it.
  cat >>"$file" <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: autobrr
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: autobrr
  template:
    metadata:
      labels:
        app: autobrr
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: autobrr
          image: ${MIGRATE_AUTOBRR_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: 7474
          volumeMounts:
            - name: config
              mountPath: /config
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/autobrr
            type: Directory
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: autobrr
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: autobrr
  ports:
    - name: http
      port: 7474
      targetPort: 7474
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: qui
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: qui
  template:
    metadata:
      labels:
        app: qui
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: qui
          image: ${MIGRATE_QUI_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: 7476
          volumeMounts:
            - name: config
              mountPath: /config
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/qui
            type: Directory
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: qui
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: qui
  ports:
    - name: http
      port: 7476
      targetPort: 7476
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pbh
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: pbh
  template:
    metadata:
      labels:
        app: pbh
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: pbh
          image: ${MIGRATE_PBH_IMAGE}
          env:
            - name: TZ
              value: UTC
            # The same switch the chart gives it: the app's state root is a Java
            # system property, and its image's entrypoint is a plain java
            # command that forwards nothing but JAVA_TOOL_OPTIONS.
            - name: JAVA_TOOL_OPTIONS
              value: -Dpbh.datadir=/config
          ports:
            - name: http
              containerPort: 9898
          # Its own template, on the 512Mi the chart gives it: the image's JVM
          # takes 85% of whatever memory limit it finds, and without one that is
          # the whole node.
          resources:
            limits:
              memory: 512Mi
          volumeMounts:
            - name: config
              mountPath: /config
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/pbh
            type: Directory
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: pbh
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: pbh
  ports:
    - name: http
      port: 9898
      targetPort: 9898
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tautulli
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: tautulli
  template:
    metadata:
      labels:
        app: tautulli
    spec:
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsUser: ${uid}
        runAsGroup: ${gid}
        fsGroup: ${gid}
      containers:
        - name: tautulli
          image: ${MIGRATE_TAUTULLI_IMAGE}
          env:
            - name: TZ
              value: UTC
          ports:
            - name: http
              containerPort: ${MIGRATE_TAUTULLI_PORT}
          # No readiness probe, like the three above: its URL root moves during
          # the fixture (migrate_source_tautulli sets it, and the app then
          # serves everything under it), so a probe on one path would be wrong
          # for half the run. The script asks the app's own API at the root it
          # is configured with, which is what says the app and its file agree.
          volumeMounts:
            - name: config
              mountPath: /config
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: config
          hostPath:
            path: ${src}/var/data/tautulli
            type: Directory
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: tautulli
  namespace: ${MIGRATE_LEGACY_NS}
spec:
  selector:
    app: tautulli
  ports:
    - name: http
      port: ${MIGRATE_TAUTULLI_PORT}
      targetPort: ${MIGRATE_TAUTULLI_PORT}
EOF
}

# migrate_legacy_conf <file> <password>: qBittorrent's settings before its first
# start. The image only generates this file when it is absent, so an install
# that has not run yet needs the WebUI section written: a login the Arrs can use
# from their own pods (no local-host-only shortcut), and nothing else. Every
# path it stores after that is written by the app itself through its own API,
# which is the shape a migrated config has.
migrate_legacy_conf() { # migrate_legacy_conf <file> <password>
  local file=$1 password=$2 hash
  hash=$(MIGRATE_PASSWORD="$password" python3 - <<'PY'
# qBittorrent 5's WebUI password: PBKDF2-HMAC-SHA512(password, salt 16 B,
# 100 000 iterations, 64 B), stored as "<salt-b64>:<hash-b64>" in a Qt
# QByteArray (internal/qbtconf/conf.go).
import base64, hashlib, os
salt = os.urandom(16)
key = hashlib.pbkdf2_hmac("sha512", os.environ["MIGRATE_PASSWORD"].encode(), salt, 100000, 64)
print(base64.b64encode(salt).decode() + ":" + base64.b64encode(key).decode())
PY
  ) || return 1
  mkdir -p "$(dirname "$file")"
  cat >"$file" <<EOF
[BitTorrent]
Session\\Port=6881

[LegalNotice]
Accepted=true

[WebUI]
Address=*
Port=${MIGRATE_QBIT_PORT}
ServerDomains=*
HostHeaderValidation=false
LocalHostAuth=false
CSRFProtection=false
Username=admin
Password_PBKDF2="@ByteArray(${hash})"
EOF
}

# migrate_source_qbittorrent <password file>: the source qBittorrent's state,
# through its own API: the save and temp paths a real install has, and the
# categories the Arrs download into. Every one is a path under /downloads, which
# is what the plan has to rewrite and the rename has to make true on disk.
migrate_source_qbittorrent() { # migrate_source_qbittorrent <password file>
  local pwfile=$1 base jar code i category
  migrate_forward "$MIGRATE_LEGACY_NS" svc/qbittorrent "$MIGRATE_QBIT_PORT" || return 1
  base="http://127.0.0.1:${MIGRATE_PF_PORT}"
  jar="$WORK/source/qbittorrent.jar"
  : >"$jar"
  code=""
  for i in $(seq 1 90); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -c "$jar" \
      --data-urlencode "username=admin" --data-urlencode "password@${pwfile}" \
      "$base/api/v2/auth/login" 2>/dev/null || true)
    # Any 2xx is a sign-in: qBittorrent 5 answers 204 with no body.
    case "$code" in 2??) break ;; esac
    sleep 2
  done
  case "$code" in
    2??) ;;
    *)
      echo "the source qBittorrent answered ${code:-nothing} to its own login"
      migrate_forward_stop
      return 1
      ;;
  esac
  curl -sS -o /dev/null -b "$jar" --max-time 15 \
    --data-urlencode 'json={"save_path": "/downloads", "temp_path": "/downloads"}' \
    "$base/api/v2/app/setPreferences" || true
  for category in tv "$MIGRATE_RADARR_CATEGORY"; do
    curl -sS -o /dev/null -b "$jar" --max-time 15 \
      --data "category=${category}" --data-urlencode "savePath=/downloads/${category}" \
      "$base/api/v2/torrents/createCategory" || true
  done
  curl -sS -b "$jar" --max-time 15 "$base/api/v2/app/preferences" >"$WORK/source/qbittorrent-preferences.json" 2>/dev/null || true
  curl -sS -b "$jar" --max-time 15 "$base/api/v2/torrents/categories" >"$WORK/source/qbittorrent-categories.json" 2>/dev/null || true
  migrate_forward_stop
  python3 - "$WORK/source/qbittorrent-preferences.json" "$WORK/source/qbittorrent-categories.json" \
    tv "$MIGRATE_RADARR_CATEGORY" <<'PY'
import json, sys
prefs = json.load(open(sys.argv[1]))
cats = json.load(open(sys.argv[2]))
if prefs.get("save_path") != "/downloads" or prefs.get("temp_path") != "/downloads":
    sys.exit(f"the source qBittorrent saves under {prefs.get('save_path')!r}/{prefs.get('temp_path')!r}, want /downloads")
for name in sys.argv[3:]:
    if (cats.get(name) or {}).get("savePath") != f"/downloads/{name}":
        sys.exit(f"the source qBittorrent category {name} is {cats.get(name)!r}, want /downloads/{name}")
print(f"  source qBittorrent: saves under /downloads, categories {', '.join(sys.argv[3:])} under /downloads/<name>")
PY
}

# migrate_source_torrent <password file>: one torrent in the source
# qBittorrent, so the rehearsal has something that has to start stopped. It is
# added through the source's own API from a synthetic single-file torrent (the
# repository's grab test builds one the same way), the client is restarted so
# qBittorrent writes its resume data on shutdown, and the API is asked whether
# the torrent is there and running.
migrate_source_torrent() { # migrate_source_torrent <password file>
  local pwfile=$1 base jar code i state
  local name=s5n-e2e-sample.bin
  printf 's5n e2e: a synthetic payload for the one torrent\n' >"$WORK/source/$name"
  python3 hack/make-torrent.py --file "$WORK/source/$name" --name "$name" \
    --announce http://tracker.example.invalid/announce \
    --out "$WORK/source/s5n-e2e-sample.torrent" --meta "$WORK/source/s5n-e2e-sample.json" \
    >/dev/null || { echo "could not build the source's test torrent"; return 1; }
  mkdir -p "$WORK/hostfs/legacy/var/data/Downloads/tv"
  cp "$WORK/source/$name" "$WORK/hostfs/legacy/var/data/Downloads/tv/$name"

  migrate_forward "$MIGRATE_LEGACY_NS" svc/qbittorrent "$MIGRATE_QBIT_PORT" || return 1
  # The tunnel's port is chosen inside migrate_forward, so the base address is
  # built after it: one read before would name the previous tunnel's port.
  base="http://127.0.0.1:${MIGRATE_PF_PORT}"
  jar="$WORK/source/qbittorrent.jar"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -b "$jar" --max-time 30 \
    -F "torrents=@$WORK/source/s5n-e2e-sample.torrent" \
    -F "savepath=/downloads/tv" -F "category=tv" -F "paused=false" -F "stopped=false" \
    "$base/api/v2/torrents/add" 2>/dev/null || true)
  migrate_forward_stop
  case "$code" in
    2??) ;;
    *) echo "the source qBittorrent answered ${code:-nothing} when the test torrent was added"; return 1 ;;
  esac

  # qBittorrent writes its resume data when it shuts down, and the snapshot is
  # of the config directory: a restart is what makes the torrent part of it.
  $K -n "$MIGRATE_LEGACY_NS" rollout restart deployment/qbittorrent >/dev/null || return 1
  $K -n "$MIGRATE_LEGACY_NS" rollout status deployment/qbittorrent --timeout=180s >/dev/null || return 1

  # The restart dropped the API session; sign in again on the client that is
  # now running, and wait until the torrent is there and not stopped.
  migrate_forward "$MIGRATE_LEGACY_NS" svc/qbittorrent "$MIGRATE_QBIT_PORT" || return 1
  base="http://127.0.0.1:${MIGRATE_PF_PORT}"
  code=""
  for i in $(seq 1 60); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -c "$jar" \
      --data-urlencode "username=admin" --data-urlencode "password@${pwfile}" \
      "$base/api/v2/auth/login" 2>/dev/null || true)
    case "$code" in 2??) break ;; esac
    sleep 2
  done
  case "$code" in
    2??) ;;
    *) echo "the source qBittorrent answered ${code:-nothing} to its own login after the restart"; migrate_forward_stop; return 1 ;;
  esac
  for i in $(seq 1 30); do
    curl -sS -b "$jar" --max-time 15 "$base/api/v2/torrents/info" \
      >"$WORK/source/qbittorrent-torrents.json" 2>/dev/null || true
    state=$(MIGRATE_NAME="$name" python3 - "$WORK/source/qbittorrent-torrents.json" <<'PY' || true
import json, os, sys
try:
    torrents = json.load(open(sys.argv[1]))
except Exception:
    raise SystemExit(1)
mine = [t for t in torrents if t.get("name") == os.environ["MIGRATE_NAME"]]
if len(mine) != 1:
    raise SystemExit(1)
print(mine[0].get("state", ""))
PY
)
    case "$state" in
      "" | stopped* | paused*) sleep 2 ;;
      *) migrate_forward_stop; echo "  the source has one torrent, ${name}: ${state}"
         # The snapshot copies a live SQLite store: give the client a moment to
         # finish writing after its own restart.
         sleep 5
         return 0 ;;
    esac
  done
  migrate_forward_stop
  echo "the source never showed its test torrent running (state ${state:-unknown})"
  return 1
}

# migrate_source_arr <app> <port> <category> <folder> <config xml> <key file>
# <password file>: one source Arr's state, through its own API -- a root folder
# on the source's library mount, and a qBittorrent download client pointing at
# the source's own Service. The API key is read out of the config the app wrote
# itself, because keeping it is what the migration promises, and the client's id
# is kept, because adopting it by id is what the wiring record has to name.
#
# The app runs the client's own /test endpoint on a save, so qBittorrent is
# seeded first and the client carries a real credential: a POST that fails the
# test leaves no client behind, and a source without one proves nothing. The
# save carries ?forceSave=true, the switch the UI's own "Save Anyway" sends, so
# a warning from this pinned version cannot block the fixture.
migrate_source_arr() { # migrate_source_arr <app> <port> <category> <folder> <config> <key out> <pw file>
  local app=$1 port=$2 category=$3 folder=$4 config=$5 keyout=$6 pwfile=$7 out
  migrate_forward "$MIGRATE_LEGACY_NS" "svc/${app}" "$port" || return 1
  out=$(python3 - "http://127.0.0.1:${MIGRATE_PF_PORT}" "$category" "$folder" \
    "qbittorrent.${MIGRATE_LEGACY_NS}" "$MIGRATE_QBIT_PORT" "$config" "$pwfile" <<'PY'
import json, sys, time, urllib.error, urllib.request

base, category, folder, qbit_host, qbit_port, config, pwfile = sys.argv[1:8]

def config_key(path):
    try:
        with open(path) as fh:
            raw = fh.read()
    except FileNotFoundError:
        return ""
    if "<ApiKey>" not in raw:
        return ""
    return raw.split("<ApiKey>", 1)[1].split("</ApiKey>", 1)[0].strip()

key = ""
for _ in range(180):
    key = config_key(config)
    if key:
        break
    time.sleep(1)
if not key:
    sys.exit(f"{config} holds no <ApiKey>: the app never wrote its config")
with open(pwfile) as fh:
    password = fh.read().strip()

def api(path, body=None, method="GET"):
    headers = {"Content-Type": "application/json", "X-Api-Key": key}
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=30) as resp:
        raw = resp.read()
    return json.loads(raw) if raw.strip() else None

for _ in range(180):
    try:
        api("/api/v3/system/status")
        break
    except (urllib.error.URLError, urllib.error.HTTPError, ConnectionError, TimeoutError):
        time.sleep(2)
else:
    sys.exit("the app's API never answered")

if not [f for f in api("/api/v3/rootfolder") if f.get("path") == folder]:
    api("/api/v3/rootfolder", {"path": folder}, "POST")

schema = api("/api/v3/downloadclient/schema")
impl = next((e for e in schema if (e.get("implementation") or "").lower() == "qbittorrent"), None)
if impl is None:
    sys.exit("the app's download client schema has no qBittorrent")
fields, category_field = [], ""
for field in impl.get("fields") or []:
    item = {"name": field["name"], "value": field.get("value")}
    name = field["name"].lower()
    if name == "host":
        item["value"] = qbit_host
    elif name == "port":
        item["value"] = int(qbit_port)
    elif name == "usessl":
        item["value"] = False
    elif name == "urlbase":
        item["value"] = ""
    elif name == "username":
        item["value"] = "admin"
    elif name == "password":
        item["value"] = password
    elif name in ("tvcategory", "moviecategory"):
        item["value"] = category
        category_field = field["name"]
    fields.append(item)
if not category_field:
    sys.exit("the app's qBittorrent settings have no category field")
body = {k: impl.get(k) for k in ("implementation", "implementationName", "configContract", "protocol", "priority", "enable")}
body.update({"name": "qBittorrent", "fields": fields, "tags": []})
created = api("/api/v3/downloadclient?forceSave=true", body, "POST")
if not created or not created.get("id"):
    sys.exit(f"the app did not return the download client it created: {created!r}")
print(f"{key} {created['id']}")
PY
  ) || { migrate_forward_stop; return 1; }
  migrate_forward_stop
  printf '%s\n' "${out%% *}" >"$keyout"
  printf '%s\n' "${out##* }" >"$WORK/source/${app}.client-id"
}

# migrate_source_account <app> <name> <password file>: the one account the
# source's app keeps, made by the app's own CLI inside its own pod, the way a
# person setting the app up would have made it. autobrr and qui allow exactly one
# account, and this name is deliberately not box's: what the migration has to do
# is make it box's.
migrate_source_account() { # migrate_source_account <app> <name> <password-file>
  local app=$1 name=$2 pwfile=$3 out="" i
  for i in $(seq 1 90); do
    case "$app" in
      autobrr)
        out=$(printf '%s\n' "$(cat "$pwfile")" | $K -n "$MIGRATE_LEGACY_NS" exec -i deployment/autobrr -- \
          /usr/local/bin/autobrrctl --config /config create-user "$name" 2>&1) &&
          { echo "  ${app}: the source's own CLI made the account ${name}"; return 0; }
        ;;
      qui)
        out=$(printf '%s\n' "$(cat "$pwfile")" | $K -n "$MIGRATE_LEGACY_NS" exec -i deployment/qui -- \
          /usr/local/bin/qui create-user --config-dir /config --username "$name" 2>&1) &&
          { echo "  ${app}: the source's own CLI made the account ${name}"; return 0; }
        ;;
      *) echo "migrate_source_account: ${app} has no account to make"; return 1 ;;
    esac
    sleep 2
  done
  echo "could not make the source ${app}'s account: ${out:-no output}"
  return 1
}

# migrate_source_pbh_config <src>: the source's PeerBanHelper writes its own
# config on a first start, out of the jar its own image ships, and that is the
# config a migration carries. This one is a v7.3.0: config-version 27, which is
# older than the config migration that writes the privacy block back (41), so
# the file has none. Both are asserted here, because the pbh assertion after the
# migration only means anything if the fixture really is the shape it claims.
#
# The one downloader is the shape a v7 user had as well: a `client:` section
# keyed by the downloader's name, pointing at the source's qBittorrent, written
# into the file the way the v7 WebUI writes it (there is no `client:` block in
# its template at all). The release's own migration 32 re-keys it to a UUID on
# the start that migrates the file, so what wiring meets is a downloader under
# an id nobody recorded.
migrate_source_pbh_config() { # migrate_source_pbh_config <src>
  local file="$1/var/data/pbh/config/config.yml" i password
  for i in $(seq 1 150); do
    [ -f "$file" ] && break
    sleep 2
  done
  [ -f "$file" ] || { echo "the source PeerBanHelper wrote no config.yml"; return 1; }
  grep -qx 'config-version: 27' "$file" ||
    { echo "the source PeerBanHelper wrote '$(sed -n '1p' "$file")', want config-version: 27"; return 1; }
  if grep -q '^privacy:' "$file"; then
    echo "the source PeerBanHelper's config has a privacy block, and the v7 config this fixture runs has none"
    return 1
  fi
  if grep -q '^client:' "$file"; then
    echo "the source PeerBanHelper's config already has a client block, and the v7 template writes none"
    return 1
  fi
  password=$(cat "$WORK/source/qbittorrent-password") ||
    { echo "could not read the source qBittorrent's password"; return 1; }
  # The app wrote this file, so on a host where the fixture runs as the
  # seedbox's user it belongs to them: this script is not that user, and the one
  # key it adds is written through the node's root (the file the migration then
  # reads is the app's own, with one block appended, which is the shape under
  # test). migrate-keep's tree is this script's, and needs nothing.
  [ -w "$file" ] || node_root "$CLUSTER" "chmod a+rw '$file'" || return 1
  cat >>"$file" <<EOF

client:
  ${MIGRATE_PBH_DOWNLOADER}:
    type: qbittorrent
    name: ${MIGRATE_PBH_DOWNLOADER}
    endpoint: http://qbittorrent:${MIGRATE_QBIT_PORT}
    username: admin
    password: "${password}"
EOF
  grep -qx "  ${MIGRATE_PBH_DOWNLOADER}:" "$file" ||
    { echo "the source PeerBanHelper's config has no ${MIGRATE_PBH_DOWNLOADER} downloader after writing one"; return 1; }
  echo "  pbh: the source's config.yml is config-version 27 with no privacy block and a downloader keyed ${MIGRATE_PBH_DOWNLOADER}, as a v7 wrote it"
}

# migrate_source_tautulli <src> <key out>: the source's Tautulli writes its own
# config.ini, and that file is the whole of what a migration reads of it. No
# line of it is hand-written here: the app writes it on its first start from
# _CONFIG_DEFINITIONS, with every key lower case (plexpy/config.py:473,
# `ini_key = name.lower()`, and Config.write() at :528-556), which is the shape
# a real install has. The fixture is the one the reported failure had, a seedbox
# behind a path-routing proxy: a URL root, and its Plex entered by hand as a URL
# (pms_url), which is what leaves the app serving /tautulli/api/v2 and nothing
# at the root the release's wiring Job asks.
#
# The two settings are set the way the app's own settings page leaves them: in
# the file it wrote, in its spelling, and then the app is restarted onto them.
# What is asserted is the result, not the file: the app answers its API under
# the root it is configured with, and no longer at the root.
migrate_source_tautulli() { # migrate_source_tautulli <src> <key out>
  local file="$1/var/data/tautulli/config.ini" keyfile=$2
  local key i code rootcode api setting
  for i in $(seq 1 150); do
    [ -s "$file" ] && break
    sleep 2
  done
  [ -s "$file" ] || { echo "the source Tautulli wrote no config.ini"; return 1; }
  # The shape this fixture exists to be. A key with a capital in it would be one
  # Tautulli itself never reads (ConfigObj's keys are case-sensitive, and the app
  # asks for `name.lower()`), so the migration would be counting a setting that
  # never was one.
  if sed -n 's/^\([^ ]*\) = .*/\1/p' "$file" | grep -q '[A-Z]'; then
    echo "the source Tautulli's config.ini has keys with capitals in them, and the app writes and reads lower-case ones only:"
    sed -n 's/^\([^ ]*\) = .*/\1/p' "$file" | grep '[A-Z]' | head -3 | sed 's/^/    /'
    return 1
  fi
  for setting in http_root http_host http_port pms_ip pms_port pms_ssl pms_url; do
    grep -qE "^${setting} = " "$file" ||
      { echo "the source Tautulli's config.ini has no ${setting}"; return 1; }
  done
  key=$(sed -n 's/^api_key = *//p' "$file" | head -1) || true
  [ -n "${key:-}" ] || { echo "the source Tautulli's config.ini holds no api_key"; return 1; }
  printf '%s' "$key" >"$keyfile"
  chmod 600 "$keyfile"
  # Its own API, on the file it just wrote: the app is up and the key is real.
  code=""
  for i in $(seq 1 60); do
    if migrate_forward "$MIGRATE_LEGACY_NS" svc/tautulli "$MIGRATE_TAUTULLI_PORT"; then
      code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
        "http://127.0.0.1:${MIGRATE_PF_PORT}/api/v2?apikey=${key}&cmd=status" 2>/dev/null || true)
      migrate_forward_stop
      case "$code" in 2??) break ;; esac
    fi
    sleep 2
  done
  case "$code" in
    2??) ;;
    *) echo "the source Tautulli answered ${code:-nothing} to its own API key at the root"; return 1 ;;
  esac
  # Tautulli writes config.ini back out of its own memory whenever it shuts down
  # (plexpy/__init__.py:2940, CONFIG.write() in shutdown()), so a setting a
  # person changes is changed while the app is not running -- which is also the
  # only state a migration may take a final snapshot of a source in. The pod
  # goes, the file the app wrote is edited in its own spelling, and the app
  # starts again on that file.
  $K -n "$MIGRATE_LEGACY_NS" scale deployment/tautulli --replicas=0 >"$WORK/tautulli-scale.txt" 2>&1 || {
    echo "could not stop the source Tautulli to write its own settings:"
    sed 's/^/    /' "$WORK/tautulli-scale.txt"
    return 1
  }
  $K -n "$MIGRATE_LEGACY_NS" wait --for=delete pod -l app=tautulli --timeout=180s >/dev/null 2>&1 || true
  # The file is the app's, and after the chown above it is the seedbox user's:
  # this script changes it through the node's root, as a person with root on the
  # host would (migrate-keep's tree is this script's, which needs nothing).
  [ -w "$file" ] || node_root "$CLUSTER" "chmod a+rw '$file'" || return 1
  # What a person setting this app up behind a proxy does. http_root is written
  # without a leading slash, as the app stores it; pms_url is a whole URL
  # because the server was entered by hand, and https because that is how a
  # proxy in front of a Plex server is reached.
  MIGRATE_TAUTULLI_ROOT="$MIGRATE_TAUTULLI_ROOT" python3 - "$file" <<'PY' || return 1
import os, re, sys
path = sys.argv[1]
with open(path, encoding="utf-8") as fp:
    raw = fp.read()
values = {
    "http_root": os.environ["MIGRATE_TAUTULLI_ROOT"],
    "pms_ip": "plex",
    "pms_port": "32400",
    "pms_ssl": "1",
    "pms_url": "https://plex:32400",
}
for name, value in values.items():
    pattern = re.compile(r"(?m)^" + name + r"[ \t]*=.*$")
    if not pattern.search(raw):
        sys.exit(f"the source Tautulli's config.ini has no {name}")
    raw = pattern.sub(lambda _m, n=name, v=value: f"{n} = {v}", raw, count=1)
with open(path, "w", encoding="utf-8") as fp:
    fp.write(raw)
PY
  $K -n "$MIGRATE_LEGACY_NS" scale deployment/tautulli --replicas=1 >"$WORK/tautulli-scale.txt" 2>&1 || {
    echo "could not start the source Tautulli on the file it wrote:"
    sed 's/^/    /' "$WORK/tautulli-scale.txt"
    return 1
  }
  code=""
  rootcode=""
  for i in $(seq 1 90); do
    if migrate_forward "$MIGRATE_LEGACY_NS" svc/tautulli "$MIGRATE_TAUTULLI_PORT"; then
      # The tunnel's port is one migrate_forward chose a moment ago, so it is
      # read here rather than before the loop.
      api="http://127.0.0.1:${MIGRATE_PF_PORT}"
      code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
        "${api}/${MIGRATE_TAUTULLI_ROOT}/api/v2?apikey=${key}&cmd=status" 2>/dev/null || true)
      rootcode=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
        "${api}/api/v2?apikey=${key}&cmd=status" 2>/dev/null || true)
      migrate_forward_stop
      case "$code" in 2??) break ;; esac
    fi
    sleep 2
  done
  case "$code" in
    2??) ;;
    *)
      echo "the source Tautulli did not come back at /${MIGRATE_TAUTULLI_ROOT}/api/v2 (answered ${code:-nothing}, /api/v2 answered ${rootcode:-nothing}, and $(sed -n '/^http_root/p' "$file" | head -1) is what its file says): the URL root is not one the app serves"
      return 1
      ;;
  esac
  case "$rootcode" in
    2??)
      echo "the source Tautulli still answers /api/v2 at the root: the fixture's URL root did not take, so nothing is being migrated"
      return 1
      ;;
  esac
  echo "  tautulli: config.ini written by the app (lower case), serving /${MIGRATE_TAUTULLI_ROOT}/api/v2 and no longer /api/v2"
}

# migrate_check_arr <app> <port> <category> <client id> <key file>: what a
# migrated Arr has to be, asked of the app itself through the chart's Service --
# the root folder under box's library, and exactly one qBittorrent download
# client, the source's by id, pointing at the release's qBittorrent and carrying
# the category its root folder is under.
migrate_check_arr() { # migrate_check_arr <app> <port> <category> <client id> <key file>
  local app=$1 port=$2 category=$3 client=$4 keyfile=$5 rc
  migrate_forward "$NS" "svc/${app}" "$port" || return 1
  python3 - "http://127.0.0.1:${MIGRATE_PF_PORT}/${app}" "$(cat "$keyfile")" \
    "$category" "$client" "$app" <<'PY'
import json, sys, urllib.request

base, key, category, want_id, app = sys.argv[1:6]

def api(path):
    request = urllib.request.Request(base + path, headers={"X-Api-Key": key})
    with urllib.request.urlopen(request, timeout=30) as resp:
        raw = resp.read()
    return json.loads(raw) if raw.strip() else None

folders = [f.get("path") for f in api("/api/v3/rootfolder")]
want_root = "/data/media/" + category
if folders != [want_root]:
    sys.exit(f"{app}'s root folders are {folders}, want [{want_root}]")

clients = api("/api/v3/downloadclient")
if len(clients) != 1:
    sys.exit(f"{app} has {len(clients)} download clients ({[c.get('name') for c in clients]}), want exactly the one it was migrated with")
client = clients[0]
if int(client.get("id") or 0) != int(want_id):
    sys.exit(f"{app}'s download client is id {client.get('id')}, and the source's was {want_id}: wiring created a second one instead of adopting the one the migration rewrote")
fields = {f["name"]: f.get("value") for f in client.get("fields") or []}
if fields.get("host") != "qbittorrent":
    sys.exit(f"{app}'s download client points at {fields.get('host')!r}, want the release's qbittorrent Service")
if str(fields.get("port")) != "8080":
    sys.exit(f"{app}'s download client points at port {fields.get('port')!r}, want 8080")
if (fields.get("tvCategory") or fields.get("movieCategory")) != category:
    sys.exit(f"{app}'s download client downloads into {fields!r}, want category {category!r}")
if fields.get("useSsl") not in (False, None):
    sys.exit(f"{app}'s download client still asks for TLS: {fields.get('useSsl')!r}")
print(f"  {app}: root folder {want_root}, one download client (id {client.get('id')}) at qbittorrent:8080, category {category}")
PY
  rc=$?
  migrate_forward_stop
  return "$rc"
}

# migrate_check_qbittorrent <password>: the migrated qBittorrent's own answer
# about the data contract -- its save path under /data, and the migrated
# categories saving under /data/downloads. The password is the release Secret's,
# because the chart's init container seals the WebUI section on every start.
migrate_check_qbittorrent() { # migrate_check_qbittorrent <password>
  local password=$1 base jar code
  base="http://127.0.0.1:${MIGRATE_PF_PORT}"
  jar="$WORK/qbittorrent.jar"
  : >"$jar"
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -c "$jar" \
    --data-urlencode "username=admin" --data-urlencode "password=${password}" \
    "$base/api/v2/auth/login" 2>/dev/null || true)
  # Any 2xx is a sign-in: qBittorrent 5 answers 204 with no body.
  case "$code" in
    2??) ;;
    *)
      echo "the migrated qBittorrent answered ${code:-nothing} to the release Secret's password"
      return 1
      ;;
  esac
  curl -sS -b "$jar" --max-time 15 "$base/api/v2/app/preferences" >"$WORK/qbittorrent-preferences.json" 2>/dev/null || true
  curl -sS -b "$jar" --max-time 15 "$base/api/v2/torrents/categories" >"$WORK/qbittorrent-categories.json" 2>/dev/null || true
  python3 - "$WORK/qbittorrent-preferences.json" "$WORK/qbittorrent-categories.json" \
    tv "$MIGRATE_RADARR_CATEGORY" <<'PY'
import json, sys
prefs = json.load(open(sys.argv[1]))
cats = json.load(open(sys.argv[2]))
if prefs.get("save_path") != "/data/downloads":
    sys.exit(f"the migrated qBittorrent saves under {prefs.get('save_path')!r}, want /data/downloads")
if not str(prefs.get("temp_path") or "").startswith("/data/downloads"):
    sys.exit(f"the migrated qBittorrent keeps partial files under {prefs.get('temp_path')!r}, want a directory under /data/downloads")
for name in sys.argv[3:]:
    save = (cats.get(name) or {}).get("savePath")
    if save != f"/data/downloads/{name}":
        sys.exit(f"the migrated qBittorrent category {name} saves under {save!r}, want /data/downloads/{name}")
print(f"  qbittorrent: saves under {prefs.get('save_path')}, categories {', '.join(sorted(cats))} under /data/downloads/<name>")
PY
}

# migrate_plex_sqlite <config dir> <arguments...>: `Plex SQLite` from the
# release's own pinned image, with a Plex config directory at /config, which is
# where the image's PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR points. The host's
# sqlite3 is not an option: Plex's full-text tables carry tokenizers compiled
# into its own libsqlite3, so a stock shell fails every write that fires a
# metadata trigger and can never check the file (internal/migrate/plex). -i is
# the SQL on stdin, and --network none because the shell needs no network.
migrate_plex_sqlite() { # migrate_plex_sqlite <config dir> <arguments...>
  local dir=$1
  shift
  if [ -z "$MIGRATE_PLEX_IMAGE" ]; then
    echo "no Plex image: migrate_box reads the chart's pin before it seeds anything"
    return 1
  fi
  # The user the shell runs as, which is this script's unless a scenario says
  # otherwise: a migrated config tree belongs to the seedbox's user, and the
  # scenario that runs box as root reads and seeds it as root (MIGRATE_PLEX_USER).
  docker run --rm -i --user "${MIGRATE_PLEX_USER:-$(id -u):$(id -g)}" \
    -v "${dir}:/config" --network none \
    --entrypoint "/usr/lib/plexmediaserver/Plex SQLite" \
    "$MIGRATE_PLEX_IMAGE" "$@"
}

# migrate_plex_rows <config dir>: the four columns a migration rewrites, the one
# it must not, and the two GUIDs whose kind decides that -- read back with Plex's
# shell, one labelled line per value. Both the seed and the assertion use it, so
# the fixture and the check cannot disagree about what the database holds.
migrate_plex_rows() { # migrate_plex_rows <config dir>
  local dir=$1
  printf '%s\n' \
    "SELECT 'root:' || root_path FROM section_locations ORDER BY id;" \
    "SELECT 'part:' || file FROM media_parts ORDER BY id;" \
    "SELECT 'stream:' || url FROM media_streams ORDER BY id;" \
    "SELECT 'guid:' || guid FROM metadata_items ORDER BY id;" \
    "SELECT 'dir:' || path FROM directories ORDER BY id;" |
    migrate_plex_sqlite "$dir" "$MIGRATE_PLEX_DB"
}

# migrate_source_plex_covers <config dir>: the parts of a Plex config directory a
# migration does not carry -- the metadata, artwork and thumbnails Plex rebuilds
# from the media it is given. Written once the source has stopped, so nothing
# Plex does at shutdown can prune the fixture, and a snapshot that carries them
# is a snapshot that carried the library.
migrate_source_plex_covers() { # migrate_source_plex_covers <config dir>
  local dir=$1
  mkdir -p "$dir/Metadata/Posters" "$dir/Cache/PhotoTranscoder" ||
    { echo "could not write the covers Plex rebuilds under ${dir}"; return 1; }
  printf 's5n e2e: a poster Plex rebuilds from the library\n' >"$dir/Metadata/Posters/poster.jpg" &&
    printf 's5n e2e: a thumbnail Plex rebuilds from the library\n' >"$dir/Cache/PhotoTranscoder/thumb.jpg"
}

# migrate_seed_plex <config dir>: the rows a library holds, added to the database
# Plex built for itself while the source ran. The schema is Plex's own -- a
# database this script created would be one Plex refuses to open -- and the rows
# are plex_docker_test.go's shapes: two section roots, a media part, a file://
# stream, a file:// GUID beside a plex:// one that must not move, and relative
# directory paths that must not either. The values name the source's own media
# mount (/data, the root the Deployment mounts), so the migration has something
# to rewrite; every one is a file the fixture put on disk.
migrate_seed_plex() { # migrate_seed_plex <config dir>
  local dir=$1 got want
  printf '%s\n' \
    "INSERT INTO section_locations (id, library_section_id, root_path) VALUES (1, 101, '/data/library/tv');" \
    "INSERT INTO section_locations (id, library_section_id, root_path) VALUES (2, 102, '/data/library/movies');" \
    "INSERT INTO media_parts (id, media_item_id, file, size) VALUES (10, 100, '/data/library/movies/Film.2020.mkv', 47);" \
    "INSERT INTO media_streams (id, media_part_id, url, stream_type_id) VALUES (20, 10, 'file:///data/library/movies/Film.2020.en.srt', 2);" \
    "INSERT INTO metadata_items (id, guid, title, metadata_type) VALUES (30, 'file:///data/library/movies/12345', 'Film (2020)', 1);" \
    "INSERT INTO metadata_items (id, guid, title, metadata_type) VALUES (31, 'plex://movie/5d776b9c8b7c2b001f5f8b3a', 'Matched Film', 1);" \
    "INSERT INTO directories (id, path, parent_directory_id) VALUES (1, '', NULL);" \
    "INSERT INTO directories (id, path, parent_directory_id) VALUES (2, 'tv', 1);" |
    migrate_plex_sqlite "$dir" "$MIGRATE_PLEX_DB" -bail >/dev/null || {
    echo "the source Plex's database would not take the rows a library holds: it has no schema Plex wrote (it builds one on its first start)"
    return 1
  }
  got=$(migrate_plex_rows "$dir") || {
    echo "the source Plex's database does not read back"
    return 1
  }
  want=$(cat <<'EOF'
root:/data/library/tv
root:/data/library/movies
part:/data/library/movies/Film.2020.mkv
stream:file:///data/library/movies/Film.2020.en.srt
guid:file:///data/library/movies/12345
guid:plex://movie/5d776b9c8b7c2b001f5f8b3a
dir:
dir:tv
EOF
  )
  if [ "$got" != "$want" ]; then
    echo "the source Plex's library database does not hold what the migration is supposed to rewrite:"
    diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | sed 's/^/    /' || true
    return 1
  fi
  echo "  source plex: its own schema, with two section roots, a media part, a file:// stream and two GUIDs"
}

# migrate_check_runner <instance>: the one-shot Job that rewrote an app's own
# database, from what Kubernetes keeps after box has deleted it. The Job is gone
# by the time apply returns (internal/migrate/apply deletes it in the run's own
# defer), so the record of it is the events: the Job controller's SuccessfulCreate
# means the pod was created, and a pod event (Scheduled, Started, ...) means the
# API server admitted it. A pod that Pod Security or the image policy refuses
# never exists and leaves FailedCreate on the Job instead, which is why that
# reason is a failure here in its own right. Then the assertion that nothing is
# left behind.
migrate_check_runner() { # migrate_check_runner <instance>
  local app=$1 i left
  $K -n "$NS" get events -o json >"$WORK/runner-events.json" 2>/dev/null || {
    echo "could not read the events of ${NS}: the runner Job left no record"
    return 1
  }
  python3 - "$app" "$WORK/runner-events.json" <<'PY' || return 1
import json, sys

app, path = sys.argv[1:3]
events = json.load(open(path)).get("items", [])

def kind(e):
    return e.get("involvedObject", {}).get("kind", "")

def name(e):
    return e.get("involvedObject", {}).get("name", "")

def reason(e):
    return e.get("reason", "")

def say(e):
    return f"{kind(e)}/{name(e)} {reason(e)}: {e.get('message', '')}".strip()

jobs = [e for e in events if kind(e) == "Job" and name(e).startswith("s5n-migrate-")]
pods = [e for e in events if kind(e) == "Pod" and name(e).startswith("s5n-migrate-")]
refused = [e for e in events if reason(e) == "FailedCreate"]
if refused:
    sys.exit("a pod was refused when it was created, so the runner never ran:\n  "
             + "\n  ".join(say(e) for e in refused))
if not any(reason(e) == "SuccessfulCreate" for e in jobs):
    sys.exit(f"no runner Job was created for {app}: no Job event in {path} says SuccessfulCreate")
created = [reason(e) for e in pods]
if not any(r in ("Scheduled", "Created", "Pulled", "Started", "Completed") for r in created):
    sys.exit(f"the runner Job for {app} created no pod: Pod events are {created or 'none'}")
print(f"  runner: {len(jobs)} Job events ({', '.join(sorted({reason(e) for e in jobs}))}), "
      f"pod events {', '.join(sorted(set(created)))}, no FailedCreate -- admitted and completed")
PY
  # box deletes the Job and its pod in a defer, and a background delete lands a
  # moment later: the run's own record is what says nothing is left behind.
  for i in $(seq 1 30); do
    left=$($K -n "$NS" get jobs,pods -l s5n.io/migrate="$app" -o name 2>/dev/null || true)
    [ -z "$left" ] && break
    sleep 2
  done
  if [ -n "$left" ]; then
    echo "the runner left something behind: $(printf '%s' "$left" | tr '\n' ' ')"
    return 1
  fi
}

# migrate_check_plex_db <config dir>: the migrated library database, read with
# Plex's own shell from the directory the release runs. Every value the plan
# promised is asserted, including the two it promised to leave: directories.path
# is relative to the section root and the plex:// GUID is not a filesystem path.
# integrity_check is Plex's own answer: on a database Plex wrote it always ends
# on its FTS4 inverted-index sentence (the shell does not implement validating
# that index), so "ok" and that one sentence are the sound answers, and a page
# error ("*** in database main ***") is not.
migrate_check_plex_db() { # migrate_check_plex_db <config dir>
  local dir=$1 got want check
  got=$(migrate_plex_rows "$dir") || {
    echo "the migrated library database cannot be read with Plex's shell"
    return 1
  }
  want=$(cat <<'EOF'
root:/data/media/tv
root:/data/media/movies
part:/data/media/movies/Film.2020.mkv
stream:file:///data/media/movies/Film.2020.en.srt
guid:file:///data/media/movies/12345
guid:plex://movie/5d776b9c8b7c2b001f5f8b3a
dir:
dir:tv
EOF
  )
  if [ "$got" != "$want" ]; then
    echo "the migrated database does not read back the paths the plan promised:"
    diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | sed 's/^/    /' || true
    return 1
  fi
  # The rows, so a green log carries the evidence and not only the promise.
  printf '%s\n' "$got" | sed 's/^/  plex database: /'
  check=$(printf '%s\n' "PRAGMA integrity_check;" | migrate_plex_sqlite "$dir" "$MIGRATE_PLEX_DB") || {
    echo "the migrated library database failed its integrity check"
    return 1
  }
  case "$check" in
    ok)
      echo "  plex: section roots, media parts, the file:// stream and the file:// GUID under /data/media; the plex:// GUID and directories.path untouched; integrity ok"
      ;;
    "unable to validate the inverted index for FTS4 table main.fts4_metadata_titles_icu: SQL logic error")
      echo "  plex: section roots, media parts, the file:// stream and the file:// GUID under /data/media; the plex:// GUID and directories.path untouched; integrity sound (Plex's own FTS4 note)"
      ;;
    *)
      echo "the migrated library database is not sound: $(printf '%s' "$check" | head -3 | tr '\n' ' ')"
      return 1
      ;;
  esac
}

# migrate_check_plex_identity: Plex, on the release's own Service, answering on
# the database the runner rewrote. A TCP Ready would only say something listens;
# /identity is the server's own document, and it is served without a plex.tv
# token, so an unclaimed server answers it.
migrate_check_plex_identity() { # migrate_check_plex_identity
  local code version
  migrate_forward "$NS" svc/plex 32400 || return 1
  code=$(curl -sS -o "$WORK/plex-identity.xml" -w '%{http_code}' --max-time 20 \
    "http://127.0.0.1:${MIGRATE_PF_PORT}/identity" 2>/dev/null || true)
  migrate_forward_stop
  if [ "$code" != 200 ] || ! grep -q 'machineIdentifier="' "$WORK/plex-identity.xml"; then
    echo "Plex answered ${code:-nothing} on /identity after the migration: it is not serving on the database the runner rewrote"
    head -c 200 "$WORK/plex-identity.xml" 2>/dev/null | sed 's/^/    /' || true
    return 1
  fi
  # The server's own version lives on the MediaContainer element; the XML
  # declaration's version="1.0" is the document's, not Plex's.
  version=$(sed -n '/<MediaContainer/ s/.* version="\([^"]*\)".*/\1/p' "$WORK/plex-identity.xml" | head -1)
  echo "  plex: /identity answers on the release's Service (${version:-no version in its document})"
}

# migrate_check_login_bypass <instance> <key file>: the login's per-app rules,
# asked through the Gateway, on the address Envoy answers on. An Arr's API
# carries its own key, so /<urlbase>/api has to reach the app without a portal
# session, and the app has to refuse a request that arrives without its key --
# that second half is what proves the API reached the app rather than being
# served open. The UI still asks the portal for a session. The rules are
# generated from the app set, so a rule list written before the migration
# switched an app on would send the Arr's own integrations to the login page.
migrate_check_login_bypass() { # migrate_check_login_bypass <instance> <key file>
  local instance=$1 keyfile=$2 key addr code location no_key ui_code ui_location
  key=$(cat "$keyfile")
  addr=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)
  if [ -z "$addr" ]; then
    echo "the Gateway reports no address: ${instance}'s API was asked through no edge"
    return 1
  fi
  # -k: a box with no DNS record is issued the box's own self-signed
  # certificate, and the fresh scenario is where TLS is asserted. What is under
  # test here is the route and the login's rule.
  code=$(curl -sS -o "$WORK/${instance}-api.json" -D "$WORK/${instance}-api.headers" \
    -w '%{http_code}' --max-time 20 -k --resolve "${DOMAIN}:443:${addr}" \
    -H "X-Api-Key: ${key}" \
    "https://${DOMAIN}/${instance}/api/v3/system/status" 2>/dev/null || true)
  location=$(grep -i '^location:' "$WORK/${instance}-api.headers" 2>/dev/null | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
  if [ "$code" != 200 ] || [ -n "$location" ]; then
    echo "${instance}'s API answered ${code:-nothing} through the Gateway with its own key${location:+ (-> ${location})}: the login is not letting the app's API through"
    return 1
  fi
  if ! grep -q '"version"' "$WORK/${instance}-api.json"; then
    echo "${instance}'s API answered 200 through the Gateway without its own status document: $(head -c 120 "$WORK/${instance}-api.json")"
    return 1
  fi
  no_key=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -k \
    --resolve "${DOMAIN}:443:${addr}" \
    "https://${DOMAIN}/${instance}/api/v3/system/status" 2>/dev/null || true)
  # The app's own refusal, not the portal's redirect and not an open API: that
  # is what says the request reached the app without a session.
  case "$no_key" in
    401 | 403) ;;
    *)
      echo "${instance}'s API answered ${no_key:-nothing} with no API key: want the app's own refusal, and a 302 would mean the login is still in front of its API"
      return 1
      ;;
  esac
  ui_code=$(curl -sS -o /dev/null -D "$WORK/${instance}-ui.headers" -w '%{http_code}' \
    --max-time 20 -k --resolve "${DOMAIN}:443:${addr}" \
    "https://${DOMAIN}/${instance}/" 2>/dev/null || true)
  ui_location=$(grep -i '^location:' "$WORK/${instance}-ui.headers" 2>/dev/null | tr -d '\r' | sed 's/^[Ll]ocation: *//' || true)
  if [ "$ui_code" != 302 ]; then
    echo "${instance}'s UI answered ${ui_code} with no session, not a redirect to the login: the app is served open"
    return 1
  fi
  case "$ui_location" in
    "https://${DOMAIN}/auth/"*) ;;
    *)
      echo "${instance}'s UI redirect does not point at the portal: ${ui_location}"
      return 1
      ;;
  esac
  echo "  ${instance}: /${instance}/api answers its own key through the Gateway (200, and ${no_key} without it), the UI still asks the portal"
}

# rehearsal_cleanup <src>: put the source's tree back and remove the rehearsal
# cluster and its host files, whatever the run did. The real install that
# follows adopts the source's data, so the tree must be exactly as it was.
rehearsal_cleanup() { # rehearsal_cleanup <src>
  local src=$1
  "$KIND" delete cluster --name "${CLUSTER}-rehearsal" >/dev/null 2>&1 || true
  rm -f "$WORK/rehearsal-grant.yaml" "$WORK/rehearsal-values.yaml" \
        "$WORK/rehearsal-cluster.yaml" "$WORK/rehearsal-kubeconfig" \
        "$WORK/rehearsal-conf.txt"
  if [ -d "$WORK/source-data" ]; then
    rm -rf "$src/var/data"
    mv "$WORK/source-data" "$src/var/data"
  fi
}

# rehearsal_probe <context> <name> <instance: yes|no> <want> <address>: one pod
# in the rehearsal seedbox's namespace, carrying the release's instance label or
# not, and the exit code `s5n probe` gave it. The pod is the image the release
# runs, under the Pod Security restricted context every app has.
rehearsal_probe() { # rehearsal_probe <context> <name> <yes|no> <open|closed> <address>
  local ctx=$1 name=$2 instance=$3 want=$4 address=$5 labels code i
  labels='s5n.io/probe: e2e'
  [ "$instance" = yes ] && labels='app.kubernetes.io/instance: s5n
    s5n.io/probe: e2e'
  cat >"$WORK/$name.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NS
  labels:
    $labels
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: probe
      image: $IMAGE
      imagePullPolicy: Never
      command: ["/usr/local/bin/s5n", "probe", "tcp", "--address", "$address", "--want", "$want", "--timeout", "2s", "--settle", "20s"]
      securityContext:
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: ["ALL"]}
        seccompProfile: {type: RuntimeDefault}
EOF
  kubectl --context "$ctx" -n "$NS" delete pod "$name" --ignore-not-found >/dev/null 2>&1 || true
  kubectl --context "$ctx" apply -f "$WORK/$name.yaml" >/dev/null || return 1
  code=""
  for i in $(seq 1 60); do
    code=$(kubectl --context "$ctx" -n "$NS" get pod "$name" \
      -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null || true)
    [ -n "$code" ] && break
    sleep 2
  done
  if [ "$code" != 0 ]; then
    echo "the ${name} probe exited ${code:-never} (want ${want} ${address}):"
    kubectl --context "$ctx" -n "$NS" logs "$name" 2>/dev/null || true
    kubectl --context "$ctx" -n "$NS" delete pod "$name" --ignore-not-found >/dev/null 2>&1 || true
    return 1
  fi
  kubectl --context "$ctx" -n "$NS" delete pod "$name" --ignore-not-found >/dev/null 2>&1 || true
  echo "  ${name}: ${address} ${want}, as asked"
  return 0
}

# rehearsal_box: apply the rehearsal snapshot on a cluster of its own, then
# question the result. The caller has moved the source's data aside and restores
# it after; this function owns nothing outside the rehearsal cluster and the
# host files it names.
rehearsal_box() {
  local rcluster="${CLUSTER}-rehearsal" rconfig="$WORK/hostfs/rehearsal-config" ctx
  local conf secret code torrents state i dns
  ctx="kind-${rcluster}"
  cp "$WORK/values.yaml" "$WORK/rehearsal-values.yaml" || return 1
  mkdir -p "$rconfig" || return 1
  $KIND create cluster --name "$rcluster" --config "$KIND_CONFIG" --wait 120s >/dev/null 2>&1 || {
    echo "could not create the rehearsal cluster"; return 1; }
  $KIND load docker-image --name "$rcluster" "$IMAGE" >/dev/null 2>&1 || {
    echo "could not load the s5n image into the rehearsal cluster"; return 1; }
  $KIND get kubeconfig --name "$rcluster" >"$WORK/rehearsal-kubeconfig" || return 1
  # What `box init` leaves behind: the config root owned by the seedbox's user.
  # Only the node's root can hand it to a uid this script is not, so it waits
  # for the rehearsal cluster's node to exist.
  node_root "$rcluster" "chown '$puid:$pgid' '$rconfig'" || {
    echo "could not give the rehearsal's config root to ${puid}:${pgid}"; return 1; }
  docker exec -i "${rcluster}-control-plane" bash -c '
    mkdir -p /var/lib/s5n/authelia
    cat >/var/lib/s5n/authelia/users_database.yml
    chown -R 65532:65532 /var/lib/s5n/authelia
    chmod 700 /var/lib/s5n/authelia
    chmod 600 /var/lib/s5n/authelia/users_database.yml
  ' <"$WORK/users_database.yml" || {
    echo "could not seed the rehearsal cluster's login state"; return 1; }

  # As root, in a container: a rehearsal is still a migration, and a migration
  # is a copy whose rewriters replace files. Run as this script's user it would
  # pass while box wrote files nobody else could read, which is what the two
  # assertions below exist to catch.
  if ! box_root "$rcluster" migrate apply "$rehearsal" --yes \
    --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --grant "$WORK/rehearsal-grant.yaml" --values "$WORK/rehearsal-values.yaml" \
    --kubeconfig "$WORK/rehearsal-kubeconfig" --record "$WORK/rehearsal-cluster.yaml" \
    --domain "$DOMAIN" --login "$LOGIN" --password-file "$WORK/authelia-password" \
    --config-path "$rconfig" --box-namespace "$NS" --paths-in-node \
    >"$WORK/apply-rehearsal.txt" 2>&1; then
    cat "$WORK/apply-rehearsal.txt"; return 1
  fi
  migrate_assert_ownership "$rcluster" "$rconfig" "$puid" "$pgid" || return 1
  migrate_assert_ready "$ctx" qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" autobrr qui pbh tautulli ||
    return 1
  box_files_back "$rcluster" "$rconfig"
  cat "$WORK/apply-rehearsal.txt"
  if grep -q "data: rename" "$WORK/apply-rehearsal.txt"; then
    echo "the rehearsal's confirm lists a data rename it does not make"; return 1
  fi
  for want in "snapshot: ${rehearsal} (rehearsal)" "rehearsal: nothing leaves this box" \
              "network lock: proven"; do
    grep -qF -- "$want" "$WORK/apply-rehearsal.txt" ||
      { echo "the rehearsal apply has no ${want}"; return 1; }
  done
  grep -qE "^ *1 torrents written stopped$" "$WORK/apply-rehearsal.txt" ||
    { echo "the rehearsal apply did not count the one torrent it stopped:";
      grep -n "torrent" "$WORK/apply-rehearsal.txt" || true; return 1; }
  # The two settings the conf carries: what is added later lands stopped, and
  # RSS grabs nothing.
  # Every app pod started behind its gate: the first init container waited
  # until the pod's own egress was closed and exited 0, and the address it
  # dialled is the one the grant records.
  grep -q '^rehearsalGate: ' "$WORK/rehearsal-grant.yaml" ||
    { echo "the rehearsal grant records no rehearsalGate"; return 1; }
  gates=$(kubectl --context "$ctx" -n "$NS" get pods -l app.kubernetes.io/instance=s5n -o json | python3 -c '
import json, sys
pods = [p for p in json.load(sys.stdin)["items"] if p["metadata"].get("labels", {}).get("s5n.io/app")]
bad = []
for p in pods:
    inits = p.get("status", {}).get("initContainerStatuses") or []
    first = inits[0] if inits else {}
    done = (first.get("state", {}).get("terminated") or {}).get("exitCode")
    if first.get("name") != "rehearsal-gate" or done != 0:
        bad.append(p["metadata"]["name"])
print(len(pods), " ".join(bad))
' 2>/dev/null || true)
  case "$gates" in
    ""|"0 "*|0) echo "the rehearsal runs no app pods to check the gate on"; return 1 ;;
    *" "?*) echo "app pods that did not pass the rehearsal gate first: ${gates#* }"; return 1 ;;
  esac
  echo "  every app pod (${gates% *}) passed its rehearsal gate before the app started"
  conf=$(config_read "$rcluster" "$rconfig/qbittorrent/qBittorrent/qBittorrent.conf") || return 1
  for want in 'Session\AddTorrentStopped=true' 'AutoDownloader\EnableProcessing=false'; do
    grep -qF -- "$want" <<<"$conf" || { echo "the rehearsal conf has no ${want}"; return 1; }
  done
  # Nothing was renamed: box's names are not the source's, and the tree the run
  # put aside still holds the hardlinked pair the source had.
  [ ! -e "$DATA_PATH/Downloads" ] ||
    { echo "the rehearsal made the source's Downloads name"; return 1; }
  [ ! -e "$DATA_PATH/library" ] ||
    { echo "the rehearsal made the source's library name"; return 1; }
  state=$(stat -c '%h' "$WORK/source-data/Downloads/tv/Show.S01E01.mkv" 2>/dev/null || true)
  [ "${state:-0}" = 2 ] ||
    { echo "the rehearsal changed the source's data: link count ${state:-none}, want 2"; return 1; }

  # Every torrent stopped, through the rehearsal qBittorrent's own API.
  secret=$(kubectl --context "$ctx" -n "$NS" get secret s5n-keys \
    -o jsonpath='{.data.qbittorrent-password}' 2>/dev/null | base64 -d || true)
  [ -n "$secret" ] || { echo "the rehearsal release Secret holds no qBittorrent password"; return 1; }
  # migrate_forward talks to whatever $K points at; the rehearsal is its own
  # cluster.
  K="kubectl --context ${ctx}"
  if ! migrate_forward "$NS" svc/qbittorrent "$MIGRATE_QBIT_PORT"; then
    K="kubectl --context ${CTX}"; return 1
  fi
  code=""
  for i in $(seq 1 60); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -c "$WORK/rehearsal-cookies" \
      --data-urlencode "username=admin" --data-urlencode "password=${secret}" \
      "http://127.0.0.1:${MIGRATE_PF_PORT}/api/v2/auth/login" 2>/dev/null || true)
    case "$code" in 2??) break ;; esac
    sleep 2
  done
  case "$code" in
    2??) ;;
    *) migrate_forward_stop; K="kubectl --context ${CTX}"
       echo "the rehearsal qBittorrent answered ${code:-nothing} to its own login"; return 1 ;;
  esac
  torrents=$(curl -sS -b "$WORK/rehearsal-cookies" --max-time 15 \
    "http://127.0.0.1:${MIGRATE_PF_PORT}/api/v2/torrents/info" 2>/dev/null || true)
  migrate_forward_stop
  K="kubectl --context ${CTX}"
  MIGRATE_TORRENTS="$torrents" python3 - <<'PY' || { echo "the rehearsal's torrents are not all stopped"; return 1; }
import json, os, sys
torrents = json.loads(os.environ["MIGRATE_TORRENTS"] or "[]")
if len(torrents) != 1:
    sys.exit(f"the rehearsal has {len(torrents)} torrents, want the source's one")
# A rehearsal has no data (it runs where the source's data is not), so a
# stopped torrent may report missingFiles: qBittorrent leaves such a torrent
# stopped and it neither announces nor downloads. Anything active is a failure.
ok = lambda st: st.startswith(("stopped", "paused")) or st == "missingFiles"
bad = [t for t in torrents if not ok(str(t.get("state", "")))]
if bad:
    sys.exit("torrents not stopped: " + ", ".join(f"{t.get('name')}={t.get('state')}" for t in bad))
print(f"  the rehearsal's torrent {torrents[0].get('name')} reports {torrents[0].get('state')}")
PY
  # And the record itself says stopped, whatever the data makes of it: box
  # wrote paused=1 and auto_managed=0 into every resume file. The directory is
  # copied out as the node's root, because the seedbox's user owns it.
  rm -rf "$WORK/rehearsal-resume"
  mkdir -p "$WORK/rehearsal-resume"
  node_root "$rcluster" "tar -C '$rconfig/qbittorrent/qBittorrent' -cf - BT_backup" \
    | tar -C "$WORK/rehearsal-resume" -xf - || {
    echo "could not read the rehearsal's resume records"; return 1; }
  python3 - "$WORK/rehearsal-resume/BT_backup" <<'PY' || { echo "the rehearsal's resume records are not written stopped"; return 1; }
import os, sys
d = sys.argv[1]
files = [f for f in os.listdir(d) if f.endswith(".fastresume")]
if not files:
    sys.exit(f"no resume files in {d}")
for f in files:
    raw = open(os.path.join(d, f), "rb").read()
    if b"6:pausedi1e" not in raw or b"12:auto_managedi0e" not in raw:
        sys.exit(f"{f} is not paused=1 auto_managed=0")
print(f"  {len(files)} resume record(s) written paused=1 auto_managed=0")
PY

  # The e2e's own proof: a pod carrying the release's label cannot reach a DNS
  # pod's metrics port, and one without it can. Then the public address, which
  # is closed both when the lock holds and when this host has no internet.
  dns=$(kubectl --context "$ctx" -n kube-system get pods -l k8s-app=kube-dns \
    -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
  [ -n "$dns" ] ||
    { echo "the rehearsal cluster runs no CoreDNS pod to prove the lock against"; return 1; }
  rehearsal_probe "$ctx" s5n-e2e-lock-control no open "${dns}:9153" || return 1
  rehearsal_probe "$ctx" s5n-e2e-lock-probe yes closed "${dns}:9153" || return 1
  rehearsal_probe "$ctx" s5n-e2e-lock-public yes closed "1.1.1.1:443" || return 1

  # The lock is in box's grant, so an apply that changes nothing must render the
  # same policy: a rehearsal box cannot be unlocked by using it.
  if ! "$WORK/box" apply --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --grant "$WORK/rehearsal-grant.yaml" --values "$WORK/rehearsal-values.yaml" \
    --kubeconfig "$WORK/rehearsal-kubeconfig" --paths-in-node \
    >"$WORK/apply-rehearsal-again.txt" 2>&1; then
    cat "$WORK/apply-rehearsal-again.txt"; return 1
  fi
  grep -qF "nothing to change" "$WORK/apply-rehearsal-again.txt" ||
    { echo "a second box apply on the rehearsal box changed the release:"; cat "$WORK/apply-rehearsal-again.txt"; return 1; }
  if kubectl --context "$ctx" -n "$NS" get networkpolicy s5n-egress -o yaml 2>/dev/null \
    | grep -q '0.0.0.0/0'; then
    echo "a second box apply unlocked the rehearsal box: its egress policy has the internet rule again"
    return 1
  fi
  echo "  a second box apply kept the lock: the release changed nothing and the egress policy has no internet rule"
  return 0
}

migrate_box() {
  local src="$WORK/hostfs/legacy" puid pgid plan report app instance folder port category
  local key secret code pin expected revision after plex_repo plex_tag plex_digest
  local i left
  local tautulli_conf tautulli_key tautulli_ready tautulli_plex tautulli_detail answer
  # The seedbox's user is this run's user, as in every other scenario, so the
  # source apps' configs stay readable to the reads below (discover and cutover
  # run as this user, the way an operator's survey can). What makes the root runs
  # a test of ownership is box itself: it writes as root, as on any real host, so
  # a file the migration fails to hand over is root's, and the assertions after
  # each apply accept nothing but this uid. The user must never be root, or that
  # check proves nothing.
  puid=$(id -u)
  pgid=$(id -g)
  [ "$puid" != 0 ] && [ "$pgid" != 0 ] || {
    echo "the migration's seedbox user ${puid}:${pgid} must not be root: box runs as root here"; fail=1; return 0; }
  # box as root, in a container: see box_root. FROM scratch: box is a static
  # binary that dials only this host's registry and the kind API, so the image
  # needs nothing but the binary, and there is no base image to pull or pin.
  mkdir -p "$WORK/boxroot"
  cp "$WORK/box" "$WORK/boxroot/box" || { echo "could not stage box for the root runs"; fail=1; return 0; }
  printf 'FROM scratch\nCOPY box /box\n' >"$WORK/boxroot/Dockerfile"
  docker build -q -t "$BOX_ROOT_IMAGE" "$WORK/boxroot" >/dev/null || {
    echo "could not build the image box runs in as root"; fail=1; return 0; }
  DATA_PATH="$src/var/data"
  CONFIG_PATH="$WORK/hostfs/config"
  mkdir -p "$WORK/source"

  # The one source image read from the chart rather than pinned here: the
  # database the migration rewrites must be the one the release's own Plex
  # binary writes and reads. Read once, into the variable the manifest and the
  # two docker runs share.
  pin=$(variant_pin plex) ||
    { echo "could not read the chart's plex pin from charts/s5n/variants.yaml"; fail=1; return 0; }
  read -r plex_repo plex_tag plex_digest <<<"$pin"
  MIGRATE_PLEX_IMAGE="${plex_repo}:${plex_tag}${plex_digest:+@${plex_digest}}"
  # The two apps whose account name is what this fixture varies run the chart's
  # own images in the source too: a version bump is not what the account rewrite
  # is about, and one image is one pull.
  pin=$(variant_pin autobrr) ||
    { echo "could not read the chart's autobrr pin from charts/s5n/variants.yaml"; fail=1; return 0; }
  read -r app_repo app_tag app_digest <<<"$pin"
  MIGRATE_AUTOBRR_IMAGE="${app_repo}:${app_tag}${app_digest:+@${app_digest}}"
  pin=$(variant_pin qui) ||
    { echo "could not read the chart's qui pin from charts/s5n/variants.yaml"; fail=1; return 0; }
  read -r app_repo app_tag app_digest <<<"$pin"
  MIGRATE_QUI_IMAGE="${app_repo}:${app_tag}${app_digest:+@${app_digest}}"
  # Tautulli's config.ini is the file this fixture is about, so the source runs
  # the chart's own Tautulli: the app writes that file itself, and this is the
  # version whose spelling the migration is written against.
  pin=$(variant_pin tautulli) ||
    { echo "could not read the chart's tautulli pin from charts/s5n/variants.yaml"; fail=1; return 0; }
  read -r app_repo app_tag app_digest <<<"$pin"
  MIGRATE_TAUTULLI_IMAGE="${app_repo}:${app_tag}${app_digest:+@${app_digest}}"

  # The source's tree, as the volumes above name it. The configs sit under the
  # data path the way an older install kept them, the downloads are `Downloads`
  # with a capital D (the rename to box's `downloads` is a step migrate
  # performs), and the library is `library`, which the Arrs see at /library.
  log "the source install's host tree under ${src}"
  # The preamble prepares box's roots inside the node, and this scenario's roots
  # are on this host (see VOLUME ROOTS): what it made landed on the shared tree,
  # owned by the node's root as uid 1000 -- which is not who box runs as here.
  # It goes, with the app directories it created, so the config root starts as
  # empty as one `box init` makes and the box's own user owns the tree.
  docker exec "${CLUSTER}-control-plane" bash -c "
    rm -rf '$CONFIG_PATH' '$WORK/hostfs/data'
    mkdir -p '$CONFIG_PATH'
    chown -R '${puid}:${pgid}' '$WORK/hostfs'"
  mkdir -p "$src/var/data/qbittorrent" "$src/var/data/sonarr" "$src/var/data/${MIGRATE_RADARR_INSTANCE}" \
    "$src/var/data/plex" "$src/var/data/autobrr" "$src/var/data/qui" "$src/var/data/pbh" \
    "$src/var/data/tautulli" \
    "$src/var/data/Downloads/tv" "$src/var/data/Downloads/${MIGRATE_RADARR_CATEGORY}" \
    "$src/var/data/library/tv" "$src/var/data/library/${MIGRATE_RADARR_CATEGORY}"
  # One hardlinked pair per category, the way an import leaves them: the same
  # file under Downloads and under library. A rename keeps one inode; a move
  # between filesystems or a copy does not. The link count after the migration
  # is the proof that the data was adopted in place.
  printf 's5n e2e: a downloaded episode, and the import that is the same inode\n' \
    >"$src/var/data/Downloads/tv/Show.S01E01.mkv"
  ln -f "$src/var/data/Downloads/tv/Show.S01E01.mkv" "$src/var/data/library/tv/Show.S01E01.mkv"
  printf 's5n e2e: a downloaded film, and the import that is the same inode\n' \
    >"$src/var/data/Downloads/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv"
  ln -f "$src/var/data/Downloads/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv" \
    "$src/var/data/library/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv"
  # The subtitle sidecar one of the library database's rows points at: the
  # migration reads it out of media_streams.url and rewrites it with the film.
  printf 's5n e2e: a subtitle the library database points at\n' \
    >"$src/var/data/library/${MIGRATE_RADARR_CATEGORY}/Film.2020.en.srt"
  # A marker in each config directory whose whole tree a migration carries: it is
  # what proves a file the apps never read came across too. Plex's is the one
  # config a snapshot carries as its state instead -- Preferences.xml and its
  # databases -- so its marker has to stay behind, and the cover trees it
  # rebuilds are written below, once the source has stopped.
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" autobrr qui pbh tautulli; do
    printf 's5n e2e: this config directory was the source install\n' >"$src/var/data/${app}/.s5n-e2e-source"
  done
  printf 's5n e2e: this config directory was the source install\n' >"$src/var/data/plex/.s5n-e2e-source"
  MIGRATE_QBIT_PASSWORD=$(openssl rand -hex 16)
  printf '%s' "$MIGRATE_QBIT_PASSWORD" >"$WORK/source/qbittorrent-password"
  chmod 600 "$WORK/source/qbittorrent-password"
  migrate_legacy_conf "$src/var/data/qbittorrent/qBittorrent/qBittorrent.conf" "$MIGRATE_QBIT_PASSWORD" ||
    { echo "could not write the source qBittorrent's config"; fail=1; return 0; }

  # The source cluster is the one this run created, and its volumes are the tree
  # on this host (see VOLUME ROOTS): the plan, box and the pods name one
  # filesystem, which is the only way a migration can read what a pod wrote.
  grep -qF "containerPath: ${WORK}" "$KIND_CONFIG" ||
    { echo "the kind config has no mount for ${WORK}: the source's files would be inside the node only"; fail=1; return 0; }

  # The tree above was written by this script; from here the apps own it, as
  # they would on a host where the source install created it. Directories stay
  # writable by everyone and files readable, because this script still writes
  # into the tree with the apps' own tools (Plex's shell, the pbh config seed)
  # and runs as neither the owner nor the node's root.
  node_root "$CLUSTER" "chown -R '$puid:$pgid' '$WORK/hostfs/legacy'
    find '$WORK/hostfs/legacy' -type d -exec chmod a+rwx {} +
    find '$WORK/hostfs/legacy' -type f -exec chmod a+r {} +" || {
    echo "could not give the source tree to ${puid}:${pgid}"; fail=1; return 0; }

  log "the source install: eight apps over host directories, in namespace ${MIGRATE_LEGACY_NS}"
  migrate_source_manifests "$WORK/source-manifests.yaml" "$src" "$puid" "$pgid" with-accounts
  if ! $K apply -f "$WORK/source-manifests.yaml" >"$WORK/source-apply.txt" 2>&1; then
    cat "$WORK/source-apply.txt"; fail=1; return 0
  fi
  # Plex is the slow one: it builds its library database on this first start,
  # and its readiness is /identity, so rollout status is what says that start
  # finished. The other three answer as soon as their API is up. autobrr, qui,
  # pbh and tautulli have no readiness probe here: a source install is not held
  # to a chart's probes, and what says each of them is really up is what the next
  # steps ask it through its own CLI and its own files.
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex autobrr qui pbh tautulli; do
    if ! $K -n "$MIGRATE_LEGACY_NS" rollout status "deployment/${app}" --timeout=300s; then
      echo "${app} never became ready in the source install"
      $K -n "$MIGRATE_LEGACY_NS" get pods
      fail=1; return 0
    fi
  done

  # The apps whose state box's own init containers write into start from what
  # their own versions wrote: an account with the source's own name (not box's),
  # the config.yml a v7 PeerBanHelper left, which has no privacy block, and
  # Tautulli's own config.ini, behind a URL root. That is the shape a migrated
  # box brings, and the migration and the release's inits have to take it as it
  # is.
  log "the source's own account names, and the configs its apps wrote for themselves"
  printf '%s' "$(openssl rand -hex 16)" >"$WORK/source/source-account-password"
  chmod 600 "$WORK/source/source-account-password"
  for app in autobrr qui; do
    if ! migrate_source_account "$app" "$MIGRATE_SOURCE_ACCOUNT" "$WORK/source/source-account-password"; then
      fail=1; return 0
    fi
  done
  if ! migrate_source_pbh_config "$src"; then
    fail=1; return 0
  fi
  if ! migrate_source_tautulli "$src" "$WORK/source/tautulli.key"; then
    fail=1; return 0
  fi

  log "seeding the source through its own APIs: paths, categories, a root folder and a download client per Arr"
  if ! migrate_source_qbittorrent "$WORK/source/qbittorrent-password"; then
    fail=1; return 0
  fi
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    case "$instance" in
      sonarr) port=8989; category=tv; folder=/library/tv ;;
      *) port=7878; category="$MIGRATE_RADARR_CATEGORY"; folder="/library/${MIGRATE_RADARR_CATEGORY}" ;;
    esac
    if ! migrate_source_arr "$instance" "$port" "$category" "$folder" \
      "$src/var/data/${instance}/config.xml" "$WORK/source/${instance}.key" \
      "$WORK/source/qbittorrent-password"; then
      echo "could not seed the source ${instance}"
      fail=1; return 0
    fi
    echo "  ${instance}: root folder ${folder}, download client $(cat "$WORK/source/${instance}.client-id")"
  done
  [ "$fail" = 0 ] || return 0

  if ! migrate_source_torrent "$WORK/source/qbittorrent-password"; then
    fail=1; return 0
  fi
  # The four apps write their databases as they run, and the rehearsal snapshot
  # copies them live: let the last seeding settle first.
  sleep 10

  # ---- a rehearsal snapshot, and the refusal on the source's own host ------
  # The source is up: the four apps were seeded through their own APIs, and one
  # torrent is running. A rehearsal snapshot copies the configs from a running
  # source. It cannot be applied here, though: this host holds the source's
  # data, and a locked copy that adopted it would write the live library. That
  # is the refusal a rehearsal must give on the source's own machine.
  #
  # A cutover is the real thing: it refuses while any planned app runs, keeps
  # the read as a survey first (so following the commands it prints is always
  # safe), and prints one line per app with the command that stops it. box never
  # stops a source, so this script is the operator.
  snapshot="$WORK/final.tar.gz"
  rehearsal="$WORK/rehearsal.tar.gz"
  log "box migrate discover --rehearsal, and apply's refusal on the source's host"
  if ! "$WORK/box" migrate discover --rehearsal --out "$rehearsal" \
    --kubeconfig "$WORK/kubeconfig" --namespace "$MIGRATE_LEGACY_NS" --root / \
    --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    >"$WORK/migrate-rehearsal.txt" 2>&1; then
    cat "$WORK/migrate-rehearsal.txt"; fail=1; return 0
  fi
  if "$WORK/box" migrate apply "$rehearsal" --yes \
    --values "$WORK/values.yaml" --grant "$WORK/grant.yaml" --record "$WORK/cluster.yaml" \
    --kubeconfig "$WORK/kubeconfig" >"$WORK/apply-rehearsal.txt" 2>&1; then
    echo "apply accepted a rehearsal snapshot on the host that holds the source's data"
    fail=1
  else
    grep -q "this host holds the source's data" "$WORK/apply-rehearsal.txt" ||
      { echo "apply refused the rehearsal snapshot for another reason:"; cat "$WORK/apply-rehearsal.txt"; fail=1; }
    grep -q "rehearse on another host or VM" "$WORK/apply-rehearsal.txt" ||
      { echo "the refusal does not name the way forward:"; cat "$WORK/apply-rehearsal.txt"; fail=1; }
  fi
  # A refusal writes nothing, so there is no record of a box on this host.
  [ ! -e "$WORK/cluster.yaml" ] ||
    { echo "the refused rehearsal wrote a cluster record"; fail=1; }

  log "box migrate cutover while the source runs: refused, with a survey kept and the stop commands printed"
  if "$WORK/box" migrate cutover --replace-cluster --yes --out "$snapshot" \
    --kubeconfig "$WORK/kubeconfig" --namespace "$MIGRATE_LEGACY_NS" --root / \
    --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --values "$WORK/values.yaml" --grant "$WORK/grant.yaml" \
    --record "$WORK/cluster.yaml" >"$WORK/cutover-refused.txt" 2>&1; then
    echo "cutover took the final snapshot with the source still running"
    fail=1; return 0
  fi
  cat "$WORK/cutover-refused.txt"
  for instance in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex autobrr qui pbh tautulli; do
    grep -q "^  ${instance}: .* in ${MIGRATE_LEGACY_NS}/${instance}; kubectl -n ${MIGRATE_LEGACY_NS} scale " \
      "$WORK/cutover-refused.txt" ||
      { echo "the refusal does not name ${instance} and the command that stops it"; fail=1; }
  done
  grep -q "box never stops a source" "$WORK/cutover-refused.txt" ||
    { echo "the refusal does not say box never stops a source"; fail=1; }
  if [ -e "$snapshot" ]; then
    echo "a refused cutover wrote the final snapshot ${snapshot}"; fail=1
  fi
  survey=$(ls "$WORK"/migrate-*-survey.tar.gz 2>/dev/null | head -1) || true
  if [ -z "${survey:-}" ] || [ ! -f "$survey" ]; then
    echo "a refused cutover kept no survey: the next cutover would carry nothing"; fail=1
  fi
  [ "$fail" = 0 ] || return 0

  # The categories have to be on disk before the source stops: qBittorrent keeps
  # them in categories.json beside its config, and migrate rewrites that file. A
  # source still holding them in the pre-5 map is a refusal the rewriter names,
  # so this wait is what keeps the fixture out of that case.
  log "stopping the source, the way an operator cuts over"
  $K -n "$MIGRATE_LEGACY_NS" scale deployment --all --replicas=0 >/dev/null
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex autobrr qui pbh tautulli; do
    $K -n "$MIGRATE_LEGACY_NS" wait --for=delete pod -l "app=${app}" --timeout=180s >/dev/null 2>&1 || true
  done
  [ -f "$src/var/data/qbittorrent/qBittorrent/categories.json" ] ||
    { echo "the source qBittorrent wrote no categories.json: migrate would have no category paths to rewrite"; fail=1; }
  [ "$fail" = 0 ] || return 0
  echo "  the source is stopped, and qBittorrent's categories are on disk"

  # The library database is closed now, which is the only state a migration may
  # see it in: Plex keeps a connection pool and a -wal beside it, and caches
  # pages. The rows go in with Plex's own shell, out of the same pinned image
  # the runner Job will run.
  log "seeding the source Plex's library database: Plex's own schema, and the rows a library holds"
  migrate_seed_plex "$src/var/data/plex" || { fail=1; return 0; }
  migrate_source_plex_covers "$src/var/data/plex" || { fail=1; return 0; }

  # ---- the final snapshot --------------------------------------------------
  cutover() { # cutover <out>
    local out=$1; shift
    "$WORK/box" migrate cutover --replace-cluster --yes --out "$out" \
      --kubeconfig "$WORK/kubeconfig" --namespace "$MIGRATE_LEGACY_NS" --root / \
      --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
      --values "$WORK/values.yaml" --grant "$WORK/grant.yaml" \
      --record "$WORK/cluster.yaml" "$@"
  }

  log "box migrate cutover --replace-cluster: the final snapshot, the reset recipe, the next command"
  if ! cutover "$snapshot" >"$WORK/cutover.txt" 2>&1; then
    cat "$WORK/cutover.txt"; fail=1; return 0
  fi
  cat "$WORK/cutover.txt"
  # The teardown names the distribution the source's own nodes carry. The
  # source here is kind's, whose kubelet reports a plain upstream version with
  # no `+k0s`/`+k3s` marker, so the recipe is the honest one: box cannot tell
  # how this cluster was installed, and says so instead of printing a k0s reset
  # for a cluster nobody proved is k0s.
  grep -q "box can't tell how this cluster was installed" "$WORK/cutover.txt" ||
    { echo "the replace path printed no teardown the operator can check"; fail=1; }
  grep -qF "box migrate apply ${snapshot}" "$WORK/cutover.txt" ||
    { echo "the replace path did not name the command that finishes the job"; fail=1; }
  [ -f "$snapshot" ] ||
    { echo "cutover took no final snapshot at ${snapshot}"; fail=1; return 0; }
  # One file, root-only: it holds the source's API keys and Plex's token.
  [ "$(stat -c '%a' "$snapshot")" = 600 ] ||
    { echo "the snapshot is $(stat -c '%a' "$snapshot"), want 600"; fail=1; }
  # The plan, the report, the source's manifests and every config are inside
  # it: that is what makes one file the whole migration.
  tar -xzOf "$snapshot" plan.yaml >"$WORK/plan.yaml" 2>/dev/null ||
    { echo "the snapshot holds no plan.yaml"; fail=1; return 0; }
  tar -xzOf "$snapshot" report.txt >"$WORK/report.txt" 2>/dev/null ||
    { echo "the snapshot holds no report.txt"; fail=1; return 0; }
  report="$WORK/report.txt"
  for instance in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex autobrr qui pbh tautulli; do
    source=$instance
    [ "$instance" = "$MIGRATE_RADARR_INSTANCE" ] && source=$MIGRATE_RADARR_INSTANCE
    grep -q "^app: ${instance} (from ${source}, config " "$report" ||
      { echo "the plan does not name ${instance}, and the source runs it"; fail=1; }
    tar -tzf "$snapshot" "config/${instance}/" >/dev/null 2>&1 ||
      { echo "the snapshot carries no config tree for ${instance}"; fail=1; }
  done
  grep -qE "^data: .* \(downloads: Downloads, media: library\)$" "$report" ||
    { echo "the plan does not name the data root with Downloads and library:"; sed -n '/^data:/p' "$report"; fail=1; }
  # Plex's snapshot is its state, not its library. The preferences and the
  # library database are in it -- Plex starts on those and nothing else -- and
  # the Metadata and Cache trees it rebuilds are not, nor is the whole-tree
  # marker the other apps carry, because Plex's config is not carried whole.
  plex_carry="plex: carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)"
  tar -tzf "$snapshot" >"$WORK/snapshot-entries.txt" 2>/dev/null ||
    { echo "the snapshot's entries cannot be listed"; fail=1; return 0; }
  plex_config="config/plex/Library/Application Support/Plex Media Server"
  for carried in "${plex_config}/Preferences.xml" \
                 "${plex_config}/Plug-in Support/Databases/com.plexapp.plugins.library.db"; do
    grep -qxF "$carried" "$WORK/snapshot-entries.txt" ||
      { echo "the snapshot carries no ${carried}: Plex has nothing to start on"; fail=1; }
  done
  if grep -qE "^config/plex/(Metadata|Cache)(/|$)" "$WORK/snapshot-entries.txt"; then
    echo "the snapshot carries Plex's Metadata or Cache tree, and Plex rebuilds both:"
    grep -E "^config/plex/(Metadata|Cache)(/|$)" "$WORK/snapshot-entries.txt" | head -3 | sed 's/^/    /'
    fail=1
  fi
  if grep -qxF "config/plex/.s5n-e2e-source" "$WORK/snapshot-entries.txt"; then
    echo "the snapshot carries Plex's whole config directory, and a Plex snapshot is its state only"; fail=1
  fi
  # Said once, in the report the snapshot holds, and on the screen the run
  # printed: what a Plex snapshot leaves behind is a decision the operator reads.
  [ "$(grep -cF "$plex_carry" "$report")" = 1 ] ||
    { echo "the snapshot's report does not say once what Plex's snapshot leaves behind ($(grep -cF 'plex: carried' "$report" || true) times):"; grep -F "plex: carried" "$report" || true; fail=1; }
  grep -qF "$plex_carry" "$WORK/cutover.txt" ||
    { echo "the cutover did not print the line about what Plex's snapshot leaves behind"; fail=1; }
  # What trimming it is worth on this fixture: the whole config tree the source
  # wrote, the bytes of it the snapshot carries, and the file itself.
  plex_tree=$(du -sb "$src/var/data/plex" | cut -f1) || plex_tree=0
  plex_carried=$(du -cb "${src}/var/data/plex/Library/Application Support/Plex Media Server/Preferences.xml" \
    "${src}/var/data/plex/Library/Application Support/Plex Media Server/Plug-in Support/Databases/"* 2>/dev/null | tail -1 | cut -f1) || plex_carried=0
  echo "  plex: ${plex_tree} bytes of config tree, ${plex_carried:-0} of them carried, the snapshot is $(stat -c %s "$snapshot") bytes"
  grep -q 'service: qbittorrent\.legacy' "$WORK/plan.yaml" ||
    { echo "the plan has no host rule for the source's qbittorrent Service"; fail=1; }
  # Nothing unsettled reaches the install, and nothing is silent about it: the
  # report the snapshot carries names every decision box made without asking, and
  # the one check only apply can make -- Plex's database is readable through its
  # own image alone -- is in it. The cutover ran unattended (--yes), so its last
  # screen lists every one of those defaults by name.
  grep -q "^called out:$" "$report" ||
    { echo "the snapshot's report carries no called-out block"; fail=1; }
  grep -q "^checked at apply:$" "$report" ||
    { echo "the report does not name what only apply can check"; sed -n '/^called out:/,$p' "$report"; fail=1; }
  grep -qE "^  plex \(its database is only readable through its own image" "$report" ||
    { echo "the report does not call out Plex's database as checked at apply"; fail=1; }
  grep -q "^defaults taken:$" "$WORK/cutover.txt" ||
    { echo "the unattended cutover did not name the defaults it took:"; tail -20 "$WORK/cutover.txt"; fail=1; }
  cutover_tail=$(sed -n '/^defaults taken:$/,$p' "$WORK/cutover.txt") || true
  case "$cutover_tail" in
    *"plex (its database is only readable through its own image"*) ;;
    *) echo "the cutover's list of defaults taken does not name the check it left to apply:"; printf '%s\n' "$cutover_tail"; fail=1 ;;
  esac
  # The blessed Plex image's entrypoint writes its transcoder directory into
  # Preferences.xml as /transcode, which no mount of the source gives a rule:
  # discovery maps it onto itself because the release mounts the same
  # /transcode, so the plan applies without a hand edit.
  grep -qF 'from: /transcode' "$WORK/plan.yaml" ||
    { echo "the plan has no rule for Plex's /transcode, and apply refuses a stored path no rule covers"; fail=1; }
  [ "$fail" = 0 ] || return 0

  # ---- the rehearsal box: a cluster of its own, the lock, and its proof ----
  # A rehearsal never adopts data, so the source's tree goes aside for the
  # length of the run and the data root stays, empty (the PV's hostPath has to
  # exist inside the node). The rehearsal runs on a cluster of its own -- the
  # same host, another node name, which is as close as one machine gets to
  # "another host or VM". It is disposable: the real install below runs on a
  # fresh cluster after this one is gone, and the source's tree goes back before
  # that install, which is the run that adopts it.
  log "the rehearsal box: the source's data aside, its own cluster, the locked apply"
  mv "$src/var/data" "$WORK/source-data" ||
    { echo "could not move the source's data aside for the rehearsal"; fail=1; return 0; }
  mkdir -p "$src/var/data"
  if ! rehearsal_box; then fail=1; fi
  rehearsal_cleanup "$src"
  [ "$fail" = 0 ] || return 0

  # ---- a fresh cluster, with box's roots bound in --------------------------
  # box never removes a cluster it did not install, and this one it did not: the
  # operator's cut-over is what throws it away, and the plan is what carries the
  # install across.
  log "deleting the source cluster (the host directories stay) and creating a fresh one"
  $KIND delete cluster --name "$CLUSTER" >/dev/null
  $KIND create cluster --name "$CLUSTER" --config "$KIND_CONFIG" --wait 120s
  $KIND load docker-image --name "$CLUSTER" "$IMAGE" >/dev/null
  $KIND get kubeconfig --name "$CLUSTER" >"$WORK/kubeconfig"
  # The login's state directory lives on the node's filesystem, and this node
  # did not exist a minute ago: the same seeding the scenario preamble does for
  # the first cluster.
  docker exec -i "${CLUSTER}-control-plane" bash -c '
    mkdir -p /var/lib/s5n/authelia
    cat >/var/lib/s5n/authelia/users_database.yml
    chown -R 65532:65532 /var/lib/s5n/authelia
    chmod 700 /var/lib/s5n/authelia
    chmod 600 /var/lib/s5n/authelia/users_database.yml
  ' <"$WORK/users_database.yml"
  # What `box init` creates on a real host: the config root, owned by the
  # seedbox's user. It runs with no app on, so it creates no app directory --
  # migrate creates the ones it moves, by moving them.
  node_root "$CLUSTER" "chown '$puid:$pgid' '$CONFIG_PATH'" || {
    echo "could not give the config root to ${puid}:${pgid}"; fail=1; return 0; }

  # Nothing may have prepared box's data categories: the rename is what creates
  # them, and a `downloads` or `media` that already existed would hide a rename
  # that never ran.
  for d in "$DATA_PATH/downloads" "$DATA_PATH/media"; do
    if [ -e "$d" ]; then
      echo "${d} exists before the install: the source is not in the layout migrate expects"; fail=1
    fi
  done
  [ "$fail" = 0 ] || return 0

  # ---- the installer: one command on a host with no box -------------------
  # This host has no /etc/s5n record and the cluster has no platform: migrate
  # apply is the installer here. It runs bootstrap's own cluster decision and
  # platform apply, then init's own flow answered from the plan -- the apps off,
  # the data root and the owner from the plan -- and asks for only the four a
  # plan cannot know. Then it adopts, in the same run.
  log "box migrate apply: the installer on a host with no box, then the adoption"
  # As root, in a container: a real host runs it as root, and that is the whole
  # difference the assertions after it are about. `box init` would ask the same
  # questions on a host whose console it owns.
  if ! box_root "$CLUSTER" migrate apply "$snapshot" --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --kubeconfig "$WORK/kubeconfig" --record "$WORK/cluster.yaml" \
    --domain "$DOMAIN" --login "$LOGIN" --password-file "$WORK/authelia-password" \
    --config-path "$CONFIG_PATH" --box-namespace "$NS" --paths-in-node \
    >"$WORK/apply-migrate.txt" 2>&1; then
    cat "$WORK/apply-migrate.txt"; fail=1; return 0
  fi
  cat "$WORK/apply-migrate.txt"
  # The two halves of the same bug the root run can now show: every path the
  # migration left in the config root belongs to the seedbox's user, and every
  # app could read what it was given.
  migrate_assert_ownership "$CLUSTER" "$CONFIG_PATH" "$puid" "$pgid" || { fail=1; return 0; }
  migrate_assert_ready "$CTX" qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" autobrr qui pbh tautulli ||
    { fail=1; return 0; }
  box_files_back "$CLUSTER" "$CONFIG_PATH"
  # The snapshot it read is named, and the chart it installed is the release
  # this run pulled once for both halves.
  grep -qF "snapshot: ${snapshot} (final)" "$WORK/apply-migrate.txt" ||
    { echo "the run did not name the snapshot it read"; fail=1; }
  grep -qF "chart: s5n ${CHART_VERSION} (${CHART_REPO}:${CHART_VERSION})" "$WORK/apply-migrate.txt" ||
    { echo "box did not pull ${CHART_REPO}:${CHART_VERSION}"; fail=1; }
  # The confirm says what the snapshot left behind, once: an operator deciding
  # to copy a Plex config reads that its artwork is not in it.
  grep -qF "plex: carried Preferences.xml and its databases; Plex rebuilds its metadata, artwork and thumbnails (artwork uploaded by hand is not carried)" \
    "$WORK/apply-migrate.txt" ||
    { echo "apply's confirm does not say what Plex's snapshot left behind"; fail=1; }
  # The box it installed runs nothing the plan migrates: the apps went in off,
  # and the adoption is what switches them on. The receipt for that empty
  # release is ok, which is what `box apply` waits on.
  grep -q "^wiring ok" "$WORK/apply-migrate.txt" ||
    { echo "box did not report a healthy receipt for the box the migration adopts into"; fail=1; }
  # The owner came from the plan, not from whoever ran box: the source's pods
  # ran as this user, discovery wrote that into `source.owner`, and the
  # installer hands it to init as --puid/--pgid.
  grep -qE "^  puid: ${puid}$" "$WORK/grant.yaml" ||
    { echo "the grant's puid is not the source's ${puid}:"; grep -n "puid" "$WORK/grant.yaml"; fail=1; }
  grep -qE "^  pgid: ${pgid}$" "$WORK/grant.yaml" ||
    { echo "the grant's pgid is not the source's ${pgid}:"; grep -n "pgid" "$WORK/grant.yaml"; fail=1; }
  # No klog line: box sets rest.NoWarnings on every config it hands out, and a
  # client-go deprecation line in this output would be the chart's lookup
  # leaking through a config that does not.
  if grep -qE '^[IWEF][0-9]{4} ' "$WORK/apply-migrate.txt"; then
    echo "box printed a klog line:"; grep -E '^[IWEF][0-9]{4} ' "$WORK/apply-migrate.txt" | head -3; fail=1
  fi
  # What the run said it would do, before it did it. The two renames, the kept
  # keys and the connections adopted by id are the whole point of a migration,
  # and a summary that does not name them is a run that did less.
  grep -qF "data: rename Downloads to downloads" "$WORK/apply-migrate.txt" ||
    { echo "the plan did not say it renames Downloads to box's downloads"; fail=1; }
  grep -qF "data: rename library to media" "$WORK/apply-migrate.txt" ||
    { echo "the plan did not say it renames library to box's media"; fail=1; }
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    grep -qF "${instance}: keep its API key (${instance}-apikey)" "$WORK/apply-migrate.txt" ||
      { echo "the plan did not say it keeps ${instance}'s API key"; fail=1; }
    grep -qF "${instance}: adopt downloadclient" "$WORK/apply-migrate.txt" ||
      { echo "the plan did not say it adopts ${instance}'s download client by id"; fail=1; }
  done
  # The pbh rewriter files the downloader it finds under the source's key, which
  # is the id a v7 config stores it under; the record is what wiring then has to
  # reconcile, so the plan naming it is the other half of the adoption.
  grep -qF "pbh: adopt pbh-downloader \"${MIGRATE_PBH_DOWNLOADER}\" (id ${MIGRATE_PBH_DOWNLOADER}) for qbittorrent" \
    "$WORK/apply-migrate.txt" ||
    { echo "the plan did not say it adopts PeerBanHelper's downloader ${MIGRATE_PBH_DOWNLOADER} by id"; fail=1; }
  grep -q "^wiring ok" "$WORK/apply-migrate.txt" ||
    { echo "the apply the migration handed over to did not report a healthy receipt"; fail=1; }
  # Plex's rows are the ones only the runner Job can have rewritten: every other
  # app's rewriter edits files, and this one runs `Plex SQLite` in the release's
  # own image. A count that is off is a Job that read a different database.
  for column in "section_locations.root_path 2" "media_parts.file 1" "media_streams.url 1" "metadata_items.guid 1"; do
    grep -qF "plex: Library/Application Support/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db ${column%% *} path ${column##* }" \
      "$WORK/apply-migrate.txt" ||
      { echo "the plan did not say the runner rewrites Plex's ${column%% *} ${column##* } time(s)"; fail=1; }
  done
  # autobrr and qui allow exactly one account and box owns it, so a source's
  # account has to become box's; the confirm says so once per app, with the name
  # the source used, because that name is the operator's own and it is the one
  # that stops working.
  for instance in autobrr qui; do
    grep -qF "${instance}: account \"${MIGRATE_SOURCE_ACCOUNT}\" becomes s5n, box's own; sign in through the box login" \
      "$WORK/apply-migrate.txt" ||
      { echo "the plan did not say ${instance}'s account ${MIGRATE_SOURCE_ACCOUNT} becomes box's own"; fail=1; }
  done
  # Tautulli's config.ini is rewritten where the app wrote it, and the plan says
  # so: the URL root (the source's spelling, without a slash, becomes the
  # chart's), the login the app had of its own (switched off, because the box's
  # is the one a person uses), and the Plex it was entered by hand as. A
  # migration that read none of that file -- the upper-case lookup the reported
  # failure came from -- listed "copy 6 files" and nothing else.
  for want in "tautulli: config.ini General/http_root url base 1" \
              "tautulli: config.ini General/http_hash_password own login 1" \
              "tautulli: config.ini General/http_hashed_password own login 1" \
              "tautulli: config.ini PMS/pms_ssl tls 1" \
              "tautulli: config.ini PMS/pms_url url 1" \
              "tautulli: note: its own login is switched off; you sign in through the box" \
              "tautulli: keep its API key (tautulli-apikey)"; do
    grep -qF "$want" "$WORK/apply-migrate.txt" ||
      { echo "the plan does not say \"${want}\":"; grep -F "tautulli:" "$WORK/apply-migrate.txt" || true; fail=1; }
  done
  [ "$fail" = 0 ] || return 0

  # ---- assertion 1: healthy, on the chart's own images ---------------------
  log "the migrated apps are ready on the chart's images"
  for instance in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" autobrr qui pbh tautulli; do
    app=$instance
    [ "$instance" = "$MIGRATE_RADARR_INSTANCE" ] && app=radarr
    expected=$(chart_image "$app") || { echo "could not read the chart's ${app} pin"; fail=1; return 0; }
    wait_image_ready "${instance}-0" "$expected" ||
      { echo "${instance}-0 is not ready on the chart's ${expected} (it runs $(pod_image "${instance}-0" || true))"; fail=1; }
  done
  [ "$fail" = 0 ] || return 0

  # ---- assertion 1c: the apps box's own init containers write into ---------
  # A migrated autobrr and qui had an account of their own name, and a migrated
  # pbh a config written by a version older than its privacy setting. All three
  # pods being Ready is what says the init containers took those shapes as they
  # were -- the account one because the app's own login is the init's, the pbh
  # one because its config gate refuses a file it cannot switch off. What that
  # readiness means is read back here.
  log "the migrated accounts signed in, and pbh's analytics switch"
  for step in autobrr/login qui/login; do
    result=$(status_field "[s['result'] for s in d['steps'] if s['name'] == '${step}']") || true
    [ "$result" = "['ok']" ] ||
      { echo "the wiring Job did not sign in to ${step%%/*}: the receipt says ${result:-nothing}"; fail=1; }
  done
  # The file is the seedbox's user's, so it is read through the node's root:
  # that it is readable by its owner and no other is the assertion above.
  pbh_conf=$(config_read "$CLUSTER" "$CONFIG_PATH/pbh/config/config.yml") || {
    echo "could not read the migrated PeerBanHelper config"; fail=1; }
  grep -qx '  analytics: false' <<<"$pbh_conf" ||
    { echo "the migrated PeerBanHelper's config does not say analytics: false:"; sed -n '/^privacy:/,+4p' <<<"$pbh_conf"; fail=1; }
  # And it is not at the source's version any more: the app ran its own config
  # migrations on the first start, so what the key survived is the migration that
  # rewrites it, not a file box quietly told the app was newer than it is.
  pbh_version=$(sed -n 's/^config-version: *//p' <<<"$pbh_conf") || true
  [ "${pbh_version:-0}" -ge 41 ] ||
    { echo "the migrated PeerBanHelper's config is at version ${pbh_version:-none}, and its own migrations did not run"; fail=1; }
  echo "  pbh's config is at version ${pbh_version} and still says analytics: false"

  # ---- assertion 1d: Tautulli, on the config its own app wrote --------------
  # The one app whose settings Tautulli's code folds to a spelling of its own:
  # every key of config.ini is lower case, so a rewriter looking for HTTP_ROOT
  # matches nothing on a real install. What is asserted here is the app's own
  # answer under the chart's prefix, the file it answers from (URL root, its own
  # login off, the Plex link), and the two receipt steps that read it.
  log "Tautulli: the config.ini the box holds, the API under its prefix, and the receipt's two steps"
  tautulli_conf=$(config_read "$CLUSTER" "$CONFIG_PATH/tautulli/config.ini") || true
  [ -n "$tautulli_conf" ] || { echo "the box holds no ${CONFIG_PATH}/tautulli/config.ini"; fail=1; }
  if [ -n "$tautulli_conf" ]; then
    # The chart's prefix, written with the slash the chart passes to the init
    # container, whatever the source's file said.
    grep -qE '^http_root = *"?/?tautulli"?$' <<<"$tautulli_conf" ||
      { echo "the box's Tautulli config.ini does not carry the chart's URL root: $(sed -n '/^http_root/p' <<<"$tautulli_conf")"; fail=1; }
    # Its own login is off: no name, no password, and the two flags that say a
    # value is a hash are back to their defaults. A source that had one is only
    # safe because the app no longer asks for it.
    for want in "^http_username = *(\"\"|'')?$" "^http_password = *(\"\"|'')?$" \
                "^http_hash_password = *0$" "^http_hashed_password = *0$" "^http_basic_auth = *0$"; do
      grep -qE "$want" <<<"$tautulli_conf" ||
        { echo "the box's Tautulli config.ini has not switched its own login off: $(grep -E '^http_(username|password|hash|hashed|basic)' <<<"$tautulli_conf" | tr '\n' ' ')"; fail=1; }
    done
    for want in "pms_ssl = 0" "pms_url = http://plex:32400"; do
      grep -qx "$want" <<<"$tautulli_conf" ||
        { echo "the box's Tautulli config.ini has no \"${want}\""; fail=1; }
    done
    # The app's spelling, kept: the rewriter edits values, never key names.
    if sed -n 's/^\([^ ]*\) = .*/\1/p' <<<"$tautulli_conf" | grep -q '[A-Z]'; then
      echo "the box's Tautulli config.ini gained a key with a capital in it, and the app writes and reads lower-case ones only:"
      sed -n 's/^\([^ ]*\) = .*/\1/p' <<<"$tautulli_conf" | grep '[A-Z]' | head -3 | sed 's/^/    /'
      fail=1
    fi
  fi
  # The key it had is the key the release holds and the app answers, under the
  # prefix the chart serves it at: the same thing the wiring Job's
  # /tautulli/api/v2 asks.
  tautulli_key=$(cat "$WORK/source/tautulli.key")
  secret=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.tautulli-apikey}' 2>/dev/null | base64 -d || true)
  [ "$secret" = "$tautulli_key" ] ||
    { echo "the release Secret's tautulli-apikey is not the key the source had"; fail=1; }
  if migrate_forward "$NS" svc/tautulli "$MIGRATE_TAUTULLI_PORT"; then
    answer=$(curl -sS --max-time 15 \
      "http://127.0.0.1:${MIGRATE_PF_PORT}/tautulli/api/v2?apikey=${tautulli_key}&cmd=status" 2>/dev/null || true)
    migrate_forward_stop
    MIGRATE_TAUTULLI_ANSWER="$answer" python3 - <<'PY' || { echo "the migrated Tautulli did not answer its own key under the prefix of the release"; fail=1; }
import json, os, sys
raw = os.environ["MIGRATE_TAUTULLI_ANSWER"]
try:
    body = json.loads(raw)["response"]
except Exception:
    sys.exit(f"the app answered {raw[:200]!r}, which is not the API's JSON")
if body.get("result") != "success":
    sys.exit(f"the app answered result {body.get('result')!r}")
print("  tautulli: /tautulli/api/v2 answers the source's key on the box")
PY
  else
    fail=1
  fi
  # And the receipt the layout's own Job published: ready where the app answers
  # now, and the Plex link pending for the one reason a box with no claimed
  # server has -- no plex-token in the release, so there is nothing to link with.
  tautulli_ready=$(status_field "[s['result'] for s in d['steps'] if s['name']=='tautulli/ready'][0]" 2>/dev/null || true)
  tautulli_plex=$(status_field "[s['result'] for s in d['steps'] if s['name']=='tautulli/plex'][0]" 2>/dev/null || true)
  tautulli_detail=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='tautulli/plex'][0]" 2>/dev/null || true)
  echo "  receipt: tautulli/ready ${tautulli_ready:-missing}, tautulli/plex ${tautulli_plex:-missing} (${tautulli_detail:-no detail})"
  [ "$tautulli_ready" = ok ] ||
    { echo "the receipt's tautulli/ready is ${tautulli_ready:-missing}: the wiring Job did not reach the app where it serves"; fail=1; }
  [ "$tautulli_plex" = pending ] ||
    { echo "the receipt's tautulli/plex is ${tautulli_plex:-missing}, want pending: this fixture's Plex is unclaimed"; fail=1; }
  case "$tautulli_detail" in
    "no Plex token"*) ;;
    *) echo "the receipt's tautulli/plex says ${tautulli_detail:-nothing}, want the line about the missing token"; fail=1 ;;
  esac

  # ---- assertion 1e: the front door a browser meets --------------------------
  # The migration's own box, browsed: Tautulli's UI under its prefix, behind the
  # box's login, and a request that carries a header credential of its own
  # answered from the session (assert_front_door). Nothing above reaches it: the
  # tunnelled API call is the app's, not the edge's.
  log "the front door of the migrated box: signed in, /tautulli under its prefix"
  gw_addr=$($K -n s5n-gateway get gateway s5n -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)
  if [ -z "$gw_addr" ]; then
    echo "the Gateway reports no address, so the front door was never browsed"
    fail=1
  else
    assert_front_door "$gw_addr" -
  fi

  # ---- the downloader the source's v7 pbh held -----------------------------
  # The source keyed its one downloader by a name, and the app's own migration
  # 32 renamed that key to a UUID. The migration could therefore only file the
  # adoption under the old name, which no longer exists when wiring runs: it has
  # to find the downloader by the endpoint the migration pointed at the
  # release's qBittorrent, take it over, and record the id the app really has.
  # A second downloader for one qBittorrent is what PBH refuses at the create,
  # and what this would have left behind if it had simply made one.
  log "PeerBanHelper's downloader: adopted by endpoint, not created again"
  pbh_result=$(status_field "[s['result'] for s in d['steps'] if s['name']=='pbh/downloader'][0]" 2>/dev/null || true)
  pbh_detail=$(status_field "[s['detail'] for s in d['steps'] if s['name']=='pbh/downloader'][0]" 2>/dev/null || true)
  echo "  receipt: pbh/downloader ${pbh_result:-missing} (${pbh_detail:-no detail})"
  [ "$pbh_result" = ok ] ||
    { echo "the wiring Job did not take over the source's PeerBanHelper downloader: ${pbh_result:-missing} (${pbh_detail:-no detail})"; fail=1; }
  case "$pbh_detail" in
    created | recreated*)
      echo "wiring created the pbh downloader instead of adopting the one the migration carried: ${pbh_detail}"
      fail=1
      ;;
  esac
  if migrate_forward "$NS" svc/pbh 9898; then
    pbh_token=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.pbh-token}' 2>/dev/null | base64 -d || true)
    if [ -z "$pbh_token" ]; then
      echo "the release Secret holds no PeerBanHelper token"; fail=1
    else
      curl -sS --max-time 20 -H "Authorization: Bearer ${pbh_token}" \
        "http://127.0.0.1:${MIGRATE_PF_PORT}/api/downloaders" \
        >"$WORK/pbh-downloaders-migrate.json" 2>/dev/null || true
      # The id the Job recorded, the ids PBH lists, and the endpoint the one it
      # holds dials: the record has to be the app's own id, there has to be
      # exactly one row, and it has to be the release's qBittorrent.
      pbh_row=$(python3 - "$WORK/pbh-downloaders-migrate.json" <<'PY' 2>/dev/null || true
import json, sys
rows = (json.load(open(sys.argv[1])).get("data") or [])
print(len(rows), rows[0]["id"] if len(rows) == 1 else "-", rows[0]["endpoint"] if len(rows) == 1 else "-")
PY
      )
      pbh_owned=$($K -n "$NS" get cm s5n-wiring -o jsonpath='{.data.owned\.json}' 2>/dev/null |
        python3 -c 'import json,sys; print((json.load(sys.stdin).get("pbh") or {}).get("downloader", ""))' 2>/dev/null || true)
      read -r pbh_count pbh_id pbh_endpoint <<<"$pbh_row"
      echo "  PBH holds ${pbh_count:-?} downloader(s): ${pbh_id:-none} -> ${pbh_endpoint:-none} (owned.json: ${pbh_owned:-none})"
      [ "${pbh_count:-0}" = 1 ] ||
        { echo "PeerBanHelper holds ${pbh_count:-no} downloaders, want exactly the one the migration carried"; fail=1; }
      [ -n "$pbh_id" ] && [ "$pbh_id" = "$pbh_owned" ] ||
        { echo "the Job recorded ${pbh_owned:-no} downloader id, and PeerBanHelper's one downloader is ${pbh_id:-none}"; fail=1; }
      [ "$pbh_id" != "$MIGRATE_PBH_DOWNLOADER" ] ||
        { echo "the downloader is still under the source's key ${MIGRATE_PBH_DOWNLOADER}: the app's own re-key did not run"; fail=1; }
      case "$pbh_endpoint" in
        "http://qbittorrent:${MIGRATE_QBIT_PORT}/api/v2") ;;
        *) echo "the downloader dials ${pbh_endpoint:-nothing}, want the release's qBittorrent"; fail=1 ;;
      esac
    fi
    migrate_forward_stop
  else
    fail=1
  fi
  [ "$fail" = 0 ] || return 0

  # ---- assertion 1b: Plex, through the runner Job --------------------------
  # The one app whose database only its own binary may write. What is asserted,
  # in the order the migration works: the Job ran and was admitted, the database
  # it left reads back the paths the plan promised, and the release's Plex
  # starts on it. The database is read with Plex stopped, which is the state the
  # migration itself needs it in -- so Plex goes down first and comes back after
  # the read, and its readiness after that is the server opening that file.
  log "Plex: the runner Job, the database it rewrote, and the server that reads it"
  migrate_check_runner plex || fail=1
  $K -n "$NS" scale statefulset plex --replicas=0 >/dev/null ||
    { echo "could not stop Plex to read the database it was migrated with"; fail=1; return 0; }
  left=""
  for i in $(seq 1 90); do
    left=$($K -n "$NS" get pods -l app.kubernetes.io/name=plex -o name 2>/dev/null || true)
    if [ -z "$left" ]; then break; fi
    sleep 2
  done
  if [ -n "$left" ]; then
    echo "Plex is still running (${left}): the database is open and cannot be read"
    fail=1
  else
    migrate_check_plex_db "$CONFIG_PATH/plex" || fail=1
    # The database names a file now; the rename is what makes it exist.
    if [ ! -f "$DATA_PATH/media/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv" ]; then
      echo "the database's media_parts.file is /data/media/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv, and ${DATA_PATH}/media/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv is not there: the rewrite points at nothing"
      fail=1
    fi
    expected=$(chart_image plex) || { echo "could not read the chart's plex pin"; fail=1; return 0; }
    $K -n "$NS" scale statefulset plex --replicas=1 >/dev/null ||
      { echo "could not start the migrated Plex"; fail=1; return 0; }
    wait_image_ready plex-0 "$expected" ||
      { echo "Plex is not ready on the chart's ${expected} after the migration (it runs $(pod_image plex-0 || true))"; fail=1; }
    migrate_check_plex_identity || fail=1
  fi
  [ "$fail" = 0 ] || return 0

  # ---- assertion 2: paths --------------------------------------------------
  log "paths: the Arrs' root folders, qBittorrent's categories, the renames on disk"
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    case "$instance" in
      sonarr) port=8989; category=tv ;;
      *) port=7878; category="$MIGRATE_RADARR_CATEGORY" ;;
    esac
    migrate_check_arr "$instance" "$port" "$category" \
      "$(cat "$WORK/source/${instance}.client-id")" "$WORK/source/${instance}.key" || fail=1
  done
  if migrate_forward "$NS" svc/qbittorrent "$MIGRATE_QBIT_PORT"; then
    secret=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' 2>/dev/null | base64 -d || true)
    if [ -z "$secret" ]; then
      echo "the release Secret holds no qBittorrent password"; fail=1
    else
      migrate_check_qbittorrent "$secret" || fail=1
    fi
    migrate_forward_stop
  else
    fail=1
  fi
  for pair in "Downloads downloads" "library media"; do
    read -r from to <<<"$pair"
    if [ ! -d "$DATA_PATH/${to}" ]; then
      echo "the data root has no ${to} directory: the rename of ${from} did not happen"; fail=1
    fi
    if [ -e "$DATA_PATH/${from}" ]; then
      echo "${DATA_PATH}/${from} is still there: the rename left the source's directory behind"; fail=1
    fi
  done
  for category in tv "$MIGRATE_RADARR_CATEGORY"; do
    links=$(stat -c '%h' "$DATA_PATH/downloads/${category}"/*.mkv 2>/dev/null | head -1) || true
    [ "${links:-0}" -eq 2 ] ||
      { echo "${DATA_PATH}/downloads/${category}: link count is ${links:-none}, want 2 -- the download and the import it was hardlinked to are not one file any more"; fail=1; }
  done
  for instance in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" autobrr qui pbh tautulli; do
    [ -f "$CONFIG_PATH/${instance}/.s5n-e2e-source" ] ||
      { echo "${CONFIG_PATH}/${instance}/ holds no marker: migrate did not copy the whole config directory"; fail=1; }
  done
  # Plex's config is its state, so its directory on the box holds the
  # preferences and the databases and none of the library. Plex serving on those
  # files is the Plex assertion below, which is what says a trimmed copy is one
  # it can run. Plex makes its own Metadata and Cache directories when it
  # starts, so what says the library was not carried is the two cover files this
  # script put in the source's trees, and the whole-tree marker: Plex never
  # writes those.
  plex_home="$CONFIG_PATH/plex/Library/Application Support/Plex Media Server"
  for carried in "$plex_home/Preferences.xml" \
                 "$plex_home/Plug-in Support/Databases/com.plexapp.plugins.library.db"; do
    [ -f "$carried" ] ||
      { echo "${CONFIG_PATH}/plex/ holds none of the state a Plex snapshot carries (${carried} is not there)"; fail=1; }
  done
  for left in ".s5n-e2e-source" "Metadata/Posters/poster.jpg" "Cache/PhotoTranscoder/thumb.jpg"; do
    if [ -e "$CONFIG_PATH/plex/${left}" ]; then
      echo "${CONFIG_PATH}/plex/${left} is on the box, and a Plex snapshot carries neither its library nor its whole config tree"; fail=1
    fi
  done

  # ---- assertion 3: the keys the source had --------------------------------
  log "keys: each Arr answers with the API key it had before the migration"
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    key=$(cat "$WORK/source/${instance}.key")
    secret=$($K -n "$NS" get secret s5n-keys -o jsonpath="{.data.${instance}-apikey}" 2>/dev/null | base64 -d || true)
    [ "$secret" = "$key" ] ||
      { echo "the release Secret's ${instance}-apikey is not the key the source had"; fail=1; }
    case "$instance" in
      sonarr) port=8989 ;;
      *) port=7878 ;;
    esac
    if migrate_forward "$NS" "svc/${instance}" "$port"; then
      code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -H "X-Api-Key: ${key}" \
        "http://127.0.0.1:${MIGRATE_PF_PORT}/${instance}/api/v3/system/status" 2>/dev/null || true)
      migrate_forward_stop
      [ "$code" = 200 ] ||
        { echo "${instance} answered ${code:-nothing} to the API key it had before the migration"; fail=1; continue; }
      echo "  ${instance}: the key from the source is the one in the release Secret, and the app answers it"
    else
      fail=1
    fi
  done

  # ---- assertion 4: the login's rules follow the app set --------------------
  # The migration switched these apps on after the login's rules were written,
  # and the rules are derived from the app list: the apply it hands over to has
  # to leave them what init would have written for the final set.
  log "the login: each migrated Arr's API is bypassed through the Gateway, and its UI is not"
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    migrate_check_login_bypass "$instance" "$WORK/source/${instance}.key" || fail=1
  done

  # ---- assertion 5: the adopted client, and the receipt --------------------
  log "ownership: the wiring record names the source's download client, and the receipt is ok"
  $K -n "$NS" get configmap s5n-wiring -o jsonpath='{.data.owned\.json}' >"$WORK/owned.json" 2>/dev/null || true
  python3 - "$WORK/owned.json" sonarr "$MIGRATE_RADARR_INSTANCE" \
    "$(cat "$WORK/source/sonarr.client-id")" "$(cat "$WORK/source/${MIGRATE_RADARR_INSTANCE}.client-id")" <<'PY' || fail=1
import json, sys
path, sonarr, radarr, sonarr_id, radarr_id = sys.argv[1:6]
try:
    rec = json.load(open(path))
except (FileNotFoundError, json.JSONDecodeError):
    sys.exit(f"{path} is not a wiring record: owned.json was not written")
arr = rec.get("arr") or {}
for app, want in ((sonarr, sonarr_id), (radarr, radarr_id)):
    got = (arr.get(app) or {}).get("downloadClient")
    if str(got) != str(want):
        sys.exit(f"owned.json records {app}'s download client as {got}, and the source's was {want}")
print(f"  owned.json: {sonarr} {arr[sonarr]['downloadClient']}, {radarr} {arr[radarr]['downloadClient']} -- the source's ids")
PY
  [ "$(status_field 'd["status"]')" = ok ] ||
    { echo "the wiring receipt is not ok: $(status_field 'd.get("error")')"; fail=1; }

  # ---- assertion 6: a second apply of the finished plan does nothing -------
  # The state record under the config root says every step is done, so the run
  # answers Done without staging again, and the apply it hands over to finds the
  # release already rendering what it would apply. One apply, and then nothing.
  log "a second box migrate apply of the finished plan: one apply, nothing else"
  revision=$(helm_revision "$NS") || true
  # Root again: the state record it reads is root's, as every later run of a
  # real host's migration is.
  if ! box_root "$CLUSTER" migrate apply "$snapshot" --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --kubeconfig "$WORK/kubeconfig" --record "$WORK/cluster.yaml" \
    --domain "$DOMAIN" --login "$LOGIN" --password-file "$WORK/authelia-password" \
    --config-path "$CONFIG_PATH" --box-namespace "$NS" --paths-in-node \
    >"$WORK/apply-migrate-again.txt" 2>&1; then
    cat "$WORK/apply-migrate-again.txt"; fail=1; return 0
  fi
  box_files_back "$CLUSTER" "$CONFIG_PATH"
  cat "$WORK/apply-migrate-again.txt"
  grep -qF "s5n in ${NS}: the plan is already applied" "$WORK/apply-migrate-again.txt" ||
    { echo "a second run did not say the plan was already applied:"; grep -v '^$' "$WORK/apply-migrate-again.txt" | head -8; fail=1; }
  grep -qF "nothing to change" "$WORK/apply-migrate-again.txt" ||
    { echo "a second run wrote the release again: the apps it moved already render what it applied"; fail=1; }
  after=$(helm_revision "$NS") || true
  [ "$after" = "$revision" ] ||
    { echo "a second run of the same plan added Helm revision ${after} (was ${revision})"; fail=1; }
  echo "  the release stayed at revision ${after}"
}

# box is the whole interface: a client-go/klog line such as
# "I1107 ... warnings.go:107] ..." must never appear in what the operator reads.
# ---- scenario: migrate-keep ------------------------------------------------
# The same source install as `migrate`, cut over the other way: the source's own
# cluster is kept.
#
# A kept cluster is self-managed: box installs onto the cluster the source already
# runs on, manages none of its version, and the whole cutover is one command --
# stop proof, final snapshot, platform layer, install, adoption. The source's
# workloads stay where they are, at 0 replicas, so rollback is scaling them back
# up, and box never touches them.
#
# What this scenario asserts, in the order a cutover asks it:
#   - with no earlier read of the source on record, an unattended cutover refuses
#     rather than migrate a read nothing was compared against, keeps the read as a
#     survey and names `box migrate discover`;
#   - an app the earlier read had and this one cannot find stops the run, naming it
#     (the operator deleted a Deployment, which a read of a running source still
#     saw and this read cannot);
#   - an app that came back since the earlier read is listed as new and kept;
#   - the keep path installs box onto that same cluster and adopts: one seedbox
#     namespace beside the source's, the configs rewritten in place, the data
#     renamed rather than moved, the source's API keys and download-client ids
#     kept, a healthy wiring receipt, and a second apply of the finished plan that
#     writes nothing.
#
# It does not re-assert what `migrate` owns and this path cannot change: the
# platform install, the login and TLS are `fresh`'s and `preinstalled`'s, and the
# rows Plex's own binary must rewrite are `migrate`'s. What differs here is where
# the seedbox lands, and that is what this scenario is about.
migrate_keep_box() {
  local src="$WORK/hostfs/legacy" puid pgid app instance folder port category key secret code
  local expected revision plex_repo plex_tag plex_digest pin snapshot kept started
  puid=$(id -u)
  pgid=$(id -g)
  DATA_PATH="$src/var/data"
  CONFIG_PATH="$WORK/hostfs/config"
  mkdir -p "$WORK/source"

  pin=$(variant_pin plex) ||
    { echo "could not read the chart's plex pin from charts/s5n/variants.yaml"; fail=1; return 0; }
  read -r plex_repo plex_tag plex_digest <<<"$pin"
  MIGRATE_PLEX_IMAGE="${plex_repo}:${plex_tag}${plex_digest:+@${plex_digest}}"

  log "the source install's host tree under ${src}"
  # The preamble prepared box's roots inside the node, on the tree this scenario
  # shares with it (see VOLUME ROOTS). It goes, so the config root starts as empty
  # as one `box init` makes, and the tree is this user's: box runs as this user here.
  docker exec "${CLUSTER}-control-plane" bash -c "
    rm -rf '$CONFIG_PATH' '$WORK/hostfs/data'
    mkdir -p '$CONFIG_PATH'
    chown -R '${puid}:${pgid}' '$WORK/hostfs'"
  mkdir -p "$src/var/data/qbittorrent" "$src/var/data/sonarr" "$src/var/data/${MIGRATE_RADARR_INSTANCE}" \
    "$src/var/data/plex" \
    "$src/var/data/Downloads/tv" "$src/var/data/Downloads/${MIGRATE_RADARR_CATEGORY}" \
    "$src/var/data/library/tv" "$src/var/data/library/${MIGRATE_RADARR_CATEGORY}"
  printf 's5n e2e: a downloaded episode, and the import that is the same inode\n' \
    >"$src/var/data/Downloads/tv/Show.S01E01.mkv"
  ln -f "$src/var/data/Downloads/tv/Show.S01E01.mkv" "$src/var/data/library/tv/Show.S01E01.mkv"
  printf 's5n e2e: a downloaded film, and the import that is the same inode\n' \
    >"$src/var/data/Downloads/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv"
  ln -f "$src/var/data/Downloads/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv" \
    "$src/var/data/library/${MIGRATE_RADARR_CATEGORY}/Film.2020.mkv"
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE"; do
    printf 's5n e2e: this config directory was the source install\n' >"$src/var/data/${app}/.s5n-e2e-source"
  done
  printf 's5n e2e: this config directory was the source install\n' >"$src/var/data/plex/.s5n-e2e-source"
  MIGRATE_QBIT_PASSWORD=$(openssl rand -hex 16)
  printf '%s' "$MIGRATE_QBIT_PASSWORD" >"$WORK/source/qbittorrent-password"
  chmod 600 "$WORK/source/qbittorrent-password"
  migrate_legacy_conf "$src/var/data/qbittorrent/qBittorrent/qBittorrent.conf" "$MIGRATE_QBIT_PASSWORD" ||
    { echo "could not write the source qBittorrent's config"; fail=1; return 0; }

  grep -qF "containerPath: ${WORK}" "$KIND_CONFIG" ||
    { echo "the kind config has no mount for ${WORK}: the source's files would be inside the node only"; fail=1; return 0; }

  log "the source install: four apps over host directories, in namespace ${MIGRATE_LEGACY_NS}"
  migrate_source_manifests "$WORK/source-manifests.yaml" "$src" "$puid" "$pgid"
  if ! $K apply -f "$WORK/source-manifests.yaml" >"$WORK/source-apply.txt" 2>&1; then
    cat "$WORK/source-apply.txt"; fail=1; return 0
  fi
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex; do
    if ! $K -n "$MIGRATE_LEGACY_NS" rollout status "deployment/${app}" --timeout=300s; then
      echo "${app} never became ready in the source install"
      $K -n "$MIGRATE_LEGACY_NS" get pods
      fail=1; return 0
    fi
  done

  log "seeding the source through its own APIs: paths, categories, a root folder and a download client per Arr"
  if ! migrate_source_qbittorrent "$WORK/source/qbittorrent-password"; then
    fail=1; return 0
  fi
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    case "$instance" in
      sonarr) port=8989; category=tv; folder=/library/tv ;;
      *) port=7878; category="$MIGRATE_RADARR_CATEGORY"; folder="/library/${MIGRATE_RADARR_CATEGORY}" ;;
    esac
    if ! migrate_source_arr "$instance" "$port" "$category" "$folder" \
      "$src/var/data/${instance}/config.xml" "$WORK/source/${instance}.key" \
      "$WORK/source/qbittorrent-password"; then
      echo "could not seed the source ${instance}"
      fail=1; return 0
    fi
    echo "  ${instance}: root folder ${folder}, download client $(cat "$WORK/source/${instance}.client-id")"
  done
  [ "$fail" = 0 ] || return 0

  # The source stops, the way an operator cuts over into a cluster they keep: the
  # workloads stay, at 0 replicas, so scaling them back up is the rollback.
  log "stopping the source, and keeping the cluster it runs on"
  $K -n "$MIGRATE_LEGACY_NS" scale deployment --all --replicas=0 >/dev/null
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex; do
    $K -n "$MIGRATE_LEGACY_NS" wait --for=delete pod -l "app=${app}" --timeout=180s >/dev/null 2>&1 || true
  done
  [ -f "$src/var/data/qbittorrent/qBittorrent/categories.json" ] ||
    { echo "the source qBittorrent wrote no categories.json: migrate would have no category paths to rewrite"; fail=1; }
  [ "$fail" = 0 ] || return 0
  migrate_source_plex_covers "$src/var/data/plex" || { fail=1; return 0; }

  keep_cutover() { # keep_cutover <snapshot> <extra arg>...
    local out=$1
    shift
    "$WORK/box" migrate cutover --yes --out "$out" \
      --kubeconfig "$WORK/kubeconfig" --namespace "$MIGRATE_LEGACY_NS" --root / \
      --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
      --values "$WORK/values.yaml" --grant "$WORK/grant.yaml" \
      --record "$WORK/cluster.yaml" \
      --domain "$DOMAIN" --login "$LOGIN" --password-file "$WORK/authelia-password" \
      --config-path "$CONFIG_PATH" --box-namespace "$NS" --paths-in-node "$@"
  }

  # ---- the first read of a source nothing has read ------------------------
  # Nothing on record has ever read this source, so an app deleted before now is
  # invisible to this read. Unattended, box refuses rather than build a plan on a
  # read nobody compared: it lists what it found, keeps the read as a survey, and
  # names discover as the better first step.
  snapshot="$WORK/final-keep.tar.gz"
  log "cutover with no earlier read: refused, keeping its read and naming box migrate discover"
  if keep_cutover "$snapshot" >"$WORK/keep-first-read.txt" 2>&1; then
    echo "cutover took the final snapshot of a source nothing had read before"; fail=1; return 0
  fi
  cat "$WORK/keep-first-read.txt"
  for want in "this is the first read of this source" \
    "sonarr (from sonarr, in ${MIGRATE_LEGACY_NS})" \
    "invisible to this read" \
    "box migrate discover" \
    "--no-prior-discover"; do
    grep -qF -- "$want" "$WORK/keep-first-read.txt" ||
      { echo "the first-read refusal does not say '${want}'"; fail=1; }
  done
  [ -e "$snapshot" ] &&
    { echo "a refused cutover wrote the final snapshot ${snapshot}"; fail=1; }
  # The read it kept is what the next cutover compares against, so the refusal has
  # to leave one behind.
  kept=$(ls "$WORK"/migrate-*-survey.tar.gz 2>/dev/null | head -1) || true
  [ -n "${kept:-}" ] && [ -f "${kept:-}" ] ||
    { echo "the first-read refusal kept no survey: nothing would be on record"; fail=1; return 0; }
  echo "  the read it kept: ${kept}"

  # ---- an app the read of record had, and this one cannot find -------------
  # The earlier read saw the Radarr; deleting the Deployment is exactly what box
  # cannot see, and it is why a read of record matters. The run stops naming it,
  # and keeps this read too.
  log "deleting the source's Radarr after a read of record: cutover stops naming it"
  $K -n "$MIGRATE_LEGACY_NS" get deployment "$MIGRATE_RADARR_INSTANCE" -o yaml >"$WORK/radarr-deployment.yaml" ||
    { echo "could not read the source's Radarr Deployment"; fail=1; return 0; }
  $K -n "$MIGRATE_LEGACY_NS" delete deployment "$MIGRATE_RADARR_INSTANCE" >/dev/null ||
    { echo "could not delete the source's Radarr Deployment"; fail=1; return 0; }
  before=$(ls "$WORK"/migrate-*-survey.tar.gz 2>/dev/null | wc -l) || true
  if keep_cutover "$snapshot" >"$WORK/keep-diff-stop.txt" 2>&1; then
    echo "cutover migrated a source that lost an app its earlier read had"; fail=1; return 0
  fi
  cat "$WORK/keep-diff-stop.txt"
  for want in "earlier read: " \
    "${MIGRATE_RADARR_INSTANCE} (it was ${MIGRATE_RADARR_INSTANCE} in ${MIGRATE_LEGACY_NS})" \
    "cutover stops rather than migrate a source that lost one"; do
    grep -qF -- "$want" "$WORK/keep-diff-stop.txt" ||
      { echo "the diff stop does not say '${want}'"; fail=1; }
  done
  [ -e "$snapshot" ] &&
    { echo "the diff stop wrote the final snapshot ${snapshot}"; fail=1; }
  after=$(ls "$WORK"/migrate-*-survey.tar.gz 2>/dev/null | wc -l) || true
  [ "${after:-0}" -gt "${before:-0}" ] ||
    { echo "the diff stop kept no read of its own (${before} then ${after} surveys)"; fail=1; }
  [ "$fail" = 0 ] || return 0

  # ---- the app comes back, and is new since the earlier read ---------------
  log "putting the Radarr back the way the source had it, stopped"
  $K apply -f "$WORK/radarr-deployment.yaml" >/dev/null ||
    { echo "could not put the source's Radarr Deployment back"; fail=1; return 0; }
  $K -n "$MIGRATE_LEGACY_NS" scale deployment "$MIGRATE_RADARR_INSTANCE" --replicas=0 >/dev/null
  $K -n "$MIGRATE_LEGACY_NS" wait --for=delete pod -l "app=${MIGRATE_RADARR_INSTANCE}" --timeout=180s >/dev/null 2>&1 || true
  [ "$fail" = 0 ] || return 0

  # ---- something on the cluster box will not install over -----------------
  # The keep path installs onto a cluster box did not make, so it looks at what is
  # already on it before it does: a Service answering on the node's port 80 or 443
  # is the source's own front door, and box's Gateway needs those ports. The
  # Service sits in a namespace of its own, as an ingress controller does, and the
  # cutover is scoped to the source's namespace: the node's ports belong to the
  # whole cluster, so the pre-flight looks past that scope. The run stops after the
  # final snapshot, names the Service with the command to look at it, and names
  # --replace-cluster as the other road. Nothing on the cluster is changed: box
  # does not repair a cluster it did not install.
  log "a Service in another namespace holding the node's port 80: the keep path stops, naming it and the other road"
  $K create namespace frontdoor --dry-run=client -o yaml | $K apply -f - >/dev/null &&
    $K -n frontdoor create service loadbalancer frontdoor --tcp=80:80 --dry-run=client -o yaml |
    $K apply -f - >/dev/null ||
    { echo "could not put a Service on the node's port 80"; fail=1; return 0; }
  # And a GitOps mark on one of the source's own workloads: Flux would scale the
  # stopped source back up beside box, so it is a blocker of its own in the same run.
  $K -n "$MIGRATE_LEGACY_NS" label deployment "$MIGRATE_RADARR_INSTANCE" \
    kustomize.toolkit.fluxcd.io/name=legacy >/dev/null ||
    { echo "could not mark the source's Radarr as Flux's"; fail=1; return 0; }
  # The blocker stops the run by design, so this is a finished run: exit 0, with the
  # snapshot written and the operator's next step named.
  if ! keep_cutover "$WORK/blocked.tar.gz" --no-prior-discover >"$WORK/keep-blocked.txt" 2>&1; then
    cat "$WORK/keep-blocked.txt"
    echo "a blocker on the cluster failed the run instead of stopping it after the snapshot"
    fail=1; return 0
  fi
  cat "$WORK/keep-blocked.txt"
  for want in \
    "  frontdoor/frontdoor: this LoadBalancer Service answers on 80" \
    "check: kubectl -n frontdoor get service frontdoor -o wide" \
    "  ${MIGRATE_LEGACY_NS}/${MIGRATE_RADARR_INSTANCE}: Flux manages this workload (kustomize.toolkit.fluxcd.io/name=legacy)" \
    "box does not change a cluster it did not install" \
    "--replace-cluster is the other road" \
    "box migrate apply $WORK/blocked.tar.gz"; do
    grep -qF -- "$want" "$WORK/keep-blocked.txt" ||
      { echo "the blocker stop does not say '${want}'"; fail=1; }
  done
  # The snapshot it took is what --replace-cluster and the apply that follows need,
  # and nothing was installed onto the cluster the blocker named.
  [ -f "$WORK/blocked.tar.gz" ] ||
    { echo "the blocker stop took no final snapshot"; fail=1; }
  if $K get namespace "$NS" >/dev/null 2>&1; then
    echo "box installed into ${NS} over a blocker it said would stop it"; fail=1
  fi
  # The operator clears it, because box will not.
  $K -n "$MIGRATE_LEGACY_NS" label deployment "$MIGRATE_RADARR_INSTANCE" \
    kustomize.toolkit.fluxcd.io/name- >/dev/null ||
    { echo "could not remove the Flux mark"; fail=1; return 0; }
  $K delete namespace frontdoor --wait=true >/dev/null ||
    { echo "could not remove the blocking Service"; fail=1; return 0; }
  [ "$fail" = 0 ] || return 0

  # ---- the keep path: one command, onto this cluster ----------------------
  # No --replace-cluster: the cluster the source runs on is the one box installs
  # onto. --no-prior-discover is given because this run's expected answer to the
  # first-read question is known either way; the diff still runs, which is what
  # the "new since the earlier read" line below proves.
  started=$(date +%s)
  log "box migrate cutover: the final snapshot, then box onto this cluster and the adoption"
  if ! keep_cutover "$snapshot" --no-prior-discover >"$WORK/keep-cutover.txt" 2>&1; then
    cat "$WORK/keep-cutover.txt"; fail=1; return 0
  fi
  cat "$WORK/keep-cutover.txt"
  echo "  the keep path (snapshot, platform, install, adoption): $(( $(date +%s) - started ))s"
  # The read on record was diffed, not skipped by the flag, and the app that came
  # back is named as new rather than read as a gap.
  grep -q "^new since the earlier read (kept):$" "$WORK/keep-cutover.txt" ||
    { echo "the run did not diff against the read on record:"; grep -n "earlier read" "$WORK/keep-cutover.txt" || true; fail=1; }
  grep -qF "  ${MIGRATE_RADARR_INSTANCE} (from ${MIGRATE_RADARR_INSTANCE}, in ${MIGRATE_LEGACY_NS})" "$WORK/keep-cutover.txt" ||
    { echo "the run does not name the app that is new since the earlier read"; fail=1; }
  # The cluster is the one the source ran on: its workloads are still there, at 0,
  # which is what makes scaling them back up the rollback.
  for app in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE" plex; do
    replicas=$($K -n "$MIGRATE_LEGACY_NS" get deployment "$app" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)
    [ "$replicas" = 0 ] ||
      { echo "the source's ${app} is at ${replicas:-gone} replicas after the cutover: box changed a workload that is not its own"; fail=1; }
  done
  grep -q "^wiring ok" "$WORK/keep-cutover.txt" ||
    { echo "box did not report a healthy receipt for the seedbox it installed beside the source"; fail=1; }
  # Plex's snapshot is its state, and the run says so once, in the report and in
  # the confirm the operator answers.
  grep -qF "plex: carried Preferences.xml and its databases" "$WORK/keep-cutover.txt" ||
    { echo "the keep path does not say what Plex's snapshot leaves behind"; fail=1; }
  # What the migration did, named before it did it: the two renames, the kept keys
  # and the connections adopted by id.
  grep -qF "data: rename Downloads to downloads" "$WORK/keep-cutover.txt" ||
    { echo "the plan did not say it renames Downloads to box's downloads"; fail=1; }
  grep -qF "data: rename library to media" "$WORK/keep-cutover.txt" ||
    { echo "the plan did not say it renames library to box's media"; fail=1; }
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    grep -qF "${instance}: keep its API key (${instance}-apikey)" "$WORK/keep-cutover.txt" ||
      { echo "the plan did not say it keeps ${instance}'s API key"; fail=1; }
    grep -qF "${instance}: adopt downloadclient" "$WORK/keep-cutover.txt" ||
      { echo "the plan did not say it adopts ${instance}'s download client by id"; fail=1; }
  done
  [ "$fail" = 0 ] || return 0

  # ---- assertion 1: healthy, on the chart's own images ---------------------
  log "the migrated apps are ready on the chart's images, beside the source they came from"
  for instance in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE"; do
    app=$instance
    [ "$instance" = "$MIGRATE_RADARR_INSTANCE" ] && app=radarr
    expected=$(chart_image "$app") || { echo "could not read the chart's ${app} pin"; fail=1; return 0; }
    wait_image_ready "${instance}-0" "$expected" ||
      { echo "${instance}-0 is not ready on the chart's ${expected} (it runs $(pod_image "${instance}-0" || true))"; fail=1; }
  done
  [ "$fail" = 0 ] || return 0

  # ---- assertion 2: paths --------------------------------------------------
  log "paths: the Arrs' root folders, qBittorrent's categories, the renames on disk"
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    case "$instance" in
      sonarr) port=8989; category=tv ;;
      *) port=7878; category="$MIGRATE_RADARR_CATEGORY" ;;
    esac
    migrate_check_arr "$instance" "$port" "$category" \
      "$(cat "$WORK/source/${instance}.client-id")" "$WORK/source/${instance}.key" || fail=1
  done
  if migrate_forward "$NS" svc/qbittorrent "$MIGRATE_QBIT_PORT"; then
    secret=$($K -n "$NS" get secret s5n-keys -o jsonpath='{.data.qbittorrent-password}' 2>/dev/null | base64 -d || true)
    if [ -z "$secret" ]; then
      echo "the release Secret holds no qBittorrent password"; fail=1
    else
      migrate_check_qbittorrent "$secret" || fail=1
    fi
    migrate_forward_stop
  else
    fail=1
  fi
  for pair in "Downloads downloads" "library media"; do
    read -r from to <<<"$pair"
    if [ ! -d "$DATA_PATH/${to}" ]; then
      echo "the data root has no ${to} directory: the rename of ${from} did not happen"; fail=1
    fi
    if [ -e "$DATA_PATH/${from}" ]; then
      echo "${DATA_PATH}/${from} is still there: the rename left the source's directory behind"; fail=1
    fi
  done
  for category in tv "$MIGRATE_RADARR_CATEGORY"; do
    links=$(stat -c '%h' "$DATA_PATH/downloads/${category}"/*.mkv 2>/dev/null | head -1) || true
    [ "${links:-0}" -eq 2 ] ||
      { echo "${DATA_PATH}/downloads/${category}: link count is ${links:-none}, want 2 -- the download and the import it was hardlinked to are not one file any more"; fail=1; }
  done
  for instance in qbittorrent sonarr "$MIGRATE_RADARR_INSTANCE"; do
    [ -f "$CONFIG_PATH/${instance}/.s5n-e2e-source" ] ||
      { echo "${CONFIG_PATH}/${instance}/ holds no marker: migrate did not copy the whole config directory"; fail=1; }
  done
  # Plex's config is its state, so its directory on the box holds the
  # preferences and the databases and none of the library. Plex serving on those
  # files is the Plex assertion below, which is what says a trimmed copy is one
  # it can run. Plex makes its own Metadata and Cache directories when it
  # starts, so what says the library was not carried is the two cover files this
  # script put in the source's trees, and the whole-tree marker: Plex never
  # writes those.
  plex_home="$CONFIG_PATH/plex/Library/Application Support/Plex Media Server"
  for carried in "$plex_home/Preferences.xml" \
                 "$plex_home/Plug-in Support/Databases/com.plexapp.plugins.library.db"; do
    [ -f "$carried" ] ||
      { echo "${CONFIG_PATH}/plex/ holds none of the state a Plex snapshot carries (${carried} is not there)"; fail=1; }
  done
  for left in ".s5n-e2e-source" "Metadata/Posters/poster.jpg" "Cache/PhotoTranscoder/thumb.jpg"; do
    if [ -e "$CONFIG_PATH/plex/${left}" ]; then
      echo "${CONFIG_PATH}/plex/${left} is on the box, and a Plex snapshot carries neither its library nor its whole config tree"; fail=1
    fi
  done

  # ---- assertion 3: the keys the source had --------------------------------
  log "keys: each Arr answers with the API key it had before the migration"
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    key=$(cat "$WORK/source/${instance}.key")
    secret=$($K -n "$NS" get secret s5n-keys -o jsonpath="{.data.${instance}-apikey}" 2>/dev/null | base64 -d || true)
    [ "$secret" = "$key" ] ||
      { echo "the release Secret's ${instance}-apikey is not the key the source had"; fail=1; }
    case "$instance" in
      sonarr) port=8989 ;;
      *) port=7878 ;;
    esac
    if migrate_forward "$NS" "svc/${instance}" "$port"; then
      code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -H "X-Api-Key: ${key}" \
        "http://127.0.0.1:${MIGRATE_PF_PORT}/${instance}/api/v3/system/status" 2>/dev/null || true)
      migrate_forward_stop
      [ "$code" = 200 ] ||
        { echo "${instance} answered ${code:-nothing} to the API key it had before the migration"; fail=1; continue; }
      echo "  ${instance}: the key from the source is the one in the release Secret, and the app answers it"
    else
      fail=1
    fi
  done
  [ "$fail" = 0 ] || return 0

  # ---- assertion 4: the login's rules follow the app set -------------------
  log "the login: each migrated Arr's API is bypassed through the Gateway, and its UI is not"
  for instance in sonarr "$MIGRATE_RADARR_INSTANCE"; do
    migrate_check_login_bypass "$instance" "$WORK/source/${instance}.key" || fail=1
  done

  # ---- assertion 5: the adopted client, and the receipt --------------------
  log "ownership: the wiring record names the source's download client, and the receipt is ok"
  $K -n "$NS" get configmap s5n-wiring -o jsonpath='{.data.owned\.json}' >"$WORK/keep-owned.json" 2>/dev/null || true
  python3 - "$WORK/keep-owned.json" sonarr "$MIGRATE_RADARR_INSTANCE" \
    "$(cat "$WORK/source/sonarr.client-id")" "$(cat "$WORK/source/${MIGRATE_RADARR_INSTANCE}.client-id")" <<'PY' || fail=1
import json, sys
path, sonarr, radarr, sonarr_id, radarr_id = sys.argv[1:6]
try:
    rec = json.load(open(path))
except (FileNotFoundError, json.JSONDecodeError):
    sys.exit(f"{path} is not a wiring record: owned.json was not written")
arr = rec.get("arr") or {}
for app, want in ((sonarr, sonarr_id), (radarr, radarr_id)):
    got = (arr.get(app) or {}).get("downloadClient")
    if str(got) != str(want):
        sys.exit(f"owned.json records {app}'s download client as {got}, and the source's was {want}")
print(f"  owned.json: {sonarr} {arr[sonarr]['downloadClient']}, {radarr} {arr[radarr]['downloadClient']} -- the source's ids")
PY
  [ "$(status_field 'd["status"]')" = ok ] ||
    { echo "the wiring receipt is not ok: $(status_field 'd.get("error")')"; fail=1; }

  # ---- assertion 6: a second apply of the finished plan does nothing -------
  log "a second box migrate apply of the finished plan: one apply, nothing else"
  revision=$(helm_revision "$NS") || true
  if ! "$WORK/box" migrate apply "$snapshot" --yes --chart-repo "$CHART_REPO" --chart-version "$CHART_VERSION" \
    --grant "$WORK/grant.yaml" --values "$WORK/values.yaml" \
    --kubeconfig "$WORK/kubeconfig" --record "$WORK/cluster.yaml" \
    --domain "$DOMAIN" --login "$LOGIN" --password-file "$WORK/authelia-password" \
    --config-path "$CONFIG_PATH" --box-namespace "$NS" --paths-in-node \
    >"$WORK/keep-apply-again.txt" 2>&1; then
    cat "$WORK/keep-apply-again.txt"; fail=1; return 0
  fi
  cat "$WORK/keep-apply-again.txt"
  grep -qF "s5n in ${NS}: the plan is already applied" "$WORK/keep-apply-again.txt" ||
    { echo "a second run did not say the plan was already applied:"; grep -v '^$' "$WORK/keep-apply-again.txt" | head -8; fail=1; }
  grep -qF "nothing to change" "$WORK/keep-apply-again.txt" ||
    { echo "a second run wrote the release again: the apps it moved already render what it applied"; fail=1; }
  after=$(helm_revision "$NS") || true
  [ "$after" = "$revision" ] ||
    { echo "a second run of the same plan added Helm revision ${after} (was ${revision})"; fail=1; }
  echo "  the release stayed at revision ${after}"
}

no_klog() {
  for f in "$WORK"/apply*.txt "$WORK"/bootstrap*.txt "$WORK"/update-*.txt "$WORK"/rollback.txt; do
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
elif [ "$SCENARIO" = update ]; then
  update_box
elif [ "$SCENARIO" = migrate ]; then
  migrate_box
elif [ "$SCENARIO" = migrate-keep ]; then
  migrate_keep_box
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
# The update and migrate scenarios' volume roots are host directories the
# seedbox's user writes into, so what the apps left there is not this user's to
# remove: the tree goes as the node's root, which is also why it goes before the
# node does. migrate's config root is 0600 in places once the migration handed
# it to a user this script is not.
if { [ "$SCENARIO" = update ] || [ "$SCENARIO" = migrate ]; } && [ "$KEEP" != --keep ]; then
  docker exec "${CLUSTER}-control-plane" rm -rf "$WORK/hostfs" >/dev/null 2>&1 || true
fi
[ "$KEEP" = --keep ] || $KIND delete cluster --name "$CLUSTER"
exit "$fail"
