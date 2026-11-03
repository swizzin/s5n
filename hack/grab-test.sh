#!/usr/bin/env bash
# End-to-end grab: mock indexer -> Prowlarr -> Sonarr -> qBittorrent -> import as a
# hardlink, through the Arr's own code.
#
# This is the test the wiring Job cannot give you. Wiring proves the apps agree with
# each other; a grab proves the data contract survives a real import, because the Arr
# decides where the file lands and links it or copies it.
#
# Everything here is lawful: the indexer is a local mock serving one synthetic torrent
# (our own bytes, no third party's content), filed under a series whose early shorts
# are in the public domain in the United States — Popeye the Sailor (1933), the
# Fleischer cartoons. No tracker, indexer or release from a real site is contacted.
#
# It is deliberately NOT part of hack/e2e-kind.sh: it needs TVDB metadata, and a CI job
# that depends on a third party's uptime is a flaky job. Run it against a kept cluster
# (hack/e2e-kind.sh --keep).
#
# Usage: hack/grab-test.sh
set -euo pipefail
cd "$(dirname "$0")/.."

CTX=${CTX:-kind-s5n}
K="kubectl --context ${CTX}"
NS=${NS:-seedbox}
RELEASE=${RELEASE:-s5n}
IDX=s5n-grab-indexer
NODE=${NODE:-s5n-control-plane}
DATA_ROOT=${DATA_ROOT:-/srv/s5n/data}   # the node path behind the release's data PVC
TVDB_ID=78435                            # Popeye the Sailor (1933), public-domain shorts
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

arr_port() { $K -n "$NS" get svc "$1" -o jsonpath='{.spec.ports[0].port}'; }
arr_key() { $K -n "$NS" get secret "${RELEASE}-keys" -o jsonpath="{.data.$1-apikey}" | base64 -d; }

api() { # api <app> <method> <path> [json-body]
  local app=$1 method=$2 path=$3 body=${4:-} port key
  port=$(arr_port "$app"); key=$(arr_key "$app")
  if [ -n "$body" ]; then
    $K -n "$NS" exec "${app}-0" -- curl -sS -X "$method" -H "X-Api-Key: ${key}" \
      -H 'Content-Type: application/json' -d "$body" "http://localhost:${port}/${app}${path}"
  else
    $K -n "$NS" exec "${app}-0" -- curl -sS -X "$method" -H "X-Api-Key: ${key}" \
      "http://localhost:${port}/${app}${path}"
  fi
}

jget() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

wait_soft() { # wait_soft <seconds> <function> — true if it succeeded in time
  local timeout=$1 fn=$2
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if "$fn" >/dev/null 2>&1; then return 0; fi
    sleep 3
  done
  return 1
}

wait_until() { # wait_until <seconds> <description> <function>
  local timeout=$1 what=$2 fn=$3
  local deadline=$((SECONDS + timeout))
  while [ $SECONDS -lt $deadline ]; do
    if "$fn" >/dev/null 2>&1; then
      echo "  ok: ${what}"; return 0
    fi
    sleep 2
  done
  die "timed out after ${timeout}s waiting for ${what}"
}

log "adding the series and picking its first episode"
series_id=$(api sonarr GET /api/v3/series | python3 -c "
import json,sys
print(next((s['id'] for s in json.load(sys.stdin) if s.get('tvdbId') == ${TVDB_ID}), ''))")
if [ -n "$series_id" ]; then
  echo "  series id: ${series_id} (already added)"
else
  series_json=$(api sonarr GET "/api/v3/series/lookup?term=tvdb:${TVDB_ID}")
  series_title=$(printf '%s' "$series_json" | jget "d[0]['title']" 2>/dev/null || echo "")
  [ -n "$series_title" ] || die "lookup for tvdb:${TVDB_ID} returned nothing (metadata lookup needs internet)"
  body=$(printf '%s' "$series_json" | python3 -c "
import json,sys
s = json.load(sys.stdin)[0]
s.update({'qualityProfileId': 1, 'rootFolderPath': '/data/media/tv', 'monitored': True,
          'seasonFolder': True,
          'addOptions': {'monitor': 'firstSeason', 'searchForMissingEpisodes': False}})
print(json.dumps(s))")
  series_id=$(api sonarr POST /api/v3/series "$body" | jget "d['id']")
  echo "  series id: ${series_id}"
fi

episode_known() { api sonarr GET "/api/v3/episode?seriesId=${series_id}" | python3 -c "
import json,sys
sys.exit(0 if any(e['seasonNumber'] >= 1 for e in json.load(sys.stdin)) else 1)"; }
log "waiting for episode metadata"
wait_until 180 "Sonarr to know the episodes" episode_known

episode=$(api sonarr GET "/api/v3/episode?seriesId=${series_id}" | python3 -c "
import json,sys
eps = [e for e in json.load(sys.stdin) if e.get('seasonNumber', 0) >= 1]
eps.sort(key=lambda e: (e['seasonNumber'], e['episodeNumber']))
e = eps[0]
print(e['id'], e['seasonNumber'], e['episodeNumber'])")
read -r episode_id season_number episode_number <<<"$episode"
echo "  first episode: S$(printf '%02d' "$season_number")E$(printf '%02d' "$episode_number") (id ${episode_id})"

series_title=$(api sonarr GET "/api/v3/series/${series_id}" | jget "d['title']")
RELEASE_NAME=$(python3 -c "
import re, sys
title = re.sub(r'[^A-Za-z0-9]+', '.', sys.argv[1]).strip('.')
print(f'{title}.S{int(sys.argv[2]):02d}E{int(sys.argv[3]):02d}.720p.WEB-DL.x264-GRP.mkv')" \
  "$series_title" "$season_number" "$episode_number")
# Sonarr shows the release in its queue without the file extension.
RELEASE_MATCH="${RELEASE_NAME%.mkv}"
echo "  series: ${series_title}"
echo "  release: ${RELEASE_NAME}"

log "building the release: a five-second test pattern, hashed into a torrent"
# A real (tiny) video, because Sonarr refuses to import a file ffprobe cannot read.
# Sixty seconds, not five: Sonarr refuses to import a file its sample detection
# classifies as a sample, and a five-second clip reads exactly like one.
# A seven-minute clip of encoded noise with an audio track, at a fixed bitrate: about
# 28 MB, a real runtime and real streams. Sonarr refuses to import a file with no audio
# track, and flags one whose runtime is far below the episode's as a sample, so the
# fixture has to look like the thing it stands in for. Noise because a test pattern
# compresses far below the target bitrate and would land under the size floor.
ffmpeg -hide_banner -loglevel error -nostdin \
  -f lavfi -i "nullsrc=s=640x480:r=12:d=420,geq=random(1)*255:128:128" \
  -f lavfi -i anullsrc=r=44100:cl=mono -shortest \
  -c:v libx264 -preset ultrafast -b:v 700k -minrate 700k -maxrate 700k -bufsize 1400k \
  -c:a aac -b:a 64k -pix_fmt yuv420p -y "$WORK/payload.mkv" </dev/null
python3 hack/make-torrent.py --file "$WORK/payload.mkv" --name "$RELEASE_NAME" \
  --announce "http://${IDX}:8080/announce" --out "$WORK/torrent.bin" --meta "$WORK/meta.json" >/dev/null

log "installing the mock indexer"
$K -n "$NS" create configmap "$IDX" --from-file=indexer.py=hack/mock-indexer.py \
  --dry-run=client -o yaml | $K -n "$NS" apply -f - >/dev/null
$K -n "$NS" create configmap s5n-grab-torrent \
  --from-file=torrent.bin="$WORK/torrent.bin" --from-file=meta.json="$WORK/meta.json" \
  --dry-run=client -o yaml | $K -n "$NS" apply -f - >/dev/null
$K -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${IDX}
  labels: {app.kubernetes.io/instance: ${RELEASE}, s5n.io/grab: indexer}
spec:
  replicas: 1
  selector:
    matchLabels: {app.kubernetes.io/instance: ${RELEASE}, s5n.io/grab: indexer}
  template:
    metadata:
      # The release label lets the release's NetworkPolicies treat it like an app, so
      # Prowlarr and Sonarr can reach it without a rule of their own.
      labels: {app.kubernetes.io/instance: ${RELEASE}, s5n.io/grab: indexer}
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: indexer
          image: python:3.13-alpine
          command: ["python", "/opt/indexer.py", "--port", "8080"]
          env:
            - {name: PYTHONDONTWRITEBYTECODE, value: "1"}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: ["ALL"]}
          volumeMounts:
            - {name: script, mountPath: /opt, readOnly: true}
            - {name: torrent, mountPath: /torrent, readOnly: true}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: script
          configMap: {name: ${IDX}}
        - name: torrent
          configMap: {name: s5n-grab-torrent}
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ${IDX}
  labels: {app.kubernetes.io/instance: ${RELEASE}, s5n.io/grab: indexer}
spec:
  selector: {app.kubernetes.io/instance: ${RELEASE}, s5n.io/grab: indexer}
  ports:
    - {name: http, port: 8080, targetPort: 8080}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ${IDX}-ingress
spec:
  podSelector:
    matchLabels: {app.kubernetes.io/instance: ${RELEASE}, s5n.io/grab: indexer}
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels: {app.kubernetes.io/instance: ${RELEASE}}
      ports:
        - {port: 8080, protocol: TCP}
EOF
# The script lives in a ConfigMap, so a changed file needs an explicit restart.
$K -n "$NS" rollout restart deploy/"$IDX" >/dev/null
$K -n "$NS" rollout status deploy/"$IDX" --timeout=180s >/dev/null

log "registering it in Prowlarr"
indexer_payload='{"name":"s5n-grab","enable":true,"implementation":"Torznab","configContract":"TorznabSettings","appProfileId":1,"priority":25,"enableRss":true,"enableAutomaticSearch":true,"enableInteractiveSearch":true,"fields":[{"name":"baseUrl","value":"http://s5n-grab-indexer:8080"},{"name":"apiPath","value":"/api"},{"name":"apiKey","value":"s5n"},{"name":"additionalParameters","value":""}],"tags":[]}'
# Prowlarr does not default `enable` to true on create, and a disabled indexer is
# silently absent from the Arr's searches — so set it, and repair an existing one.
api prowlarr GET /api/v1/indexer >"$WORK/indexers.json"
if python3 -c "import json,sys; sys.exit(0 if any(i['name']=='s5n-grab' for i in json.load(open('$WORK/indexers.json'))) else 1)"; then
  idx_id=$(python3 -c "import json; print(next(i['id'] for i in json.load(open('$WORK/indexers.json')) if i['name']=='s5n-grab'))")
  if python3 -c "import json,sys; sys.exit(0 if next(i for i in json.load(open('$WORK/indexers.json')) if i['name']=='s5n-grab')['enable'] else 1)"; then
    echo "  already registered and enabled"
  else
    python3 -c "
import json
d = json.load(open('$WORK/indexers.json'))
i = next(x for x in d if x['name'] == 's5n-grab')
i['enable'] = True
print(json.dumps(i))" >"$WORK/indexer.json"
    $K -n "$NS" exec -i prowlarr-0 -- curl -sS -X PUT -H "X-Api-Key: $(arr_key prowlarr)" \
      -H 'Content-Type: application/json' -d @- "http://localhost:$(arr_port prowlarr)/prowlarr/api/v1/indexer/${idx_id}" \
      <"$WORK/indexer.json" >/dev/null
    echo "  enabled an indexer that was registered but disabled"
  fi
else
  api prowlarr POST /api/v1/indexer "$indexer_payload" >/dev/null
  echo "  added"
fi

# Prowlarr only pushes an indexer to the Arrs on its own schedule; a forced sync is
# what makes the test deterministic.
log "syncing indexers to the Arr"
api prowlarr POST /api/v1/command '{"name":"ApplicationIndexerSync","forceSync":true}' >/dev/null

indexer_synced() { api sonarr GET /api/v3/indexer | grep -q 's5n-grab'; }
log "waiting for the indexer to sync into Sonarr"
wait_until 150 "Sonarr to list s5n-grab" indexer_synced

in_queue() { api sonarr GET "/api/v3/queue?pageSize=50" | grep -qF "$RELEASE_MATCH"; }
grab() { api sonarr POST /api/v3/command "{\"name\":\"EpisodeSearch\",\"episodeIds\":[${episode_id}]}" >/dev/null; }

# The first search after an indexer appears can run before Sonarr has cached it, and
# returns nothing without an error. Re-issue it rather than wait out a timeout.
log "waiting for Sonarr to hand the release to qBittorrent"
for attempt in 1 2 3; do
  grab
  if wait_soft 100 in_queue; then
    echo "  ok: the grab appeared in Sonarr's queue"
    break
  fi
  echo "  no grab after attempt ${attempt}; re-issuing the search"
done
in_queue || die "Sonarr never handed the release to qBittorrent"

infohash=$(api sonarr GET "/api/v3/queue?pageSize=50" | python3 -c "
import json,sys
d = json.load(sys.stdin)
recs = [r for r in d['records'] if '$RELEASE_MATCH' in r.get('title', '') + r.get('sourceTitle', '')]
print(recs[0]['downloadId'])")
echo "  infohash: ${infohash}"

log "completing the download the way a real box does: payload on disk, then a recheck"

qpw=$($K -n "$NS" get secret "${RELEASE}-keys" -o jsonpath='{.data.qbittorrent-password}' | base64 -d)
$K -n "$NS" exec qbittorrent-0 -- sh -c "curl -s -c /tmp/jar -o /dev/null -X POST \
  --data-urlencode 'username=admin' --data-urlencode 'password=${qpw}' \
  http://localhost:8080/api/v2/auth/login" || die "qBittorrent login failed"

save_path=$($K -n "$NS" exec qbittorrent-0 -- sh -c \
  "curl -s -b /tmp/jar http://localhost:8080/api/v2/torrents/info" |
  python3 -c "
import json,sys
d = json.load(sys.stdin)
print([t for t in d if t['hash'].lower() == '${infohash}'.lower()][0]['save_path'])")
echo "  qBittorrent save path: ${save_path}"
case "$save_path" in
  /data/*) ;;
  *) die "qBittorrent saved the torrent at ${save_path}, outside /data: the category path was not applied" ;;
esac

# kubectl exec cannot carry 90 MB over the API socket, so write through the node's
# hostPath — the same filesystem the PVC is backed by — and hand it to puid:pgid.
# The data claim is mounted at /data in the pods, so its node path is the host
# directory plus the rest of the path.
node_path="${DATA_ROOT}${save_path#/data}/${RELEASE_NAME}"
docker cp "$WORK/payload.mkv" "${NODE}:${node_path}"
docker exec "$NODE" chown 1000:1000 "${node_path}"
docker exec "$NODE" chmod 664 "${node_path}"

$K -n "$NS" exec qbittorrent-0 -- sh -c "curl -s -b /tmp/jar -o /dev/null -X POST \
  --data-urlencode 'hashes=${infohash}' http://localhost:8080/api/v2/torrents/recheck" || die "recheck failed"

season_dir=$(printf 'Season %d' "$season_number")
series_dir="/data/media/tv/${series_title}"
imported() { $K -n "$NS" exec sonarr-0 -- sh -c "ls '${series_dir}/${season_dir}'/*.mkv" >/dev/null 2>&1; }
log "waiting for Sonarr to import the file"
wait_until 300 "the imported file" imported

log "asserting the import is a hardlink, not a copy"
$K -n "$NS" exec sonarr-0 -- sh -c "
  set -e
  src='${save_path}/${RELEASE_NAME}'
  dst=\$(ls '${series_dir}/${season_dir}'/*.mkv)
  n=\$(stat -c %h \"\$dst\"); d1=\$(stat -c %d \"\$src\"); d2=\$(stat -c %d \"\$dst\")
  echo \"  torrent: \$src\"
  echo \"  library: \$dst\"
  echo \"  nlink=\$n dev=\$d1/\$d2\"
  [ \"\$n\" -ge 2 ] && [ \"\$d1\" = \"\$d2\" ]"

log "PASS: grabbed through Prowlarr, imported by Sonarr, hardlinked into the library"
