#!/usr/bin/env bash
# The release rehearsal: release.yaml's s5n steps, run through the scripts the release
# itself calls (hack/release.sh) against the cases the path has met. Two bugs lived in that
# path until the 0.3.0 tag -- a packaged chart rendered with no auth.edge, and a first-tag
# grep that ended the step under pipefail -- so this runs the same code, never a copy: a
# copy passes while the release fails.
#
# The decision in front of those steps is the same: release.yaml is called by ci.yaml,
# whose `github` context is a push to main, so route decides called-or-tag from the version
# input and this runs it for both, feeding route's own answer into the next steps.
#
# CI runs it as test.yaml's release-rehearsal job. It needs git, go, helm, kubeconform, jq
# and python3, builds no image and touches no cluster. Tags are made and deleted only in a
# scratch clone under $TMPDIR, which has its own origin: nothing is pushed.
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

# The digest a release stamps is its image build's; here it is fixed, so the render check
# has something exact to look for.
digest="sha256:$(printf 'a%.0s' {1..64})"
chart_version=$(awk '/^version:/ { print $2; exit }' charts/s5n/Chart.yaml)
auto_version="${chart_version%.*}.$(( ${chart_version##*.} + 1 ))"

ok() { echo "== $*"; }
bad() { echo "FAIL: $*" >&2; fail=1; }

# run keeps a command's exit status ours to read: under `set -e` the assignment alone would
# end the script. Leaves $out and $rc.
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
# Bug 1: the render check passed no values and the chart refuses to render without
# auth.edge=true, so the step died after a successful package. Stamping before packaging is
# the release's order, so the render sees the stamped copy.
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
# The scripts ask git what the tags are, so the cases are made where the tags are: a clone
# of this checkout whose origin is itself, so a fetch cannot pull a released tag back in.
echo "== b, c. tag states in a scratch clone"
clone="$tmp/clone"
git clone --no-local --quiet . "$clone"
git -C "$clone" remote set-url origin "$clone"
git -C "$clone" tag -l 's5n-v*' | xargs -r git -C "$clone" tag -d >/dev/null
git -C "$clone" config user.name rehearsal
git -C "$clone" config user.email rehearsal@example.com
# The clone runs this checkout's script, not the committed one, so a maintainer rehearsing
# an edit does not rehearse the commit before it. In CI the two are the same bytes.
cp -p "$release" "$clone/hack/release.sh"

# b.1: no s5n-v* tag at all -- the --all branch, the first release's body. No workflow run
# reaches this state (ci.yaml creates the tag before it calls), so the empty body here is
# recorded, not fixed; b.2 proves the lists when the tag is there.
body="$tmp/body-none.md"
expect_ok "notes: no s5n-v* tag at all takes the --all branch" \
  "The first s5n release: every image and platform pin it ships." \
  env VERSION="$chart_version" "$clone/hack/release.sh" notes "$body"
hasnt "notes: no tag -- not the diff body" "$body" "that moved since"

# b.2: only the tag being released (bug 2 for notes): grep prints nothing, and with the fix
# the step reaches the --all branch instead of ending.
git -C "$clone" tag "s5n-v${chart_version}"
body="$tmp/body-first.md"
expect_ok "notes: the tag being released is the only s5n-v* tag" "The first s5n release:" \
  env VERSION="$chart_version" "$clone/hack/release.sh" notes "$body"
has "notes: first tag -- the --all body, not an empty diff" "$body" "- qbittorrent: ghcr.io/swizzin/qbittorrent:"
has "notes: first tag -- the platform pins are listed too" "$body" "### Platform pins"

# b.3: a previous tag present -- the diff body. One variant's digest moves in the clone, so
# the body has something specific to name and everything else has to stay out.
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

# c. the version check. Back to one previous tag by this point, so the automatic cases have
# exactly one tag to be one patch above.
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

# c.6: the automatic release whose tag is the only s5n-v* tag (bug 2 for the version check):
# without the fix it ended at the assignment, silently.
git -C "$clone" tag -d "s5n-v${chart_version}" >/dev/null
git -C "$clone" tag "s5n-v${auto_version}"
expect_fail "version check: the tag being released is the only one -- says why" \
  "no previous s5n-v* tag for ${auto_version} to be one patch above" \
  env VERSION="$auto_version" AUTOMATED=true "$clone/hack/release.sh" verify-version

# c.7: no s5n-v* tag at all: the same message, said out loud rather than dying on grep.
git -C "$clone" tag -d "s5n-v${auto_version}" >/dev/null
expect_fail "version check: no s5n-v* tag at all -- says why" \
  "no previous s5n-v* tag for ${auto_version} to be one patch above" \
  env VERSION="$auto_version" AUTOMATED=true "$clone/hack/release.sh" verify-version

# c.8: the version matches and appVersion has drifted, which would tag an image the chart
# does not name.
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

# ---- d. route: a call from ci.yaml, or a tag push ---------------------------
# A called workflow's `github` context is the caller's, so nothing may ask the event name
# which branch it is on: route asks whether the version input is set, which only exists
# under workflow_call. The four cases route has met are decided here, and route's own answer
# then drives the next two steps, so the automatic path is not hand-set.
echo "== d. route"
rclone="$tmp/clone-route"
git clone --no-local --quiet . "$rclone"
git -C "$rclone" remote set-url origin "$rclone"
git -C "$rclone" tag -l 's5n-v*' | xargs -r git -C "$rclone" tag -d >/dev/null
# The previous release, so the automatic version route reports has a tag above it.
git -C "$rclone" tag "s5n-v${chart_version}"
cp -p "$release" "$rclone/hack/release.sh"
route_dir="$tmp/route"
mkdir -p "$route_dir"

called_out="$route_dir/called.out"
expect_ok "route: the call from ci.yaml is the s5n tag it was told to build" \
  "family=s5n version=${auto_version} ref=s5n-v${auto_version}" \
  env IN_VERSION="$auto_version" IN_REF="s5n-v${auto_version}" REF_NAME=main REF=refs/heads/main \
  "$release" route "$called_out"
has "route: the call is automated" "$called_out" "automated=true"
hasnt "route: the call builds the input's ref, not the caller's refs/heads/main" "$called_out" "refs/heads/main"

# The reverse: no version input, so a tag push, even with EVENT saying workflow_call. A
# step reading the event name takes the called branch and finds no version.
tag_out="$route_dir/tag.out"
expect_ok "route: a maintainer's tag push is s5n at the pushed tag" \
  "family=s5n version=${chart_version} ref=refs/tags/s5n-v${chart_version}" \
  env EVENT=workflow_call REF_NAME="s5n-v${chart_version}" REF="refs/tags/s5n-v${chart_version}" \
  "$release" route "$tag_out"
has "route: a tag push is not automated" "$tag_out" "automated=false"

box_out="$route_dir/box.out"
expect_ok "route: a box tag is the box family at the pushed tag" \
  "family=box version=${chart_version} ref=refs/tags/box-v${chart_version}" \
  env REF_NAME="box-v${chart_version}" REF="refs/tags/box-v${chart_version}" "$release" route "$box_out"
has "route: a box tag is not automated" "$box_out" "automated=false"

expect_fail "route: a tag that is neither family is refused" \
  "tag v0.2.0 is neither s5n-v* nor box-v*" \
  env REF_NAME=v0.2.0 REF=refs/tags/v0.2.0 "$release" route "$route_dir/neither.out"

expect_fail "route: a version that is not <major>.<minor>.<patch> is refused" \
  "version '0.3': a release version is <major>.<minor>.<patch>" \
  env IN_VERSION=0.3 IN_REF=s5n-v0.3 REF_NAME=main REF=refs/heads/main \
  "$release" route "$route_dir/badversion.out"

# route's answer, read the way the workflow reads its outputs, drives the two steps that
# used to build AUTOMATED from the event name: if route refused, they fail with their own
# message, and the rehearsal reports that rather than stopping.
called_version=$(sed -n 's/^version=//p' "$called_out" || true)
called_automated=$(sed -n 's/^automated=//p' "$called_out" || true)
expect_ok "route -> verify-version: the automatic version route reported (${called_version}) is one patch above ${chart_version}" \
  "releasing s5n ${called_version}" \
  env VERSION="$called_version" AUTOMATED="$called_automated" "$rclone/hack/release.sh" verify-version

work="$tmp/route-chart"
run env VERSION="$called_version" DIGEST="$digest" AUTOMATED="$called_automated" "$rclone/hack/release.sh" stamp-digest "$work"
if [ "$rc" -ne 0 ]; then
  printf '%s\n' "$out" >&2
  bad "route -> stamp-digest: exit ${rc}"
fi
expect_ok "route -> package-chart: the automatic chart is packaged at ${called_version} and renders it" \
  "packaged s5n-${called_version}.tgz naming ghcr.io/swizzin/s5n:${called_version}@${digest}" \
  env VERSION="$called_version" DIGEST="$digest" AUTOMATED="$called_automated" \
  "$rclone/hack/release.sh" package-chart "$work"

# ---- e. the signed checksums -------------------------------------------------
# box verifies the signature over sha256sums.txt before it replaces itself and refuses a
# release it cannot verify, so a box release without one reaches no host. The step is
# rehearsed through hack/release.sh with a throwaway key, verified by the code box runs
# (hack/sign verify imports internal/selfupdate).
#
# It signs in a clone, because sign refuses a seed whose public half is not the committed
# key and that file is empty on purpose. The clone gets this checkout's release.sh, hack/sign
# and internal/selfupdate, so an edit is what is rehearsed.
echo "== e. the signed checksums"
sclone="$tmp/clone-sign"
git clone --no-local --quiet . "$sclone"
cp -p "$release" "$sclone/hack/release.sh"
cp -p hack/sign/main.go "$sclone/hack/sign/main.go"
cp -p internal/selfupdate/*.go "$sclone/internal/selfupdate/"
cp -p internal/release/signature.go "$sclone/internal/release/"

# signcmd runs hack/sign where the release runs it: from the replica's root, so the
# signing.pub it reads is the one a box built there would embed. Built once rather than run
# with `go run`, whose "go: downloading" lines would land in keygen's output.
signbin="$tmp/sign"
( cd "$sclone" && go build -o "$signbin" ./hack/sign ) || { echo "building hack/sign failed" >&2; exit 1; }
signcmd() {
  ( cd "$sclone" && "$signbin" "$@" )
}

dist="$tmp/dist"
mkdir -p "$dist"
sums_digest=$(printf 'a%.0s' {1..64})
printf '%s  box-linux-amd64\n' "$sums_digest" > "$dist/sha256sums.txt"
box_version=0.5.0
box_version_next=0.5.1
published="$sclone/internal/selfupdate/signing.pub"
key="$tmp/signing.key"
run signcmd keygen "$key"
if [ "$rc" -ne 0 ]; then
  bad "keygen: exit ${rc}: ${out}"
else
# What keygen prints is what the maintainer commits; here it is the release's key for the
# length of this run.
  printf '%s\n' "$out" > "$tmp/signing.pub"
  cp "$tmp/signing.pub" "$published"
  ok "keygen: the throwaway public key is committed in the replica's signing.pub"
fi
seed=$(cat "$key" 2>/dev/null || true)

expect_ok "sign-sums signs the checksums the release publishes" "signed sha256sums.txt for box-v${box_version}" \
  env BOX_SIGNING_KEY="$seed" VERSION="$box_version" "$sclone/hack/release.sh" sign-sums "$dist"
expect_ok "the signature is one box accepts" "sha256sums.txt: signature ok for box-v${box_version}" \
  signcmd verify "$box_version" "$dist/sha256sums.txt" "$published"
expect_fail "sign-sums refuses an empty key instead of publishing an unsigned release" \
  "BOX_SIGNING_KEY is empty" \
  env BOX_SIGNING_KEY= VERSION="$box_version" "$sclone/hack/release.sh" sign-sums "$dist"

# F1: the signed message names the release, so an older release's signature cannot ride
# under a newer tag even with identical bytes.
expect_fail "a signature made for box-v${box_version} does not verify as box-v${box_version_next}" \
  "does not verify" \
  signcmd verify "$box_version_next" "$dist/sha256sums.txt" "$published"

# F2: a seed whose public half is not the committed key makes boxes that can never verify
# another release, so signing refuses and names both keys.
other_key="$tmp/other.key"
run signcmd keygen "$other_key"
if [ "$rc" -ne 0 ]; then
  bad "keygen for the mismatched case: exit ${rc}: ${out}"
fi
other_seed=$(cat "$other_key" 2>/dev/null || true)
expect_fail "sign-sums refuses a seed that is not the committed key's" \
  "BOX_SIGNING_KEY signs as" \
  env BOX_SIGNING_KEY="$other_seed" VERSION="$box_version" "$sclone/hack/release.sh" sign-sums "$dist"

# An empty committed key is its own sentence: it is a step not taken, not a wrong key.
: > "$published"
expect_fail "sign-sums refuses to sign while signing.pub is empty" \
  "commit the public key first" \
  env BOX_SIGNING_KEY="$seed" VERSION="$box_version" "$sclone/hack/release.sh" sign-sums "$dist"
cp "$tmp/signing.pub" "$published"

# A checksum changed after signing is a different release, and the verifier says so.
cp "$dist/sha256sums.txt" "$tmp/tampered.txt"
cp "$dist/sha256sums.txt.sig" "$tmp/tampered.txt.sig"
sed -i 's/^a/b/' "$tmp/tampered.txt"
expect_fail "a checksum edited after signing does not verify" "does not verify" \
  signcmd verify "$box_version" "$tmp/tampered.txt" "$published"

# The committed file is empty until the maintainer fills it in, and box's build must not
# need it to be a key: that box refuses to update itself and says so, not a compile error.
expect_ok "box builds with the committed signing.pub still empty" "" \
  go build -o "$tmp/box" ./cmd/box

# Signing is only half of it: the asset has to be uploaded, the secret wired up, and the
# step given the version it signs for, or every box built from this release cannot update.
has "release.yaml uploads the signature with the release's other assets" \
  .github/workflows/release.yaml "dist/sha256sums.txt.sig"
has "release.yaml gives the signing step the release's secret" \
  .github/workflows/release.yaml "secrets.BOX_SIGNING_KEY"
run python3 -c 'import sys
text = open(sys.argv[1]).read()
step = text.split("secrets.BOX_SIGNING_KEY", 1)[1].split("hack/release.sh sign-sums", 1)[0]
sys.exit(0 if "needs.route.outputs.version" in step else 1)' .github/workflows/release.yaml
if [ "$rc" -eq 0 ]; then
  ok "release.yaml gives the signing step the version it signs for"
else
  bad "release.yaml does not give the signing step needs.route.outputs.version: sign-sums refuses without one"
fi

# ---- f. the chart's signature ------------------------------------------------
# box resolves a chart's tag to a digest, checks the signature the chart's s5n-v<version>
# release publishes over that digest, and pulls by it: a spec naming only a tag installs
# whatever the tag points at when the pull runs.
echo "== f. the chart's signature"
signed="$tmp/signed"
chart_digest="sha256:$(printf 'c%.0s' {1..64})"
expect_ok "sign-chart signs the chart's digest for its release" "signed chart.sig for s5n-v${chart_version}" \
  env BOX_SIGNING_KEY="$seed" VERSION="$chart_version" CHART_DIGEST="$chart_digest" \
  "$sclone/hack/release.sh" sign-chart "$signed"
expect_ok "the chart's signature is one box accepts" "chart.sig: signature ok for s5n-v${chart_version}" \
  signcmd verify-chart "$chart_version" "$chart_digest" "$signed/chart.sig" "$published"
expect_fail "the chart's signature does not verify as another release's" "does not verify" \
  signcmd verify-chart "$auto_version" "$chart_digest" "$signed/chart.sig" "$published"
expect_fail "sign-chart refuses something that is not a digest instead of signing it" \
  "not a chart's manifest digest" \
  env BOX_SIGNING_KEY="$seed" VERSION="$chart_version" CHART_DIGEST="$chart_version" \
  "$sclone/hack/release.sh" sign-chart "$tmp/signed-bad"

# Signing a chart that is already published reads its digest from the registry: a manifest
# digest cannot be computed from a local archive, and the tag is the only thing that says
# which bytes a host would pull now.
portfile="$tmp/registry.port"
python3 - "$portfile" "$chart_digest" <<'PY' &
import http.server
import json
import sys

portfile, digest = sys.argv[1], sys.argv[2]


class Registry(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"{}"
        if self.path.startswith("/token"):
            body = json.dumps({"token": "rehearsal"}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
        elif self.path.startswith("/v2/") and "/manifests/" in self.path:
            self.send_response(200)
            self.send_header("Docker-Content-Digest", digest)
        else:
            body = b""
            self.send_response(404)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("127.0.0.1", 0), Registry)
open(portfile, "w").write(str(server.server_address[1]))
server.serve_forever()
PY
fake_pid=$!
for _ in $(seq 1 100); do
  [ -s "$portfile" ] && break
  sleep 0.05
done
if [ ! -s "$portfile" ]; then
  bad "the rehearsal's registry never started"
fi
digest_out="$tmp/digest.txt"
expect_ok "chart-digest reads the digest a published tag points at now" "digest=${chart_digest}" \
  env VERSION="$chart_version" CHART_REPO="oci://127.0.0.1:$(cat "$portfile")/charts" \
  "$release" chart-digest "$digest_out"
has "chart-digest writes the line the workflow reads back" "$digest_out" "digest=${chart_digest}"
kill "$fake_pid" 2>/dev/null || true

# The release has to sign the chart and upload the asset, or every box refuses the chart it
# publishes: the asset's name is box's, so it is read out of the code box is built from.
chart_asset=$(sed -n 's/^const ChartSignatureAsset = "\(.*\)"$/\1/p' internal/release/signature.go)
has "release.yaml uploads the chart's signature the release job signs" \
  .github/workflows/release.yaml "hack/release.sh sign-chart"
has "release.yaml uploads the asset name box fetches" \
  .github/workflows/release.yaml "${chart_asset}"
has "release.yaml gives the chart's signing step the release's secret" \
  .github/workflows/release.yaml "secrets.BOX_SIGNING_KEY"
run python3 -c 'import sys
text = open(sys.argv[1]).read()
at = text.index("hack/release.sh sign-chart")
window = text[max(0, at - 1500):at]
sys.exit(0 if "CHART_DIGEST" in window else 1)' .github/workflows/release.yaml
if [ "$rc" -eq 0 ]; then
  ok "release.yaml gives the chart's signing step the digest to sign"
else
  bad "release.yaml does not give the chart's signing step a CHART_DIGEST: sign-chart refuses without one"
fi

if [ "$fail" -ne 0 ]; then
  echo "release-rehearsal: FAILED" >&2
  exit 1
fi
echo "release-rehearsal: PASS"
