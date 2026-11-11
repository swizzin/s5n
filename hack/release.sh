#!/usr/bin/env bash
# The s5n release's steps that are not pushes: everything release.yaml does
# between the tag and `gh release create`. They live here, not in the workflow,
# so test.yaml's release-rehearsal job runs the same code the release runs; a
# copy of this logic in a test job passes while the release fails, which is how
# two bugs reached the 0.3.0 tag.
#
# Usage: hack/release.sh <step> [argument]
#
#   verify-version            (VERSION, AUTOMATED) the version is the chart's,
#                             or one patch above the last s5n-v* tag
#   stamp-digest <workdir>    (DIGEST) copy charts/s5n there and stamp
#                             s5n.default.digest in the copy's variants.yaml
#   package-chart <workdir>   (VERSION, DIGEST, AUTOMATED, IMAGE) the chart's
#                             validation, the package, the lint and the render
#   notes <body>              (VERSION) write the release body and print it
#
# Inputs are the environment variable names release.yaml's steps already set.
# A path argument must be absolute: each step writes only the path it is given
# (the workflow passes ${RUNNER_TEMP}/..., a local run $TMPDIR/...). The pushes
# -- helm registry login, helm push, docker push, gh release create, git tag --
# stay in the workflow: a script that could publish is a script that can publish
# by accident.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: hack/release.sh verify-version
       hack/release.sh stamp-digest <workdir>
       hack/release.sh package-chart <workdir>
       hack/release.sh notes <body.md>
USAGE
  exit 2
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

# need_path <what> <path>: every path a step writes is absolute, so the caller's
# working directory is never part of the answer.
need_path() {
  case "${2:-}" in
  /*) ;;
  *) echo "release: $1 needs an absolute path (got '${2:-}')" >&2; exit 2 ;;
  esac
}

# step_verify_version is release.yaml's s5n-verify question: a maintainer's tag
# equals Chart.yaml's version and appVersion; an automatic release is the
# chart's major.minor, one patch above the previous s5n-v* tag.
step_verify_version() {
  : "${VERSION:?VERSION is unset}"
  local automated=${AUTOMATED:-false}
  # Only top-level keys: Chart.yaml's annotations and kubeVersion are not
  # this question, and helm is not needed to read one line.
  local chart_version app_version
  chart_version=$(awk '/^version:/ { print $2; exit }' charts/s5n/Chart.yaml)
  app_version=$(awk '/^appVersion:/ { print $2; exit }' charts/s5n/Chart.yaml | tr -d '"')
  if [[ "$automated" = true ]]; then
    # The automatic release's version. The workflow never commits the
    # bump, so Chart.yaml still carries the previous patch: the version
    # has to be the chart's major.minor, one patch above the last
    # s5n-v* tag.
    local want_major_minor prev want
    want_major_minor="${chart_version%.*}"
    if [[ "${VERSION%.*}" != "$want_major_minor" ]]; then
      echo "the automatic version ${VERSION} is not ${want_major_minor}.x, charts/s5n/Chart.yaml's major.minor" >&2
      exit 1
    fi
    git fetch --tags --force --quiet
    # grep exits 1 when the new tag is the only one; under pipefail
    # that would end the step before the check below could say so.
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

# step_stamp_digest builds the packaged chart's working copy: the tree as it
# ships, with the image digest this run built in s5n.default.digest. The
# committed variants.yaml keeps its "" and a checkout stays reproducible.
step_stamp_digest() {
  local work=${1:-}
  need_path workdir "$work"
  : "${DIGEST:?DIGEST is unset}"
  rm -rf "$work"
  mkdir -p "$work/charts" "$work/hack" "$work/dist"
  cp -a charts/s5n "$work/charts/s5n"
  cp hack/validate-chart.sh "$work/hack/validate-chart.sh"

  # values.schema.json owns what a digest may look like. Reading the
  # pattern from the schema means a tightened pattern fails this step
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

  # Only s5n.default.digest: every other variant keeps the digest it
  # shipped with.
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

# step_package_chart validates the stamped copy, packages it, and renders the
# archive: the tree is not what ships, this copy is. An automated release takes
# its version from the tag/input (Chart.yaml still carries the previous patch,
# because the workflow never commits to main); a maintainer's tag already
# matches Chart.yaml, so helm reads it there.
step_package_chart() {
  local work=${1:-}
  need_path workdir "$work"
  : "${VERSION:?VERSION is unset}"
  : "${DIGEST:?DIGEST is unset}"
  local automated=${AUTOMATED:-false}
  # release.yaml sets IMAGE at the workflow level. The default is the same
  # value, so a local run and the rehearsal need no second copy of it.
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
  # The package itself lints, reads back, and renders the image this run
  # pushed, by tag and by digest: the archive is the chart that ships.
  # The chart refuses to render without a login in front, so the render
  # says there is one, as box does; an app served at its own host name
  # also needs the domain box serves it at, so gateway.host is the same
  # fixture the chart's own validation renders with (hack/validate-chart.sh).
  helm lint "$tgz"
  helm show values "$tgz" >/dev/null
  local rendered want
  rendered=$(helm template release "$tgz" -n seedbox \
    --set auth.edge=true --set gateway.host=box.example.com)
  want="${image}:${VERSION}@${DIGEST}"
  if ! grep -q "$want" <<<"$rendered"; then
    echo "the packaged chart does not render ${want}" >&2
    exit 1
  fi
  echo "packaged $(basename "$tgz") naming ${want}"
}

# step_notes is release.yaml's release body: the app images and the platform
# pins that moved since the previous s5n-v* tag, or everything a first tag
# ships. Both files, through the same parser the release decision uses, so
# "moved" means the same thing to the body as to the job that cut the tag.
step_notes() {
  local out=${1:-}
  need_path body "$out"
  : "${VERSION:?VERSION is unset}"
  git fetch --tags --force --quiet
  local tag prev
  tag="s5n-v${VERSION}"
  # The first s5n-v* tag leaves grep nothing to print, and its exit 1
  # under pipefail would end the step before the --all branch.
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
  verify-version) step_verify_version ;;
  stamp-digest) step_stamp_digest "${2:-}" ;;
  package-chart) step_package_chart "${2:-}" ;;
  notes) step_notes "${2:-}" ;;
  *) usage ;;
esac
