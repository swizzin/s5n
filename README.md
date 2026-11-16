<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/s5n-dark.svg">
    <img alt="s5n" src="docs/brand/s5n-light.svg" width="240">
  </picture>
</h1>

[swizzin](https://github.com/swizzin/swizzin) re-imagined for a modern era, powered by Kubernetes. A free, self-hosted seedbox environment: qBittorrent, Prowlarr, Sonarr, Radarr, Plex, Seerr and Tautulli, plus opt-in autobrr, qui, PeerBanHelper and Maintainerr, installed and managed by `box`. Applications wired for minimal config, hardlink-friendly, and locked down by default. Your media server isn't a pet -- stop treating it like one.

You never type `kubectl`. `box`, your familiar friend since 2017, finds or installs the cluster, asks
you a few questions, and runs the stack. s5n is built on Kubernetes to address current
limitations and pain points of swizzin: continuous updates tested and deployed with CI/CD, the freedom to choose
your Linux distribution of choice, applications managed by charts, meaningful application
isolation, ease of upgrading _all the things_ and a way back when something breaks.

> [!WARNING]
> s5n is in early PoC stages. While v0.5.0 is currently public, it's not quite ready for public consumption.
> Some of the day-two commands (`backup`, `uninstall`) aren't written yet, and a box has
> one user's login for now. Try it on a VM or a spare box, not on the seedbox you depend on.
> Expect nothing yet. No support, a shifting layout with no migration path, etc.
> The bar for support will rise once this is my daily driver.

## What you get

- **Managed k0s.** Don't know Kubernetes and don't care? You're in luck, `box` will gladly install k0s for you. Managing and upgrading it, backed by E2E compatibility testing, is *(planned)*.
- **Unmanaged Kubernetes.** Prefer to have more control over your Kubernetes environment? Want to use `kubeadm` or `k3s` instead? Be my guest!
- **A Kubernetes cluster.** Install out-of-scope projects easily in their own namespace, as long as they ship a container image.
- **Hardlinks that just work.** One data layout for every app: imports are hardlinks, never copies, and no remote path mappings.
- **Apps that already talk to each other.** qBittorrent, Prowlarr, the Arrs, Seerr and Tautulli come wired together, and box keeps them that way.
- **Opt-in extras.** qui, autobrr, PeerBanHelper and Maintainerr, off until you ask for them.
- **HD and 4K.** Sonarr and Radarr can each run a second instance for 4K.
- **swizzin's own qBittorrent.** The project's libtorrent 1.2 build, or its 10G build for AMD Zen 3, one line in your values away.
- **One login in front of everything.** Sign in once at the gateway, with single sign-on for the apps that speak OIDC.
- **HTTPS by default.** Let's Encrypt when your name is public, one self-signed root to trust when it isn't.
- **Locked down by default.** Restricted pods, default-deny networking and digest-pinned images. From outside, only the gateway and the torrent port answer.
- **The swizzin layout.** Every app at `https://box.example.com/<app>`, the way swizzin always laid them out.
- **Your colours.** Seven themes for box and the sign-in page, s5n mint by default.
- **Updates that arrive on their own.** box checks for a new release every hour, moves your seedbox to it and tells you what changed.
- **A way back.** An app that won't start after an update is put back the way it was, and `box rollback <app>` does the same when something breaks later.
- **Notices where you already are.** Updates, rollbacks and apps that stop being healthy go to one URL you give box -- Slack, Discord, anything that takes JSON.
- **Adopt what you already run.** `box migrate` puts a Kubernetes install box didn't make onto a box seedbox, leaving the data where it is so every hardlink survives.
- **Zero obligation to use box:** The charts are freely available and can be used directly with helm in a completely standalone environment.

## Requirements

### Basic Assumptions
- Single node clusters
- Local storage
- Cloud provider support (EKS, GKE, etc) not in scope
- No bare IP support
  - Every user requires a DNS name, and a record for each app served at its own name (Seerr, Plex's web app, PeerBanHelper); a wildcard covers them all
- Runs wherever `k0s`, `k3s` or `kubeadm` runs

### Hardware

This will expand as the project grows. Kubernetes has overhead and this project may not be suitable for low powered hardware, expect ~800MB of RAM utilization on an idling k0s process. This will be filled out as testing is performed.

## Install

You need a Linux server (amd64 or arm64) you're root on, and a DNS name pointing at it.

```sh
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fLO "https://github.com/swizzin/s5n/releases/latest/download/box-linux-${arch}"
curl -fLO https://github.com/swizzin/s5n/releases/latest/download/sha256sums.txt
sha256sum --ignore-missing -c sha256sums.txt
sudo install -m 0755 "box-linux-${arch}" /usr/local/bin/box
sudo box bootstrap
```

`bootstrap` does three things, and every one of them is safe to run again:

1. **Finds a cluster or installs one.** On a clean host it installs a pinned k0s
   (single node). If the server already runs a usable single-node cluster, box uses
   that instead, and never installs a second one over it.
2. **Installs the platform layer** a bare cluster lacks: the Gateway API, Envoy Gateway,
   MetalLB, cert-manager and the login (Authelia). It uses whatever already works, and
   stops with the reason when something would conflict.
3. **Runs `box init`,** which asks for your domain, your data and config paths, which
   apps you want, the login's password (leave it empty and box makes one) and where box
   should post notices, then installs them.

0.5.0 doesn't upgrade a 0.4.0 box: install it again.

Then open `https://<your domain>/sonarr` and sign in with the account box asked for (the
one you ran `sudo` as, by default). The gateway answers only on that name, not the bare
IP. Open ports 80, 443 and the torrent port box prints at the end. A name that isn't
public gets a self-signed certificate, and your browser will say so.

For an unattended install, every question is a flag or a key in the values file. `--apps`
is required: a comma separated list of catalog names.
`--set-transient secrets.plexClaimToken=claim-...` carries the Plex claim token, which
lives in no file. Without `--password-file`, box generates the login's password and
prints it once:

```sh
sudo box bootstrap --yes --domain box.example.com \
  --data-path /srv/s5n/data --config-path /srv/s5n/config \
  --apps qbittorrent,sonarr,radarr,prowlarr,plex,tautulli,seerr
```

`box bootstrap --dry-run` prints what it would decide, and why, without changing
anything.

## Changing things

box keeps two files per seedbox:

- **the grant,** `/etc/swizzin/seedboxes/root.yaml`. Root owns it. It holds what a
  seedbox is allowed: its namespace, uid and gid, host paths, ports, domain, the login's
  name, and which apps are switched on.
- **your values,** `~/.config/box/values.yaml`. Everything else about the apps and how
  they're set up.

Edit the values file by hand, then run `box apply`. box shows the diff and asks before
changing anything. Running `box init` again asks the same questions, with your current
answers filled in.

qBittorrent's build is a values edit too: `apps.qbittorrent.image.variant: lt1.2` swaps
in swizzin's libtorrent 1.2 build, or `lt1.2-10g-znver3-mimalloc` for the 10G build on
an AMD Zen 3 machine (amd64 only). `box apply` moves it.

## Keeping it current

`box update` moves the seedbox to the newest chart release and leaves your values alone;
`box apply` keeps the release you're on. `box update --check` says what it would move to
and writes nothing.

On a host with systemd, bootstrap leaves a timer behind; anywhere else it prints the cron
line that does the same job. Root's `/etc/swizzin/box.yaml` says whether it runs
(`update.timer: off` stops it), how often it checks (`update.interval`, an hour by
default), and where notices go.

An app that won't start on its new image is put back on the one it ran, and held there
until a release ships a different image for it. `box rollback <app>` is the same by hand,
from the copy box took before it moved the app, and it says how old that copy is before
it changes anything. An app you pin in your values (`apps.radarr.image.digest`) stays on
that image until you remove the pin.

box updates itself too, but only from a release whose checksums are signed. `--hold-box`,
or `update.hold-box` in root's file, keeps the box you have; `box update --rollback-box`
puts the previous one back.

`box status` is one line: the release, what the last update did, when the next check is,
and anything held.

Notices go to one address: an update, a rollback, an update that stopped, an app that
stopped being healthy or got better. Slack, Mattermost and Discord show them as a plain
sentence. `box init` asks for the address; leave it empty and there are no notices.

## Coming from another install

`box migrate` takes a Kubernetes install box didn't make and puts its apps on a box
seedbox: their settings are copied and rewritten for box's paths and ports, their data is
adopted where it is so every hardlink survives, and the connections between them are
pointed at the box.

It is two runs. `box migrate discover --out plan/` reads the source cluster (or the
manifests you saved from it) and writes the plan without changing anything; read the
plan, and edit a rule if a path or a host needs one. `box migrate apply plan/` does what
the plan says, after showing you the changes.

qBittorrent, the Arrs, Plex, Seerr, Tautulli, qui, autobrr, PeerBanHelper and Maintainerr
are all covered. Anything else the source runs is left alone and listed in the report.

## Using the chart without box

The chart works on any conformant Kubernetes 1.30 or newer with plain Helm:

```sh
helm install s5n oci://ghcr.io/swizzin/charts/s5n --version 0.5.0 -f values.yaml
```

Bring two claims (`storage.data.existingClaim` and `storage.config.existingClaim`), a
Gateway for its routes, and your domain in `gateway.host`. The chart ships no login: put
one in front at your Gateway and set `auth.edge: true` to say you have, or it refuses to
render. `charts/s5n/values.yaml` documents every key, and `values.schema.json` rejects
anything it doesn't know.

## The data layout

```
/data/downloads/{tv,movies,tv-4k,movies-4k,music,misc}
/data/media/{tv,movies,tv-4k,movies-4k,music}
```

This is the TRaSH single-mount layout, with `downloads` in place of `torrents`. The
category names are yours to change, and an existing seedbox's names, including nested
ones like `4K/Movies`, are accepted as they are.

## What's next

Roughly in order: `box doctor`, the slow probes behind `status`, `install` and `remove`
per app, backup and restore, more users on one server (`adduser`), and `box migrate` from
a Docker or a bare-metal swizzin install, not only a Kubernetes one.
`PLAN.md` holds the plan. `docs/box.md` describes how box behaves, with each part
marked decided or proposed.

## Development

```sh
hack/e2e-kind.sh   # the chart: build, install everything on kind, assert, tear down
hack/e2e-box.sh    # the same stack through box: fresh, preinstalled, foreign-gateway, update, migrate
hack/grab-test.sh  # against a kept cluster: a real grab, imported as a hardlink
```

CI runs both e2e scripts on every pull request and every push to `develop` and `main`.
The e2e runs the wiring twice and fails if the second run changes anything. The `update`
scenario moves a box to a later release and watches a crashlooping app get put back;
`migrate` takes an install box didn't make onto a box, rewriting every path and
connection. `grab-test.sh` sends a synthetic torrent from a local mock indexer through
Prowlarr, Sonarr and qBittorrent, and checks that it lands in the library as a hardlink.
It needs TVDB, so it stays out of CI; run it after `hack/e2e-kind.sh --keep`. The e2e
scripts keep their kubeconfig in `${XDG_CACHE_HOME:-~/.cache}/s5n-e2e/kubeconfig`
(override with `S5N_E2E_KUBECONFIG`) and never read or write `~/.kube/config`.

The release's own steps are a script too (`hack/release.sh`), and CI rehearses them on
every pull request, so a bug in the release path fails before a tag rather than after
one.

`AGENTS.md` is the working brief: the layout of the repo, the rules every change
follows, and what CI checks.

## AI disclosure

s5n is built with the assistance of Anthropic's Claude and
DeepSeek. The maintainer directs the design, reviews the work, and is responsible for
every commit. `AGENTS.md` is the brief those assistants work from.

## Licence

GPL-3.0, like swizzin. See `LICENSE`.
