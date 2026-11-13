{{/*
s5n helpers. The data contract lives here: every volume and security context is
derived once, so no app template can drift from it.
*/}}

{{- define "s5n.fullname" -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{- define "s5n.labels" -}}
app.kubernetes.io/part-of: s5n
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "s5n.selector" -}}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/name: {{ .inst.name }}
{{- end -}}

{{- /*
s5n.image: the image reference for one app (or for s5n itself), resolved from
the chart's variant list. The repository is never a value: values choose a
variant by name and may pin a tag or a digest inside that variant's
repository, so a release cannot be pointed at an image the chart has not seen.

  include "s5n.image" (dict "root" $ "app" "sonarr" "path" "apps.sonarr.image" "image" $cfg.image)

Pinning a tag drops the variant's digest, because the digest belongs to the
variant's own tag; pin both to hold an exact image at a new tag.
*/}}
{{- define "s5n.image" -}}
{{- $variants := .root.Files.Get "variants.yaml" | fromYaml -}}
{{- $app := get $variants .app -}}
{{- if not $app -}}
{{- fail (printf "%s.variant: this chart has no variants for %q" .path .app) -}}
{{- end -}}
{{- $name := toString .image.variant -}}
{{- $v := get $app $name -}}
{{- if not $v -}}
{{- fail (printf "%s.variant: %q is not a blessed variant; this chart has %s" .path $name (join ", " (keys $app | sortAlpha))) -}}
{{- end -}}
{{- $tag := default $v.tag .image.tag -}}
{{- if and (not $tag) (eq .app "s5n") -}}
{{- $tag = .root.Chart.AppVersion -}}
{{- end -}}
{{- $digest := $v.digest -}}
{{- if .image.digest -}}
{{- $digest = .image.digest -}}
{{- else if .image.tag -}}
{{- $digest = "" -}}
{{- end -}}
{{- if not $tag -}}
{{- fail (printf "%s: variant %q has no tag and none was pinned" .path $name) -}}
{{- end -}}
{{- if $digest -}}{{ $v.repository }}:{{ $tag }}@{{ $digest }}{{- else -}}{{ $v.repository }}:{{ $tag }}{{- end -}}
{{- end -}}

{{- /*
The claims every pod mounts. The chart creates no storage: both claims must exist
before install. Box creates them from the grant's host paths; a plain `helm install`
expects the operator to have created them. A host path is therefore never in a pod
spec, and Pod Security "restricted" still admits every pod.
*/ -}}
{{- define "s5n.dataClaim" -}}
{{- required "storage.data.existingClaim: create the PersistentVolumeClaim before helm install; the chart creates no storage (box does)" .Values.storage.data.existingClaim -}}
{{- end -}}

{{- define "s5n.configClaim" -}}
{{- required "storage.config.existingClaim: create the PersistentVolumeClaim before helm install; the chart creates no storage (box does)" .Values.storage.config.existingClaim -}}
{{- end -}}

{{- /*
s5n.claimsExist: the claims the pods mount must exist before install. The schema
requires both keys, but a claim name is only a promise: without this check a
missing claim renders a Pending pod minutes later, far from the values line that
named it.

The check runs only where a cluster answers. It lists the release namespace's
claims once: a cluster answers a list with a typed (possibly empty) list even for
a namespace that does not exist yet, while `lookup` returns an empty map under
`helm template`, `helm lint` and a client-side dry-run, so those renders are
unchanged. The list needs only `list persistentvolumeclaims` in the release
namespace, which any identity that can install the release already has; a
cluster-scoped read (the Namespace itself) would refuse a namespace-scoped
kubeconfig with Forbidden. `helm install --create-namespace` into a namespace
with no claims fails here, as it should: the pods could never start.
*/ -}}
{{- define "s5n.claimsExist" -}}
{{- $found := lookup "v1" "PersistentVolumeClaim" .Release.Namespace "" -}}
{{- if $found.kind -}}
{{- $names := list -}}
{{- range ($found.items | default list) }}{{ $names = append $names .metadata.name }}{{ end -}}
{{- range $kind := list "data" "config" -}}
{{- $claim := (index $.Values.storage $kind).existingClaim -}}
{{- if and $claim (not (has $claim $names)) -}}
{{- fail (printf "storage.%s.existingClaim: PersistentVolumeClaim %q does not exist in namespace %q; create it before helm install, the chart creates no storage (box does)" $kind $claim $.Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
s5n.authEdge: the operator's statement that a login sits in front of this
release. The chart ships no login of its own and there is no default, so a plain
install must set auth.edge=true; box always does. False is refused as well: a
release whose admin apps nobody signs in to is not a release this chart renders.
*/ -}}
{{- define "s5n.authEdge" -}}
{{- $auth := .Values.auth -}}
{{- if not (and $auth (kindIs "bool" $auth.edge) $auth.edge) -}}
{{- fail "auth.edge: set it to true to say a login is in front of this release; the chart ships no login, so false is refused too (box sets it)" -}}
{{- end -}}
{{- end -}}

{{- define "s5n.secretName" -}}
{{- default (printf "%s-keys" (include "s5n.fullname" .)) .Values.secrets.existingSecret -}}
{{- end -}}

{{/*
s5n.instances: the flat list of workloads, as JSON, built from the keys under `apps:`.
Every key there is an app -- values.schema.json rejects anything else -- so a new
catalog app needs a values entry and its schema definition, never a template edit.
Arr apps have `instances:` and expand into one entry per enabled instance (sonarr,
sonarr-4k, ...); every other app contributes one entry. Helm ranges map keys in
sorted order, so the list is alphabetical by app name; nothing downstream depends on
that order (wiring.json's instance list is per-Arr work with no cross-entry ordering).
*/}}
{{- define "s5n.instances" -}}
{{- $out := list -}}
{{- range $app, $cfg := $.Values.apps -}}
  {{- if and $cfg $cfg.enabled -}}
    {{- if $cfg.instances -}}
      {{- range $i := $cfg.instances -}}
        {{- if $i.enabled -}}
          {{- $name := ternary $app (printf "%s-%s" $app $i.suffix) (empty $i.suffix) -}}
          {{- $out = append $out (dict "name" $name "app" $app "cfg" $cfg "category" $i.category "arr" true) -}}
        {{- end -}}
      {{- end -}}
    {{- else -}}
      {{- $out = append $out (dict "name" $app "app" $app "cfg" $cfg "category" "" "arr" (hasKey $cfg "envPrefix")) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- /*
s5n.exposure: how the chart serves one app, from the catalog manifest, which
ships inside the chart and values cannot override. "path" is the default;
"host" means the app is served at <app>.<domain> with a listener and
certificate of its own, because its UI writes absolute asset paths. The chart
owns this, not a values file: an operator choosing it wrongly gets a broken UI,
not tuning. internal/catalog validates the value box reads; the chart trusts
the file it ships.
*/ -}}
{{- define "s5n.exposure" -}}
{{- $catalog := .root.Files.Get "catalog.yaml" | fromYaml -}}
{{- $entry := get $catalog.apps .app | default dict -}}
{{- default "path" $entry.exposure -}}
{{- end -}}

{{- /*
s5n.oidc: whether the catalog marks one app as a client of the login's own OIDC
provider. It is the chart's half of the same flag box reads (`oidc: true` in
catalog.yaml), and the flag is the whole decision: an app that speaks OIDC is one
entry there plus its own environment, never a name written into a template.
"true" or "" -- a template compares it to "true" rather than trusting truthiness,
because include hands back a string.
*/ -}}
{{- define "s5n.oidc" -}}
{{- $catalog := .root.Files.Get "catalog.yaml" | fromYaml -}}
{{- $entry := get $catalog.apps .app | default dict -}}
{{- if $entry.oidc -}}true{{- end -}}
{{- end -}}

{{- /*
s5n.appHost: the name one app is served on when it has a host of its own,
<app>.<domain>. A domain is required: a route on a name that is not there would
serve nothing, so a plain install that leaves gateway.host empty is refused
with the app named.
*/ -}}
{{- define "s5n.appHost" -}}
{{- $domain := required (printf "gateway.host: apps.%s is served at its own host name (<app>.<domain>), so set gateway.host to the box's domain" .app) .root.Values.gateway.host -}}
{{- printf "%s.%s" .app $domain -}}
{{- end -}}

{{- /* Restricted-profile container security context, shared by every container. */}}
{{- define "s5n.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "s5n.podSecurity" -}}
runAsUser: {{ .Values.global.puid }}
runAsGroup: {{ .Values.global.pgid }}
fsGroup: {{ .Values.global.pgid }}
fsGroupChangePolicy: OnRootMismatch
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- /*
s5n.readinessPath: the path an app's readinessProbe asks for, or "" for an app
whose readiness stays a TCP connect (the chart's default). Chart-owned, like the
security contexts: a probe a user can point at any path is a probe that can call
a dead app Ready.

The paths are the app's own, at the tag variants.yaml pins. qui's is not under
its baseUrl: it registers /healthz/readiness on the root router and mounts only
/api and the UI under the prefix (internal/api/server.go), so a prefixed request
lands on the SPA's catch-all and answers HTML, not health. autobrr and
Maintainerr apply their base path to every route, health included: autobrr's is
/api/healthz/readiness, because at v1.90.0 the whole API router is mounted under
baseUrl + "/api" (internal/http/server.go:226,302) and the UI's router under the
base URL alone.
*/ -}}
{{- define "s5n.readinessPath" -}}
{{- if eq .app "qui" -}}
/healthz/readiness
{{- else if eq .app "autobrr" -}}
{{ printf "/%s/api/healthz/readiness" .name }}
{{- else if eq .app "maintainerr" -}}
{{ printf "/%s/api/health/ready" .name }}
{{- end -}}
{{- end -}}
