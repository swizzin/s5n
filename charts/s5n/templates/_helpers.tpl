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

{{- define "s5n.dataClaim" -}}
{{- default (printf "%s-data" (include "s5n.fullname" .)) .Values.storage.data.existingClaim -}}
{{- end -}}

{{- define "s5n.configClaim" -}}
{{- default (printf "%s-config" (include "s5n.fullname" .)) .Values.storage.config.existingClaim -}}
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

{{/* Restricted-profile container security context, shared by every container. */}}
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
