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

# A Bazarr instance reads one Sonarr and one Radarr, and its pair is the Arrs
# with the same suffix. A side this release does not run must be *absent* from
# the Job's config, not an empty object (which the Job refuses) and not an empty
# string (which sprig's `get` answers for a missing key, and which the Job reads
# as a wired side). charts/s5n/ci/bazarr-half-pair-values.yaml switches on the
# 4K Radarr and not the 4K Sonarr, so bazarr-4k has exactly one side.
pair=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/bazarr-half-pair-values.yaml |
  sed -n 's/^    \({"bazarrs".*\)$/\1/p')
[ -n "$pair" ] || { echo "the half-pair render has no bazarrs in the Job's config"; exit 1; }
if ! printf '%s' "$pair" | python3 -; then
  echo "the Job's bazarrs block is wrong for a half pair"; exit 1
fi <<'PY'
import json, sys
d = json.loads(sys.stdin.read())
by = {b["name"]: b for b in d["bazarrs"]}
problems = []
if sorted(by) != ["bazarr", "bazarr-4k"]:
    problems.append(f"instances = {sorted(by)}, want both")
hp = by.get("bazarr", {})
if not isinstance(hp.get("sonarr"), dict) or not isinstance(hp.get("radarr"), dict):
    problems.append("bazarr must read both Arrs")
k4 = by.get("bazarr-4k", {})
if "sonarr" in k4:
    problems.append(f"bazarr-4k has a sonarr entry ({k4['sonarr']!r}) while the 4K Sonarr is off")
if not isinstance(k4.get("radarr"), dict):
    problems.append("bazarr-4k must read the 4K Radarr")
elif k4["radarr"].get("host") != "radarr-4k":
    problems.append(f"bazarr-4k reads {k4['radarr'].get('host')!r}, want radarr-4k")
if problems:
    print("; ".join(problems))
    sys.exit(1)
PY
echo "== bazarr half pair: one side wired, the other absent from the Job's config"

# SABnzbd's two 403 traps are settings in a file, not environment the image
# passes through: url_base is what mounts its API, and X-Forwarded-For through
# the Gateway carries the person's real address, which SAB calls external and
# refuses. The render has to carry both, plus the API key the release Secret
# mints and the two download paths on the data claim.
sab=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml)
for want in '- sabnzbd-config' '--url-base=/sabnzbd'             '--complete-dir=/data/downloads' '--download-dir=/data/downloads/incomplete'             '--api-key-file=/etc/s5n/keys/sabnzbd-apikey'             '- name: SABNZBD__API_KEY' '- name: SABNZBD__HOST_WHITELIST_ENTRIES'             'sabnzbd-apikey:' '"sabnzbd":{"apiKeyFile":"/etc/s5n/keys/sabnzbd-apikey"'             'path: /sabnzbd/api?mode=version&output=json'; do
  grep -qF -- "$want" <<<"$sab" || { echo "the sabnzbd render has no ${want}"; exit 1; }
done
if helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml | grep -qi 'sabnzbd'; then
  echo "the default render mentions sabnzbd: an opt-in app must render nothing when it is off"
  exit 1
fi
echo "== sabnzbd render: sealed settings and key, host whitelist, readiness; nothing when off"

# FileBrowser Quantum's whole configuration is one config.yaml the chart owns:
# the prefix the route keeps, its sign-in through the login's provider, the data
# contract as its one source read-write, the database on the config claim and
# the cache on the pod's emptyDir (the file index and the previews are
# regenerable; the config claim is every app's settings). The default render
# carries none of it.
#
# The app has no way to read a secret from a file, so the client secret is the
# one setting the file cannot carry: it reaches the pod through the environment,
# from the Secret box wrote. A chart that rendered it (or the digest) into the
# ConfigMap would be a secret in a ConfigMap.
fb=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml)
for want in 'name: filebrowser-config' 'config.yaml: |' 'baseURL: "/filebrowser"' \
            'trustProxyHeaders: true' 'disableWebDAV: true' 'path: /data' \
            'path: /config/database.sqlite' 'cacheDir: /cache' \
            'enabled: true' 'clientId: "seedbox-filebrowser"' \
            'issuerUrl: "https://box.example.com/auth"' \
            'password:' 'enabled: false' \
            '- name: FILEBROWSER_OIDC_CLIENT_SECRET' 'name: s5n-oidc-filebrowser' \
            'key: client-secret' \
            '- name: FILEBROWSER_CONFIG' 'mountPath: /config/config.yaml' \
            'sizeLimit: 5Gi' 'path: /filebrowser/health'; do
  grep -qF -- "$want" <<<"$fb" || { echo "the filebrowser render has no ${want}"; exit 1; }
done
# ... and not the other login: this render carries the Gateway's address, so the
# app is a client of the provider, and the release that has none keeps noauth.
grep -qF 'noauth: true' <<<"$fb" &&
  { echo "the filebrowser render has both a provider and noauth: an app that overrides every method has no login"; exit 1; }
# The mounted file is the app's whole configuration, so it has to be YAML the
# app can read: a bad indent in a ConfigMap's block scalar is a crash at start,
# and kubeconform sees the block as one opaque string.
fb_file=$(mktemp)
awk '/^  config.yaml: \|$/{want=1; next}
     want && /^[^ ]/{want=0}
     want{ sub(/^    /, ""); print }' <<<"$fb" >"$fb_file"
[ -s "$fb_file" ] || { echo "the filebrowser render has no config.yaml block"; rm -f "$fb_file"; exit 1; }
fb_problems=$(python3 - "$fb_file" <<'PY'
import sys, yaml
c = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
problems = []
http = c.get("http") or {}
if http.get("baseURL") != "/filebrowser":
    problems.append(f"http.baseURL = {http.get('baseURL')!r}")
if http.get("trustProxyHeaders") is not True:
    problems.append("http.trustProxyHeaders is off: the OIDC redirect_uri would be built from the pod's own http view")
methods = ((c.get("auth") or {}).get("methods") or {})
oidc = methods.get("oidc") or {}
if methods.get("noauth"):
    problems.append("auth.methods.noauth is set beside the provider")
if oidc.get("enabled") is not True:
    problems.append(f"auth.methods.oidc.enabled = {oidc.get('enabled')!r}")
if oidc.get("clientId") != "seedbox-filebrowser":
    problems.append(f"auth.methods.oidc.clientId = {oidc.get('clientId')!r}, want box's <namespace>-<app>")
if oidc.get("issuerUrl") != "https://box.example.com/auth":
    problems.append(f"auth.methods.oidc.issuerUrl = {oidc.get('issuerUrl')!r}")
if "clientSecret" in oidc:
    problems.append("auth.methods.oidc.clientSecret is in the ConfigMap: the secret belongs in the Secret the pod reads")
if ((methods.get("password") or {}).get("enabled")) is not False:
    problems.append("auth.methods.password is on: the form it shows has no account that can answer it")
server = c.get("server") or {}
if server.get("cacheDir") != "/cache":
    problems.append(f"server.cacheDir = {server.get('cacheDir')!r}, want the emptyDir")
sources = server.get("sources") or []
if [s.get("path") for s in sources] != ["/data"]:
    problems.append(f"server.sources = {[s.get('path') for s in sources]}, want the data contract's one source")
if problems:
    print("; ".join(problems))
PY
)
if [ -n "$fb_problems" ]; then
  rm -f "$fb_file"
  echo "the filebrowser render's config.yaml is not the configuration this chart means: $fb_problems"
  exit 1
fi
rm -f "$fb_file"
# The other branch: a release with no Gateway address (a plain helm install, or
# a grant from before box recorded one) is not an OIDC client at all, and the
# edge's login is the only one -- the shape every other app has on such a box.
fb_plain=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml \
  --set apps.filebrowser.enabled=true)
grep -qF 'noauth: true' <<<"$fb_plain" ||
  { echo "the filebrowser render with no provider address has no noauth: nobody could sign in to it"; exit 1; }
grep -qF 'FILEBROWSER_OIDC_CLIENT_SECRET' <<<"$fb_plain" &&
  { echo "the filebrowser render with no provider address mounts a client secret nobody wrote"; exit 1; }
if helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml | grep -qi 'filebrowser'; then
  echo "the default render mentions filebrowser: an opt-in app must render nothing when it is off"
  exit 1
fi
echo "== filebrowser render: sealed config, the provider as its login, no secret in it, noauth without one; nothing when off"

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

