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

{{- /* s5n.image: the image reference for one app (or for s5n itself), resolved from the
       chart's variants.yaml. A value chooses a variant by name and may pin a tag or a digest
       inside that variant's repository, never a repository, so a release cannot be pointed
       at an image the chart has not seen.

         include "s5n.image" (dict "root" $ "app" "sonarr" "path" "apps.sonarr.image" "image" $cfg.image)

       Pinning a tag drops the variant's digest, which belongs to the variant's own tag; pin
       both to hold an exact image at a new tag. */}}
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

{{- /* s5n.qbtDiskQueueSize: the disk write queue the 10 Gbit libtorrent 2.1 builds are
       seeded with, in bytes, and empty for every other variant, which keeps qBittorrent's
       own 100 MiB default (src/base/bittorrent/sessionimpl.cpp:502 at release-5.3.1). */}}
{{- define "s5n.qbtDiskQueueSize" -}}
{{- if has (toString (default "default" .image.variant)) (list "lt2.1-10g" "lt2.1-10g-znver3") -}}
536870912
{{- end -}}
{{- end -}}

{{- /* The claims every pod mounts. The chart creates no storage: box creates them from the
       grant's host paths, and a plain `helm install` expects the operator to have. So no host
       path is in a pod spec, and Pod Security "restricted" admits every pod. */}}
{{- define "s5n.dataClaim" -}}
{{- required "storage.data.existingClaim: create the PersistentVolumeClaim before helm install; the chart creates no storage (box does)" .Values.storage.data.existingClaim -}}
{{- end -}}

{{- define "s5n.configClaim" -}}
{{- required "storage.config.existingClaim: create the PersistentVolumeClaim before helm install; the chart creates no storage (box does)" .Values.storage.config.existingClaim -}}
{{- end -}}

{{- /* s5n.claimsExist: the claims the pods mount must exist before install. Without this check
       a missing claim renders a Pending pod minutes later, far from the values line that
       named it.

       It is one read of the release namespace's claims, which any identity that can install
       the release already has; a cluster-scoped read would refuse a namespace-scoped
       kubeconfig. `lookup` answers nothing under `helm template`, `helm lint` and a
       client-side dry-run, so those renders are unchanged. */}}
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

{{- /* s5n.authEdge: the operator's statement that a login sits in front of this release.
       There is no default, so a plain install must set auth.edge=true (box always does), and
       false is refused too: a release nobody signs in to is not one this chart renders. */}}
{{- define "s5n.authEdge" -}}
{{- $auth := .Values.auth -}}
{{- if not (and $auth (kindIs "bool" $auth.edge) $auth.edge) -}}
{{- fail "auth.edge: set it to true to say a login is in front of this release; the chart ships no login, so false is refused too (box sets it)" -}}
{{- end -}}
{{- end -}}

{{- define "s5n.secretName" -}}
{{- default (printf "%s-keys" (include "s5n.fullname" .)) .Values.secrets.existingSecret -}}
{{- end -}}

{{- /* s5n.wiringAccount: the one account the wiring Job signs in with, in the apps that keep
       their own user table (qui, autobrr): both hand an API key only to a signed-in session,
       so an unattended install needs an account it owns (docs/box.md). One name for both
       because it is one thing, and the Job's config, the init container and an operator's
       `kubectl rollout restart` all have to name the same account. */}}
{{- define "s5n.wiringAccount" -}}
s5n
{{- end -}}

{{- /* s5n.instances: the flat list of workloads, as JSON, built from the keys under `apps:` --
       values.schema.json rejects anything else, so a new catalog app needs a values entry and
       its schema definition, never a template edit. An app with `instances:` expands into one
       entry per enabled instance; every other app contributes one.

       `arr` is a property of the app, not of having instances: the Arrs' own convention (a
       `<PREFIX>__SERVER__*` environment, a URL base, an API key the chart mints). `envPrefix`
       is that convention's switch, so Bazarr takes instances without being one. */}}
{{- define "s5n.instances" -}}
{{- $out := list -}}
{{- range $app, $cfg := $.Values.apps -}}
  {{- if and $cfg $cfg.enabled -}}
    {{- if $cfg.instances -}}
      {{- range $i := $cfg.instances -}}
        {{- if $i.enabled -}}
          {{- $name := ternary $app (printf "%s-%s" $app $i.suffix) (empty $i.suffix) -}}
          {{- $out = append $out (dict "name" $name "app" $app "cfg" $cfg "suffix" $i.suffix
                "category" (default "" $i.category)
                "downloadCategory" (default "" (default $i.category $i.downloadCategory))
                "host" (default "" $i.host)
                "arr" (hasKey $cfg "envPrefix")) -}}
        {{- end -}}
      {{- end -}}
    {{- else -}}
      {{- $out = append $out (dict "name" $app "app" $app "cfg" $cfg "suffix" "" "category" ""
            "host" (default "" $cfg.host)
            "arr" (hasKey $cfg "envPrefix")) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- /* s5n.bazarrs: every Bazarr instance and the Arr pair it reads, as JSON. Upstream Bazarr
       has one Sonarr and one Radarr per instance, so a 4K pair is a second pod and
       `bazarr-4k` reads `sonarr-4k` and `radarr-4k`; a side switched off is a side this
       Bazarr does not read.

       An instance with neither side is refused before anything renders: Bazarr would run and
       fetch nothing, and a skipped instance is a values line saying bazarr is on while no pod
       exists. */}}
{{- define "s5n.bazarrs" -}}
{{- $out := list -}}
{{- $all := include "s5n.instances" . | fromJsonArray -}}
{{- $byName := dict -}}
{{- range $i := $all -}}
{{- $_ := set $byName $i.name $i -}}
{{- end -}}
{{- range $inst := $all -}}
{{- if eq $inst.app "bazarr" -}}
{{- $suffix := $inst.suffix -}}
{{- $names := dict "sonarr" (ternary "sonarr" (printf "sonarr-%s" $suffix) (empty $suffix)) "radarr" (ternary "radarr" (printf "radarr-%s" $suffix) (empty $suffix)) -}}
{{- $entry := dict "name" $inst.name "port" $inst.cfg.port -}}
{{- range $side := list "sonarr" "radarr" -}}
{{- $want := get $names $side -}}
{{- with (get $byName $want) -}}
{{- $_ := set $entry $side (dict "host" .name "port" .cfg.port "urlBase" (printf "/%s" .name)
      "apiKeyFile" (printf "/etc/s5n/keys/%s-apikey" .name)) -}}
{{- end -}}
{{- end -}}
{{- if and (not (hasKey $entry "sonarr")) (not (hasKey $entry "radarr")) -}}
{{- fail (printf "apps.bazarr.instances: %s reads one Sonarr and one Radarr, and neither %s nor %s is enabled; switch one on, or switch %s off" $inst.name (get $names "sonarr") (get $names "radarr") $inst.name) -}}
{{- end -}}
{{- $out = append $out $entry -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- /* s5n.seerrs: every Seerr instance and the media server it is set up against, as JSON.
       A Seerr talks to exactly one server (settings.main.mediaServerType) and its first admin
       is a sign-in to that server, so a box with both servers runs one Seerr per server:
       `seerr` and `seerr-jellyfin`, each at its own name. An instance that does not name a
       server gets Plex when the release runs it, and Jellyfin otherwise.

       A server this release does not run is refused with the app to switch on: Seerr on a box
       with neither has no account anyone could sign in to it with. */}}
{{- define "s5n.seerrs" -}}
{{- $cfg := .Values.apps.seerr -}}
{{- $plex := and .Values.apps.plex .Values.apps.plex.enabled -}}
{{- $jellyfin := and .Values.apps.jellyfin .Values.apps.jellyfin.enabled -}}
{{- if not (or $plex $jellyfin) -}}
{{- fail "apps.seerr: Seerr requests from one media server and this release runs neither; switch plex or jellyfin on, or switch seerr off" -}}
{{- end -}}
{{- $out := list -}}
{{- range $i := $cfg.instances -}}
{{- if $i.enabled -}}
{{- $name := ternary "seerr" (printf "seerr-%s" $i.suffix) (empty $i.suffix) -}}
{{- $media := default (ternary "plex" "jellyfin" $plex) $i.mediaServer -}}
{{- if eq $media "jellyfin" -}}
{{- if not $jellyfin -}}
{{- fail (printf "apps.seerr.instances: %s is set up against jellyfin and apps.jellyfin is off; switch jellyfin on, or make %s a plex one" $name $name) -}}
{{- end -}}
{{- else if eq $media "plex" -}}
{{- if not $plex -}}
{{- fail (printf "apps.seerr.instances: %s is set up against plex and apps.plex is off; switch plex on, or make %s a jellyfin one" $name $name) -}}
{{- end -}}
{{- else -}}
{{- fail (printf "apps.seerr.instances: %s names mediaServer %q, and Seerr is set up against one server: plex or jellyfin" $name $media) -}}
{{- end -}}
{{- /* publicUrl is where a person opens Seerr, not where the Job dials it. Seerr stores
       mediaServerUrl as the Jellyfin external URL it builds its links from, so it is the
       address people reach the server at. */ -}}
{{- $publicURL := include "s5n.appHost" (dict "root" $ "app" "seerr" "name" $name "host" (default "" $i.host)) -}}
{{- $entry := dict "name" $name "url" (printf "http://%s:%v" $name $cfg.port) "publicUrl" (trim $publicURL) "mediaServer" $media -}}
{{- if eq $media "jellyfin" -}}
{{- $serverURL := include "s5n.appHost" (dict "root" $ "app" "jellyfin" "name" "jellyfin" "host" (default "" $.Values.apps.jellyfin.host)) -}}
{{- $_ := set $entry "mediaServerUrl" (printf "https://%s" (trim $serverURL)) -}}
{{- end -}}
{{- $out = append $out $entry -}}
{{- end -}}
{{- end -}}
{{- if not $out -}}
{{- fail "apps.seerr.instances: Seerr is on and no instance is enabled; switch one on, or switch seerr off" -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- /* s5n.exposure: how the chart serves one app, from the catalog manifest values cannot
       override. "path" is the default; "host" serves it at <app>.<domain> with a listener and
       certificate of its own, because its UI writes absolute asset paths. The chart owns the
       choice: a wrong one is a broken UI, not tuning. */}}
{{- define "s5n.exposure" -}}
{{- $catalog := .root.Files.Get "catalog.yaml" | fromYaml -}}
{{- $entry := get $catalog.apps .app | default dict -}}
{{- default "path" $entry.exposure -}}
{{- end -}}

{{- /* s5n.oidc: whether the catalog marks one app as a client of the login's own OIDC
       provider (`oidc: true` in catalog.yaml), which is the whole decision. "true" or "" --
       callers compare the string, because include hands back a string. */}}
{{- define "s5n.oidc" -}}
{{- $catalog := .root.Files.Get "catalog.yaml" | fromYaml -}}
{{- $entry := get $catalog.apps .app | default dict -}}
{{- if $entry.oidc -}}true{{- end -}}
{{- end -}}

{{- /* s5n.appHost: the name one workload is served on when its app has a host of its own
       (`exposure: host`). The name is the instance's, not the app's, so a second Seerr is
       reachable under a name of its own; the first instance is named after the app, which is
       every name a one-instance box has. `host` is the instance's override: a label
       (`requests` -> requests.<domain>), or a name under this box's domain. A name outside it
       is refused -- the login's session cookie is the domain's -- and so is the box's own
       name, which the domain's certificate already answers. A domain is required: a route on
       a name that is not there serves nothing. */}}
{{- define "s5n.appHost" -}}
{{- $domain := required (printf "gateway.host: apps.%s is served at its own host name (<instance>.<domain>), so set gateway.host to the box's domain" .app) .root.Values.gateway.host -}}
{{- $override := trim (default "" .host) -}}
{{- if not $override -}}
{{- printf "%s.%s" .name $domain -}}
{{- else if not (contains "." $override) -}}
{{- printf "%s.%s" $override $domain -}}
{{- else -}}
{{- if eq $override $domain -}}
{{- fail (printf "apps.%s: host %q is the box's own name, which its own certificate already answers; give this instance a name of its own" .app $override) -}}
{{- end -}}
{{- $suffix := printf ".%s" $domain -}}
{{- if not (hasSuffix $suffix $override) -}}
{{- fail (printf "apps.%s: host %q is not under %s; a hosted instance is reached at a name under the box's domain, because the login's session cookie is the domain's" .app $override $domain) -}}
{{- end -}}
{{- if contains "." (trimSuffix $suffix $override) -}}
{{- fail (printf "apps.%s: host %q has more than one label before %s; a hosted instance is one name under the box's domain" .app $override $domain) -}}
{{- end -}}
{{- $override -}}
{{- end -}}
{{- end -}}

{{- /* s5n.checkHosts: every hosted instance's name, refused when two of them are one name.
       A name two instances share is two Certificates, two listeners and one login rule: the
       first host served would answer for both. */}}
{{- define "s5n.checkHosts" -}}
{{- $seen := dict -}}
{{- range $inst := include "s5n.instances" . | fromJsonArray -}}
{{- if and $inst.host (ne (include "s5n.exposure" (dict "root" $ "app" $inst.app) | trim) "host") -}}
{{- fail (printf "apps.%s.host: %s is served under a path, so it has no name of its own; drop the key, or stop serving it at a path (catalog.yaml decides)" $inst.app $inst.name) -}}
{{- end -}}
{{- if eq (include "s5n.exposure" (dict "root" $ "app" $inst.app) | trim) "host" -}}
{{- $name := include "s5n.appHost" (dict "root" $ "app" $inst.app "name" $inst.name "host" $inst.host) | trim -}}
{{- if hasKey $seen $name -}}
{{- fail (printf "apps.%s.host: %s names %q, which %s already serves; two workloads on one name is one certificate and one login rule for both" $inst.app $inst.name $name (get $seen $name)) -}}
{{- end -}}
{{- $_ := set $seen $name $inst.name -}}
{{- end -}}
{{- end -}}
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

{{- /* s5n.readinessPath: the path an app's readinessProbe asks for, or "" for a TCP connect
       (the chart's default). Chart-owned like the security contexts: a probe a user can point
       anywhere can call a dead app Ready.

       The paths are the app's own at the pinned tag. qui's is on the root router, not under
       its baseUrl (internal/api/server.go), where a prefixed request lands on the SPA's
       catch-all and answers HTML. autobrr and Maintainerr apply their base path to every
       route, health included (autobrr's API router sits under baseUrl + "/api",
       internal/http/server.go:226,302 at v1.90.0).

       PeerBanHelper has no health path: its index answers 200 for any unknown path, so /
       would prove only the listener, and a database that never loaded still serves the SPA.
       /api/downloaders is the route the wiring Job writes through, answering without a token
       only because this app's own login is off (PBH_WEB_REQUIRELOGIN); everything reaching
       the port is behind the edge's login and the NetworkPolicy. */}}
{{- define "s5n.readinessPath" -}}
{{- if eq .app "qui" -}}
/healthz/readiness
{{- else if eq .app "autobrr" -}}
{{ printf "/%s/api/healthz/readiness" .name }}
{{- else if eq .app "maintainerr" -}}
{{ printf "/%s/api/health/ready" .name }}
{{- else if eq .app "pbh" -}}
/api/downloaders
{{- else if eq .app "bazarr" -}}
{{ printf "/%s/api/system/ping" .name }}
{{- else if eq .app "sabnzbd" -}}
{{ printf "/%s/api?mode=version&output=json" .name }}
{{- else if eq .app "filebrowser" -}}
{{ printf "/%s/health" .name }}
{{- else if eq .app "jellyfin" -}}
{{- /* /System/Info/Public carries no [Authorize] at v12.4
       (Jellyfin.Api/Controllers/SystemController.cs) and there is no /health
       route in that tag: the public info is what a client discovers a server
       with, so it answers before the wizard is complete too. */ -}}
/system/info/public
{{- end -}}
{{- end -}}
