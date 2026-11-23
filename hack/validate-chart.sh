#!/usr/bin/env bash
# Static chart validation: helm lint, then kubeconform for every values file in
# charts/s5n/ci/ against one Kubernetes version. A render that produces nothing is a
# failure, not a pass.
# Usage: hack/validate-chart.sh [kubernetes-version]   (default 1.37.0)
set -euo pipefail
cd "$(dirname "$0")/.."
KUBE=${1:-1.37.0}
CRDS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

helm lint charts/s5n --set auth.edge=true --set gateway.host=box.example.com \
  --set apps.qbittorrent.torrent.port=30080

# auth.edge is the operator's statement that a login sits in front of the
# release. The chart ships no login, so a render with the key missing and a
# render with it false must both fail with the message naming the key and the
# fix: an open admin stack must never be the result of forgetting a value.
auth_message='auth.edge: set it to true to say a login is in front of this release; the chart ships no login, so false is refused too (box sets it)'
for mode in missing false; do
  err=$(mktemp)
  args=()
  [ "$mode" = false ] && args=(--set auth.edge=false)
  if helm template t charts/s5n -n seedbox --set apps.qbittorrent.torrent.port=30080 "${args[@]}" >/dev/null 2>"$err"; then
    echo "auth.edge $mode: rendered; the chart must refuse a release with no login in front"
    rm -f "$err"; exit 1
  fi
  if ! grep -qF "$auth_message" "$err"; then
    echo "auth.edge $mode: failed without the refusal message:"
    cat "$err"
    rm -f "$err"; exit 1
  fi
  rm -f "$err"
  echo "== auth.edge $mode: refused"
done

# The chart ships no peer port: a fixed one in the upper band of the NodePort
# range is a port the API server can hand to another Service first, and the
# peers Service is then refused for the rest of the install. box records one;
# a helm install must set it, and a render without it says which key and what it
# is for.
port_message='apps.qbittorrent.torrent.port: set the peer port peers reach this box on'
err=$(mktemp)
if helm template t charts/s5n -n seedbox --set auth.edge=true \
     --set gateway.host=box.example.com >/dev/null 2>"$err"; then
  echo "the peer port missing: rendered; a box whose peers Service has no port cannot install"
  rm -f "$err"; exit 1
fi
if ! grep -qF "$port_message" "$err"; then
  echo "the peer port missing: failed without the refusal message:"
  cat "$err"
  rm -f "$err"; exit 1
fi
rm -f "$err"
echo "== peer port missing: refused, naming the key and the band box picks from"

# Plex must never trust a network range by default. The public Gateway's Envoy
# proxy connects to Plex from its own pod IP, which is inside the pod CIDR, so
# PLEX_NO_AUTH_NETWORKS containing that range would make the whole internet look
# like a "local" client: unauthenticated admin of the server. apps.plex.noAuthNetworks
# is opt-in only, and the default render must not carry the env var at all.
if helm template t charts/s5n -n seedbox --set auth.edge=true --set gateway.host=box.example.com \
     --set apps.qbittorrent.torrent.port=30080 | grep -q 'PLEX_NO_AUTH_NETWORKS'; then
  echo "default render sets PLEX_NO_AUTH_NETWORKS: Plex would trust the pod network"
  exit 1
fi
echo "== default render: no PLEX_NO_AUTH_NETWORKS"

# A rehearsal box keeps DNS and its own pods as its only egress: the
# public-internet ipBlocks go, and the policies render even when the grant
# switched networkPolicy.enabled off. Both halves are asserted, because a lock
# that renders nothing is a box with no policy at all, and one that keeps the
# internet rule is a rehearsal announcing to trackers.
plain=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml \
  --set 'networkPolicy.apiServerCIDRs[0]=10.0.0.1/32')
grep -q 'cidr: 0.0.0.0/0' <<<"$plain" ||
  { echo "the default render has no public-internet egress rule: a normal box cannot reach its indexers"; exit 1; }
locked=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/rehearsal-values.yaml \
  --set 'networkPolicy.apiServerCIDRs[0]=10.0.0.1/32')
grep -q 'cidr: 0.0.0.0/0' <<<"$locked" &&
  { echo "the rehearsal render keeps the public-internet egress rule: a copy would announce and query as the live box"; exit 1; }
grep -q 'cidr: ::/0' <<<"$locked" &&
  { echo "the rehearsal render keeps the IPv6 internet egress rule"; exit 1; }
# The lock is only a lock if the policy exists: this file sets
# networkPolicy.enabled false and the render must still carry it.
for want in 'name: t-egress' 'name: t-default-deny' 'k8s-app: kube-dns' \
            'name: t-wiring-egress'; do
  grep -qF -- "$want" <<<"$locked" ||
    { echo "the rehearsal render has no ${want}"; exit 1; }
done
echo "== rehearsal render: no internet egress, DNS and in-namespace only, policies on"

# Every app pod of a rehearsal box waits, first, until its own pod's egress is
# closed: a CNI programs a new pod's policy a moment after it starts, and an app
# that dials out in that moment reaches the internet. One gate per app pod, run
# before anything else, and a lock with no gate address does not render at all.
apps=$(grep -c '^kind: StatefulSet' <<<"$locked")
gates=$(grep -c -- '- name: rehearsal-gate' <<<"$locked")
[ "$apps" -gt 0 ] && [ "$gates" = "$apps" ] ||
  { echo "the rehearsal render has ${gates} rehearsal gates for ${apps} app pods"; exit 1; }
first=$(awk '/^      initContainers:/ {want=1; next} want && /^        - name:/ {print $3; want=0}' <<<"$locked" | grep -c '^rehearsal-gate$')
[ "$first" = "$apps" ] ||
  { echo "the rehearsal gate is not the first init container of every app pod (${first} of ${apps})"; exit 1; }
grep -qF -- '--address=10.96.0.10:9153' <<<"$locked" ||
  { echo "the rehearsal gate does not dial the grant's address"; exit 1; }
grep -q 'rehearsal-gate' <<<"$plain" &&
  { echo "a box that is not a rehearsal renders a rehearsal gate"; exit 1; }
if helm template t charts/s5n -n seedbox -f charts/s5n/ci/rehearsal-values.yaml \
     --set 'networkPolicy.apiServerCIDRs[0]=10.0.0.1/32' --set rehearsalGate= >/dev/null 2>&1; then
  echo "a rehearsal box with no gate address rendered: its apps would start before their policy"; exit 1
fi
echo "== rehearsal render: every app pod gated first, on the grant's address; no gate, no render"

for values in charts/s5n/ci/*-values.yaml; do
  out=$(mktemp)
  helm template t charts/s5n -n seedbox -f "$values" \
    --set 'networkPolicy.apiServerCIDRs[0]=10.0.0.1/32' --set gateway.host=box.example.com >"$out"
  count=$(grep -c '^kind: ' "$out" || true)
  echo "== ${values##*/} on ${KUBE}: ${count} objects"
  [ "$count" -gt 0 ] || { echo "render produced no objects"; exit 1; }
  # stdin, not the temp file: kubeconform skips paths without a .yaml extension
  # and reports "0 resources" as success.
  summary=$(kubeconform -strict -summary -kubernetes-version "$KUBE" \
    -schema-location default -schema-location "$CRDS" - <"$out")
  rm -f "$out"
  echo "$summary"
  valid=$(sed -n 's/.*Valid: \([0-9]*\).*/\1/p' <<<"$summary" | tail -1)
  [ "$valid" = "$count" ] || { echo "kubeconform validated ${valid:-0} of ${count} objects"; exit 1; }
done

# A catalog app that is switched off renders nothing at all: no workload, no
# route, no egress or ingress peer derived from its entry, no Secret key and no
# wiring config. The defaults are the render every plain install gets, so
# "nothing" here is the difference between a box that was asked for the app and
# one that was not.
#
# Secret values are random base64 (templates/secret.yaml), and 32 random
# characters spell "pbh" about once in fifty renders, so they are blanked before
# the grep. Their keys stay: a pbh key in the Secret is one of the leaks this
# looks for.
redact_secret_values() {
  awk '/^---/ { secret = 0; data = 0 }
       /^kind: Secret$/ { secret = 1 }
       /^[^ ]/ { data = 0 }
       secret && /^(data|stringData):/ { data = 1; print; next }
       data && /^  [^ ]/ { sub(/:.*/, ": <value>") }
       { print }'
}
if helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml | redact_secret_values | grep -qi 'pbh'; then
  echo "the default render mentions pbh: an opt-in app must render nothing when it is off"
  exit 1
fi
echo "== default render: no pbh"

# The other half: with the opt-in app on, the difference against the same values
# with it off is exactly the objects it adds. Every ci set is rendered twice, so
# a change that leaks pbh into another app's objects (a shared listener, a secret
# key, a peer list) fails here rather than in an operator's cluster.
kinds_of() { # kinds_of <render file>
  awk '/^kind: /{kind=$2} /^  name: /{print kind "/" $2}' "$1" | sort
}
for values in charts/s5n/ci/*-values.yaml; do
  off=$(mktemp); on=$(mktemp); delta=$(mktemp)
  helm template t charts/s5n -n seedbox -f "$values" --set gateway.host=box.example.com \
    --set apps.pbh.enabled=false >"$off"
  helm template t charts/s5n -n seedbox -f "$values" --set gateway.host=box.example.com \
    --set apps.pbh.enabled=true >"$on"
  # diff answers 1 when the renders differ, which is the point; under pipefail an
  # unguarded pipeline would end the script with no message at all.
  diff <(kinds_of "$off") <(kinds_of "$on") >"$delta" || true
  added=$(sed -n 's/^> //p' "$delta" | sort | tr '\n' ' ')
  removed=$(sed -n 's/^< //p' "$delta" | sort | tr '\n' ' ')
  rm -f "$off" "$on" "$delta"
  want="HTTPRoute/pbh NetworkPolicy/pbh-ingress Service/pbh StatefulSet/pbh "
  if [ "$added" != "$want" ] || [ -n "$removed" ]; then
    echo "${values##*/}: switching pbh on added [${added}] and removed [${removed}], want only [${want}]"
    exit 1
  fi
  echo "== ${values##*/}: pbh adds exactly its own four objects"
done

# What the on-render has to carry: the host route (the catalog says the app gets
# a name of its own), the memory limit the JVM sizes its heap from, the Secret key
# the app and the Job both read, and the wiring entry that connects it.
pbh=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml)
for want in 'hostnames: ["pbh.box.example.com"]' 'memory: 512Mi' 'pbh-token:' \
            '"pbh":{"tokenFile":"/etc/s5n/keys/pbh-token"' \
            '- name: PBH_WEB_REQUIRELOGIN' '- name: PBH_APITOKEN'; do
  grep -qF -- "$want" <<<"$pbh" || { echo "the pbh render has no ${want}"; exit 1; }
done
echo "== pbh render: host route, memory limit, Secret key, wiring entry, one-login switch"

# No app route keeps Envoy's 15s request timeout: an app behind the one login
# is one person doing real work, and a long request (autobrr cleaning a large
# releases database, an Arr's bulk edit or import) answered 504 by the gateway
# is cut off in the app too. Every rule that sends traffic to an app asks for
# no request timeout ("0s"); only the slash redirect, which has no backend,
# has none to set. Counted per HTTPRoute in the render with every app on.
routes=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml |
  awk '/^---/{if(kind=="HTTPRoute")print name, refs, zero; kind=""; name=""; refs=0; zero=0}
       /^kind: /{kind=$2} /^  name: /{if(name=="")name=$2}
       /^ *backendRefs:/{refs++} /^ *request: 0s$/{zero++}
       END{if(kind=="HTTPRoute")print name, refs, zero}')
[ -n "$routes" ] || { echo "the catalog render has no HTTPRoute"; exit 1; }
while read -r name refs zero; do
  [ "$refs" -gt 0 ] || continue
  [ "$refs" = "$zero" ] ||
    { echo "HTTPRoute ${name}: ${refs} rules send to the app, ${zero} lift Envoy's 15s request timeout"; exit 1; }
done <<<"$routes"
echo "== every app route: no request timeout ($(grep -c . <<<"$routes") routes)"

# A category is a value inside wiring.json only: never an object name, label,
# port name or ConfigMap key. Upper case and "/" are legal in a category and
# illegal in a Kubernetes name, so two renders of the same four instances --
# one with the old names (tv, movies-4k), one nested and mixed case
# (4K/Movies) -- must produce exactly the same objects; if a category ever
# reaches metadata.name, this comparison catches it. wiring.json still carries
# the nested paths the Job creates.
render_objects() { # render_objects <values file>
  helm template t charts/s5n -n seedbox -f "$1" |
    awk '/^kind: /{kind=$2} /^  name: /{print kind "/" $2}'
}
default_objects=$(render_objects charts/s5n/ci/4k-values.yaml)
nested_objects=$(render_objects charts/s5n/ci/nested-values.yaml)
if [ "$default_objects" != "$nested_objects" ]; then
  echo "nested categories changed an object's kind or name:"
  diff <(echo "$default_objects") <(echo "$nested_objects") || true
  exit 1
fi
echo "== nested categories: $(grep -c . <<<"$nested_objects") objects, same kinds and names as the old names"
nested_render=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/nested-values.yaml)
grep -q '/data/downloads/4K/Movies' <<<"$nested_render" ||
  { echo "the nested render does not carry /data/downloads/4K/Movies"; exit 1; }
grep -q '/data/media/4K/TV' <<<"$nested_render" ||
  { echo "the nested render does not carry /data/media/4K/TV"; exit 1; }

