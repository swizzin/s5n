#!/usr/bin/env bash
# One dev build of box, the chart and the s5n image, carried to a real machine.
#
# `box bootstrap` and `box migrate apply` are what a user runs, and the e2e runs them on
# kind: no k0s, no disks, no operator. So before a release, a build goes onto a machine
# with a fresh OS and is installed there the way a user installs one, publishing nothing.
#
# Three artifacts share one version, `<next patch>-dev.<short sha>`:
#   box                  built with -X main.version, so the chart's min-box check is real
#   s5n-<dev>.tgz        the chart at that version and appVersion, naming the dev image
#   s5n-image-<dev>.tar  that image, for the test machine's node's containerd
#
# The *next patch*, because min-box is the last release's version (0.5.0) and semver puts
# 0.5.0-dev.x below it: a dev build of the released version would be refused by the chart
# it just built. `<next patch>-dev.<sha>` is above that floor and below the next release.
#
# box pulls charts over OCI only and speaks plain HTTP only to a loopback registry
# (internal/release's plainHTTPFor), so `serve` runs a distribution container on this
# machine's 127.0.0.1 and the test machine reaches it through `ssh -R`. The unpublished
# image is imported into the node by an import loop NEXT.txt starts before the install.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() {
  cat >&2 <<'USAGE'
usage: hack/dev-box.sh build [--out <dir>] [--dirty]
       hack/dev-box.sh serve <dir> [--port N]

build  writes box, the chart and the s5n image, all at one dev version, into
       <dir> (default: dist/dev-box), with VERSION and NEXT.txt: the commands
       to run on the test machine, in order. A tree with uncommitted changes is
       refused unless --dirty, which tags the version `.dirty`.
serve  serves <dir>'s chart from a registry container on 127.0.0.1:<port>
       (default: 5001) and prints the oci:// reference, the `ssh -R` line and
       how to stop it. Running it again reuses or replaces that container.
USAGE
  exit 2
}

need() { # need <tool>...
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null || { echo "dev-box: ${tool} is not on PATH" >&2; exit 2; }
  done
}

# The default --out directory, and the one container serve owns. serve reads the version
# from the VERSION a build writes, so it names the build it was pointed at.
DEFAULT_OUT="dist/dev-box"
CONTAINER="dev-box-registry"

# chart_version: the tree's chart -- one patch below the dev build, never its name.
chart_version() {
  awk '/^version:/ { print $2; exit }' charts/s5n/Chart.yaml | tr -d '"'
}

# ---- build ------------------------------------------------------------------
cmd_build() {
  local out=$DEFAULT_OUT dirty=false
  while [ $# -gt 0 ]; do
    case "$1" in
      --out) out=${2:?--out needs a directory}; shift 2 ;;
      --out=*) out=${1#--out=}; shift
               [ -n "$out" ] || { echo "dev-box: --out needs a directory" >&2; usage; } ;;
      --dirty) dirty=true; shift ;;
      *) echo "dev-box: build takes no argument '$1'" >&2; usage ;;
    esac
  done
  need git go helm docker awk

  local base dev
  base=$(chart_version)
  # The version is a release's: the next patch has to be counted from a real one.
  if ! printf '%s' "$base" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "dev-box: charts/s5n/Chart.yaml version '${base}' is not <major>.<minor>.<patch>" >&2
    exit 1
  fi
  # The next patch, not this one: see the header.
  dev="${base%.*}.$(( ${base##*.} + 1 ))-dev.$(git rev-parse --short HEAD)"
  if [ -n "$(git status --porcelain)" ]; then
    $dirty || {
      echo "dev-box: the tree has uncommitted changes, and a dev build names the commit it came from:" >&2
      git status --short >&2
      echo "dev-box: commit them, or pass --dirty to build what is here as ${dev}.dirty" >&2
      exit 1
    }
    dev="${dev}.dirty"
  fi

  mkdir -p "$out"
  local box="$out/box" tgz="$out/s5n-${dev}.tgz" image="ghcr.io/swizzin/s5n:${dev}" tar="$out/s5n-image-${dev}.tar"

  echo "== box ${dev}"
  # The release's own flags (release.yaml), so what is tested is the shipped binary.
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${dev}" -o "$box" ./cmd/box
  # A box built without -X main.version says "dev" and skips the chart's min-box check.
  # The binary only runs here when the host is the architecture it was built for.
  if [ "$(go env GOOS)/$(go env GOARCH)" = linux/amd64 ]; then
    local built
    built=$("$box" version)
    [ "$built" = "$dev" ] || { echo "dev-box: the box that was built reports '${built}', not '${dev}'" >&2; exit 1; }
  else
    echo "   (not run: this host is $(go env GOOS)/$(go env GOARCH), and the binary is linux/amd64)"
  fi

  echo "== chart ${dev}"
  # A copy, as the release's own packaging step does: nothing may write in charts/.
  build_work=$(mktemp -d "${TMPDIR:-/tmp}/dev-box.XXXXXX")
  trap 'rm -rf "$build_work"' EXIT
  mkdir -p "$build_work/charts"
  cp -a charts/s5n "$build_work/charts/s5n"
  helm package "$build_work/charts/s5n" --destination "$out" --version "$dev" --app-version "$dev" >/dev/null
  [ -f "$tgz" ] || { echo "dev-box: helm package did not produce $(basename "$tgz")" >&2; exit 1; }
  # The archive's version and appVersion are read back: they are what the min-box check
  # compares and what the image is tagged.
  local field want
  for field in version appVersion; do
    want=$dev
    local got
    got=$(helm show chart "$tgz" | awk -v k="${field}:" '$1 == k { print $2; exit }' | tr -d '"')
    [ "$got" = "$want" ] || {
      echo "dev-box: $(basename "$tgz") declares ${field} '${got}', not '${want}'" >&2
      exit 1
    }
  done
  if ! helm template dev-box "$tgz" -n seedbox -f "$build_work/charts/s5n/ci/no-apps-values.yaml" |
    grep -qF "image: ${image}"; then
    echo "dev-box: $(basename "$tgz") does not render ${image}: the chart does not name the image this build makes" >&2
    exit 1
  fi

  echo "== image ${image}"
  # VERSION is the Dockerfile's ARG for the s5n binary's version, which a release passes
  # too: `s5n version` in the node then says which build the imported image is.
  docker build -q --build-arg "VERSION=${dev}" -t "$image" . >/dev/null
  docker save "$image" -o "$tar"
  # docker save writes 0600, and the file is copied by an operator, then imported by root.
  chmod 0644 "$tar"
  local arch
  arch=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
  # box is linux/amd64 and the image is the host's architecture: a mismatch is worth
  # saying before both are carried to a machine only one of them runs on.
  [ "$arch" = linux/amd64 ] ||
    echo "   warning: the image is ${arch} and box is linux/amd64: build on the architecture of the test machine" >&2

  printf '%s\n' "$dev" >"$out/VERSION"
  write_next "$out" "$dev" "$arch"
  echo "== wrote ${out}: box, $(basename "$tgz"), $(basename "$tar"), VERSION, NEXT.txt"
  echo "   next: hack/dev-box.sh serve ${out}   (then follow ${out}/NEXT.txt)"
}

# write_next is the test machine's order of operations. The port stays a variable: the
# tunnel, the serve command and every box command name the same one.
write_next() {
  local out=$1 dev=$2 arch=$3
  cat >"$out/NEXT.txt" <<'NEXT'
dev-box build @DEV@

Run the commands below from the checkout this directory was built in.

  box                    box @DEV@, linux/amd64
  chart                  s5n-@DEV@.tgz, version and appVersion @DEV@
  image                  s5n-image-@DEV@.tar, ghcr.io/swizzin/s5n:@DEV@ (@ARCH@)
  VERSION                @DEV@

Nothing here is published. The chart is served from the build machine over
loopback and reaches the test machine through a tunnel; the image is imported
into the test machine's node by hand. Every command below names this release.

  PORT=5001
  CHART_VERSION=@DEV@
  CHART_REPO=oci://127.0.0.1:${PORT}/charts/s5n

`PORT` must be the port `serve` uses (its default, 5001, unless --port says
otherwise) and it must not be 5000.

1. On the build machine, serve the chart. This starts a registry container on
   127.0.0.1:${PORT} and pushes the chart to it; running it again reuses or
   replaces that container:

     hack/dev-box.sh serve @OUT@ --port ${PORT}

2. On the build machine, open the tunnel to the test machine and leave it
   running for every box command. Connections to 127.0.0.1:${PORT} on the test
   machine then arrive at this machine's registry, so box on the test machine
   pulls over loopback -- the only host box speaks plain HTTP to. The test
   machine's sshd has to allow remote forwarding, which is its default:

     ssh -N -R ${PORT}:127.0.0.1:${PORT} <user>@<test-host>

3. Copy the three files over:

     scp @OUT@/box @OUT@/s5n-@DEV@.tgz @OUT@/s5n-image-@DEV@.tar <user>@<test-host>:

4. On the test machine, as root. This one changes nothing and is the cheap
   check that the tunnel, the chart and the box version agree before a host is
   touched:

     sudo ./box bootstrap --dry-run --chart-repo "${CHART_REPO}" --chart-version "${CHART_VERSION}"

   The release runs ghcr.io/swizzin/s5n:@DEV@ (the wiring Job, qBittorrent's
   init container, a rehearsal's gate and lock probes), and nothing publishes
   that image, so it goes into the node's containerd. There is no containerd
   until box has installed k0s, and box installs the release right after, so
   start this loop in a second shell BEFORE the install: it retries until k0s's
   containerd answers and imports the image as soon as it does, ahead of
   anything that needs it. Leave it running; it exits once the image is in.

     until sudo /usr/local/bin/k0s ctr -n k8s.io images import s5n-image-@DEV@.tar >/dev/null 2>&1; do sleep 5; done; echo "s5n image imported"

   Then install, one of:

     # a fresh box
     sudo ./box bootstrap --chart-repo "${CHART_REPO}" --chart-version "${CHART_VERSION}"

     # a migration onto this clean host: apply is the installer here (it runs
     # bootstrap and init itself), so there is no separate bootstrap. Copy the
     # snapshot over with the other files; it is root-only, keep it 0600.
     sudo ./box migrate apply <snapshot> --chart-repo "${CHART_REPO}" --chart-version "${CHART_VERSION}"

   A pod that starts before the import waits in ImagePullBackOff and retries,
   so a late import still lands while box waits; one that never comes leaves
   the release failed, and a failed release refuses the next apply.

5. Every later command on the test machine names the same release, with the
   tunnel open:

     sudo ./box apply --chart-repo "${CHART_REPO}" --chart-version "${CHART_VERSION}"
     sudo ./box status
     sudo ./box update --chart-repo "${CHART_REPO}" --chart-version "${CHART_VERSION}"

   A newer build is a new version: `box update` is the command that moves an
   installed box to it (and that build's image goes into the node first, the
   same way).

6. When you are done here: stop the registry with `docker rm -f dev-box-registry`
   and close the tunnel. On the test machine nothing is left behind but the box
   you installed.
NEXT
  # The port is serve's; the version, directory and architecture are this build's.
  sed -i -e "s|@DEV@|${dev}|g" -e "s|@OUT@|${out}|g" -e "s|@ARCH@|${arch}|g" "$out/NEXT.txt"
}

# ---- serve ------------------------------------------------------------------
cmd_serve() {
  local dir=${1:-}
  case "$dir" in
    ""|-*) echo "dev-box: serve needs the directory a build wrote" >&2; usage ;;
    *) shift ;;
  esac
  local port=5001
  while [ $# -gt 0 ]; do
    case "$1" in
      --port) port=${2:?--port needs a number}; shift 2 ;;
      --port=*) port=${1#--port=}; shift ;;
      *) echo "dev-box: serve takes no argument '$1'" >&2; usage ;;
    esac
  done
  need docker helm sed curl
  case "$port" in
    ''|*[!0-9]*) echo "dev-box: --port '${port}' is not a number" >&2; exit 2 ;;
  esac
  [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || { echo "dev-box: --port ${port} is not a port" >&2; exit 2; }
  # 5000 is the e2e's registry, which this must not disturb.
  [ "$port" -ne 5000 ] || { echo "dev-box: 5000 is the e2e's registry; pass --port with another one" >&2; exit 2; }

  local version tgz
  [ -d "$dir" ] || { echo "dev-box: ${dir} is not a directory: run 'hack/dev-box.sh build' first" >&2; exit 1; }
  [ -f "$dir/VERSION" ] || { echo "dev-box: ${dir}/VERSION is missing: run 'hack/dev-box.sh build' first" >&2; exit 1; }
  version=$(tr -d '[:space:]' <"$dir/VERSION")
  tgz="$dir/s5n-${version}.tgz"
  [ -f "$tgz" ] || { echo "dev-box: ${tgz} is missing: run 'hack/dev-box.sh build' again" >&2; exit 1; }

  # The registry image is the e2e's pin, the one place Renovate watches.
  local image
  image=$(sed -n 's/^REGISTRY_IMAGE=\([^[:space:]]*\).*/\1/p' hack/e2e-box.sh)
  [ -n "$image" ] || { echo "dev-box: hack/e2e-box.sh no longer declares REGISTRY_IMAGE, so there is no pinned registry to serve from" >&2; exit 1; }

  # Idempotent: ours on this port is reused, anything else of ours is replaced.
  local running="" mapped=""
  if docker inspect "$CONTAINER" >/dev/null 2>&1; then
    running=$(docker inspect --format '{{.State.Running}}' "$CONTAINER")
    mapped=$(docker port "$CONTAINER" 5000/tcp 2>/dev/null | head -1 | sed 's/.*://' || true)
  fi
  if [ "$running" = true ] && [ "$mapped" = "$port" ]; then
    echo "== reusing the registry container ${CONTAINER} on 127.0.0.1:${port}"
  else
    if docker inspect "$CONTAINER" >/dev/null 2>&1; then
      echo "== replacing the registry container ${CONTAINER} (it was not running on 127.0.0.1:${port})"
      docker rm -f "$CONTAINER" >/dev/null
    else
      echo "== starting the registry container ${CONTAINER} on 127.0.0.1:${port}"
    fi
    docker run -d --rm --name "$CONTAINER" -p "127.0.0.1:${port}:5000" "$image" >/dev/null ||
      { echo "dev-box: could not bind 127.0.0.1:${port}; pass --port with a free one" >&2; exit 1; }
    local i
    for i in $(seq 1 30); do
      curl -fsS "http://127.0.0.1:${port}/v2/" >/dev/null 2>&1 && break
      if [ "$i" = 30 ]; then
        echo "dev-box: the registry never answered on 127.0.0.1:${port}; the container's logs are 'docker logs ${CONTAINER}'" >&2
        exit 1
      fi
      sleep 1
    done
  fi

  echo "== pushing ${tgz} to 127.0.0.1:${port}"
  helm push "$tgz" "oci://127.0.0.1:${port}/charts" --plain-http >/dev/null

  cat <<EOF

chart    oci://127.0.0.1:${port}/charts/s5n   version ${version}   (plain HTTP, loopback only)
tunnel   ssh -N -R ${port}:127.0.0.1:${port} <user>@<test-host>     (run on this machine)
box      --chart-repo oci://127.0.0.1:${port}/charts/s5n --chart-version ${version}
stop     docker rm -f ${CONTAINER}

${dir}/NEXT.txt has the same commands in the order the test machine runs them.
EOF
}

case "${1:-}" in
  build) shift; cmd_build "$@" ;;
  serve) shift; cmd_serve "$@" ;;
  *) usage ;;
esac
