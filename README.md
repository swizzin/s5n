# s5n

swizzin on Kubernetes. A free, self-hosted seedbox: qBittorrent, Prowlarr, Sonarr,
Radarr, Plex, Seerr and Tautulli, installed on one server by one command, wired to each
other, hardlink-safe, and locked down by default.

You never type `kubectl`. `box`, a single binary, finds or installs the cluster, asks
you a few questions, and runs the stack. Kubernetes is underneath because it gives a
seedbox real updates, real isolation and a way back when something breaks, not because
you should have to learn it.

> [!WARNING]
> s5n is early. v0.2.0 is the first release and the first one meant to install from a
> clean host. It serves plain HTTP only, and most of the day-two commands (`update`,
> `status`, `backup`, `migrate`, `uninstall`) aren't written yet. Try it on a VM or a
> spare box, not on the seedbox you depend on.

## What you get

- **One data layout where hardlinks always work.** Every app mounts the same directory
  at `/data`, with `downloads/` and `media/` side by side on one filesystem. Imports are
  hardlinks, never copies, and no app needs a remote path mapping.
- **Apps that are already talking to each other.** After every install or upgrade a
  wiring job connects qBittorrent, Prowlarr and the Arrs, points Seerr and Tautulli at
  them and at Plex, creates the category folders, and proves that a hardlink really
  works between your download and library paths. It writes only what's missing, so
  running it again changes nothing.
- **HD and 4K instances.** Sonarr and Radarr can each run a second instance with its
  own categories and library folders.
- **Hardened by default.** Pod Security `restricted`, read-only root filesystems, every
  capability dropped, default-deny networking with only the flows the apps need, and
  images pinned by digest. From outside, only the web gateway and the torrent port are
  reachable.
- **Every app at a path under one name:** `http://box.example.com/sonarr`, `/radarr`,
  `/qbittorrent` and so on, the way swizzin always laid them out.

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
   (single node) and waits until it's ready. If the server already runs a usable
   single-node cluster, box uses that instead. It never installs a second one over
   something it finds.
2. **Installs the platform layer** a bare cluster lacks: the Gateway API, Envoy Gateway,
   and MetalLB so the gateway gets the server's own address. It checks what's already
   there first, uses whatever works, and stops with the reason when something would
   conflict.
3. **Runs `box init`,** which asks for your domain, where your data and app settings
   live, and which apps you want, then installs them.

Then open `http://<your domain>/sonarr`. The gateway answers only on that name, so
browsing to the bare IP address gets you nothing. Open port 80 and the torrent port
(31111 by default) in your firewall.

For an unattended install, every question is also a flag:

```sh
sudo box bootstrap --yes --domain box.example.com \
  --data-path /srv/s5n/data --config-path /srv/s5n/config
```

`box bootstrap --dry-run` prints what it would decide, and why, without changing
anything.

## Changing things

box keeps two files per seedbox:

- **the grant,** `/etc/swizzin/seedboxes/root.yaml`. Root owns it. It holds what a
  seedbox is allowed: its namespace, uid and gid, host paths, ports, domain, and which
  apps are switched on.
- **your values,** `~/.config/box/values.yaml`. Everything else about the apps and how
  they're set up.

Edit the values file by hand, then run `box apply`. box shows the diff and asks before
changing anything. Running `box init` again asks the same questions, with your current
answers filled in.

## Using the chart without box

The chart works on any conformant Kubernetes 1.30 or newer with plain Helm:

```sh
helm install s5n oci://ghcr.io/swizzin/charts/s5n --version 0.2.0 -f values.yaml
```

It creates no storage of its own. Bring two claims (`storage.data.existingClaim` and
`storage.config.existingClaim`) and a Gateway for its routes. `charts/s5n/values.yaml`
documents every key, and `values.schema.json` rejects anything it doesn't know.

## The data layout

```
/data/downloads/{tv,movies,tv-4k,movies-4k,music,misc}
/data/media/{tv,movies,tv-4k,movies-4k,music}
```

This is the TRaSH single-mount layout, with `downloads` in place of `torrents`. The
category names are yours to change, and an existing seedbox's names, including nested
ones like `4K/Movies`, are accepted as they are.

## What's next

Roughly in order: TLS (cert-manager, Let's Encrypt when the name is public, self-signed
when it isn't), `box status` and `doctor`, `install` and `remove` per app, `update`,
backup and restore, more users on one server (`adduser`), and `box migrate` to adopt an
existing bare-metal swizzin install with its Arr databases and torrents intact.
`PLAN.md` holds the plan. `docs/box.md` describes how box behaves, with each part
marked decided or proposed.

## Development

```sh
hack/e2e-kind.sh   # the chart: build, install everything on kind, assert, tear down
hack/e2e-box.sh    # the same stack, installed through box bootstrap and init
hack/grab-test.sh  # against a kept cluster: a real grab, imported as a hardlink
```

The e2e runs the wiring twice and fails if the second run changes anything.
`grab-test.sh` sends a synthetic torrent from a local mock indexer through Prowlarr,
Sonarr and qBittorrent, and checks that it lands in the library as a hardlink. It
needs TVDB, so it stays out of CI; run it after `hack/e2e-kind.sh --keep`. The e2e
scripts keep their kubeconfig in `${XDG_CACHE_HOME:-~/.cache}/s5n-e2e/kubeconfig`
(override with `S5N_E2E_KUBECONFIG`) and never read or write `~/.kube/config`.

`AGENTS.md` is the working brief: the layout of the repo, the rules every change
follows, and what CI checks.

## AI disclosure

s5n is built with substantial help from AI coding assistants (Anthropic's Claude and
DeepSeek). The maintainer directs the design, reviews the work, and is responsible for
every commit. `AGENTS.md` is the brief those assistants work from, and it's public on
purpose.

## Licence

GPL-3.0, like swizzin. See `LICENSE`.
