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

{{- define "s5n.image" -}}
{{- if .digest -}}{{ .repository }}:{{ .tag }}@{{ .digest }}{{- else -}}{{ .repository }}:{{ .tag }}{{- end -}}
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
s5n.instances: the flat list of workloads, as JSON. Arr apps expand into one entry per
enabled instance (sonarr, sonarr-4k, ...). Every other template ranges over this.
*/}}
{{- define "s5n.instances" -}}
{{- $out := list -}}
{{- range $app := list "qbittorrent" "prowlarr" "sonarr" "radarr" "plex" "tautulli" "seerr" -}}
  {{- $cfg := index $.Values.apps $app -}}
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
