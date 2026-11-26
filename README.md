<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/s5n-dark.svg">
    <img alt="s5n" src="docs/brand/s5n-light.svg" width="240">
  </picture>
</h1>

[swizzin](https://github.com/swizzin/swizzin) re-imagined for a modern era, powered by Kubernetes. A free, self-hosted seedbox environment: qBittorrent, Prowlarr, Sonarr, Radarr, Plex, Seerr and Tautulli, plus opt-in Lidarr, Bazarr, SABnzbd, FileBrowser Quantum, autobrr, qui, PeerBanHelper and Maintainerr, installed and managed by `box`. Applications wired for minimal config, hardlink-friendly, and locked down by default. Your media server isn't a pet -- stop treating it like one.

You never type `kubectl`. `box`, your familiar friend since 2017, finds or installs the cluster, asks
you a few questions, and runs the stack. s5n is built on Kubernetes to address current
limitations and pain points of swizzin: continuous updates tested and deployed with CI/CD, the freedom to choose
your Linux distribution of choice, applications managed by charts, meaningful application
isolation, ease of upgrading _all the things_ and a way back when something breaks.

> [!WARNING]
> s5n is in early PoC stages. While v0.6.0 is currently public, it's not quite ready for public consumption.
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
- **Opt-in extras.** Lidarr, Bazarr, SABnzbd, FileBrowser Quantum, qui, autobrr, PeerBanHelper and Maintainerr, off until you ask for them.
- **HD and 4K.** Sonarr and Radarr can each run a second instance for 4K, and Bazarr gets one per Arr pair.
- **swizzin's own qBittorrent.** The project's builds on libtorrent 1.2 or 2.1, a 10G build of each, and one for AMD Zen 3, one line in your values away.
- **One login in front of everything.** Sign in once at the gateway for qBittorrent, the Arrs, Prowlarr, Tautulli, autobrr and qui, with Tautulli's and PeerBanHelper's own logins switched off behind it, and a sign-in that lasts a week (an idle hour still ends it).
- **HTTPS by default.** Let's Encrypt when your name is public, one self-signed root to trust when it isn't.
- **Locked down by default.** Restricted pods, default-deny networking and digest-pinned images. From outside, only the gateway and the torrent port answer.
- **The swizzin layout.** Every app at `https://box.example.com/<app>`, the way swizzin always laid them out.
- **Your colours.** Seven themes for box and the sign-in page, s5n mint by default.
- **Updates that arrive on their own.** box checks for a new release every hour, moves your seedbox to it and tells you what changed.
- **A way back.** An app that won't start after an update is put back the way it was, and `box rollback <app>` does the same when something breaks later.
- **Notices where you already are.** Updates, rollbacks and apps that stop being healthy go to one URL you give box -- Slack, Discord, anything that takes JSON.
- **Your install, moved over.** `box migrate` takes a seedbox you already run on Kubernetes and puts it on box in one guided run, keeping its settings, connections, torrents and Plex's state, and asking one question before anything is installed.
- **Try the move first.** A rehearsal applies a copy of the source's settings to a throwaway box with no internet and every torrent stopped, so you can watch the migration work before you commit to it.
- **Zero obligation to use box:** The charts are freely available and can be used directly with helm in a completely standalone environment.

## Requirements

### Basic Assumptions
- Single node clusters
- Local storage
- Cloud provider support (EKS, GKE, etc) not in scope
- No bare IP support
  - Every user requires a DNS name, and a record for each app served at its own name (Seerr, Plex's web app, PeerBanHelper); a wildcard covers them all
- Hosts running firewalld (the Fedora family) work out of the box: when box installs the cluster, it lets the cluster's own networks through the firewall. On a cluster you already run, it prints the commands instead of guessing at its networks.
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

0.6.0 doesn't upgrade a 0.5.0 box: install it again.

Then open `https://<your domain>/sonarr` and sign in with the account box asked for (the
one you ran `sudo` as, by default). The gateway answers only on that name, not the bare
IP. Open ports 80, 443 and the torrent port box prints at the end. A name that isn't
public gets a self-signed certificate, and your browser will say so.

Your seedbox gets a namespace of its own, `s5n-<login>` by default, which is what will let a
second person on the same server have one beside yours.

For an unattended install, every question is a flag or a key in the values file. `--apps`
is required: a comma separated list of catalog names, or `none` for a box with nothing on
it yet.
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

- **the grant,** `/etc/s5n/seedboxes/root.yaml`. Root owns it. It holds what a
  seedbox is allowed: its namespace, uid and gid, host paths, ports, domain, the login's
  name, and which apps are switched on.
- **your values,** `~/.config/box/values.yaml`. Everything else about the apps and how
  they're set up.

Edit the values file by hand, then run `box apply`. box shows the diff and asks before
changing anything. Running `box init` again asks the same questions, with your current
answers filled in.

qBittorrent's build is a values edit too: `apps.qbittorrent.image.variant` picks one
of swizzin's own — `lt1.2` and `lt2.1` for libtorrent 1.2 or 2.1, `lt1.2-10g` and
`lt2.1-10g` for the same builds with larger peer buffers on a 10 Gbit link, or
`lt1.2-10g-znver3` and `lt2.1-10g-znver3` for those on an AMD Zen 3 machine (amd64
only). `box apply` moves it.

## Keeping it current

`box update` moves the seedbox to the newest chart release and leaves your values alone;
`box apply` keeps the release you're on. `box update --check` says what it would move to
and writes nothing.

On a host with systemd, bootstrap leaves a timer behind; anywhere else it prints the cron
line that does the same job. Root's `/etc/s5n/box.yaml` says whether it runs
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

`box migrate` takes a seedbox you already run on Kubernetes -- your own manifests or another
chart -- and puts its apps on a box, in one guided run. Their
settings and connections are rewritten to the box's, their data stays where it is so every
hardlink survives, and the library moves into box's layout with every app following it.
Downloads are never moved.

**Read the source.** `sudo box migrate discover` reads the cluster, or the manifests you
saved from it (`--from-dir`), and writes one snapshot: every app it found, the paths and
addresses they use, and what it would change. Where it can only guess -- an address that
was a pod's, a library directory two apps saw differently -- it proposes a value, says why,
and asks. Nothing is stopped, and nothing outside the snapshot is written.

**Stop it, then cut over.** box prints the command to stop the source on the platform it
found, and never stops it itself. When the apps are down, `sudo box migrate cutover` takes
the final snapshot; it refuses while anything it plans to move still runs, so a cutover that
starts has nothing left to overwrite. On the source's own cluster it then installs, with the
source's apps left stopped. `--replace-cluster` is the other road: the run stops after the
snapshot with the command that wipes the source's cluster, and `sudo box migrate apply`
finishes on the clean host.

**Install.** On a host with no box, `box migrate apply` is the installer: it asks only for
the domain, the login, the password and where config should live -- or takes all four as
flags -- and everything else comes from the snapshot. On a host that already has a box, it
adopts the snapshot into it. With no file named it takes the newest snapshot in
`/var/lib/s5n/migrate/`; a snapshot from another host is the file you carry over and pass.

Everything a migration does is shown once, in one question, before anything changes: the
platform a bare host gets, then a row per app with what it carries and what changes, the
directories it renames, and what needs you. Answer no and the host is left exactly as it
was -- no cluster, no platform, no files. The snapshot holds the whole report either way,
and `--details` prints every change column by column.

**Try it first.** `box migrate discover --rehearsal` adds a copy of the source's settings to
the snapshot, and applying it on a throwaway host of its own gives a locked box: it reaches
no internet, every torrent starts stopped, and no data is renamed. It is how you watch the
migration work before the real one; a rehearsal box is disposable, and the real run starts
from a final snapshot on a clean host.

What a migration keeps:

- **Every app's settings and connections.** API keys stay as they are, and the apps find each
  other at the box's addresses -- an Arr's download clients, Prowlarr's applications, Seerr's
  servers -- with the clients the source already had taken over rather than duplicated.
- **Plex's own state, and the library it built.** Its preferences, libraries and watch
  history come across, and so do the metadata, artwork, chapter thumbnails and preview
  thumbnails it spent hours building -- a snapshot carries them whenever the disk has room
  for them, and only Plex's caches, codecs, logs and crash reports always stay behind. On a
  disk without the room (`--plex-minimal`, or a snapshot that will not fit beside the
  config) it carries the state alone and Plex builds the rest again. The tree is written
  once, straight from the snapshot into the app's config directory, and everything a
  migration makes on the way lives beside the snapshot rather than in `/tmp`.
- **Your torrents, where they are.** qBittorrent's torrents and the categories they are in
  are kept, and downloads are not moved, so seeding carries on from the same files and every
  import is still a hardlink.
- **Your library, in box's layout.** The source's library directories become box's own
  (`Movies` becomes `media/movies`, `4K/TV` becomes `tv-4k`), renamed in place so hardlinks
  survive, with the Arrs' root folders, Plex's libraries and Seerr's root folders following
  them.
- **Your apps' own accounts.** autobrr's and qui's single account becomes box's own, so you
  sign in through the box's login rather than the password you had.

qBittorrent, Sonarr, Radarr, Lidarr, Bazarr, SABnzbd, Prowlarr, Plex, Seerr, Tautulli, autobrr, qui,
PeerBanHelper and Maintainerr are all covered. Anything else the source runs is left alone and listed in the
report.

## Using the chart without box

The chart works on any conformant Kubernetes 1.30 or newer with plain Helm:

```sh
helm install s5n oci://ghcr.io/swizzin/charts/s5n --version 0.6.3 -f values.yaml
```

Bring two claims (`storage.data.existingClaim` and `storage.config.existingClaim`), a
Gateway for its routes, and your domain in `gateway.host`. The chart ships no login: put
one in front at your Gateway and set `auth.edge: true` to say you have, or it refuses to
render. It ships no torrent port either: set `apps.qbittorrent.torrent.port` to the port
peers reach the node on, and pick it from the lower band of your NodePort range
(30000-30085 for the default range), where the API server does not hand out its own
nodePorts. A render without it names the key and the band. `charts/s5n/values.yaml`
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

Roughly in order: `box doctor`, the slow probes behind `status`, `install` and `remove`
per app, backup and restore (on the same snapshot a migration writes), more users on one
server (`adduser`), `box migrate` from a Docker or a bare-metal swizzin install, and
`box uninstall`.
`PLAN.md` holds the plan. `docs/box.md` describes how box behaves, with each part
marked decided or proposed.

## Development

```sh
hack/e2e-kind.sh   # the chart: build, install everything on kind, assert, tear down
hack/e2e-box.sh    # the same stack through box: fresh, preinstalled, foreign-gateway,
                   # update, migrate, migrate-keep
hack/grab-test.sh  # against a kept cluster: a real grab, imported as a hardlink
```

CI runs both e2e scripts on every pull request and every push to `main`; a push to
`develop` runs them too unless everything it changed is documentation.
The e2e runs the wiring twice and fails if the second run changes anything. The `update`
scenario moves a box to a later release and watches a crashlooping app get put back;
`migrate` takes an install box didn't make onto a box after the source's cluster is thrown
away, and `migrate-keep` installs onto the source's own cluster with its apps left stopped.
`grab-test.sh` sends a synthetic torrent from a local mock indexer through
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
