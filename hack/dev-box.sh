#!/usr/bin/env bash
# One dev build of box, the chart and the s5n image, carried to a real machine.
#
# `box bootstrap` and `box migrate apply` are what a user runs, and the e2e runs
# them on kind, which is not a host: no k0s, no disks, no operator. So before a
# release is cut, a build is taken to a machine running a fresh OS and installed
# there the way a user installs one -- without tagging or publishing anything.
#
# Three artifacts share one version, `<next patch>-dev.<short sha>`:
#
#   box                  built at that version with -X main.version, so the
#                        chart's box.s5n/min-box check is the real one
#   s5n-<dev>.tgz        the chart at that version and appVersion, so the image
#                        it names is ghcr.io/swizzin/s5n:<dev>
#   s5n-image-<dev>.tar  that image, for the test machine's node's containerd
#
# Why the *next patch*: the chart's box.s5n/min-box is the last release's
# version (0.5.0), and semver puts 0.5.0-dev.x below 0.5.0, so a dev build of
# the released version would be refused by the chart it just built.
# `<next patch>-dev.<sha>` is above that floor, and still below the release that
# follows it.
#
# box pulls charts over OCI only, and speaks plain HTTP only to a loopback
# registry (internal/release's plainHTTPFor), so the chart is served from a
# distribution container on this machine's 127.0.0.1 and the test machine
# reaches it through `ssh -R`. That is why there is a `serve` here and not a
# chart directory: there is no local-chart flag, on purpose.
#
# The image cannot be pulled either: ghcr.io/swizzin/s5n:<dev> is not published,
# so it is imported into the test machine's node by hand. NEXT.txt says where in
# the install that has to happen.
#
# Usage: hack/dev-box.sh build [--out <dir>] [--dirty]
#        hack/dev-box.sh serve <dir> [--port N]
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

# The directory a build writes when --out is not given, and the one container
# serve owns. The version itself is read from the VERSION file the build writes,
# so serve names the build it was pointed at rather than a second guess at it.
DEFAULT_OUT="dist/dev-box"
CONTAINER="dev-box-registry"

# chart_version is the chart in the tree: the version the dev build is one patch
# above, and never what the artifacts are called.
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
  # The version is a release's, not a hand-written one: the next patch is
  # ${base##*.} + 1, so there has to be a major.minor.patch to count from.
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
  # The release's own flags (release.yaml), so what is tested is the binary a
  # release ships, not a fatter unstripped one.
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${dev}" -o "$box" ./cmd/box
  # The version is the whole point: a box built without it is "dev", and skips
  # the chart's min-box check instead of meeting it. The build host can only
  # run the binary it made when it is the architecture the binary is for.
  if [ "$(go env GOOS)/$(go env GOARCH)" = linux/amd64 ]; then
    local built
    built=$("$box" version)
    [ "$built" = "$dev" ] || { echo "dev-box: the box that was built reports '${built}', not '${dev}'" >&2; exit 1; }
  else
    echo "   (not run: this host is $(go env GOOS)/$(go env GOARCH), and the binary is linux/amd64)"
  fi

  echo "== chart ${dev}"
  # A copy, like the release's own packaging step: the tree's chart is never
  # what ships, and nothing here may write inside charts/.
  build_work=$(mktemp -d "${TMPDIR:-/tmp}/dev-box.XXXXXX")
  trap 'rm -rf "$build_work"' EXIT
  mkdir -p "$build_work/charts"
  cp -a charts/s5n "$build_work/charts/s5n"
  helm package "$build_work/charts/s5n" --destination "$out" --version "$dev" --app-version "$dev" >/dev/null
  [ -f "$tgz" ] || { echo "dev-box: helm package did not produce $(basename "$tgz")" >&2; exit 1; }
  # The archive is what box pulls, so its version and appVersion are read back
  # from it: they are what the min-box check compares and what the s5n image is
  # tagged, and a chart that names the released image would test the wrong one.
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
  # VERSION is the Dockerfile's ARG for the s5n binary's own version, which a
  # release passes too: it is how `s5n version` in the node says which build the
  # imported image is.
  docker build -q --build-arg "VERSION=${dev}" -t "$image" . >/dev/null
  docker save "$image" -o "$tar"
  # docker save writes 0600; the file is carried to another machine, where the
  # operator's own account copies it and root imports it.
  chmod 0644 "$tar"
  local arch
  arch=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
  # box is built for linux/amd64; the image is the build host's architecture.
  # They pair only when the host is amd64, and a mismatch is worth saying out
  # loud before somebody carries both to a machine only one of them runs on.
  [ "$arch" = linux/amd64 ] ||
    echo "   warning: the image is ${arch} and box is linux/amd64: build on the architecture of the test machine" >&2

  printf '%s\n' "$dev" >"$out/VERSION"
  write_next "$out" "$dev" "$arch"
  echo "== wrote ${out}: box, $(basename "$tgz"), $(basename "$tar"), VERSION, NEXT.txt"
  echo "   next: hack/dev-box.sh serve ${out}   (then follow ${out}/NEXT.txt)"
}

# write_next is the test machine's order of operations, with this build's
# version filled in. The port stays a variable: the chart is served after the
# build, and the tunnel, the serve command and every box command have to name
# the same one.
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

   Then install. bootstrap installs k0s and the platform layer, and only then
   asks init's questions; the release it installs runs
   ghcr.io/swizzin/s5n:@DEV@ in the wiring Job and in qBittorrent's init
   container, and nothing publishes that image. So import it into the node in a
   second shell once bootstrap reports k0s up, before answering init:

     sudo ./box bootstrap --chart-repo "${CHART_REPO}" --chart-version "${CHART_VERSION}"

     # second shell, while init is asking:
     sudo k0s ctr -n k8s.io images import s5n-image-@DEV@.tar

   The release's pods wait for that image, and an apply that cannot find it
   leaves the release failed -- and a failed release refuses the next apply.

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
  # The port is serve's, and the commands carry it as a variable; the version,
  # the directory and the image's architecture are this build's.
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
  # 5000 is the e2e's registry, and the e2e's run is the one thing this must not
  # disturb: it publishes its own container on 127.0.0.1 and picks a free port.
  [ "$port" -ne 5000 ] || { echo "dev-box: 5000 is the e2e's registry; pass --port with another one" >&2; exit 2; }

  local version tgz
  [ -d "$dir" ] || { echo "dev-box: ${dir} is not a directory: run 'hack/dev-box.sh build' first" >&2; exit 1; }
  [ -f "$dir/VERSION" ] || { echo "dev-box: ${dir}/VERSION is missing: run 'hack/dev-box.sh build' first" >&2; exit 1; }
  version=$(tr -d '[:space:]' <"$dir/VERSION")
  tgz="$dir/s5n-${version}.tgz"
  [ -f "$tgz" ] || { echo "dev-box: ${tgz} is missing: run 'hack/dev-box.sh build' again" >&2; exit 1; }

  # The registry image is the e2e's pin, read from where it lives: one test-only
  # image in the repo, one place Renovate watches, and this script cannot drift
  # onto a registry the e2e does not prove.
  local image
  image=$(sed -n 's/^REGISTRY_IMAGE=\([^[:space:]]*\).*/\1/p' hack/e2e-box.sh)
  [ -n "$image" ] || { echo "dev-box: hack/e2e-box.sh no longer declares REGISTRY_IMAGE, so there is no pinned registry to serve from" >&2; exit 1; }

  # Idempotent: a container of ours that is running on this port is reused, and
  # anything else of ours is replaced. Two serves never stack.
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
