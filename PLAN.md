# s5n — swizzin on Kubernetes, Community Edition plan

Name: **s5n** (swizzin → s5n). Licence: GPL-3.0, continuing swizzin's.

## What it is

A free, self-hosted replacement for the bare-metal `bash <(curl …swizzin)` install.
The user is the same person who runs swizzin today: one VPS or home box, Debian or
Ubuntu, no Kubernetes knowledge. They should never have to type `kubectl`.

What carries over from swizzin is the provisioning contract, not the apps:

- one data layout where hardlinks always work
- one uid/gid
- one place that knows how the client, the indexer manager and the Arrs talk to each other
- `box install <app>`, and it is safe to run again

## Non-goals for v1 (parked for the enterprise track)

Multi-tenancy, shell pods, SSH routing (sshpiper), per-user ZFS datasets and quotas,
uid allocation, user namespaces, the Kata tier, bandwidth metering and quotas
(`s5n-meter`, torrent-only billing with grace ports), abuse controls, billing ledger.

v1 has to leave room for these without building them (see "Constraints that protect the
enterprise track").

## Architecture

```
curl installer ──► k0s (single node) ──► box (Go CLI)
                                          │  helm SDK
                                          ▼
                              chart: s5n      (one release = one namespace = one seedbox)
                                          │  post-install/post-upgrade hook
                                          ▼
                              wiring Job (Go, idempotent, reads the same values)
```

### Distribution: k0s is the opinion, not a requirement

The installer provisions k0s (single binary, no host packages, and what the maintainer runs).
The chart itself is distribution-agnostic: vanilla kubeadm, k0s, k3s and managed clusters
all work through plain `helm install`, and nothing in the chart may depend on a distro
add-on. `kubeVersion` is a floor only (`>=1.29`), never pinned to one release.

k0s ships no ingress, Gateway, or LoadBalancer, so the installer adds:
Gateway API CRDs (standard channel), Traefik as the Gateway implementation (hostNetwork or
NodePort on 80/443), cert-manager, and the k0s local-path / hostPath storage.
CI runs the chart on kind (vanilla) on every PR and on k0s + k3s nightly.

### Routing: Gateway API, not Ingress

ingress-nginx was retired in March 2026, so a new project must not start on it. The chart
emits `HTTPRoute`s against one `Gateway`. The installer deploys Traefik with its Gateway provider enabled
(`hack/traefik-values.yaml` is the reference config). Every app gets a path under one
host (`https://box.example.com/sonarr`), matching swizzin's URL habits.
TLS: cert-manager with Let's Encrypt HTTP-01 when a domain is given, self-signed otherwise.
Optional Tailscale exposure is a later add-on.

### The data contract (the most important decision in the project)

Every pod mounts the same data claim at the same path, `/data`:

```
/data/torrents/{tv,movies,tv-4k,movies-4k,music,misc}
/data/media/{tv,movies,tv-4k,movies-4k,music}
```

That puts everything on one filesystem with one path, so hardlinks always work and no
remote path mappings are needed. This is the TRaSH layout that most of the ecosystem
already documents.

- `dataRoot`: a hostPath, default `/srv/swizzin/data`, configurable to a mounted disk
- `configRoot`: one hostPath per app under `/srv/swizzin/config/<app>`, so a single path
  covers backup. A `storageClass` alternative exists for BYO clusters.
- `puid`/`pgid`: set once for the whole release. Per-app overrides are possible but need
  a value written out explicitly.

### Chart

- Thin, self-owned templates. bjw-s app-template is not a dependency, because the chart
  exists to guarantee how volumes and security contexts are derived.
- One app = one values entry (`image`, `port`, `routes`, `dataSubdirs`, `env`).
  One shared workload template ranges over them, and a per-app template is the exception.
- HD/4K pairs of an Arr are one entry with `instances: [hd, 4k]`.
- Images come from `home-operations/containers` (rootless, single-process, semver),
  pinned by digest. Renovate bumps them in the chart repo, and users get updates through
  chart releases (`box update`). No keel, no `:latest`.
- Secrets (API keys, the qBittorrent password hash, the Plex claim) are generated or
  prompted at install, stored in a Kubernetes Secret, and never written into values.

### Wiring Job

This is swizzin's `install.sh`, made idempotent and run after every install or upgrade.
It is written in Go, so it shares code with `box` and later with the operator.

- Arr API keys are injected by env (`SONARR__AUTH__APIKEY`, …), so nothing has to be
  scraped from `config.xml`.
- The qBittorrent WebUI password is seeded as a PBKDF2 hash by an init container, and one
  category is created per Arr instance.
- In every Arr: download client, category, root folder, `copyUsingHardlinks: true`, and
  Forms auth with the shared admin credential. Every setting is read back and asserted,
  not assumed.
- Prowlarr: Arr applications registered, so indexers sync out.
- Seerr: connected to the Arrs and the media server.
- Plex: claimed with the token prompted at install (it expires in 4 minutes, so `box init`
  asks for it last). Jellyfin is the alternative media server.
- End-to-end hardlink proof: write a file under `/data/torrents`, link it under
  `/data/media`, assert `nlink == 2` and the same device, then clean up.
- The result goes into a `swizzin-wiring` ConfigMap: what is connected to what and when it
  was verified.

### `box` CLI (Go, single binary)

The swizzin UX, backed by Helm instead of apt.

```
box init                 # interactive: domain, data path, admin user, apps, Plex claim
box install sonarr       # flip apps.sonarr.enabled, helm upgrade, wait, run wiring
box remove sonarr        # disable; config kept unless --purge
box update               # pull the next chart release, show the diff, apply
box status               # pods, routes, certificate, wiring ConfigMap, free space
box backup / restore     # configRoot + qBittorrent BT_backup + Arr DB backups (restic optional)
box logs <app>
```

`box` owns `/etc/swizzin/values.yaml`. Hand edits are allowed and preserved: `box` reads,
merges and diffs; it never regenerates the file from scratch.

### Installer

`curl -fsSL get.<domain> | bash`: checks the OS, installs k0s (single node, pinned version), Gateway API CRDs,
Traefik as the Gateway, cert-manager, creates `/srv/s5n/{data,config}` owned by puid:pgid, installs `box`, runs `box init`.
It must be safe to re-run.

### Default hardening (the community box finally gets it)

- Pod Security `restricted` on the namespace. Rootless images allow it.
- `readOnlyRootFilesystem`, all capabilities dropped, seccomp `RuntimeDefault`.
- Default-deny NetworkPolicy, with only the needed flows allowed
  (Arr → qBittorrent, Prowlarr ↔ Arr, Seerr → Arr/media, Gateway → app).
- Only the Gateway and the torrent port (qBittorrent `hostPort`) are reachable from outside.
- No credentials in values, in the repo, or in the wiring ConfigMap.

## Catalog

- **v1:** qBittorrent, Sonarr, Radarr (HD and 4K instances), Prowlarr, Plex or Jellyfin,
  Seerr, Tautulli.
- **v1.x:** Lidarr, autobrr, qui, Bazarr, Maintainerr, SABnzbd, a Transmission/Deluge
  alternative, a dashboard.
- **Excluded:** Readarr (archived June 2025).

## Repository layout

```
charts/s5n/              chart, values.schema.json, ci/ values matrices
cmd/box/                 CLI
cmd/wiring/              wiring Job
internal/apps/           per-app clients and wiring logic shared by box and the Job
install/                 installer script and k0s config fragments
hack/                    kind config, Traefik values, e2e-kind.sh (phase-0 smoke)
test/e2e/                k0s + k3s end-to-end suites (phase 1+)
docs/
```

## CI and acceptance

- Every PR: `helm lint`, kubeconform against the supported minors, chart-testing, Go tests.
- e2e on kind (every PR) and k0s + k3s (nightly): a clean install with v1 apps, then:
  - wiring ConfigMap complete
  - hardlink proof passes
  - wiring re-run is a no-op (no writes, verified via Arr API diffs)
  - Sonarr can grab a test torrent through Prowlarr → qBittorrent → import as a hardlink
- Upgrade e2e: previous chart release → current release with data intact and wiring still
  green.
- Fresh-VPS smoke test (scheduled): Debian and Ubuntu images, curl → working stack in
  under 10 minutes with no `kubectl` typed.

## Phases and exit criteria

**Phase 0 — foundations**
- Repo, CI skeleton, values schema, the data contract written down.
- Exit: empty chart passes lint, kubeconform and the kind smoke test (`hack/e2e-kind.sh`).

**Phase 1 — chart and wiring for the v1 catalog**
- Exit: the e2e suite above passes on kind, k0s and k3s, and wiring is idempotent.

**Phase 2 — `box` and the installer**
- Exit: a fresh VPS goes from curl to a working, wired stack; `install`, `remove`,
  `update` and `status` all work; hand-edited values survive `box update`.

**Phase 3 — hardening and operations**
- PSS restricted, NetworkPolicies, a backup → wipe → restore drill, and the update path
  with rollback.
- Exit: the restore drill brings back Arr state and qBittorrent torrents with no
  re-download.

**Phase 4 — breadth and migration**
- v1.x catalog.
- `box migrate`: import a bare-metal swizzin install (Arr configs and DBs, qBittorrent
  BT_backup, paths rewritten to `/data`). This is the adoption lever, and the hard part is
  rewriting root folders and paths inside the Arr SQLite DBs.
- Exit: a real swizzin box is migrated in a test VM without losing anything.

## Constraints that protect the enterprise track

- One release = one namespace = one seedbox. Nothing assumes it is alone in the cluster;
  no hardcoded names, ports or paths.
- uid/gid, data root, host and torrent port are all per-release values. A future operator
  stamps out releases rather than replacing the chart.
- The wiring Job is namespace-scoped and has no cluster-wide RBAC.
- The Go packages in `internal/apps` are written to be imported by a controller later.

## Open decisions

- **Name:** decided — `s5n`.
- **Licence:** decided — GPL-3.0 (`LICENSE`), continuing swizzin's.
