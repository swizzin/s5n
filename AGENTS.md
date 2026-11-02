# AGENTS.md — working on s5n

Read `PLAN.md` first. It is the architecture; this file is how to work in the repo.

## What exists (Phase 0 done, Phase 1 in progress)

- `charts/s5n/` — the chart. qBittorrent, Prowlarr, Sonarr, Radarr (HD + optional 4K
  instance per Arr). Storage claims, generated credentials Secret, StatefulSets,
  Services, HTTPRoutes, NetworkPolicies, and `values.schema.json`, which rejects unknown
  keys, floating image tags and malformed values before anything renders.
- `cmd/s5n/` — the one binary the image ships. Subcommands: `wiring` (the Job),
  `qbt-config` (qBittorrent's init container), `version`. `box` lands here too.
- `internal/apps/` — schema-driven HTTP clients for qBittorrent (`/api/v2`), Sonarr and
  Radarr (`/api/v3`) and Prowlarr (`/api/v1`). Every `Ensure…` reads live state, writes
  only what differs, and returns `Created`/`Updated`/`Unchanged`.
- `internal/wiring/` — the reconciler: config, steps, the hardlink proof on `/data`, and
  the receipt published into the `<release>-wiring` ConfigMap (`status.json`,
  `summary.txt`).
- `internal/qbtconf/` — qBittorrent 5 credential hashing and `qBittorrent.conf` editing.
- `Dockerfile` — static binary on distroless; the image the wiring Job and the
  qBittorrent init container run.
- `hack/e2e-kind.sh` — the acceptance test. Creates a kind cluster, installs Gateway API
  CRDs and Traefik, enforces Pod Security `restricted`, builds and loads the image,
  installs the chart, then asserts: pods Ready, routes Accepted, every app answers
  through the Gateway, `/data` hardlinks work, NetworkPolicy allows Prowlarr→Sonarr and
  blocks an outside pod, the wiring Job connects everything, and a second `helm upgrade`
  performs **zero writes** with app state identical before and after. `--keep` leaves the
  cluster up.
- No `box` yet. No installer yet.

## Commands

```sh
gofmt -l . && go vet ./... && go test ./...
helm lint charts/s5n
helm template t charts/s5n -n seedbox | kubeconform -strict -summary \
  -kubernetes-version 1.31.0 -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' -
docker build -t s5n:e2e .     # what the e2e uses
hack/e2e-kind.sh              # full smoke; needs docker, kind, helm, kubectl
```

Run lint + kubeconform against 1.31, 1.34 and 1.37 before every commit. Run the e2e before
calling any chart change done. A change is not done until the e2e prints PASS.

`helm template` renders the wiring Job but *not* the API-server egress policy, because that
one needs a live `lookup`. The e2e asserts the policy exists in the cluster.

## Invariants — do not break these

1. **One release = one namespace = one seedbox.** No hardcoded release names, namespaces,
   ports or paths. A second release in another namespace must install cleanly.
2. **Distribution-agnostic.** Nothing may depend on k0s, k3s or kind specifics.
   `kubeVersion` in Chart.yaml is a floor, never a pin. No Ingress objects; HTTP is
   Gateway API `HTTPRoute` only (ingress-nginx is retired).
3. **The data contract.** Every app mounts the same data claim at `/data`
   (`/data/torrents/<category>`, `/data/media/<category>`) and its config at `/config`
   via `subPath: <instance-name>`. Never add a second data mount, a remote path mapping,
   or a per-app data volume. That is what keeps hardlinks working.
4. **Pod Security `restricted` must admit every pod.** No hostPath in pod specs (hostPath
   is a static PV in `storage.yaml`), no hostPort, no hostNetwork, no privileged, no root.
   Security contexts come only from `s5n.podSecurity` / `s5n.containerSecurity` in
   `_helpers.tpl`; do not write ad-hoc ones in templates.
5. **One pod spec.** `workloads.yaml` is the only place an app pod spec is written.
   Per-app differences are values (`apps.<name>`), or a small `if eq $inst.app` branch when
   unavoidable. Adding an app = adding a values entry + adding its name to the list in
   `s5n.instances`. The wiring Job and the qBittorrent init container are separate
   templates because they are not app replicas.
6. **Images pinned by tag + digest** from `ghcr.io/home-operations/*`. No `:latest`,
   no `:rolling`, no keel.
7. **No secrets in values, ConfigMaps, the repo, or logs.** Credentials live in the Secret
   (`secret.yaml`, preserved across upgrades via `lookup`). The wiring config points at
   *file paths* in the mounted Secret, never at values.
8. **Enterprise features stay out** (quotas, metering, multi-tenancy, SSH, shells).
   See PLAN.md "Non-goals".
9. **Nothing personal or private enters the repo.** Commit identity comes from the repo's
   git config; never override it. No personal names beyond the maintainer's handle, no
   real hostnames, IPs, domains, cluster names or details of anyone's own infrastructure,
   in code, docs, examples, test fixtures or commit messages. Use `example.com` and
   generic names.
10. **Secrets never round-trip through an API.** Sonarr, Radarr and Prowlarr return
    passwords and API keys masked (`********`). An `Ensure…` compares only fields it may
    compare, never treats a masked value as a difference, and never writes one back. The
    proof that the secret still works is the app's own `/test` endpoint — called *before*
    the write, with the existing `id` carried, or Arr reports "Name should be unique"
    against the object being tested.

## Gotchas already hit

- Helm templates are not valid YAML. Editors/linters that YAML-validate `templates/*.yaml`
  will refuse them; that is expected — write templates with a script, not a YAML-aware
  editor.
- `values.schema.json` is validated by Helm with gojsonschema, whose regexes are Go RE2:
  lookahead such as `^(?!latest$).+$` fails the metaschema check. Express "not this tag"
  as `"not": {"enum": ["latest", "rolling"]}` instead.
- `lookup` returns nothing under `helm template` / `--dry-run`, so offline renders show
  fresh credentials every time and omit the API-server egress policy. Only a live
  install/upgrade preserves credentials or renders that policy.
- Arr apps get their port, URL base, API key and auth mode from env
  (`SONARR__SERVER__URLBASE`, `SONARR__AUTH__APIKEY`, ...). qBittorrent has no URL base,
  so its HTTPRoute uses a `ReplacePrefixMatch` rewrite.
- Arr readiness is `GET <urlbase>/ping` (no auth). qBittorrent's API answers **403 on
  every endpoint, including `/api/v2/app/version`, until you log in** — a readiness probe
  must accept any status, or the Job waits forever.
- qBittorrent 5 keeps settings in sections: `[WebUI] Password_PBKDF2`, not
  `[Preferences] WebUI\Password`. The hash is PBKDF2-HMAC-SHA512(password, salt 16 B,
  100 000 iterations, 64 B) stored as `"@ByteArray(<salt-b64>:<hash-b64>)"`. Its image
  only generates a config when the file is *absent*, so the init container writes the
  whole `[WebUI]` section (Address, Port, ServerDomains, HostHeaderValidation,
  LocalHostAuth, whitelist, CSRF), not just credentials.
- Never retry a rejected qBittorrent login: five failures ban the client IP for an hour,
  and the ban answers 403 on everything, which looks like a routing fault.
- NetworkPolicy is evaluated *after* kube-proxy DNATs a Service, so the wiring Job's
  API-server rule needs the `kubernetes` Endpoints IPs and the endpoint port (6443 on
  k0s/k3s/kind, 443 behind a load balancer), not the ClusterIP alone. The chart looks up
  both and falls back to `networkPolicy.apiServerCIDRs`.
- Secrets mounted into a pod are owned root:fsGroup, so `defaultMode: 0440` is what a
  non-root process can read. `0400` locks it out.
- The wiring Job is a Helm hook, so it is not a tracked release resource:
  `helm upgrade --wait` waits for it, `helm rollback` does not revert its ConfigMap
  changes, and `helm uninstall` leaves `<release>-wiring` behind.
- Pod template labels must not duplicate keys from `s5n.labels`
  (`app.kubernetes.io/instance` is already there).
- hostPath directories must exist and be owned by puid:pgid *before* install; the PV uses
  `type: Directory`. In e2e the script creates them inside the kind node; on real hosts
  the installer will.
- The torrent port is a NodePort (default 31111), so it must sit in the cluster's
  NodePort range (30000–32767 by default).

## Next work, in order (Phase 1)

1. **Seerr + Plex/Jellyfin + Tautulli** entries (v1 catalog) and their wiring steps.
2. Extend `hack/e2e-kind.sh` with a real grab-through-Prowlarr→qBittorrent→hardlink-import
   assertion, then add k0s and k3s e2e (`test/e2e/`), and a GitHub Actions workflow
   running lint + kubeconform + kind e2e.

Keep commits small. Each commit: lint + kubeconform green; chart-changing commits also
e2e green.
