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

# auth.edge is the operator's statement that a login sits in front of the release. Both a
# missing key and a false one must fail naming the key and the fix: an open admin stack
# must never be the result of forgetting a value.
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

# The chart ships no peer port: a fixed one in the upper band is a port the API server can
# hand to another Service first, and the peers Service is then refused for the rest of the
# install. box records one; a helm install must set it.
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

# Plex must never trust a network range by default: the Gateway's proxy dials from the pod
# CIDR, so a PLEX_NO_AUTH_NETWORKS covering it makes the internet look local -- an
# unauthenticated admin. The default render must not carry the env var at all.
if helm template t charts/s5n -n seedbox --set auth.edge=true --set gateway.host=box.example.com \
     --set apps.qbittorrent.torrent.port=30080 | grep -q 'PLEX_NO_AUTH_NETWORKS'; then
  echo "default render sets PLEX_NO_AUTH_NETWORKS: Plex would trust the pod network"
  exit 1
fi
echo "== default render: no PLEX_NO_AUTH_NETWORKS"

# A rehearsal box keeps DNS and its own pods as its only egress: the public-internet
# ipBlocks go, and the policies render even when the grant switched networkPolicy.enabled
# off. A lock that renders nothing is a box with no policy at all.
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
# The lock is only a lock if the policy exists: this file sets networkPolicy.enabled false
# and the render must still carry it.
for want in 'name: t-egress' 'name: t-default-deny' 'k8s-app: kube-dns' \
            'name: t-wiring-egress'; do
  grep -qF -- "$want" <<<"$locked" ||
    { echo "the rehearsal render has no ${want}"; exit 1; }
done
echo "== rehearsal render: no internet egress, DNS and in-namespace only, policies on"

# Every app pod of a rehearsal box waits until its own egress is closed: a CNI programs a
# new pod's policy a moment after it starts, and an app that dials out then reaches the
# internet. A lock with no gate address does not render at all.
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
  # stdin, not the temp file: kubeconform skips paths without a .yaml extension and reports
  # "0 resources" as success.
  summary=$(kubeconform -strict -summary -kubernetes-version "$KUBE" \
    -schema-location default -schema-location "$CRDS" - <"$out")
  rm -f "$out"
  echo "$summary"
  valid=$(sed -n 's/.*Valid: \([0-9]*\).*/\1/p' <<<"$summary" | tail -1)
  [ "$valid" = "$count" ] || { echo "kubeconform validated ${valid:-0} of ${count} objects"; exit 1; }
done

# A catalog app that is switched off renders nothing at all: no workload, route, peer, Secret
# key or wiring config. Secret values are random base64 and 32 characters spell "pbh" about
# once in fifty renders, so values are blanked before the grep while keys stay.
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

# With the opt-in app on, the difference against the same values with it off is exactly
# the objects it adds, so a leak into another app's objects fails here.
kinds_of() { # kinds_of <render file>
  awk '/^kind: /{kind=$2} /^  name: /{print kind "/" $2}' "$1" | sort
}
for values in charts/s5n/ci/*-values.yaml; do
  off=$(mktemp); on=$(mktemp); delta=$(mktemp)
  helm template t charts/s5n -n seedbox -f "$values" --set gateway.host=box.example.com \
    --set apps.pbh.enabled=false >"$off"
  helm template t charts/s5n -n seedbox -f "$values" --set gateway.host=box.example.com \
    --set apps.pbh.enabled=true >"$on"
  # diff answers 1 when the renders differ, which is the point; unguarded under pipefail it
  # would end the script with no message.
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

# What the on-render carries: the host route, the memory limit the JVM sizes its heap from,
# the Secret key both the app and the Job read, and the wiring entry.
pbh=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml)
for want in 'hostnames: ["pbh.box.example.com"]' 'memory: 512Mi' 'pbh-token:' \
            '"pbh":{"tokenFile":"/etc/s5n/keys/pbh-token"' \
            '- name: PBH_WEB_REQUIRELOGIN' '- name: PBH_APITOKEN'; do
  grep -qF -- "$want" <<<"$pbh" || { echo "the pbh render has no ${want}"; exit 1; }
done
echo "== pbh render: host route, memory limit, Secret key, wiring entry, one-login switch"

# A Bazarr instance reads one Sonarr and one Radarr, its pair being the Arrs with the same
# suffix. A side this release does not run must be absent from the Job's config -- not an
# empty object (refused) and not an empty string (sprig's `get` answers one for a missing
# key, which the Job reads as wired). bazarr-half-pair-values.yaml has one side.
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

# SABnzbd's two 403 traps are settings in a file: url_base mounts its API, and
# X-Forwarded-For through the Gateway carries the person's address, which SAB calls external
# and refuses. The render must carry both, the minted key and the download paths.
sab=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml)
for want in '- sabnzbd-config' '--url-base=/sabnzbd'             '--complete-dir=/data/downloads' '--download-dir=/data/downloads/incomplete'             '--api-key-file=/etc/s5n/keys/sabnzbd-apikey'             '- name: SABNZBD__API_KEY' '- name: SABNZBD__HOST_WHITELIST_ENTRIES'             'sabnzbd-apikey:' '"sabnzbd":{"apiKeyFile":"/etc/s5n/keys/sabnzbd-apikey"'             'path: /sabnzbd/api?mode=version&output=json'; do
  grep -qF -- "$want" <<<"$sab" || { echo "the sabnzbd render has no ${want}"; exit 1; }
done
if helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml | grep -qi 'sabnzbd'; then
  echo "the default render mentions sabnzbd: an opt-in app must render nothing when it is off"
  exit 1
fi
echo "== sabnzbd render: sealed settings and key, host whitelist, readiness; nothing when off"

# A Seerr talks to one media server, so the Job's config names one per instance and a box
# with both servers runs two Seerrs at names of their own. On a box with no Plex the server
# is Jellyfin: the Job signs its owner in with the person's own server account, so the entry
# carries the address people reach that server at.
seerr_wiring() { # seerr_wiring <values file> [helm args...]: this render's Job entries for Seerr
  local values=$1; shift
  helm template t charts/s5n -n seedbox -f "$values" --set gateway.host=box.example.com \
    --set apps.qbittorrent.torrent.port=30080 "$@" -s templates/wiring.yaml |
    python3 -c 'import json, sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
cm = [d for d in docs if d.get("kind") == "ConfigMap"][0]
print(json.dumps(json.loads(cm["data"]["wiring.json"])["seerrs"]))'
}
seerr_cfg=$(mktemp)
{ seerr_wiring charts/s5n/ci/default-values.yaml
  seerr_wiring charts/s5n/ci/jellyfin-values.yaml
  seerr_wiring charts/s5n/ci/seerr-both-servers-values.yaml; } >"$seerr_cfg"
seerr_problems=$(python3 - "$seerr_cfg" <<'PY'
import json, sys
lines = [line for line in open(sys.argv[1], encoding="utf-8") if line.strip()]
if len(lines) != 3:
    print(f"the render gave {len(lines)} Job configs, want three")
    sys.exit(0)
plex, jellyfin, both = (json.loads(line) for line in lines)
problems = []
if [e.get("mediaServer") for e in plex] != ["plex"] or "mediaServerUrl" in plex[0]:
    problems.append(f"a Plex box's Seerrs = {plex}")
if [e.get("name") for e in jellyfin] != ["seerr"] or jellyfin[0].get("mediaServer") != "jellyfin":
    problems.append(f"a Jellyfin-only box's Seerrs = {jellyfin}")
elif jellyfin[0].get("mediaServerUrl") != "https://jellyfin.box.example.com":
    problems.append(f"a Jellyfin-only box's Seerr names {jellyfin[0].get('mediaServerUrl')!r} as the server's address")
want = [("seerr", "plex", None), ("seerr-jellyfin", "jellyfin", "https://jellyfin.box.example.com")]
if [(e.get("name"), e.get("mediaServer"), e.get("mediaServerUrl")) for e in both] != want:
    problems.append(f"a box with both servers runs {[(e.get('name'), e.get('mediaServer')) for e in both]}")
if problems:
    print("; ".join(problems))
PY
)
rm -f "$seerr_cfg"
if [ -n "$seerr_problems" ]; then
  echo "the Job's Seerr entries are wrong for the server each one names: $seerr_problems"
  exit 1
fi
echo "== seerr render: Jellyfin mode on a box with no Plex, Plex mode with one, and one per server when it has both"

# The second Seerr is a workload of its own: its own StatefulSet, Service, route and name,
# because it is set up against the other server and its bundle would otherwise share a
# config directory with the first.
both_render=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/seerr-both-servers-values.yaml \
  --set gateway.host=box.example.com)
for want in 'name: seerr-jellyfin' 'hostnames: ["seerr-jellyfin.box.example.com"]' \
            'name: seerr-jellyfin' 'mountPath: /app/config' 'subPath: seerr-jellyfin'; do
  grep -qF -- "$want" <<<"$both_render" ||
    { echo "the both-servers render has no ${want}"; exit 1; }
done
echo "== a second Seerr: its own workload, name, route and config directory"

# A server this release does not run is refused, naming the app to switch on: Seerr has
# nothing to sign anyone in to otherwise.
for spec in "jellyfin:apps.jellyfin.enabled=false:switch jellyfin on" \
            "plex:apps.plex.enabled=false:switch plex on"; do
  media=${spec%%:*}; rest=${spec#*:}; flag=${rest%%:*}; want=${rest#*:}
  err=$(mktemp)
  if helm template t charts/s5n -n seedbox -f charts/s5n/ci/seerr-both-servers-values.yaml \
       --set "$flag" --set apps.tautulli.enabled=false >/dev/null 2>"$err"; then
    echo "a ${media}-mode Seerr rendered with ${media} off: it has no server to sign anyone in to"
    rm -f "$err"; exit 1
  fi
  grep -qF "$want" "$err" ||
    { echo "a ${media}-mode Seerr with ${media} off failed without naming the app to switch on:"; cat "$err"; rm -f "$err"; exit 1; }
  rm -f "$err"
done
echo "== a Seerr set up against a server this release does not run: refused, naming the app"

# Seerr with no media server at all is a request page nobody can sign in to: refused,
# naming the two apps that would make it work.
err=$(mktemp)
if helm template t charts/s5n -n seedbox -f charts/s5n/ci/default-values.yaml \
     --set apps.plex.enabled=false --set apps.tautulli.enabled=false >/dev/null 2>"$err"; then
  echo "a release with Seerr and no media server rendered: nobody could sign in to it"
  rm -f "$err"; exit 1
fi
grep -qF 'apps.seerr: Seerr requests from one media server and this release runs neither' "$err" ||
  { echo "Seerr with no media server failed without the refusal message:"; cat "$err"; rm -f "$err"; exit 1; }
rm -f "$err"
echo "== seerr with no media server: refused, naming what to switch on"

# FileBrowser Quantum's configuration is one config.yaml the chart owns: the route's prefix,
# the login's provider, the data contract as its one source read-write, the database on the
# config claim, the cache on the emptyDir. The app cannot read a secret from a file, so the
# client secret is the one setting the file cannot carry: a chart that rendered it into the
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
# ... and not the other login: this render carries the Gateway's address, so noauth is out.
grep -qF 'noauth: true' <<<"$fb" &&
  { echo "the filebrowser render has both a provider and noauth: an app that overrides every method has no login"; exit 1; }
# The mounted file is the app's whole configuration, so it has to be YAML the app can read:
# a bad indent in a block scalar is a crash at start, and kubeconform sees one string.
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
# The other branch: no Gateway address means not an OIDC client at all, and the edge's login
# is the only one -- the shape every other app has on such a box.
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

# No app route keeps Envoy's 15s request timeout: a long request (autobrr cleaning a large
# database, an Arr's bulk import) answered 504 by the gateway is cut off in the app too.
# Only the slash redirect, which has no backend, has none to set.
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

# A category is a value inside wiring.json only: never an object name, label, port name or
# ConfigMap key. Upper case and "/" are legal in a category and illegal in a Kubernetes
# name, so two renders of the same instances -- old names, and nested mixed case -- must
# produce exactly the same objects. wiring.json still carries the nested paths.
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

# qui keeps an Arr instance per Sonarr and Radarr for ID lookups, and its own test is an Arr
# API call from its pod. The callee's ingress is inverted from the catalog's `calls`, so an
# entry naming neither Arr drops that call without a word -- and on a migrated box the rows
# are already pointed at the release. Every instance is checked.
qui_policy=$(mktemp)
helm template t charts/s5n -n seedbox -f charts/s5n/ci/4k-values.yaml \
  --set apps.qui.enabled=true --set gateway.host=box.example.com >"$qui_policy"
qui_problems=$(python3 - "$qui_policy" <<'PY'
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
policies = {d["metadata"]["name"]: d for d in docs if d.get("kind") == "NetworkPolicy"}
want = ["sonarr-ingress", "sonarr-4k-ingress", "radarr-ingress", "radarr-4k-ingress"]
problems = []
for name in want:
    policy = policies.get(name)
    if policy is None:
        problems.append(f"no {name} in the render")
        continue
    peers = [peer for rule in policy["spec"].get("ingress", []) for peer in (rule.get("from") or [])]
    callers = {((peer.get("podSelector") or {}).get("matchLabels") or {}).get("s5n.io/app") for peer in peers}
    if "qui" not in callers:
        problems.append(f"{name} admits {sorted(c for c in callers if c)} and not qui")
if problems:
    print("; ".join(problems))
PY
)
rm -f "$qui_policy"
if [ -n "$qui_problems" ]; then
  echo "qui's Arr instances: $qui_problems"
  exit 1
fi
echo "== qui render: every Sonarr and Radarr instance admits qui"

# Every app on, both 4K pairs: each instance's ingress admits exactly the apps whose `calls`
# name it. The spot list is integrations an operator sets up by hand (an Arr's Connect, a
# second media server, autobrr's usenet action), which nothing in e2e dials.
calls_policy=$(mktemp)
helm template t charts/s5n -n seedbox -f charts/s5n/ci/all-apps-values.yaml \
  --set gateway.host=box.example.com >"$calls_policy"
calls_problems=$(python3 - "$calls_policy" charts/s5n/catalog.yaml <<'PY'
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
catalog = yaml.safe_load(open(sys.argv[2], encoding="utf-8"))["apps"]
spot = [("plex", "sonarr-4k"), ("jellyfin", "radarr"), ("qui", "lidarr"), ("jellyfin", "seerr"),
        ("jellyfin", "maintainerr"), ("jellyfin", "bazarr-4k"), ("tautulli", "seerr"),
        ("sabnzbd", "autobrr"), ("prowlarr", "qui"), ("qbittorrent", "prowlarr")]
admitted = {}
for d in docs:
    name = d["metadata"]["name"]
    if d.get("kind") != "NetworkPolicy" or not name.endswith("-ingress"):
        continue
    peers = [peer for rule in d["spec"].get("ingress", []) for peer in (rule.get("from") or [])]
    admitted[name[: -len("-ingress")]] = {
        ((peer.get("podSelector") or {}).get("matchLabels") or {}).get("s5n.io/app") for peer in peers} - {None}
problems = []
for inst, callers in sorted(admitted.items()):
    app = next((a for a in sorted(catalog, key=len, reverse=True) if inst == a or inst.startswith(a + "-")), None)
    want = {c for c, e in catalog.items() if app in (e.get("calls") or [])}
    if callers != want:
        problems.append(f"{inst} admits {sorted(callers)}, the catalog says {sorted(want)}")
for callee, caller in spot:
    caller_app = caller.split("-")[0]
    if caller_app not in admitted.get(callee, set()):
        problems.append(f"{callee} does not admit {caller_app}")
if not admitted:
    problems.append("the render has no ingress policies")
print("; ".join(problems))
PY
)
rm -f "$calls_policy"
if [ -n "$calls_problems" ]; then
  echo "catalog calls: $calls_problems"
  exit 1
fi
echo "== all-apps render: every instance admits exactly the apps whose calls name it"

# Jellyfin's first-run window is closed inside its pod: an init container runs the app's own
# image, binds it to loopback and completes the wizard before the app starts, so the route
# carries no /Startup rule to be bypassed in another spelling. The password is mounted by
# that init container alone. The second init container owns the bind address, after the
# wizard on every start: a trap cannot be relied on (SIGKILL runs none), and a start that
# skips the wizard must still clear it, or the app binds loopback for good.
jf_render=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml)
jf=$(awk 'BEGIN{RS="\n---\n"} /kind: StatefulSet/ && /\n  name: jellyfin\n/ {print; exit}' <<<"$jf_render")
[ -n "$jf" ] || { echo "the catalog render has no jellyfin StatefulSet"; exit 1; }
grep -q 'name: startup-wizard' <<<"$jf" ||
  { echo "jellyfin's pod has no startup-wizard init container"; exit 1; }
grep -q '<LocalNetworkAddresses><string>127.0.0.1</string></LocalNetworkAddresses>' <<<"$jf" ||
  { echo "the wizard's server is not bound to loopback: a pod-network peer could reach the open wizard"; exit 1; }
jf_inits=$(awk '/^      initContainers:/{f=1; next} /^      containers:/{f=0} f && /^        - name: /{print $3}' <<<"$jf")
[ "$jf_inits" = "$(printf 'startup-wizard\nnetwork-config')" ] ||
  { echo "jellyfin's init containers are '$(echo $jf_inits)': the wizard runs first and network-config, which owns the bind address, runs last"; exit 1; }
if grep -q 'value: /Startup' <<<"$jf_render" || grep -q 'value: /Library/VirtualFolders' <<<"$jf_render"; then
  echo "an HTTPRoute rule names a Jellyfin first-run prefix: the window is closed in the pod, not at the edge"
  exit 1
fi
jf_init_keys=$(awk '/^      initContainers:/{f=1} /^      containers:/{f=0} f && /^            - name: keys$/' <<<"$jf")
jf_main_keys=$(awk '/^      containers:/{f=1} f && /^            - name: keys$/' <<<"$jf")
[ -n "$jf_init_keys" ] ||
  { echo "the wizard's init container does not mount the person's password"; exit 1; }
[ -z "$jf_main_keys" ] ||
  { echo "the jellyfin app container mounts the person's password"; exit 1; }
# A box naming no proxy still gets the container that writes the bind address: it is the
# only writer of that element.
jf_noproxy_render=$(helm template t charts/s5n -n seedbox -f charts/s5n/ci/catalog-values.yaml \
  --set-json 'apps.jellyfin.knownProxies=[]')
jf_noproxy=$(awk 'BEGIN{RS="\n---\n"} /kind: StatefulSet/ && /\n  name: jellyfin\n/ {print; exit}' <<<"$jf_noproxy_render")
[ -n "$jf_noproxy" ] || { echo "the render with no knownProxies has no jellyfin StatefulSet"; exit 1; }
grep -q 'name: network-config' <<<"$jf_noproxy" ||
  { echo "a box with no knownProxies renders no network-config container: nothing would clear the bind address"; exit 1; }
if grep -q -- '--known-proxy=' <<<"$jf_noproxy"; then
  echo "a box with no knownProxies renders a --known-proxy flag"
  exit 1
fi
echo "== jellyfin: the wizard runs in an init container on loopback, network-config owns the bind address after it, the app container never sees its password"
