# box architecture

`box` is the host CLI for s5n: it installs the platform, runs seedboxes, keeps them
updated and removes them again. This document is how box is used and how it behaves.
`PLAN.md` holds the product plan and the data contract; where the two disagree, this
document is newer for anything about box itself.

Each section marks its status:

- **Decided:** the maintainer's call.
- **Proposed:** a design that follows from the decisions, not yet agreed.
- **Open:** not answered yet.

## Principles (decided)

- **box is the whole interface.** Users never need to know Kubernetes is underneath. No
  Kubernetes words in output: "restarting", not `CrashLoopBackOff`.
- **Users range from novices to people who want to change everything.** Novices get
  guided terminal dialogs. Tweakers get one values file they edit by hand. Both are the
  same thing underneath.
- **One source of truth.** Guided mode only fills in the values file. Every question it
  asks is also a file key, so unattended installs work from day one.
- **The bar is the maintainer's own seedbox.** If box can't run it (multiple Arr
  instances, the catalog apps it uses, automatic updates), box isn't done.
- **No migration spaghetti.** Upgrade logic is bounded, versioned and deletable (see
  Migrations).
- **box can be removed** from a server, cleanly.
- **Personality stays.** Dialogs keep the voice: cancelling says "oh heck, nvm", not
  "Operation aborted by user". Not tied to whiptail.
- **Dialogs are huh (decided),** behind box's own prompt interface, with box's theme
  on top: its masthead, accent and step header. Where huh can't behave (no terminal,
  `TERM=dumb`), box falls back to plain line prompts. `NO_COLOR` drops the styling.
  Choosing another library later means rewriting one adapter, not init.

## Scopes: root and users (decided, mechanism proposed)

What box acts on depends on who runs it.

| invoked as | acts on |
|---|---|
| root | the host: cluster, platform layer, users, every seedbox |
| a user | that user's seedbox only |

One user gets one seedbox: their own apps, their own downloads and their own config,
isolated from everyone else's. Multi-user stays barebones: no quotas, billing,
panels or other enterprise features.

**Each user needs a domain (decided).** A bare IP can't be served, because routing and
certificates are per host name.

**Users get a shell (decided).** They need one to run box and to reach their own media.

**Root installs and removes apps; users run their own (decided).** Users can restart,
read logs, update and back up their own apps. Root decides which apps they have.

### Two files per seedbox (decided)

A user's values file can't be applied with root's power. One `storage.data.path`
pointing at someone else's downloads would end the isolation. So each seedbox is split
into what root grants and what the user tunes:

- **Grant**, `/etc/swizzin/seedboxes/<user>.yaml`, root-owned: domain, namespace,
  puid/pgid, data and config paths, the apps the user has. Root edits it with
  `box adduser`, `box install` and `box remove`.
- **Values**, `~<user>/.config/box/values.yaml`, user-owned: everything else the
  schema allows (app settings, tuning, image variants, schedules).

Hand edits to either file are preserved: box reads, merges and diffs, and never
regenerates a file from scratch. box merges the two, and the grant always wins. A grant
key in the values file is an error that names the key, not something silently ignored.
Root's own seedbox works the same way, with root as the user.

`internal/seedbox` implements the split: it loads both files, merges them (a grant key in
values is an error naming the key and its line), checks the result against a chart
release's `values.schema.json`, and edits either file by splicing the bytes of the one
key that changed, so comments, key order, blank lines and indentation survive. `box` is
not wired to it yet. The per-key decision of what the grant owns lives in
`internal/seedbox/grant.go`; the torrent port below is treated as the grant's for now.

### Storage: host paths and claims box creates (decided)

A seedbox's data and config live on host paths. Mounting them (fstab, a pool, a union
filesystem) is the host's business; box never mounts anything. There is no CSI driver
and no provisioner: one that creates a directory or volume per claim splits `/data`
across filesystems, and hardlinks don't cross them.

- **The chart takes a claim and nothing else.** `storage.data.existingClaim` and
  `storage.config.existingClaim` name claims that already exist. The chart creates no
  PersistentVolume or claim and has no hostPath, storage class or size setting. On a
  plain `helm install` the operator brings the claims, from any storage they like.
- **box creates them.** From the paths in the grant, box writes a static PersistentVolume
  (reclaim `Retain`, bound to exactly one claim) and its claim, before the release:
  for root at `init`, for each user at `adduser`. A host path never appears in a pod
  spec, so Pod Security `restricted` still admits every pod.
- **The namespace comes first, when it is missing.** box creates it with Pod Security
  `restricted` -- the labels root's `adduser` sets -- and only before the first volume:
  a claim written into a namespace that is not there fails after its cluster-scoped PV
  has been created. An existing namespace is never relabelled: root's or an admin's
  labels win, and one that does not enforce `restricted` is reported, not changed. A
  user's kubeconfig is namespace-scoped and cannot create a namespace, so root creates
  a user's at `adduser`; apply refuses and names that command instead of writing a
  volume it cannot finish.
- **Size is nominal.** A host path PV's capacity is not enforced; the disk is the
  limit. box writes a fixed size, and nothing a user edits can resize a static claim.
  box enforces no quota.
- **The data outlives the release.** The claims aren't release objects, so
  `helm uninstall` can't delete them; only box's `--purge` does.
- **`init` creates the paths.** Guided init reports back what it found: device, mount
  point, free space and owner. Unattended runs the same check at the start, with fewer
  guardrails.

### How a user's box gets its power (proposed)

Without root, a user's box gets a kubeconfig scoped to that user's namespace. Everything
that would let the user reach past it is created by root at `box adduser`, where the
user can't change it:

- **Storage.** Root creates the PersistentVolumes and claims (above), and the user's
  release uses them through `existingClaim`. PVs are cluster-scoped, so the user
  couldn't create one anyway.
- **Pod Security.** Root sets `restricted` on the namespace. Namespaces are
  cluster-scoped, so the user can't relabel their own.
- **Routing.** Root adds a Gateway listener for the user's domain that only accepts
  routes from the user's namespace. Without it, one user's HTTPRoute could claim
  another user's host name.

**The cluster enforces the grant (decided).** box refuses a grant key in values, but a
user holds a kubeconfig for their namespace and can skip box (`kubectl set image`,
`helm upgrade` with their own values, a Service on any nodePort). So root installs a
ValidatingAdmissionPolicy at `bootstrap` and binds it to each user namespace at
`adduser`, with that user's grant as its parameters: only blessed image repositories,
only the granted nodePort and Service type. Pod Security `restricted` still does the
rest. This raises the chart's `kubeVersion` floor to 1.30, where the API is GA.

**Root assigns the torrent port (decided).** It's a port on the node that every seedbox
shares, so it lives in the grant with the Service type, and `box adduser` picks a free
one.

## Commands

Status per command: **D** decided, **P** proposed.

| command | who | does | |
|---|---|---|---|
| `box bootstrap` | root | prepares the host: finds or installs the cluster, the platform layer, directories; then runs `init` for root | D |
| `box init` | root, user | sets up a seedbox: domain, paths, apps, existing data, Plex claim | D |
| `box adduser <name>` | root | creates the OS user and shell, the grant, and the namespace pieces above; then runs `init` as that user | D |
| `box deluser <name>` | root | removes that user's seedbox, with the same keep rules as `uninstall` | P |
| `box install <app>` | root | adds an app to a seedbox; an Arr can take a second instance | D |
| `box remove <app>` | root | removes an app, keeping its config; `--purge` deletes it | D |
| `box apply` | root, user | makes the seedbox match the edited values: diff, confirm, apply | D |
| `box update [app]` | root, user | moves to the next chart release; with an app, only that app | D |
| `box status` | root, user | the one-screen answer to "is my box okay?" | D |
| `box doctor` | root, user | the slow probes behind status | D |
| `box logs <app>` | root, user | the app's logs | D |
| `box restart <app>` | root, user | restarts one app | P |
| `box backup` / `restore` | root, user | app configs, qBittorrent state, Arr databases; nothing until configured | D |
| `box migrate` | root | adopts an existing install | D |
| `box uninstall` | root | removes box from the server | D |

`--user <name>` lets root run any user command against that user's seedbox.

### bootstrap and init (decided)

They are different jobs. `bootstrap` sets up the machine once. `init` sets up one
seedbox: once for root, once per user through `adduser`. Every step of both is safe to
re-run. The cluster decision is in `PLAN.md`.

What `init` does (decided):

- **It asks, then writes the two files.** Domain, data and config paths, which apps,
  and each chosen app's setup questions from the catalog manifest. Answers are spliced
  into the grant and values files key by key, then `init` hands over to `apply`, whose
  diff and confirm are the review. There is no separate "here is your file" step.
- **Re-running it edits.** On a seedbox that already has its files, `init` asks the
  same questions with the current answers filled in and splices only the keys that
  changed. Hand edits elsewhere in either file survive.
- **Paths.** For each path `init` reports the device, mount point, filesystem, free
  space and owner. A path that doesn't exist yet is the case to be careful with: a disk
  that didn't mount leaves an empty directory on `/`, and a seedbox would fill it.
  Guided `init` asks before it creates a missing path and says which filesystem it
  would land on. Unattended `init` refuses a missing path. The directories s5n needs
  beneath an existing path are created without asking, owned by the seedbox's
  puid:pgid.
- **Existing data is used as it is.** A path that already holds downloads or configs
  is reported ("2.1 TiB in 3 directories") and used without changes. Adopting a
  previous install properly (rewriting paths in the Arr databases, carrying
  qBittorrent's state) is `migrate`'s job, never `init`'s.
- **Unattended.** Every question is a flag or a file key. With `--yes`, or when there's
  no terminal, `init` asks nothing. A missing required answer is an error that names
  the flag or key to set.

### install, remove and instances (decided)

Only the Arrs take multiple instances (an HD and a 4K Sonarr, for example). The chart
already models this as `instances:` on the Arr entry. Proposed syntax:
`box install sonarr --instance 4k`. No other app needs a second instance.

### apply and update (decided)

They change different things, so they stay separate commands:

- The **values file** holds what the user wants. `apply` keeps the current chart release
  and applies the edited file.
- The **chart release** holds the pins: image digests, platform versions, migrations.
  `update` moves to a newer release and keeps the values file as it is.

Both end in the same step: diff, confirm, apply. Using one never locks out the other.

### How box versions against the chart (proposed)

box and the chart live in one repo but ship as separate artifacts. The image and the
chart release together: one tag, appVersion equal to the image tag. box has its own
version. What they share is a short list of interfaces, and the rule versions those
interfaces, not the code:

- the values schema and the grant key set (what box may write and merge);
- `status.json` in the wiring ConfigMap (what `status` and `doctor` read);
- the catalog manifest, once it exists (what `init` and `install` ask);
- the migration floor (which older releases a release can upgrade from).

**A minimum box version (decided).** The chart declares it in `Chart.yaml`, next to
its migration floor:

```yaml
annotations:
  box.s5n/min-box: "0.4.0"
  box.s5n/upgrade-from: "0.2.0"
```

`min-box` goes up only when one of those interfaces changes in a way an older box
would get wrong: a renamed grant key, a status field whose meaning changed, a new kind
of setup question. Adding an app, bumping a digest or adding an optional values key
leaves it alone. A box older than `min-box` refuses that release until it's updated.
In the other direction, box keeps reading every release back to the oldest
`upgrade-from` it knows, so a new box never strands an old install.

**box only uses published charts.** It pulls a released chart by version from the OCI
registry and reads the schema, manifest and annotations from that artifact. It never
loads `charts/` from its own tree, so a box built from main can't depend on templates
no user has. box's own tests run against the chart packaged from the same commit,
the same way.

**What update does with it:**

- **box updates itself first (decided), then re-runs as the new binary (proposed).**
  `box update` checks for a newer box before it looks at charts, replaces itself, and
  execs the new binary with the same arguments, so the chart update always runs under
  the newest box. The timer does the same, which is why a release that raises
  `min-box` never leaves an unattended box stuck.
- Self-update rules (proposed):
  - **Verified before it runs.** The new binary is checked against the release's signed
    checksum before it replaces anything. It runs as root, so an unverified self-update
    would be remote code execution as root.
  - **Atomic, with one step back.** Written beside the old binary and renamed over it.
    The previous binary is kept as `box.previous`, and `box update --rollback-box` puts
    it back.
  - **Root only.** The binary is root-owned. A user's `box update` that needs a newer
    box says so and stops: "box 0.4.0 is needed; root's next update brings it".
  - **Held like a pinned image.** `--hold-box` (or a key in root's config) skips the
    self-update and says so on every run. A release whose `min-box` a held box doesn't
    meet is shown, not taken.
  - **Package-managed installs defer.** If box came from a distro package, it never
    overwrites itself. It names the package command instead.
- It picks the newest release this box meets `min-box` for. A newer release that needs
  a newer box is shown, not taken: "s5n 0.6.0 is out and needs a newer box".
- It never skips past `upgrade-from`. If the installed release is older than the
  target's floor, update steps through the intermediate releases, each with its own
  health and wiring gate. This is what lets old migrations be deleted safely.
- `apply` re-reads the annotations of the release already installed. A box that's too
  old for the installed release refuses to apply rather than writing values it can't
  check.

**What CI proves:** box at HEAD against the newest released chart and the oldest
release back to its `upgrade-from` floor; the chart at HEAD against the newest released
box, which must meet its `min-box`. A chart change that needs a newer box but forgets
to raise `min-box` fails there and not on a user's server. The self-update path gets
its own test: the previous box release updates itself to HEAD and finishes a chart
update.

### Image choice: blessed variants (decided in principle)

A user can't run an arbitrary image. A different image can move config files, mounts or
users, and the chart can't promise anything about an image it hasn't seen.

Instead, the chart lists **blessed variants** per app, such as a qBittorrent build on
libtorrent 1.2 or 2.x (see `PLAN.md`, open decisions). Each variant is tested by the
e2e like the default. Proposed command: `box update qbittorrent --variant lt2`, which
writes the choice into values.

A digest pinned by hand in values is held: `update` leaves that app where it is and
says so ("radarr: held at 6.1 by your values"). Removing the pin releases it.

**A user can't name an image repository (decided).** Today `apps.*.image.*` is a values
key and the schema accepts any repository, so a user could patch in an arbitrary image.
The fix: the repository comes only from the chart's variant list. Values choose a
variant by name and may hold a pin inside that variant's repository, nothing else.
The admission policy under Scopes enforces it in the cluster, not only in box.

**Two jobs, one list (decided).** The variant list does two different jobs, and they
are enforced in different places:

- **Layout.** The chart only promises mounts, users and config paths for images it has
  tested. That holds on every seedbox, root's included, and the schema and box enforce
  it. Root can still `kubectl set image` on their own seedbox; it's their server, and
  the guardrail is there to stop a values file from breaking it, not to stop root.
- **Tenancy.** On a shared server, a user mustn't run images or take ports root didn't
  grant. Only the admission policy enforces that, and it's bound only to user
  namespaces, so a single-user box never meets it.

The policy's allowlist is generated from the variant list, never kept by hand. Adding
an app already means a values entry and its variants, so it reaches the allowlist with
no extra step. Anything outside the catalog stays outside a user's namespace, which is
the decided rule that root decides which apps a user has.

### uninstall (decided)

- **Never deleted:** media and downloads, even when asked. The user removes those
  themselves.
- **Deleted only on opt-in:** app configs; k0s, and only if box installed it.
- **Always removed:** the seedbox releases, the platform layer box installed, box's own
  records and the box binary.

## Updates (decided goal, proposed mechanism)

Goal: everything stays current without anyone thinking about it, apps and k0s
included.

The chart pins every image by digest; there's no in-cluster image updater and no
`:latest` (`PLAN.md`, Chart). Freshness comes from the repo side instead:

1. Renovate opens a digest bump when an app image publishes.
2. CI runs the e2e suite on it; a passing bump cuts a chart release automatically.
3. Each box runs `update` on a timer (on by default). A health check and a wiring check
   gate each update; a failure rolls back to the previous release (Phase 3).

New images arrive hours after upstream instead of minutes, and a broken upstream image
fails in CI instead of on a user's box.

### The cluster itself (decided)

The cluster has the same modes as the platform components in `PLAN.md`:

- **managed:** box upgrades k0s on the same timer, pinned and tested like everything
  else, with doctor run before and after. A k0s box installed starts here.
- **external:** box never touches the cluster's lifecycle; it only checks it. A cluster
  migrate finds starts here.

`migrate` offers to take over a k0s it finds. Accepting switches it to managed;
declining keeps it external and shows the support warning once:

> This cluster is self-managed. s5n is tested end to end only on the latest k0s and
> Kubernetes releases, and only tested versions are supported.

After that, status and doctor warn when an external cluster drifts outside the tested
range. They don't refuse.

Workloads outside box's namespaces are the admin's business and aren't touched.

## Migrations (decided in principle, rules proposed)

Images change their config layout now and then. The rules keep migrations from turning
into swizzin's upgrade scripts:

- **Mounts are the chart's job.** The chart fixes the paths inside the container
  (`/config`, `/data`). If an image moves its config directory, one template line
  changes and nothing migrates.
- **Migrations only edit what's inside config files.** Each one is an init step tied to
  a chart version, forward-only and safe to run twice.
- **They touch only what they own.** User-owned files such as the qBittorrent tuning
  seeded at install get one key changed, never regenerated. A change that can't be made
  that way is a breaking release, not a migration.
- **Old migrations get deleted.** Each release declares the oldest release it can
  upgrade from, so migrations older than that can be removed.
- **Every migration is proven** by the upgrade e2e: previous release → current, data
  intact, wiring green.

## The catalog and box (decided)

Every seedbox-oriented app belongs in the catalog, and contributors are expected to
add most of them. Adding an app shouldn't need a box release.

box names no app today; wiring runs in the s5n image, which the chart pins. What's
still missing is the setup questions. The chart should ship a **catalog manifest**,
one entry per app:

- name and one-line description;
- whether it takes multiple instances;
- its setup questions: prompt, type, default, and the values key each answer goes to.

box reads the manifest from the chart release it's installing and builds its dialogs
from it. Because every answer maps to a values key, the manifest also documents the
unattended install. A new app ships as one chart PR, plus a wiring step in the image if
it needs connecting to the others. box changes only when an app needs a new *kind* of
question.

The manifest lives in the chart beside `variants.yaml` and is read the same way: values
can't override it. A question may only name a key the user owns. Grant keys (domain,
paths, the app list) are box's own questions, never a manifest's.

Some answers can't live in a file. The Plex claim token expires about four minutes
after it's issued, so a question can be **transient**: box asks it last, after the
confirm, passes it to this one apply and never writes it to either file.

`s5n.instances` in `charts/s5n/templates/_helpers.tpl` builds the workload list from the
keys under `apps:`, so a new app is a values entry plus its entry in
`values.schema.json`, never a template edit.

## Ownership and kubectl (decided)

Admins keep full kubectl, but seedbox namespaces are managed by box. Proposed: box
labels what it owns. If a managed object is edited by hand, status reports it as
"changed outside box, `box apply` will undo it", and the next apply does. Nothing is
locked, and box never pretends a hand edit stuck.

## status and doctor (proposed)

`status` is what's true now, from cheap reads, and it answers in its first line. Healthy
rows stay quiet; problems point at the command that fixes them.

```
box · tv.example.com · s5n 0.4.1 (0.4.2 available → box update)
  ● 1 problem    certificate ok (61 days)    torrent port reachable

apps
  qbittorrent   running 12d   412 seeding   ↑ 8.2 MB/s
  sonarr        running 12d   3 queued
  sonarr-4k     running 12d
  radarr        restarting (3 times, last 4 min ago)   → box logs radarr
  plex          running 12d   2 streaming

storage   /srv/s5n/data   18.2 of 24.0 TiB (76%)   full in ~41 days at this month's rate
backup    not configured   → box backup setup
values    edited since last apply   → box apply
```

Run as a user, it shows that user's seedbox. Run as root, the host comes first, then
one line per seedbox.

`doctor` is why, from the slow probes: does DNS for the domain point here from outside,
can certificates issue, is the torrent port reachable from outside, is the cluster in
the tested range, is the disk healthy. On swizzin most support pain came from distro
upgrades and an installer that wouldn't start. s5n avoids most of the first; doctor
should be what the installer runs when it can't continue, so a failed start ends in a
reason instead of a stack trace.

## Open questions

- **Apps with no base URL** (Seerr/Overseerr, likely Plex's web app). The chart routes
  them under `/<name>` with a prefix rewrite. That keeps the API answering, which is
  all the e2e checks, but a UI that writes absolute asset paths breaks under a prefix.
  The likely fix is a subdomain for those apps (`seerr.<domain>`), which puts more
  host names in the grant and needs DNS and certificates for each one, or a wildcard.
- `migrate` sources: bare-metal swizzin, Docker, an existing Kubernetes stack. Proposed:
  one command that detects the source, since the hard part (rewriting paths in the Arr
  databases, carrying qBittorrent's state) is the same for all of them.
- Who assigns the torrent port with multiple users (see Scopes).
