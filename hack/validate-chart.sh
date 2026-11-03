#!/usr/bin/env bash
# Static chart validation: helm lint, then kubeconform for every values file in
# charts/s5n/ci/ against one Kubernetes version. A render that produces nothing is a
# failure, not a pass.
# Usage: hack/validate-chart.sh [kubernetes-version]   (default 1.31.0)
set -euo pipefail
cd "$(dirname "$0")/.."
KUBE=${1:-1.31.0}
CRDS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'

helm lint charts/s5n
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
