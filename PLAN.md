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

Also out of v1 entirely: a monitoring stack. s5n ships no Prometheus, Grafana or
exporters. A user who runs their own can scrape the apps; the chart's only obligation is
that `networkPolicy.extraHttpFrom` lets them in.

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
add-on. `kubeVersion` is a floor only (`>=1.30`, where ValidatingAdmissionPolicy is GA),
never pinned to one release.
`box`, the installer's CLI, is stricter than the chart: it drives one seedbox on one node
it owns, and refuses managed and multi-node clusters by name (see cluster detection
below). The chart's licence to install anywhere is not `box`'s licence to manage it.

k0s ships no ingress, Gateway, or LoadBalancer, so the box carries a **platform layer**,
managed and on by default:

| component | role | why this one |
|---|---|---|
| Gateway API CRDs | standard channel | the routing contract the chart emits against |
| MetalLB (L2) | makes `type: LoadBalancer` work on bare metal | default pool is the node's own primary IP as a /32, which works on any VPS with no extra addresses or ARP. The Gateway's Service takes that address on 80/443. The torrent port is a NodePort on the same node address with `externalTrafficPolicy: Local`, so one IP serves both and real peer IPs survive. MetalLB address sharing cannot do this: it only shares between Services that are both `Cluster` (which SNATs peers) or select the same pods (verified in kind). A box with a spare address can set the peers Service to LoadBalancer instead |
| Envoy Gateway | Gateway API implementation | the Envoy project's own Gateway controller, started by the Contour and Emissary teams on the same data plane, with a minor release roughly every quarter. Contour itself has had no minor release since 1.33 (Sept 2025), and almost all of its non-bot commits since May 2026 come from one maintainer |
| cert-manager | TLS | Gateway API support on; HTTP-01 through the Gateway when a domain is given, self-signed otherwise |

Each is its own Helm release pinned by `box`, and each has a `platform.<component>.mode`:

- `auto` (default): probe for a working instance; use it if found, install ours if not.
- `managed`: always ours. `box` installs, upgrades and removes it.
- `external`: the user's. `box` never installs or touches it, only verifies it.

Detection checks capability, never brand. "Traefik is running" is not evidence; "a
GatewayClass reports `Accepted` and a Gateway attached to it gets an address" is. The probes:

| component | counts as present when |
|---|---|
| Gateway API CRDs | the CRDs exist at or above the bundle version the chart needs (k3s now ships them itself) |
| LoadBalancer | box's own Gateway is Programmed with an address within 60s and answers HTTP from the host (MetalLB, k3s servicelb, a cloud provider, a host-networked Gateway all pass); see docs/box.md |
| Gateway | a GatewayClass is `Accepted`; `platform.gateway.className` picks one when there are several |
| cert-manager | its webhook answers and its version supports Gateway API HTTP-01 |

A component with no working instance is installed only in `auto` or `managed`; in
`external` mode a failed probe stops `box` with the reason. A probe that finds something
*conflicting* (an ingress controller already holding 80/443 on the only address, a second
LoadBalancer implementation) stops `box` in every mode: it reports and never fights it.

Distribution presets only fill in what probes cannot decide. On k3s, the bundled Traefik
does not serve Gateway API until its provider is enabled, so `box` offers to write the
`HelmChartConfig` that turns it on and then uses it as `external`, rather than installing
Envoy Gateway beside it. k3s servicelb satisfies the LoadBalancer probe, so MetalLB is
skipped. `box migrate` runs the same probes, so it uses what a box already has.

The chart itself stays platform-agnostic: it emits HTTPRoutes and one LoadBalancer or
NodePort Service, and never depends on which implementation sits behind them.
CI runs the chart on kind (vanilla) on every PR and on k0s + k3s nightly. The kind e2e
installs Envoy Gateway (the chart's own CRDs) and MetalLB L2, then drives the apps
through the Gateway's LoadBalancer address.

### Routing: Gateway API, not Ingress

ingress-nginx was retired in March 2026, so a new project must not start on it. The chart
emits `HTTPRoute`s against one `Gateway`, served by Envoy Gateway on a managed install
(see the platform layer above). Every app gets a path under one
host (`https://box.example.com/sonarr`), matching swizzin's URL habits.
TLS: cert-manager with Let's Encrypt HTTP-01 when a domain is given, self-signed otherwise.
Optional Tailscale exposure is a later add-on.

### The data contract (the most important decision in the project)

Every pod mounts the same data claim at the same path, `/data`:

```
/data/downloads/{tv,movies,tv-4k,movies-4k,music,misc}
/data/media/{tv,movies,tv-4k,movies-4k,music}
```

That puts everything on one filesystem with one path, so hardlinks always work and no
remote path mappings are needed. This is the TRaSH single-mount layout, with `downloads` in place of `torrents` so
the name still fits once a Usenet client shares it. The download directory is
`downloads` by decision.

- `storage.data.existingClaim` / `storage.config.existingClaim`: the claims every pod
  mounts, which must exist before install. The chart creates no PersistentVolume, no
  claim and no pod hostPath, and has no storage class or size setting; a plain
  `helm install` operator brings the claims. The config claim holds one subdirectory
  per app instance, so a single claim covers backup.
- box creates them: a static PersistentVolume (reclaim `Retain`, bound to exactly one
  claim, `type: Directory`) and its claim, from the host paths in the grant, before the
  release. Capacity is nominal and never enforced; the disk is the limit, and there is
  no quota. `box init` creates the paths. The data outlives the release: the claims are
  not release objects, so `helm uninstall` cannot delete them.
- `puid`/`pgid`: set once for the whole release. Per-app overrides are possible but need
  a value written out explicitly.
- The two trees themselves are the wiring Job's, not the installer's: before it contacts
  any application it creates `downloads/<category>` and `media/<category>` for every
  instance, as the release's `puid`/`pgid`. An Arr refuses a root folder whose directory
  does not exist, and qBittorrent's category save paths are the other half of the same
  contract, so both are made up front, under the one `/data` mount. `box init` creates the
  volume roots and `config/<instance>`, and says the categories follow on the first run.

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

**Ownership is narrow.** Users live in the Arr and qBittorrent UIs, and an upgrade must
not undo their settings. The Job owns only the objects it created, and on those only the
fields the data contract depends on: host, port, credentials, category, save paths and
hardlink import. Everything else on those objects, and every object the user adds, belongs
to the user and is never written. An update writes the owned fields on top of the live
object; it never resends a schema template. When an owned field has drifted, the Job puts
it back and the receipt records the old and new value, because a changed category or save
path silently breaks hardlinks and the user should know their edit didn't stick. A
deleted owned object is recreated and reported. Owned objects are identified by the ids
recorded in the wiring ConfigMap, not by name and not by tag (see AGENTS.md invariant 11).

- Arr API keys are injected by env (`SONARR__AUTH__APIKEY`, …), so nothing has to be
  scraped from `config.xml`.
- The qBittorrent WebUI password is seeded as a PBKDF2 hash by an init container, and one
  category is created per Arr instance. Save path, temp path and category paths are set
  through the API and read back, because the image re-applies its own default at runtime.
- qBittorrent performance settings (send-buffer watermarks, async I/O threads, disk
  queue, disk cache, uTP, preallocation) come in three tiers. Compiled defaults are baked
  into the image and fixed at build, so they have to be right. Seeded preferences come from
  a runtime profile (`light`, `1g`, `10g`) and are written once, at first install, as a
  starting point. After that they are user-owned: nothing re-applies them on upgrade,
  `box update` or reset, and an edited watermark is not drift. Only the data-contract
  fields above are re-asserted.
- In every Arr: download client, category, root folder, `copyUsingHardlinks: true`, and
  Forms auth with the shared admin credential. Every setting is read back and asserted,
  and the Arr↔qBittorrent link is proven with the Arr's own connection test — Arr never
  returns the stored password, so a passing test is the only honest evidence.
- Prowlarr: Arr applications registered, so indexers sync out; each registration is
  likewise proven with Prowlarr's test endpoint before it is written.
- Seerr: connected to the Arrs and the media server.
- Plex: claimed with the token prompted at install (it expires in 4 minutes, so `box init`
  asks for it last). Jellyfin is the alternative media server.
- Tautulli: connected to Plex with the server token (`plex-token` in the release Secret,
  `secrets.plexToken`, the same token a claimed server hands out). The token is proven
  against Plex itself *before* Tautulli is touched: `GET /library/sections` with
  `X-Plex-Token` answers 200 only for a token that server accepts, and a 401 fails the
  run with no Tautulli write and no contact at all. `get_server_identity` alone cannot
  prove it — it reads Plex's unauthenticated `/identity`, so even a bogus token passes.
  Tautulli hides `pms_token` from `get_settings`, so the sha256 fingerprint of the token
  last written and proven is recorded in the wiring ConfigMap's `owned.json`; a changed
  fingerprint (a rotated Secret) rewrites the link although the live value is unreadable.
  Plex is never told to trust a network range: the public
  Gateway's Envoy reaches Plex from its own pod IP, inside the cluster pod CIDR, so
  `apps.plex.noAuthNetworks` defaults to empty — a trusted range there would expose the
  server unauthenticated through the Gateway. Without a token the row is `pending`.
- End-to-end hardlink proof, for every distinct download/library pair: write a file
  under the download folder, link it into the library folder, assert `nlink == 2` and the
  same device, then clean up. Every pair, because one claim is not one filesystem: a disk
  mounted inside the data directory breaks hardlinks for that one category only.
- The result goes into the `<release>-wiring` ConfigMap: what is connected to what and when it
  was verified.

### `box` CLI (Go, single binary)

The swizzin UX, backed by Helm instead of apt. Commands, scopes, values files, updates,
migrations and uninstall are specified in [`docs/box.md`](docs/box.md); this plan keeps
only the installer's cluster decision below.

### Installer and cluster detection

The installer is deliberately thin: `curl -fsSL get.<domain> | bash` checks OS and
architecture, downloads `box` with its sha256, and runs `box bootstrap`. Every decision
lives in Go, where it can be tested, and none in bash.

`box bootstrap` decides which cluster to use, in this order:

1. **An explicit cluster.** `--kubeconfig` or `KUBECONFIG`: use it or fail; never fall back.
2. **A cluster on this host.** Look for a kubeconfig where distributions put one
   (`/etc/rancher/k3s/k3s.yaml`, `/etc/rancher/rke2/rke2.yaml`, k0s's
   `/var/lib/k0s/pki/admin.conf`, kubeadm's `/etc/kubernetes/admin.conf`,
   `~/.kube/config`). A candidate counts only if `/version` answers and a
   SelfSubjectAccessReview says this user can create namespaces, CRDs and cluster roles.
3. **A single-node cluster, or nothing.** The distribution is identified only to choose
   presets and to refuse by name: the server version suffix (`+k3s`, `+k0s`, `-eks-`,
   `-gke.`), a `kubeadm-config` ConfigMap, the node's labels. A managed cluster (EKS,
   GKE) is refused, because there is no node `box` owns: the hostPath data path with
   hardlinks, the peers port with real source addresses and the one host `box` backs up
   all have nowhere to live. A cluster listing more than one node is refused for the
   same reason — the data contract is one filesystem, the peers port shares that node's
   address, and `box` backs up one host — and one listing no nodes is refused as a
   fault. `box` names which and stops. The chart is unaffected: it still installs
   anywhere, managed clusters included, through plain `helm install`.
4. **Something present but not usable.** A running kubelet, a `k3s`/`k0s`/`rke2`/
   `microk8s` binary or unit, or anything listening on 6443, 10250 or 2379. Docker or
   containerd alone is not a cluster: it is reported as a note and the install goes
   ahead, and a runtime holding one of those ports is caught by the listener. `box`
   stops and explains; it never installs a second cluster over a broken first one.
5. **A clean host.** Only now does it install k0s: pinned version and sha256,
   `k0s install controller --single`, then wait for the node to be Ready.

Candidates that were found but not used are always printed, on every path: a remote
cluster's `~/.kube/config` skipping silently would read as "there was nothing there".

The result is recorded in `/etc/swizzin/cluster.yaml` (source: `k0s-managed` or
`external`, kubeconfig path, distribution, platform modes, node count). Re-running
reads that record before the candidate scan, then probes the cluster it names again:
version, privileges, node count and distribution, and the platform layer, with the
same refusals a freshly found cluster gets. The record supplies the decisions
(lifecycle owner, platform modes, GatewayClass); the cluster supplies the evidence. A
recorded cluster that now fails a gate stops, and one whose kubeconfig no longer
answers stops naming that path rather than installing k0s over it. A `k0s-managed`
box's start and upgrade still belong to `box update`; an `external` cluster's lifecycle
is never touched. `box` owns the lifecycle of exactly what it installed.

The rest of the bootstrap: the platform layer by mode, `/srv/s5n/{data,config}` owned by
puid:pgid, then `box init`. Every step is safe to re-run.

### Default hardening (the community box finally gets it)

- Pod Security `restricted` on the namespace. Rootless images allow it.
- `readOnlyRootFilesystem`, all capabilities dropped, seccomp `RuntimeDefault`.
- Default-deny NetworkPolicy, with only the needed flows allowed
  (Arr → qBittorrent, Prowlarr ↔ Arr, Seerr → Arr/media, Gateway → app).
- Only the Gateway and the torrent port (a NodePort Service with
  `externalTrafficPolicy: Local`; hostPort is forbidden under `restricted`) are reachable
  from outside.
- The Gateway is the only address a user ever has to remember. The torrent port is
  plumbing: `box init` picks a free port in the NodePort range at random, records it in
  values, and the chart hands the same number to qBittorrent and the Service. Peers learn
  it from the tracker announce, so nobody types it. `box migrate` re-randomizes rather
  than carrying an old port across; trackers accept the new one on the next announce.
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
cmd/s5n/                 in-cluster binary: wiring (the Job), qbt-config (init container); stdlib only
cmd/box/                 host CLI over the Helm SDK; never shipped in the cluster image
internal/apps/           per-app clients, shared by the Job and box
internal/wiring/         the wiring reconciler and the receipt it publishes
internal/qbtconf/        qBittorrent credential hashing and config editing
install/                 installer script and k0s config fragments
hack/                    kind config, Envoy Gateway manifest, e2e-kind.sh
test/e2e/                k0s + k3s end-to-end suites (phase 1+)
docs/
Dockerfile               the image the wiring Job and the qBittorrent init container run
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
- Source boxes are not empty. `migrate` imports catalog apps only and leaves everything
  else running untouched: other apps, an existing ingress controller, cert-manager,
  monitoring. It detects shared infrastructure instead of installing a second copy.
- Foreign clients of migrated apps (exporters, peer-ban tools, autobrr/qui before they
  are catalog entries, a Prometheus scraping the apps) are listed by the dry run and
  become explicit `networkPolicy` allow entries, because default-deny would otherwise
  cut them off silently on cut-over.
- Auto-updaters (keel, Watchtower) watching migrated workloads are reported; a digest-
  pinned release must not be fought by one.
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
- **libtorrent 2.x image:** not built. qBittorrent 5.3 still compiles against either
  branch (`QBT_USES_LIBTORRENT2` guards in `sessionimpl.cpp`) and userdocs publishes
  `release-5.3.0_v1.2.20` and `release-5.3.0_v2.1.2` from the same app version, so it
  would be an image tag axis (`lt2.1.2`), not a fork. 2.x removes the disk cache and
  deprecates the 1.2 cache settings, so 1.2-era cache advice does not carry over; the
  compiled-defaults patch ports by value.
