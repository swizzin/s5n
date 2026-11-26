#!/usr/bin/env bash
# The s5n release's steps that are not pushes, which release.yaml runs between the tag
# and `gh release create`. They live here so test.yaml's rehearsal runs the same code: a
# copy of this logic in a test job passes while the release fails, which is how two bugs
# reached the 0.3.0 tag.
#
# Usage: hack/release.sh <step> [argument]
#   route <output>            (IN_VERSION, IN_REF, REF_NAME, REF) family, version, ref,
#                             and whether this is the automatic release
#   verify-version            (VERSION, AUTOMATED) the chart's version, or one patch
#                             above the last s5n-v* tag
#   stamp-digest <workdir>    (DIGEST) copy charts/s5n and stamp s5n.default.digest
#   package-chart <workdir>   (VERSION, DIGEST, AUTOMATED, IMAGE) validate, package,
#                             lint and render
#   sign-sums <dist-dir>      (BOX_SIGNING_KEY, VERSION) sign sha256sums.txt
#   notes <body>              (VERSION) write the release body and print it
#
# Inputs are the variables release.yaml's steps already set. Paths must be absolute. The
# pushes -- helm registry login, helm push, docker push, gh release create, git tag --
# stay in the workflow: a script that could publish is a script that can publish by
# accident.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: hack/release.sh route <output>
       hack/release.sh verify-version
       hack/release.sh stamp-digest <workdir>
       hack/release.sh package-chart <workdir>
       hack/release.sh sign-sums <dist-dir>
       hack/release.sh notes <body.md>
USAGE
  exit 2
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

# need_path <what> <path>: every path a step writes is absolute.
need_path() {
  case "${2:-}" in
  /*) ;;
  *) echo "release: $1 needs an absolute path (got '${2:-}')" >&2; exit 2 ;;
  esac
}

# step_route is release.yaml's first question: a tag push or a call from the push to main,
# then which family, version and ref. It reads the version input, never github.event_name:
# a called workflow's `github` context is the caller's, so event_name there is `push` and
# ref_name `main`, and a test for workflow_call is false forever -- route once took the
# tag-push branch on main and refused. inputs exist only under workflow_call, so a set
# version input is the only signal that survives, and it is what makes a step automatic.
step_route() {
  local out=${1:-}
  need_path output "$out"
  local family version ref automated=false
  if [[ -n "${IN_VERSION:-}" ]]; then
    # Only the chart family is ever called.
    family=s5n
    version="$IN_VERSION"
    ref="$IN_REF"
    automated=true
  else
    # A tag push: the family is the prefix, the version what follows it.
    case "$REF_NAME" in
      s5n-v*) family=s5n; version="${REF_NAME#s5n-v}" ;;
      box-v*) family=box; version="${REF_NAME#box-v}" ;;
      *) echo "tag ${REF_NAME} is neither s5n-v* nor box-v*" >&2; exit 1 ;;
    esac
    ref="$REF"
  fi
  case "$family" in
    s5n|box) ;;
    *) echo "family '${family}': only s5n or box" >&2; exit 1 ;;
  esac
  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]]; then
    echo "version '${version}': a release version is <major>.<minor>.<patch>" >&2
    exit 1
  fi
  echo "family=${family} version=${version} ref=${ref}"
  {
    echo "family=${family}"
    echo "version=${version}"
    echo "ref=${ref}"
    echo "automated=${automated}"
  } >> "$out"
}

# step_verify_version: a maintainer's tag equals Chart.yaml's version and appVersion; an
# automatic release is the chart's major.minor, one patch above the previous tag.
step_verify_version() {
  : "${VERSION:?VERSION is unset}"
  local automated=${AUTOMATED:-false}
  # Only top-level keys: annotations and kubeVersion are not this question.
  local chart_version app_version
  chart_version=$(awk '/^version:/ { print $2; exit }' charts/s5n/Chart.yaml)
  app_version=$(awk '/^appVersion:/ { print $2; exit }' charts/s5n/Chart.yaml | tr -d '"')
  if [[ "$automated" = true ]]; then
    # The workflow never commits the bump, so Chart.yaml carries the previous patch.
    local want_major_minor prev want
    want_major_minor="${chart_version%.*}"
    if [[ "${VERSION%.*}" != "$want_major_minor" ]]; then
      echo "the automatic version ${VERSION} is not ${want_major_minor}.x, charts/s5n/Chart.yaml's major.minor" >&2
      exit 1
    fi
    git fetch --tags --force --quiet
    # grep exits 1 when the new tag is the only one; under pipefail that ends the step.
    prev=$(git tag --list 's5n-v*' | sed 's/^s5n-v//' | { grep -vx "$VERSION" || true; } | sort -V | tail -1)
    if [[ -z "$prev" ]]; then
      echo "no previous s5n-v* tag for ${VERSION} to be one patch above" >&2
      exit 1
    fi
    want="${prev%.*}.$(( ${prev##*.} + 1 ))"
    if [[ "$VERSION" != "$want" ]]; then
      echo "the automatic version ${VERSION} is not one patch above ${prev} (want ${want})" >&2
      exit 1
    fi
  else
    if [[ "$chart_version" != "$VERSION" ]]; then
      echo "charts/s5n/Chart.yaml version is ${chart_version}, but the tag is s5n-v${VERSION}: bump the chart in the commit you tag" >&2
      exit 1
    fi
    if [[ "$app_version" != "$VERSION" ]]; then
      echo "charts/s5n/Chart.yaml appVersion is ${app_version}, but the tag is s5n-v${VERSION}: the image tag and the chart's appVersion are the same version" >&2
      exit 1
    fi
  fi
  echo "releasing s5n ${VERSION}"
}

# step_stamp_digest: the tree as it ships, with this run's image digest in
# s5n.default.digest. The committed variants.yaml keeps its "" and a checkout stays
# reproducible.
step_stamp_digest() {
  local work=${1:-}
  need_path workdir "$work"
  : "${DIGEST:?DIGEST is unset}"
  rm -rf "$work"
  mkdir -p "$work/charts" "$work/hack" "$work/dist"
  cp -a charts/s5n "$work/charts/s5n"
  cp hack/validate-chart.sh "$work/hack/validate-chart.sh"

  # values.schema.json owns what a digest looks like: a tightened pattern fails this step
  # instead of quietly accepting something the chart would refuse.
  local pattern
  pattern=$(jq -r '.definitions.digest.pattern' charts/s5n/values.schema.json)
  if [[ "$pattern" != '^(|sha256:[a-f0-9]{64})$' ]]; then
    echo "values.schema.json's digest pattern is '${pattern}'; this workflow stamps a sha256 digest" >&2
    exit 1
  fi
  if [[ ! "$DIGEST" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    echo "digest '${DIGEST}' is not a sha256 digest" >&2
    exit 1
  fi

  # Only s5n.default.digest; every other variant keeps the digest it shipped with.
  python3 - "$work/charts/s5n/variants.yaml" "$DIGEST" <<'PY'
import sys
path, digest = sys.argv[1], sys.argv[2]
lines = open(path).read().splitlines(keepends=True)
out, in_s5n, in_default, stamped = [], False, False, False
for line in lines:
    stripped = line.strip()
    if stripped and not line[:1].isspace() and not stripped.startswith("#"):
        in_s5n = stripped == "s5n:"
        in_default = False
    elif in_s5n and line.startswith("  ") and not line.startswith("    ") and stripped:
        in_default = stripped == "default:"
    elif in_s5n and in_default and stripped.startswith("digest:"):
        indent = line[: len(line) - len(line.lstrip())]
        line = f'{indent}digest: "{digest}"\n'
        stamped = True
    out.append(line)
if not stamped:
    sys.exit("variants.yaml: no s5n.default.digest line to stamp")
open(path, "w").write("".join(out))
PY
}

# step_package_chart validates the stamped copy, packages it and renders the archive:
# the tree is not what ships, this copy is. An automated release takes its version from
# the input, because the workflow never commits to main; a maintainer's tag already
# matches Chart.yaml.
step_package_chart() {
  local work=${1:-}
  need_path workdir "$work"
  : "${VERSION:?VERSION is unset}"
  : "${DIGEST:?DIGEST is unset}"
  local automated=${AUTOMATED:-false}
  # release.yaml sets IMAGE at the workflow level; the default is the same value.
  local image=${IMAGE:-ghcr.io/swizzin/s5n}

  "$work/hack/validate-chart.sh" 1.37.0
  if [[ "$automated" = true ]]; then
    helm package "$work/charts/s5n" --destination "$work/dist" \
      --version "$VERSION" --app-version "$VERSION"
  else
    helm package "$work/charts/s5n" --destination "$work/dist"
  fi
  local tgz="$work/dist/s5n-${VERSION}.tgz"
  test -f "$tgz" || { echo "helm package did not produce $(basename "$tgz")" >&2; exit 1; }
  # The package lints, reads back and renders the image this run pushed, by tag and
  # digest. The chart refuses to render without a login, so the render says there is one
  # (as box does) and gives the host its apps are served at; it ships no torrent port,
  # since a helm install sets one from the band Kubernetes does not assign itself.
  helm lint "$tgz" --set auth.edge=true --set gateway.host=box.example.com \
    --set apps.qbittorrent.torrent.port=30080
  helm show values "$tgz" >/dev/null
  local rendered want
  rendered=$(helm template release "$tgz" -n seedbox \
    --set auth.edge=true --set gateway.host=box.example.com \
    --set apps.qbittorrent.torrent.port=30080)
  want="${image}:${VERSION}@${DIGEST}"
  if ! grep -q "$want" <<<"$rendered"; then
    echo "the packaged chart does not render ${want}" >&2
    exit 1
  fi
  echo "packaged $(basename "$tgz") naming ${want}"
}

# step_sign_sums signs the checksums the box release publishes. box refuses a release
# whose signature it cannot verify and never trusts the checksums alone, so the step fails
# here rather than publishing one nobody can install. It runs here, not in the workflow, so
# the rehearsal signs with a throwaway key through the same code.
#
# The version is part of what is signed: a signature over the checksums alone could be
# reused by any release publishing the same binaries, and an older release's validly
# signed sums under this tag would be a downgrade.
step_sign_sums() {
  local dist=${1:-}
  need_path dist-dir "$dist"
  : "${VERSION:?VERSION is unset}"
  if [[ -z "${BOX_SIGNING_KEY:-}" ]]; then
    echo "release: BOX_SIGNING_KEY is empty; a box release is signed, so this one must not publish" >&2
    exit 1
  fi
  local sums="$dist/sha256sums.txt"
  if [[ ! -f "$sums" ]]; then
    echo "release: ${sums} is not there to sign: build the binaries before signing" >&2
    exit 1
  fi
  go run ./hack/sign sign "$VERSION" "$sums"
}

# step_notes: the app images and platform pins that moved since the previous s5n-v* tag,
# or everything a first tag ships -- through the same parser the release decision uses.
step_notes() {
  local out=${1:-}
  need_path body "$out"
  : "${VERSION:?VERSION is unset}"
  git fetch --tags --force --quiet
  local tag prev
  tag="s5n-v${VERSION}"
  # The first s5n-v* tag leaves grep nothing to print, and its exit 1 under pipefail would
  # end the step before the --all branch.
  prev=$(git tag --list 's5n-v*' | { grep -vx "$tag" || true; } | sort -V | tail -1)
  mkdir -p "$(dirname "$out")"
  if [[ -n "$prev" ]]; then
    go run ./hack/release notes "$prev" "$tag" > "$out"
  else
    go run ./hack/release notes --all "$tag" > "$out"
  fi
  cat "$out"
}

case "${1:-}" in
  route) step_route "${2:-}" ;;
  verify-version) step_verify_version ;;
  stamp-digest) step_stamp_digest "${2:-}" ;;
  package-chart) step_package_chart "${2:-}" ;;
  sign-sums) step_sign_sums "${2:-}" ;;
  notes) step_notes "${2:-}" ;;
  *) usage ;;
esac
