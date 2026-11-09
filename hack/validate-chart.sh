#!/usr/bin/env bash
# Static chart validation: helm lint, then kubeconform for every values file in
# charts/s5n/ci/ against one Kubernetes version. A render that produces nothing is a
# failure, not a pass.
# Usage: hack/validate-chart.sh [kubernetes-version]   (default 1.37.0)
set -euo pipefail
cd "$(dirname "$0")/.."
KUBE=${1:-1.37.0}
CRDS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

helm lint charts/s5n

# Plex must never trust a network range by default. The public Gateway's Envoy
# proxy connects to Plex from its own pod IP, which is inside the pod CIDR, so
# PLEX_NO_AUTH_NETWORKS containing that range would make the whole internet look
# like a "local" client: unauthenticated admin of the server. apps.plex.noAuthNetworks
# is opt-in only, and the default render must not carry the env var at all.
if helm template t charts/s5n -n seedbox | grep -q 'PLEX_NO_AUTH_NETWORKS'; then
  echo "default render sets PLEX_NO_AUTH_NETWORKS: Plex would trust the pod network"
  exit 1
fi
echo "== default render: no PLEX_NO_AUTH_NETWORKS"

for values in charts/s5n/ci/*-values.yaml; do
  out=$(mktemp)
  helm template t charts/s5n -n seedbox -f "$values" \
    --set 'networkPolicy.apiServerCIDRs[0]=10.0.0.1/32' >"$out"
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

