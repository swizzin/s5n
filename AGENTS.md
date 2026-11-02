# AGENTS.md — working on s5n

Read `PLAN.md` first. It is the architecture; this file is how to work in the repo.

## What exists (Phase 0)

- `charts/s5n/` — the chart. qBittorrent, Prowlarr, Sonarr, Radarr (HD + optional 4K
  instance per Arr). Storage claims, generated API-key Secret, StatefulSets, Services,
  HTTPRoutes, NetworkPolicies.
- `hack/e2e-kind.sh` — the acceptance test. Creates a kind cluster, installs Gateway API
  CRDs and Traefik, enforces Pod Security `restricted` on the namespace, installs the
  chart, then asserts: pods Ready, routes Accepted, every app answers through the
  Gateway, `/data` hardlink works (`nlink == 2`, same device), NetworkPolicy allows
  Prowlarr→Sonarr and blocks an outside pod. `--keep` leaves the cluster up.
- No Go code yet. No wiring Job yet. No `box` yet. No installer yet.

## Commands

```sh
helm lint charts/s5n
helm template t charts/s5n -n seedbox | kubeconform -strict -summary \
  -kubernetes-version 1.31.0 -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' -
hack/e2e-kind.sh            # full smoke; needs docker, kind, helm, kubectl
```

Run lint + kubeconform against 1.31, 1.34 and 1.37 before every commit. Run the e2e before
calling any chart change done. A change is not done until the e2e prints PASS.

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
5. **One pod spec.** `workloads.yaml` is the only place a pod spec is written. Per-app
   differences are values (`apps.<name>`), or a small `if eq $inst.app` branch when
   unavoidable. Adding an app = adding a values entry + adding its name to the list in
   `s5n.instances`.
6. **Images pinned by tag + digest** from `ghcr.io/home-operations/*`. No `:latest`,
   no `:rolling`, no keel.
7. **No secrets in values, ConfigMaps, the repo, or logs.** API keys live in the Secret
   (`secret.yaml`, preserved across upgrades via `lookup`).
8. **Enterprise features stay out** (quotas, metering, multi-tenancy, SSH, shells).
   See PLAN.md "Non-goals".
9. **Nothing personal or private enters the repo.** Commit identity comes from the repo's
   git config; never override it. No personal names beyond the maintainer's handle, no
   real hostnames, IPs, domains, cluster names or details of anyone's own infrastructure,
   in code, docs, examples, test fixtures or commit messages. Use `example.com` and
   generic names.

## Gotchas already hit

- Helm templates are not valid YAML. Editors/linters that YAML-validate `templates/*.yaml`
  will refuse them; that is expected.
- `lookup` returns nothing under `helm template` / `--dry-run`, so offline renders show
  new API keys each time. Only a live upgrade preserves them.
- Arr apps get their port, URL base, API key and auth mode from env
  (`SONARR__SERVER__URLBASE`, `SONARR__AUTH__APIKEY`, ...). qBittorrent has no URL base,
  so its HTTPRoute uses a `ReplacePrefixMatch` rewrite.
- Arr readiness is `GET <urlbase>/ping` (no auth). qBittorrent readiness is TCP.
- Pod template labels must not duplicate keys from `s5n.labels`
  (`app.kubernetes.io/instance` is already there).
- hostPath directories must exist and be owned by puid:pgid *before* install; the PV uses
  `type: Directory`. In e2e the script creates them inside the kind node; on real hosts
  the installer will.
- The torrent port is a NodePort (default 31111), so it must sit in the cluster's
  NodePort range (30000–32767 by default).

## Next work, in order (Phase 1)

1. **Wiring Job** (`cmd/wiring`, Go, `internal/apps/*` clients). Helm
   post-install/post-upgrade hook, namespace-scoped RBAC only, reads the API-key Secret.
   Steps and acceptance are in PLAN.md "Wiring Job". It must be idempotent: a second run
   makes zero writes. Prove it in e2e by running the hook twice and diffing Arr API state.
2. **qBittorrent credentials**: init container that seeds `WebUI\Password_PBKDF2` and
   `WebUI\Username` from a Secret into `qBittorrent.conf` before first start; and
   categories per Arr instance via the API (wiring Job).
3. **values.schema.json** for the chart; fail fast on bad values.
4. **Seerr + Plex/Jellyfin + Tautulli** entries (v1 catalog).
5. Extend `hack/e2e-kind.sh` with the wiring assertions, then add k0s and k3s e2e
   (`test/e2e/`), and a GitHub Actions workflow running lint + kubeconform + kind e2e.

Keep commits small. Each commit: lint + kubeconform green; chart changes also e2e green.
