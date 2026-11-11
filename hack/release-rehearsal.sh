#!/usr/bin/env bash
# The release rehearsal: release.yaml's s5n steps, run through the scripts the
# release itself calls (hack/release.sh) against the cases the path has met.
# Two bugs lived in that path until the 0.3.0 tag -- a packaged chart rendered
# with no auth.edge, and a first-tag grep that ended the step under pipefail --
# so this runs the same code, never a copy of it: a copy passes while the
# release fails, which is the failure that shipped both bugs.
#
# CI runs it as test.yaml's release-rehearsal job; a maintainer runs it on a
# checkout. It needs git, go, helm, kubeconform, jq and python3, builds no
# image and touches no cluster. Tags are made and deleted only in a scratch
# clone under $TMPDIR, which has its own origin: nothing is pushed.
#
# Usage: hack/release-rehearsal.sh
set -euo pipefail
cd "$(dirname "$0")/.."

for tool in git go helm kubeconform jq python3; do
  command -v "$tool" >/dev/null || { echo "release-rehearsal: ${tool} is not on PATH" >&2; exit 2; }
done

release="$(pwd)/hack/release.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

# The digest a release stamps is the one its image build returned; here it is
# fixed, so the render check has something exact to look for.
digest="sha256:$(printf 'a%.0s' {1..64})"
chart_version=$(awk '/^version:/ { print $2; exit }' charts/s5n/Chart.yaml)
auto_version="${chart_version%.*}.$(( ${chart_version##*.} + 1 ))"

ok() { echo "== $*"; }
bad() { echo "FAIL: $*" >&2; fail=1; }

# run keeps a command's exit status ours to read: under `set -e` the assignment
# alone would end the script. Leaves $out and $rc for the caller.
run() {
  if out=$("$@" 2>&1); then rc=0; else rc=$?; fi
}

expect_ok() { # expect_ok <name> <substring of stdout> <command...>
  local name=$1 want=$2
  shift 2
  run "$@"
  if [ "$rc" -ne 0 ]; then
    printf '%s\n' "$out" >&2
    bad "$name: exit ${rc}, wanted 0"
  elif [[ "$out" != *"$want"* ]]; then
    printf '%s\n' "$out" >&2
    bad "$name: exit 0, but its output does not say '${want}'"
  else
    ok "$name"
  fi
}

expect_fail() { # expect_fail <name> <the message it must refuse with> <command...>
  local name=$1 want=$2
  shift 2
  run "$@"
  if [ "$rc" -eq 0 ]; then
    bad "$name: exit 0, wanted a refusal"
  elif [[ "$out" != *"$want"* ]]; then
    printf '%s\n' "$out" >&2
    bad "$name: exit ${rc} without the message '${want}'"
  else
    ok "$name: refused, exit ${rc}: ${want}"
  fi
}

has() { # has <name> <file> <text>
  if [ ! -f "$2" ]; then
    bad "$1: $(basename "$2") was not written"
  elif grep -qF -- "$3" "$2"; then
    ok "$1"
  else
    sed -n '1,20p' "$2" >&2 || true
    bad "$1: $(basename "$2") does not contain '${3}'"
  fi
}

hasnt() { # hasnt <name> <file> <text>
  if [ ! -f "$2" ]; then
    bad "$1: $(basename "$2") was not written"
  elif grep -qF -- "$3" "$2"; then
    bad "$1: $(basename "$2") contains '${3}'"
  else
    ok "$1"
  fi
}

# ---- a. the packaged chart ---------------------------------------------------
# Bug 1: the render check passed no values and the chart refuses to render
# without auth.edge=true, so the step died after a successful package. Stamping
# before packaging is the release's order, so the render sees the stamped copy.
echo "== a. the packaged chart"
work="$tmp/chart"
run env VERSION="$chart_version" DIGEST="$digest" AUTOMATED=false "$release" stamp-digest "$work"
if [ "$rc" -ne 0 ]; then
  printf '%s\n' "$out" >&2
  bad "stamp-digest: exit ${rc}"
fi

expect_ok "a maintainer's chart version (${chart_version}) packages and renders its image by digest" \
  "packaged s5n-${chart_version}.tgz naming ghcr.io/swizzin/s5n:${chart_version}@${digest}" \
  env VERSION="$chart_version" DIGEST="$digest" AUTOMATED=false "$release" package-chart "$work"

expect_ok "an automatic patch (${auto_version}) packages with --version/--app-version and renders it" \
  "packaged s5n-${auto_version}.tgz naming ghcr.io/swizzin/s5n:${auto_version}@${digest}" \
  env VERSION="$auto_version" DIGEST="$digest" AUTOMATED=true "$release" package-chart "$work"

# ---- b and c. the tag states, in a scratch clone -----------------------------
# The scripts ask git what the tags are, so the cases are made where the tags
# are: a clone of this checkout whose origin is itself, so a `git fetch --tags`
# inside a step cannot pull a released tag back into a case that needs it gone.
echo "== b, c. tag states in a scratch clone"
clone="$tmp/clone"
git clone --no-local --quiet . "$clone"
git -C "$clone" remote set-url origin "$clone"
git -C "$clone" tag -l 's5n-v*' | xargs -r git -C "$clone" tag -d >/dev/null
git -C "$clone" config user.name rehearsal
git -C "$clone" config user.email rehearsal@example.com
# The clone runs this checkout's script, not the committed one, so a maintainer
# rehearsing an edit does not rehearse the commit before it. In CI the tree is
# clean and the two are the same bytes.
cp -p "$release" "$clone/hack/release.sh"

# b.1: no s5n-v* tag at all -- the --all branch, the first release's body. The
# ref the step hands `go run ./hack/release notes --all` is the tag it is
# releasing, and in this state that tag is gone, so the two lists come out
# empty; the branch is what this case can prove, and b.2 proves the lists when
# the tag is there. No workflow run reaches this state -- ci.yaml creates the
# tag before it calls -- so the step's empty body here is recorded, not fixed.
body="$tmp/body-none.md"
expect_ok "notes: no s5n-v* tag at all takes the --all branch" \
  "The first s5n release: every image and platform pin it ships." \
  env VERSION="$chart_version" "$clone/hack/release.sh" notes "$body"
hasnt "notes: no tag -- not the diff body" "$body" "that moved since"

# b.2: only the tag being released (bug 2 for notes). grep prints nothing, and
# with the fix in place the step reaches the --all branch instead of ending.
git -C "$clone" tag "s5n-v${chart_version}"
body="$tmp/body-first.md"
expect_ok "notes: the tag being released is the only s5n-v* tag" "The first s5n release:" \
  env VERSION="$chart_version" "$clone/hack/release.sh" notes "$body"
has "notes: first tag -- the --all body, not an empty diff" "$body" "- qbittorrent: ghcr.io/home-operations/qbittorrent:"
has "notes: first tag -- the platform pins are listed too" "$body" "### Platform pins"

# b.3: a previous tag present -- the diff body. One variant's digest moves in
# the clone, so the body has something specific to name and everything that did
# not move has to stay out.
python3 - "$clone/charts/s5n/variants.yaml" <<'PY'
import sys

path = sys.argv[1]
text = open(path).read()
start = text.index("qbittorrent:")
digest_at = text.index("digest: ", start)
line_end = text.index("\n", digest_at)
open(path, "w").write(text[:digest_at] + 'digest: sha256:' + "b" * 64 + text[line_end:])
PY
git -C "$clone" commit --quiet -am "rehearsal: one variant's digest moves"
git -C "$clone" tag "s5n-v${auto_version}"
body="$tmp/body-diff.md"
expect_ok "notes: a previous tag gives the diff body" "### App images that moved since s5n-v${chart_version}" \
  env VERSION="$auto_version" "$clone/hack/release.sh" notes "$body"
has "notes: the diff body names the app that moved" "$body" "- qbittorrent: "
hasnt "notes: the diff body is not the first-release body" "$body" "The first s5n release"
hasnt "notes: the diff body lists only what moved" "$body" "- sonarr: "

# c. the version check. Back to one previous tag (0.3.0) by this point, so the
# automatic cases have exactly one tag to be one patch above.
git -C "$clone" tag -d "s5n-v${auto_version}" >/dev/null

expect_ok "version check: a maintainer's tag equal to Chart.yaml passes" "releasing s5n ${chart_version}" \
  env VERSION="$chart_version" AUTOMATED=false "$clone/hack/release.sh" verify-version

expect_fail "version check: a maintainer's tag that is not Chart.yaml's is refused" \
  "charts/s5n/Chart.yaml version is ${chart_version}, but the tag is s5n-v9.9.9: bump the chart in the commit you tag" \
  env VERSION=9.9.9 AUTOMATED=false "$clone/hack/release.sh" verify-version

expect_ok "version check: one patch above the previous tag passes" "releasing s5n ${auto_version}" \
  env VERSION="$auto_version" AUTOMATED=true "$clone/hack/release.sh" verify-version

two_patch="${chart_version%.*}.$(( ${chart_version##*.} + 2 ))"
expect_fail "version check: two patches above the previous tag is refused" \
  "the automatic version ${two_patch} is not one patch above ${chart_version} (want ${auto_version})" \
  env VERSION="$two_patch" AUTOMATED=true "$clone/hack/release.sh" verify-version

expect_fail "version check: another major.minor than the chart's is refused" \
  "the automatic version 9.9.0 is not ${chart_version%.*}.x, charts/s5n/Chart.yaml's major.minor" \
  env VERSION=9.9.0 AUTOMATED=true "$clone/hack/release.sh" verify-version

# c.6: the automatic release whose tag is the only s5n-v* tag (bug 2 for the
# version check). The fixed step says there is no previous tag; without the fix
# it ended at the assignment, silently.
git -C "$clone" tag -d "s5n-v${chart_version}" >/dev/null
git -C "$clone" tag "s5n-v${auto_version}"
expect_fail "version check: the tag being released is the only one -- says why" \
  "no previous s5n-v* tag for ${auto_version} to be one patch above" \
  env VERSION="$auto_version" AUTOMATED=true "$clone/hack/release.sh" verify-version

# c.7: no s5n-v* tag at all. Same message, and it must still be said out loud
# rather than the step dying on an empty grep.
git -C "$clone" tag -d "s5n-v${auto_version}" >/dev/null
expect_fail "version check: no s5n-v* tag at all -- says why" \
  "no previous s5n-v* tag for ${auto_version} to be one patch above" \
  env VERSION="$auto_version" AUTOMATED=true "$clone/hack/release.sh" verify-version

# c.8: the hand-tag branch's other half -- the version matches and appVersion
# has drifted, which would tag an image the chart does not name.
python3 - "$clone/charts/s5n/Chart.yaml" <<'PY'
import sys

path = sys.argv[1]
lines = open(path).read().splitlines(keepends=True)
for i, line in enumerate(lines):
    if line.startswith("appVersion:"):
        lines[i] = 'appVersion: "9.9.9"\n'
open(path, "w").write("".join(lines))
PY
git -C "$clone" commit --quiet -am "rehearsal: appVersion drifts from version"
expect_fail "version check: a maintainer's tag equal to the version but not appVersion is refused" \
  "charts/s5n/Chart.yaml appVersion is 9.9.9, but the tag is s5n-v${chart_version}: the image tag and the chart's appVersion are the same version" \
  env VERSION="$chart_version" AUTOMATED=false "$clone/hack/release.sh" verify-version

if [ "$fail" -ne 0 ]; then
  echo "release-rehearsal: FAILED" >&2
  exit 1
fi
echo "release-rehearsal: PASS"
