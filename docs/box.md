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
  The palette is a theme (`internal/theme`), chosen by the `theme` key in the values file:
  a pair name (`s5n`, the default, or `swizzin`, `catppuccin`, `solarized`, `gruvbox`,
  `nord`, `dracula`) follows the terminal's background, and a variant name (`s5n-light`,
  `catppuccin-mocha`) pins one. An unknown name stops box before it draws anything and
  lists the names. Every variant passes a contrast test. box asks the terminal for its
  background with OSC 11 followed by a device-attributes request every terminal answers,
  so a terminal with no colour to report (PuTTY, screen, mosh) costs one round trip, not
  a timeout; one that answers neither is given a second, and the answer is dark.
  Painting the terminal's own background is a separate opt-in, restored on every exit
  box can catch; `kill -9` or a dropped connection leaves it behind.
  Choosing another library later means rewriting one adapter, not init.
- **The whole run is box (decided).** Progress outside a dialog is not raw text either:
  cluster detection, the platform plan, installs and waits, file writes and the apply all
  go through one reporter (`internal/say`), so a bootstrap reads as one product. The
  plain reporter is the exact transcript a pipe, `--plain` or `TERM=dumb` has always
  printed; the reporter only decides how a phase, a step, a timed wait and a plan look.
  The policy packages keep their wording and their decisions.

## Scopes: root and users (decided, mechanism proposed)

What box acts on depends on who runs it.

| invoked as | acts on |
|---|---|
| root | the host: cluster, platform layer, users, every seedbox |
| a user | that user's seedbox only |

One user gets one seedbox: their own apps, their own downloads and their own config,
isolated from everyone else's. Multi-user stays barebones: no quotas, billing,
panels or other enterprise features.

**A seedbox's namespace is `s5n-<login>` (decided).** Root's own too: login `alice`
lands in `s5n-alice`, beside box's own `s5n-system` and `s5n-gateway`, so a second
user is another `s5n-<login>`. `--namespace` on a first run still wins, and a re-run
keeps the grant's namespace, so a box installed before this rule keeps the
namespace it has. When box derives the namespace from the login (a first run with
no `--namespace`), a login that would collide with box's own namespaces (`system`,
`gateway`) or that does not make a DNS-1123 label after the prefix (length
included) is refused where the login is given. Anywhere else the login is only
the account and is held to the account's own shape, so an existing box's login
keeps working.

**Each user needs a domain (decided).** A bare IP can't be served, because routing and
certificates are per host name.

**Users get a shell (decided).** They need one to run box and to reach their own media.

**Root installs and removes apps; users run their own (decided).** Users can restart,
read logs, update and back up their own apps. Root decides which apps they have.

### Two files per seedbox (decided)

A user's values file can't be applied with root's power. One `storage.data.path`
pointing at someone else's downloads would end the isolation. So each seedbox is split
into what root grants and what the user tunes:

- **Grant**, `/etc/s5n/seedboxes/<user>.yaml`, root-owned: domain, namespace,
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

### Homes, data paths and SSH (decided)

A user's seedbox files and their home directory are different places, because box hands
the grant's paths to kubelet as host paths, and mounting a host path follows symlinks on
the host. A user who owns a directory above their data path can swap the data directory
for a link to someone else's, and their next pod start mounts it.

- **Every directory above a grant path is root-owned and writable only by root.** box
  checks this at `init`, `adduser` and `apply` and stops, naming the directory, when it
  doesn't hold. The leaf itself (`data/`, `config/`) belongs to the seedbox's puid:pgid.
  Root's own seedbox is exempt, since its user can already become root, which keeps
  existing installs with data under `/home/<user>` adoptable.
- **The default is `/srv/s5n/<user>/{data,config}`,** with `/srv/s5n/<user>` owned by
  `root:<user>` and mode 0750, so other users can't list it. It is only the default:
  root can give any paths at `init` or `adduser` (a pool, a union filesystem, a dataset
  per user, config on a separate disk) as long as the rule above holds. Hardlinks only
  matter inside `data`, so data and config may sit on different filesystems.
- **A user's puid is their OS uid.** What they see over SSH is what the apps wrote, owned
  by them.
- **The home directory is an ordinary home:** dotfiles, `~/.ssh`, the values file and the
  user's kubeconfig. `adduser` puts `~/data` and `~/config` symlinks in it, so a login
  lands next to the files. box never reads through those links, and a user who breaks
  them breaks nothing but the shortcut.
- **box never follows a symlink when it walks a seedbox's files on the host** (init's
  report of what a path holds, status, backup), so a link a user plants can't turn a
  root-run walk into a read of someone else's data.

SSH is the host's business, the way mounts are: box never edits sshd's configuration.

- **Password logins work.** `adduser` asks for one password, or generates one and shows
  it once, and writes its hash to both the login (argon2id) and `/etc/shadow`. No copy is
  kept in plain text anywhere. Root's `box chpasswd <user>` sets both again. A user's own
  `box chpasswd` changes the web login only, since their box can't write `/etc/shadow`;
  `passwd` changes the SSH one, as on any Linux host.
- **Keys:** `adduser --ssh-key <file|->` (repeatable) writes `authorized_keys`.
- **When the host's sshd refuses passwords,** `adduser` says so, names the setting
  (`PasswordAuthentication`), and points at `--ssh-key`. It doesn't change the setting.
- **Users join no group with power over the host** (no sudo, no docker, no k0s).
- **box never asks a user for their current password.** Logging in proved who they are,
  and their box acts through their namespace kubeconfig (mode 0600 in their home), not
  through a password.
- **SFTP-only accounts are not part of v1.** They need sshd configuration, which box
  doesn't own; they belong with the enterprise track's SSH routing.

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
  another user's host name. The login is attached to that listener (see Logins).

**The cluster enforces the grant (decided).** box refuses a grant key in values, but a
user holds a kubeconfig for their namespace and can skip box (`kubectl set image`,
`helm upgrade` with their own values, a Service on any nodePort). So root installs a
ValidatingAdmissionPolicy at `bootstrap` and binds it to each user namespace at
`adduser`, with that user's grant as its parameters: only blessed image repositories,
only the granted nodePort and Service type, and no SecurityPolicies or Gateways of
the user's own (see Logins). Pod Security `restricted` still does the
rest. This raises the chart's `kubeVersion` floor to 1.30, where the API is GA.

**Root assigns the torrent port (decided).** It's a port on the node that every seedbox
shares, so it lives in the grant with the Service type. `box init` settles it once: a port
the grant already holds is kept, and so is the one an installed seedbox's peers Service is
already on; otherwise box picks one at random from the lower band of the NodePort range
(30000-30085 for the default range), skipping every nodePort a Service in the cluster
holds. Kubernetes assigns its own nodePorts (a LoadBalancer's, one per listener) from the
rest of the range and reaches into that band only when the rest is full, so a port there
isn't taken out from under the box; a fixed port outside it can be, and then every apply
fails. A band with no free port refuses before any write. A migrate install decides it
too, though it installs with every app off (the plan's qBittorrent is switched on
afterwards): the source's own peer port is kept when it lies in this cluster's NodePort
range and no Service holds it, because trackers and any forward the operator set already
point at it, and one is picked for the band otherwise. The line init prints says which.
The chart ships no default: a fixed number is the collision this decision exists for, so a
helm install must set the key. A box installed while the chart did default it has no port
in either file; `box apply` and `box update` keep the number its release already renders
(31111, the chart's old default) and never pick a new one. A first install with no port
in its grant is refused and names `box init`, which picks one.

## Logins (decided, mechanism proposed)

**One login per seedbox user, checked at the edge (decided).** A person signs in once,
with their account name, and reaches every app that has an admin UI: qBittorrent, the
Arrs, Prowlarr, qui, Tautulli. One auth provider checks the login at the Gateway. It is part of the
platform layer: root's `bootstrap` installs it, and `box adduser` adds the user to it.
Each user's domain admits only that user. Behind it the apps have no login of their own
(the Arrs run with `External` authentication, Tautulli with its own switched off,
PeerBanHelper with its login switched off, qBittorrent with its password bypassed for
private addresses) and are reachable only through the Gateway.

**qBittorrent skips its password for private addresses (decided).** qBittorrent can't
switch its login off, so it bypasses it for the private ranges
(`AuthSubnetWhitelist`: 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10,
fc00::/7, the same list the release's egress policy treats as internal) and for
loopback (`kubectl port-forward`). Everything that reaches it is then one of two
callers: Envoy, which forwards only requests the edge's login has already admitted
(qBittorrent's paths have no bypass at the edge, unlike an Arr's `/api`), or an app
the release's NetworkPolicy lets in. That is the same boundary the Arrs sit behind
with `External`. The apps that call it (the Arrs, qui, autobrr, PeerBanHelper, the
wiring Job) connect with an empty username and password, so no app's database holds
a copy of a qBittorrent credential. The init container still seals a generated
password into `qBittorrent.conf`, so qBittorrent never mints a temporary one and
prints it to its log, but nothing is given it. `box init` no longer asks for a
qBittorrent login name; `apps.qbittorrent.webuiUsername` keeps its `admin` default only
so that existing values still validate.

**Who the login is (proposed).** The web login and the account that owns the files are
different things, so `init` offers one and shows the other. The default is the name of
the OS account behind puid, read from the host's own account database, and only then
the account of whoever ran the command; the answer is editable, and box couples
nothing else to it. Guided `init` prints both in its record, `files  core  (uid 1000,
gid 1000)` and `login name  liara  (web login)`, so a box where the two differ says
so instead of looking like a box where they agree. Neither account is created by box.

**One auth mechanism, no fallback (decided).** Wiring a login into each app separately
would be a second auth system with its own reset path, tests and docs, so box doesn't
build one. That is why Envoy Gateway is the only Gateway implementation box supports.

**The password is never stored where it can be read back (decided).** The provider keeps
a hash and nothing else does. A forgotten password is reset, not recovered. `init` (and
`adduser`) asks for one; an empty answer means box generates a random one and shows it
once, at the end of the run, the way swizzin always has. Credentials
between apps (API keys, the qui and autobrr accounts the wiring Job signs in with) are
generated, kept in the release Secret, and never the person's password.

**Plex and Seerr sign in through Plex (decided).** Players talk to Plex
directly, and Seerr's users are the Plex server's friends, so these two stay outside
the provider. Skipping the Plex claim at `init` stays allowed, with a warning that
until the server is claimed, whoever signs in to Seerr first becomes its owner. A
migrated server that is already claimed needs no warning. Seerr's first admin is that
one interactive sign-in and nothing in a release can create it, so the wiring Job's
receipt asks for it only where there is none: it reads Seerr's own `initialized`
flag, prints no row on a box whose Seerr already has its admin (a migrated one does,
from the settings its snapshot carried), and names `seerr.<domain>` — never a
`/seerr` path — as the address to sign in at.

**Jellyfin keeps its own accounts, and box makes the one a person has to know
(decided).** Its players authenticate against the server with its own tokens, so it
is served at `jellyfin.<domain>` and that host bypasses the login, like Plex's. Its
users, its API keys and its libraries are rows in `data/jellyfin.db`, and its first-run
wizard is an API, so nothing can be sealed into a file before it starts. The chart
closes that wizard inside the pod: an init container runs the app's own image
against the same `/config`, bound to localhost only, waits for the server it
started, names the person's account (the box login's name) with the generated
password, completes the wizard, stops that server with `SIGTERM` and waits for it so
the database and its WAL are flushed, then exits. A pod is not Ready while an init
container runs, so the Service has no endpoint and the Gateway has nowhere to send a
request: there is no moment a server answers the outside with the wizard open. The
wiring Job then keeps an automation account of its own for every later run and gives
each Arr instance's library directory a library.

The pod's second init container owns `network.xml`'s bind address, and runs after the
wizard on *every* start. The wizard leaves `LocalNetworkAddresses=127.0.0.1` behind --
that is what keeps the server it starts off the pod network -- and a restore in the
wizard could not be trusted with it, because a trap does not run on `SIGKILL` (an OOM
kill, an eviction, a reboot, a pod deleted past its grace period), and the start after
that one never runs the wizard again: `system.xml` says the wizard is complete, so
nothing would put the file back and the app would bind loopback, unreachable and never
Ready, for good. So the second container writes that element empty (every interface,
the only value that leaves the app reachable from outside its pod; the NetworkPolicy is
the gate) and edits nothing else, on every start. It is also the container that clears
a proxy list the values no longer justify, and it runs whether or not the box names
proxies at all.

The person's password is the one app credential box shows, once, on the run that
made it: the chart generates it into the release Secret and box reads it back after
that apply, so no values file and no later run carries it. The release Secret is its
only copy, so a person who changes it in the app breaks nothing and a re-run never
resets it. A migrated server keeps the accounts it had -- the database rewriter
renames nothing -- and box's automation account is added beside them; when the
source's own administrator password is not one box holds, the receipt names the
accounts it found rather than renaming or resetting one. When box holds no password
that opens the server at all -- the normal state of a server it did not create -- the
receipt says so as one `pending` login line naming the account to give the Secret's
password to, the steps that need a session are `skipped` with the same sentence, and
the Job carries on to every other app: nothing about a Jellyfin password is a reason
to leave a box's Arrs unwired. A server that is unreachable or answers 5xx is still a
failed step.

That same host has to be told whose forwarded address to trust (`KnownProxies`): with
the list empty Jellyfin ignores `X-Forwarded-For` and every request through the
Gateway counts as local, and a local caller reaches the endpoints Jellyfin answers
without a credential. `init` reads the cluster's pod network from its Nodes'
`podCIDR`s and records it in the grant, and the chart writes it into `network.xml`
before every app start -- the app builds its forwarded-header handling once, at
startup, so a value written through the API afterwards would not take -- so the range
is a fact about the cluster rather than a guess in a file. It is the same container
that owns the bind address, and it writes the list even when the box names none: the
app saves the file whole, so an earlier values file's list would otherwise outlive it.

Seerr stays on Plex: nothing in this release wires a Jellyfin server to it, so a box
that runs both has one media server per requester rather than a shared library.
Hardware transcoding is not here either; Jellyfin transcodes in software until a GPU
is a platform feature Plex and Jellyfin share (PLAN.md, "Catalog").

Tautulli used to be the third, with its own Plex-based login on top of the app. That
login is switched off behind the box's, and box seals it off on every start: a
non-empty `http_password` is the switch that makes Tautulli ask for a password
(plexpy/webstart.py at v2.18.2), so the app's init container clears the pair and the
two flags that hash it, and `migrate` writes the same into a source's file and says
so in its confirm. 2.19 put two more switches in front of a password that is empty,
and box seals both: without `first_run_complete` the app mints itself a random
password on a start where the setup wizard never finished
(plexpy/__init__.py:3121-3145 at v2.19.1), and without
`allow_public_no_password` it refuses every client outside the local network and
every request a reverse proxy forwarded (webstart.py:406-441) — which is every
request the Gateway makes, because Envoy sends `X-Forwarded-For`. It is served under
`/tautulli`, and its own URL root is sealed to
that prefix: the redirects it builds are absolute (`/` to `/home`, a refused request
to `auth/logout`), so an app that does not know its prefix leaves it -- and lands on
the box's own `/auth`.

**A plain `helm install` must say who checks the login (decided).** The chart requires an
explicit `auth.edge: true`, with no default, and refuses to render without it. box sets
it; an operator without box sets it to say they put a login in front themselves, the
way an ingress with basic auth does. The chart never wires per-app logins.

**The provider is Authelia (decided).** Users live in a file with argon2id hashes, and
Envoy's ext_authz asks Authelia about every request. Apps that speak OIDC (qui) use its
OIDC provider. box installs it from its own small manifests, not the upstream chart, so
that it runs as a non-root user under Pod Security `restricted`. Authelia requires a
storage backend (SQLite, with its encryption key), so the users file and the database sit
together on a claim box creates, never in a Secret, because Authelia can't watch a file
in a Secret volume. That means exactly one replica, with file watching on, so a password
change shows up without a restart. Its sign-in page is served at `/auth` on each
seedbox's own host name, so a seedbox needs one certificate and one cookie domain.
A request that is not a navigation is refused rather than redirected: ext-authz answers
401 when the request accepts JSON or marks itself as an XHR (`X-Requested-With`), and
the portal's 302 otherwise, so an app whose session has ended fails its own background
call instead of handing a page the login document. A request that accepts anything
(`Accept: */*`, which a `fetch` with no header of its own sends) is read as a
navigation and gets the 302, which is the one shape nothing at the edge can tell apart.

**Apps that speak OIDC sign in through the login (decided).** Authelia is also an OIDC
provider, with issuer `https://<host>/auth`; an app the chart's catalog marks `oidc: true`
(qui, autobrr and FileBrowser Quantum) has its own login switched to it, so a user still
signs in once.
autobrr's sign-in page sends the browser to the provider itself (its own automatic OIDC
login, new in 1.91); qui at v1.36.1 offers the provider as the one button its sign-in
page has and has no automatic redirect, so that first visit is one click. FileBrowser
Quantum is told its whole login in the `config.yaml` the chart writes -- its client id and
the issuer, with the secret in its environment because it has no file spelling for one --
and it refuses to start when the provider will not answer, so its Ready is that proof as
well; qui and autobrr only discover the provider when a page or a request asks, so for
them only a sign-in does. Each seedbox has its own client per
app, `<namespace>-<app>`, and its own authorization policy that admits only that seedbox's
user. root's `init` writes the client secret once into a Secret in the seedbox's
namespace and only its digest into the login, and so does root's `apply` when it enables
an OIDC app whose client doesn't exist yet: the plan counts a missing client as login
work. A user's `apply` can't write the login, so when the release would mount a client
Secret that isn't there it stops before the confirm and names `box init` as root. The
login serves OIDC exactly when its configuration names a client: the same render decides
the provider block and the Deployment's mount of the signing material, because Authelia
exits on a provider with no clients. The provider's signing key is generated
once and kept beside the login's other key, outside its ConfigMap. The app reaches the
issuer from its own pod over the box's own HTTPS name: the chart aliases that name to the
Gateway's proxy Service (an address root's `init` records in the grant whenever the
chart's schema declares the key -- not only when an app that uses it is on, because a
migrate install answers `init` with the plan's apps off and switches them on afterwards),
opens egress to the proxy alone, and trusts the box's root where the certificate is
self-signed. Only the
Gateway's proxy may reach the login's pod at all. A plain `helm install`, or a grant from
before the provider, has no recorded address, and the app keeps its own login switched
off behind the edge's.

**The login gives an account no group, so no app can make it its admin (open).** An app
that grants its own admin by a group claim -- FileBrowser Quantum is the one in the
catalog -- gets nothing to match, because box writes each login's row with a name and a
digest and no `groups:`, and a client may not ask for the `groups` scope. Such an app is
served to the provider's one account as a full-permission user of `/data`, and its own
settings stay the chart's, which is the honest split while the chart owns its config file.
Making the owner an app's admin is a login change -- a group on the row, `groups` in the
client's scopes and a client's `adminGroup` -- and it raises `min-box`, because an older
box would write neither.

**An app that keeps its own user table has one account box owns (decided).** qui and autobrr hand out API keys only to a signed-in session, so the wiring Job
can't be given a key up front. The chart generates a password for each in the release's
Secret, beside the qBittorrent password and the Arr keys, and box owns the account
outright: the app's init container creates it if it is missing and sets that password on
every start, so a password changed inside the app is put back at the next restart, and
until then the wiring Job's report names the mismatch. The Job signs in with it on every
run; no API key is minted or kept. With that session it registers what the box holds: qui's
one qBittorrent instance and one Arr instance per Sonarr and Radarr, each adopted rather
than duplicated when the box already has a row for it, and each proved by the app's own
connection test. This account's password login is the one the Job uses,
so it stays routable: since autobrr 1.91 switching its built-in login off does not hide
the form, it makes `POST /api/auth/login` answer 403
(`internal/http/auth.go:122-126` at v1.91.1), and the Job then has no way in at all. For
autobrr the form is not offered because the app sends the login page to the provider
itself (its own automatic OIDC login), and the account is reachable only past the edge's
login or from the wiring Job, which the NetworkPolicy allows. The account is named `s5n`
in both apps; it is the wiring Job's, not another way in for the person, whose way in is
still the provider.
PeerBanHelper's token is that app's version of the same trade. It is generated once in
the release Secret and handed to the app through its environment, and it is load-bearing
twice over even though nobody signs in with it: PBH reads it as its own `pbh.apiToken`,
and a blank one sends the app into its first-run wizard instead of serving the API (and
while it is uninitialised, `/api/oobe/*` is open). It is not a check the app makes: with
`pbh.web.requireLogin=false` the switch is off before PBH reads a token at all, so a
request carrying no token -- or the wrong one -- is let through, and nothing would
notice the Secret and the app disagreeing. What keeps PBH's API from anyone but this
seedbox's user is the edge in front and the NetworkPolicy, not the token; the wiring
Job is the only client that carries it. Readiness depends on the same switch: the probe
asks the app's API, which answers without a credential only because the login is off.

A browser that was ever given PBH's token keeps it in `localStorage` (a `?token=` link,
or the form), along with the API endpoint it was pointed at: a stale or bogus endpoint
(a port on `127.0.0.1`, say) makes the page ask for one in a popup and answer
"unable to parse", which is the same stored state and the same cure -- neither is
something the box can fix from the server. And PBH counts every failed login against
the caller's network: three per
page load trip its 15-minute lockout, after which every request answers 429 and a
working box looks like a routing fault. The cure is the browser's own: clear the site's
data for the app's host (or open it in a private window). The token the form asks for is
the release Secret's `pbh-token`, which is not something a person should have to
re-enter.

**The login is on root's Gateway, and users can't take it off (decided).** The
SecurityPolicy that sends requests to Authelia targets the `s5n` Gateway itself and lives
beside it in `s5n-gateway`, so every listener is behind the login unless root says
otherwise: each host's HTTPS listener, the shared port-80 listener, and any listener added
later. Every route attached to them is covered, including any route the user adds later.
Envoy Gateway lets a policy on a route override the Gateway's, so the grant denies users
any write to SecurityPolicies, and to Gateways so a user can't open a listener of their
own. Users still create their own HTTPRoutes, so the chart is unaffected. Exemptions live
in Authelia's own rules, which root owns: paths that check their own key (the Arrs' `/api`,
and `/ping` for health checks), Plex and Seerr (each at a name of its own), and the
sign-in page itself.

**The sign-in path is root's (decided).** Authelia lets everything under `/auth` through
without a login, and Gateway API sends a request to the most specific route, so a user's
route under `/auth` on their own host would be served without one. The grant refuses a
user's HTTPRoute that matches `/auth` or anything under it. It also refuses regular
expression and percent-encoded path matches in a user's routes, because those can reach
`/auth` without spelling it. The chart uses neither.

One listener is left out on purpose: each host's own port-80 listener gets a root-owned
policy with no login, written with the listener before box asks for the certificate. Only
root's redirect to HTTPS and cert-manager's challenge route can attach there, so nothing
opens. It has to be this way: Authelia refuses any plain-HTTP request before it reads its
rules, so a challenge behind the login would never pass, and a plain `http://` link would
fail at the login instead of redirecting. Each user's domain admits only that user.

**Sessions live in Authelia's memory (decided).** Authelia keeps sessions in memory unless
it has Redis, and box doesn't run one. Adding a user changes Authelia's rules, which means
a restart, so `adduser` signs every user out. A login expires after an hour idle; "remember
me" keeps it for 30 days.

**A password changes only through `box chpasswd` (decided).** Root can set any user's
password, and a user can set their own from their own box, in their own scope. The
provider offers no self-service change and no reset, because Authelia allows those only
after an emailed one-time code, and box asks for no mail server. A user who has
forgotten their password asks root. Authelia still needs a notifier configured to start,
so box gives it the filesystem one, which sends nothing.

Proposed:

- A user's `box chpasswd` can't reach the users file: their box holds a kubeconfig for
  their own namespace and nothing else. So the user's box hashes the password locally and
  leaves a request in the user's own namespace. A root-owned writer beside Authelia picks
  it up, maps the namespace to its login name from root's record, rewrites that one row
  and deletes the request. Root's `chpasswd` takes the same path. With self-service off,
  Authelia never writes the file itself, so the users file has one writer, and the
  password never leaves the machine it was typed on.
- Unattended, a password comes from a file or stdin, never a flag (it would sit in shell
  history and `ps`), and none supplied means a generated one, printed once.

**The sign-in page carries the s5n mark, in root's theme (decided).** Authelia can be
themed only through its `theme:` and `server.asset_path`, so box draws `logo.png` and
`favicon.ico` from root's `theme:` and puts them beside the login's configuration. A theme
pair (`s5n`, the default) gets Authelia's dark page and the pair's dark palette, so the
mark keeps its real colours; a pinned light variant gets the light page. There is no
custom CSS, and the sign-in page's Content-Security-Policy is left as Authelia ships it.
Root's `apply` re-renders the login whenever its configuration or pictures differ, so a
theme change shows up at once.

## Commands

Status per command: **D** decided, **P** proposed.

| command | who | does | |
|---|---|---|---|
| `box bootstrap` | root | prepares the host: finds or installs the cluster, the platform layer, directories; then runs `init` for root | D |
| `box init` | root, user | sets up a seedbox: domain, paths, apps, existing data, Plex claim | D |
| `box adduser <name>` | root | creates the OS user and shell, the grant, and the namespace pieces above; then runs `init` as that user | D |
| `box chpasswd [name]` | root, user | sets a login password: root for any user, a user for their own; asked for, or generated and shown once | D |
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
| `box migrate` | root | adopts an install box didn't make: `discover` reads and plans, `cutover` takes the final snapshot, `apply` installs or adopts from it | D |
| `box uninstall` | root | removes box from the server | D |

`--user <name>` lets root run any user command against that user's seedbox.

### bootstrap and init (decided)

They are different jobs. `bootstrap` sets up the machine once. `init` sets up one
seedbox: once for root, once per user through `adduser`. Every step of both is safe to
re-run. The cluster decision is in `PLAN.md`.

**The cluster, when there isn't one (decided).** On a clean host, bootstrap is the
installer. It runs only as root, only after the decision found no usable cluster, and it
refuses if anything named k0s is already there — the binary, a k0s state directory with
something in it, its unit, or something listening on 6443 — because that k0s is not
box's to replace. The release and the sha256 of its asset for each architecture (amd64,
arm64) are pinned together in the chart's `platform.yaml`, never read from k0s's release:
a checksum served beside the archive proves only that both came from the same place, and
what box writes runs as root. Box can't carry them, because it can't know the sum of a
release newer than itself. The sums are written in the pull request that moves the pin:
`.github/workflows/k0s-sums.yaml` runs `go run ./hack/k0s-sums --write` on Renovate's
branch (it refuses when the asset and k0s's published sum disagree) and commits the two
lines as the sums bot — a GitHub App whose token, not `GITHUB_TOKEN`, pushes them, because
a `GITHUB_TOKEN` push starts no workflow and Renovate, which has no post-upgrade hook,
would wait on a head CI never saw. The bot's email is in `gitIgnoredAuthors`, so Renovate
still commits to its own branch. CI checks the sums against the published assets, and a
k0s bump still never merges itself: no CI job boots k0s (the box e2e runs on kind), so
a green run has not tested the new release, and the maintainer merges it.
The download goes to a temporary file beside `/usr/local/bin/k0s` and is hashed while it
is written; only a match is renamed into place, so an interrupted or tampered download
never lands where the unit would exec it. Then `k0s install controller --single
--config /etc/k0s/k0s.yaml --kubelet-extra-args=--node-ip=<uplink address>`, `k0s
start`, and box waits until the API answers `/readyz` *and* a node is Ready through
`/var/lib/k0s/pki/admin.conf`: readyz turns true a moment before the node registers, and
the decision that follows refuses a cluster that lists no nodes. Only then is the cluster
probed for its distribution and platform layer and recorded — a cluster box started but
cannot reach is never written down as one box manages. Any failure after the binary is in
place says exactly what is left on the host and the command that removes it
(`k0s stop; k0s reset`), then stops. box never runs that reset itself: it would also
destroy a k0s installation box did not make. With the cluster up, bootstrap goes on to the
platform layer and `init` exactly as it does on a host that already had one.

**The node's address is box's choice (decided).** kube-router's automatic MTU is the
MTU of the interface that holds the node's primary address, not the smallest on the
host. Left to itself, kubelet takes the address the node's name resolves to, and k0s
takes the first non-loopback IPv4 in interface order for the API, so a host already
running a tailnet or a VPN can register the node on that tunnel. Every pod then gets the
tunnel's MTU (1280 for Tailscale), and anything tunnelled from a pod fragments every
packet. So box pins the address when it installs k0s: the source address the kernel
would use on the default route, given to kubelet as `--node-ip` and written as
`spec.api.address` in a minimal `/etc/k0s/k0s.yaml`. box never sets an MTU; with the
node on the uplink, the automatic one is right for ethernet, PPPoE (which counts as an
uplink, not a tunnel) and whatever a cloud gives.
- **A default route through a tunnel stops the install,** naming the interface and the
  MTU every pod would inherit. There is no override.
- **A `/etc/k0s/k0s.yaml` box didn't write is never overwritten.** It is box's own
  file, and it carries the two things box cannot leave to k0s: the API address, and the
  cluster's pod and service networks (the host's firewall rule reads those back out of
  it rather than holding a second copy).
- **Once the API answers, the Node's address is checked against the pin;** a mismatch
  fails like any other step after the binary is in place, naming both addresses.
- **On a cluster box didn't install,** a node address on a tunnel this host holds is one
  warning line. box doesn't change that cluster.
- **Only a real host proves it:** kind never boots k0s, so the e2e can't.

**Only a cluster box installed gets its host firewall opened (proposed).** A
Fedora-family host runs firewalld, and its default zone ends the FORWARD chain with a
reject. That is exactly enough for the traffic kube-proxy DNATs — pod to Service, and a
Service's traffic from outside — so the apps, the wiring Job and the lock proof all run;
what it refuses is the traffic kube-router routes pod to pod through the host, which is
Envoy dialling an app's own pod address, refused at once, and every request behind the
login answering 403. On a host where box installs k0s, box writes the cluster's pod and
service networks into the k0s config it installs with (`spec.network`) and adds them to
firewalld's trusted zone, permanently and in the running configuration, asking first so a
source that is already there is left alone; the plan says so before the confirm and the
run says so when it is done. It does not reload the host's firewall: a reload re-reads
every zone from the permanent configuration and would drop runtime-only changes another
tool made. On a cluster box did not install, box knows neither network and changes
nothing: the plan carries a warning, printed before the confirm, with the two commands
k0s's own documentation names (`firewall-cmd --permanent --zone=trusted
--add-source=<podCIDR>` and the same for `<serviceCIDR>`), and the operator runs them.
Nothing else is opened. The Gateway's address and the torrent Service are DNAT'd like any
other Service, so they pass; a single-node k0s box needs no port opened for its API, its
kubelet or its etcd, because box and the kubelet reach them over loopback, which
firewalld's trusted zone already admits. Only a real host proves any of this — kind has
no firewalld — so the unit tests pin the command sequence, and a VM rehearsal is the
proof.

**Only a cluster that enforces NetworkPolicy gets apps (proposed).** The Arrs run
External auth and qBittorrent skips its login for private addresses, so the chart's
NetworkPolicies are the only thing between another pod on the cluster and an app. A
NetworkPolicy is enforced by the CNI, not the API server: k0s's kube-router and kind's
kindnet enforce one, and a cluster on plain flannel accepts every policy object and
ignores it. So box proves it, in the seedbox's namespace, before the first app exists:
two pods listen on a port, a client pod dials the one no policy selects and must reach
it, and a second client dials the one a deny-all ingress policy selects and must not.
The control comes first, because a network that cannot carry the traffic at all must not
be reported as a cluster that ignores policy — that is its own refusal, and it says the
pods could not reach each other. A client that gets through the deny-all policy stops
the run before any app is installed, naming the plugin box recognised and the ones that
do enforce policies (Calico, Cilium, kube-router). The probe pods run the release's own
image, pass Pod Security `restricted` like any app pod, and the pods and the policy are
removed on every path, a cancelled run included. The proof costs a few pods, so it is
made once per cluster: a cluster box installed itself (k0s, which brings kube-router)
never needs it, and a passed proof is recorded in `/etc/s5n/cluster.yaml`, so no apply
repeats it. A cluster with no record is proved by every apply, because apply doesn't
choose a cluster and so writes no record that would let the next run skip the proof.
A chart whose s5n image predates the proof's listener cannot be asked at all: its
`probe listen` prints the usage and exits, which box reads from the container's log and
reports as the chart being older than box, not as the cluster ignoring policy. That
apply warns, records nothing and installs, and the first chart whose image has the
listener proves the cluster.
This is the ingress half of the same check `migrate` makes for a rehearsal's egress
lock; kind enforces policies, so the e2e exercises the passing path and the refusals are
unit tests.

**A record is checked, not trusted (decided).** A re-run reads
`/etc/s5n/cluster.yaml` for what box decided — which cluster, who owns its
lifecycle, the platform modes, the GatewayClass — then probes that cluster again:
`/version`, the privilege checks, the node count and distribution, and the platform
layer. Those are the same probes and the same refusals a freshly found cluster gets,
so a recorded cluster that has since gone multi-node or lost its class stops before
anything is written. A record whose kubeconfig no longer answers stops with that path
named, and never falls through to installing k0s over it. An explicit `--kubeconfig`
or `KUBECONFIG` naming the recorded path is that same re-run, with the record's
decisions kept and nothing rewritten; naming a different path stops before any probe
and names both files, the record's source and the record file, because moving the host
to another cluster is the operator's decision (unset `KUBECONFIG`, drop
`--kubeconfig`, or remove the record deliberately). The recorded class is kept
while the cluster still serves it (Accepted, and publishing the feature the routes
need); what a re-run repairs instead is box's own missing implementation, which is
what a first run whose platform apply failed leaves behind. On a fresh host, a
kubeconfig at a distribution's own path that exists but does not answer blocks the
install as a broken cluster; an unusable `~/.kube/config` or a dry run's
remote-cluster skip is only a skip. So a second bootstrap
plans and installs whatever the probe says is missing — Envoy Gateway, the Gateway API
CRDs, MetalLB and its pool, the admission policy — instead of reporting components it
never looked at as unknown.

**box's host files are `/etc/s5n` (decided).** The record, root's config and
`seedboxes/` live beneath `/etc/s5n`, not `/etc/swizzin`: swizzin classic installs
itself as a git checkout there, so writing into it meant writing into another
program's repository, and `rm -rf /etc/swizzin` — how swizzin is retired — would have
taken box's records with it. No command upgrades a 0.4 host: one that still has
box's files under `/etc/swizzin` and nothing under `/etc/s5n` refuses before it reads
a record or writes anything, and prints the `mkdir`/`mv` to check and run. A file
both roots have is read from `/etc/s5n`, and the stale copy is named once.

**What bootstrap installs (decided).** It applies only the platform components the
decision says are missing: Envoy Gateway from its OCI chart (the chart ships the Gateway
API CRDs at the channel the controller is built against), MetalLB in L2 mode with a
single `/32` pool at the node's primary address, the `GatewayClass` and `Gateway` the
chart's HTTPRoutes attach to, and the admission policy that enforces the grant. Charts
are fetched at run time at the versions the chart release pins (see "The platform
layer"), so bootstrap pulls the release before it installs anything; the manifests box
owns are compiled in. The plan names every release, version and namespace and asks
before the first write (`--yes`, or no terminal, is unattended). A release already at its pinned
version is verified rather than upgraded, so a second bootstrap creates no Helm
revision; the objects box applies are server-side applies under field manager `box`, so
re-applying them changes nothing. cert-manager is installed the same way, with the
issuers box owns (see "Certificates").

**MetalLB is installed unless the Gateway gets an address without it (decided).** In
s5n, MetalLB has one job: giving the Gateway's Service the node address (the torrent port
is a NodePort and needs no LoadBalancer). So box asks the Gateway, not a generic
LoadBalancer Service. In auto mode the plan shows MetalLB as conditional. After the
confirm, box installs Envoy Gateway if needed, applies the Gateway and gives it 15
seconds to report an address. If by then it has none and says the address was not
assigned, box installs MetalLB and its pool and waits again, up to 60 seconds. Anything
else at 15 seconds (an address with Envoy's proxy still rolling out, another reason) is
still settling and gets the rest of a 60-second wait before box judges it; any reason
other than an unassigned address at that deadline stops bootstrap. External mode gets
the whole 60 seconds at once, since there is no MetalLB to fall back to. Either way, box then sends an HTTP request from the host to the Gateway's
address on port 80. Any HTTP answer passes; silence stops bootstrap with the address
named, and box never installs MetalLB over an address something else handed out. In
external mode MetalLB is never installed and the Gateway must pass the same checks.
Managed mode installs MetalLB without waiting to see, and so does auto mode on a cluster
box has just installed: k0s hands out no addresses, so the question could only answer no.
The record keeps auto, so later runs ask the Gateway as usual.

**Envoy Gateway is the only Gateway implementation (decided).** box checks logins at the
edge (see "Logins"), and Gateway API has no standard auth filter yet, so box supports one
implementation rather than one auth mechanism per implementation. A cluster with no
GatewayClass gets box's own Envoy Gateway. An existing Envoy Gateway (a GatewayClass
accepted by `gateway.envoyproxy.io/gatewayclass-controller`) is used: box creates its own
Gateway on that class, the chart's routes attach to it, and the Gateway checks above
(address, HTTP answer) apply unchanged. With several Envoy classes, guided bootstrap lets
the operator pick and unattended bootstrap stops naming `platform.gateway.className`. Any
other implementation stops bootstrap with the class and its controller named, and box
never installs Envoy Gateway beside it: both would want the same address and ports.
GatewayClasses that no controller has accepted stop bootstrap with each class's reason.
On k3s this means Traefik's Gateway provider is not used; bootstrap stops and names k3s's
`--disable=traefik`. If Gateway API's ExternalAuth filter (GEP-1494) reaches the standard
channel, this can widen to every implementation that supports it, still with one auth
mechanism. The NetworkPolicy lets in traffic from wherever the Envoy proxy runs, not from
a namespace box assumes.

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
- **Who owns the files (proposed).** Neither `bootstrap` nor `init` creates an OS
  account: the files belong to an account that already exists, and `adduser` is the
  only command that makes one. puid:pgid are settled after the data path is known, in
  this order: `--puid`/`--pgid`; the grant's values on a re-run (an installed box keeps
  its owner, whatever a directory now says); the owner of the data path when it already
  holds files, because an install that changed the owner of an existing library would
  leave the apps unable to write it; `$SUDO_UID`/`$SUDO_GID`; the invoking user. Root is
  never the answer: a uid or gid of 0 stops the run, with the flags named when
  unattended and a question for the owner on a terminal. A data path that is itself a
  symlink is not read through (above, "Homes, data paths and SSH"), so the account on
  the other side of the link is never taken for the box's: box says so and falls back.

### Certificates (decided)

PLAN settles the goal: cert-manager, HTTP-01 through the Gateway when the domain can be
validated, self-signed otherwise. This is how box would do it.

- **What bootstrap installs.** cert-manager from its OCI chart at a pinned version, with
  its CRDs and Gateway API support switched on (`config.gatewayAPI.enabled`), in
  `cert-manager`. An existing cert-manager is used when its webhook answers, its version
  has the Gateway HTTP-01 solver, and its controller runs with Gateway API enabled. box
  reads that from the controller's config and never changes another install's flags. One
  that is present but has it switched off stops bootstrap and names the setting.
- **Issuers are root's.** Two ClusterIssuers hosts use: `s5n-letsencrypt` (ACME, HTTP-01
  solver whose HTTPRoute names the `s5n` Gateway but no listener, so it attaches to
  every listener that admits `s5n-gateway`) and
  `s5n-selfsigned`. No ACME email: Let's Encrypt no longer sends expiry mail, so there is
  nothing to ask. `s5n-selfsigned` is a CA issuer, not a bare self-signed one: box keeps
  one long-lived root certificate, `s5n-root-ca` in `cert-manager` (ten years, renewed a
  year early, its key never rotated), signed by a third issuer, `s5n-bootstrap`, that
  signs nothing else. Bootstrap waits for the root to be Ready before it writes
  `s5n-selfsigned`. A browser still warns, as for any self-signed certificate, but the
  root doesn't change when a host's certificate renews, so something inside the cluster
  can trust it once. A box from before the root (its host certificates their own CA) is
  repaired by the next bootstrap or init: each self-signed host certificate not signed
  by the root is re-issued from it, because cert-manager re-issues nothing when only an
  issuer's spec changes.
- **Apps that call the login themselves trust the box's root.** An app that signs in
  through the login's OIDC provider fetches the provider's documents from its own pod,
  over HTTPS to the box's own host name. With a Let's Encrypt certificate the image's
  public roots are enough. Otherwise root's `init` copies the root into ConfigMap
  `s5n-host-ca` in the seedbox's namespace, and the chart adds it to those apps' trust
  store (`SSL_CERT_DIR`, which adds a directory and keeps the image's bundle). The copy
  follows the issuer the host's certificate is actually issued by, never the name's DNS
  answer, so a record added before the next `init` leaves both where they are. When the
  host moves to Let's Encrypt the copy is removed. `apply` never touches it.
- **Certificates live beside the Gateway.** A listener can only reference a Secret in
  its own namespace without a ReferenceGrant, so each host name gets a `Certificate` in
  `s5n-gateway`, plus an HTTPS listener on 443 and its own host-matched port-80 listener
  on the `s5n` Gateway, all written by root
  from the grant's host name. Users never write listeners or Certificates. That keeps
  issuance in root's hands, the way the grant keeps ports and images there.
- **HTTP stays open for the challenge only.** Before it asks for a host's certificate,
  box gives the host its own port-80 listener. It admits only `s5n-gateway`, so no app
  route can attach there, and Gateway API's listener isolation sends the host's port-80
  requests to it and no other. cert-manager's solver route attaches to it and answers the
  challenge. Once the certificate is Ready, box adds the HTTPS listener on 443 and a
  redirect route to HTTPS on the port-80 one; the solver's own route for
  `/.well-known/acme-challenge/` is an exact match and keeps winning, so renewals work.
  A host is never served over plain HTTP: until its certificate is Ready it serves only
  the challenge, and box says so.
- **Which issuer a host gets.** Each user has a domain, but not every domain is publicly
  resolvable (`box.lan`, a split-horizon name). init resolves the name. A
  public address picks Let's Encrypt; a private address, or no answer, picks
  self-signed, and init says which and why. It doesn't compare the address with the
  node's, because a box behind NAT has a private node address and a public name that
  still validates. `tls.issuer` in root's values forces either one. A name with no
  answer is usually a record that doesn't exist yet, so init doesn't stop on it (a test
  box with a made-up name still installs), but its final card says the name has no DNS
  record, that the certificate is self-signed because of it, and that running `box init`
  again once the record exists moves the host to Let's Encrypt. That works because init
  resolves the name on every run and keeps no earlier answer; the e2e proves the move
  from a self-signed certificate to an issued one on the same host.
- **Apps with no base URL get a host of their own.** An app whose UI writes absolute
  asset paths (Seerr, Plex's web app, PeerBanHelper) breaks under a `/<name>` prefix, so it is served
  at `<app>.<domain>` instead, with its own listener and certificate the same way as the
  box's domain. The operator provides the names: one wildcard record (`*.<domain>`) or
  one record per app, whichever their DNS allows. box doesn't manage DNS and holds no
  DNS-provider credentials, so a wildcard *certificate*, which needs DNS-01, is not
  part of this. Before box writes anything for an app host, it checks that the name
  resolves to the same addresses as the box's domain, and stops naming the record to
  add when it doesn't, rather than leaving a certificate that can never issue. When
  neither the domain nor the app name answers (a test box), that counts as a match: the
  app host is self-signed like the domain, and the final card names its record too.
  Which apps get a host is the catalog's `exposure: host`, owned by the chart.
- **An app host is guarded the way its path was.** Each one has its own access-control
  rule carrying the posture the app had on its path. The one session cookie is set for
  the box's domain, and the browser sends it to every `<app>.<domain>`.
- **An app host is three objects: its login rule, its listener and its Certificate.**
  One per-host step writes all three, for `init` and `apply` alike, and a host counts as
  served only when all three exist: without the rule the login's default deny answers
  403. The login's configuration is rendered whole from the same inputs `init` uses and
  applied only when it differs, so a rule never depends on which command wrote it.
  Adding a rule rolls the login, and its sessions live in memory, so everyone signs in
  again.
- **A first install serves its hosts before the release (decided).** `init` decides each
  host's issuer once, then, after the confirm and before the release moves, installs the
  login, writes the seedbox namespace's own objects and runs the per-host step for every
  host it decided, so an app that calls the box's own HTTPS name at startup (an OIDC
  client) finds it there. A host whose certificate is not the one this run decided (an
  issuer that moved once the name started answering, or a self-signed one not on the box
  root) counts as work like a missing one; a box where everything is right still says
  there is nothing to change.
- **No run leaves an app host unserved (decided).** When root's `apply` would serve an
  enabled app on a host that is missing any of the three, it runs the per-host step after
  the confirm and before the release moves, DNS check included; a mismatch stops with the
  record named and the release untouched. When that's the only work, no new release
  revision is made. A user's run that would need it stops and names root's command,
  because a user can't write a listener or the login. `update` will call the same step.
- **A hosted instance's name is a value (decided).** It is `<instance>.<domain>` unless the
  instance sets `host:` (a label, or a full name under the domain): `seerr` can be
  `requests.<domain>`. The chart and box refuse a name two instances share, the box's own
  name and a name outside the domain, which the login's session cookie does not cover.
- **A name no longer served is removed (decided).** After a release that succeeded, root's
  run drops the listener, redirect, policy and Certificate of every name box's own managers
  wrote and this run does not serve: a renamed or switched-off hosted app leaves nothing
  behind, and the old name answers 404 with no redirect. Before the release the old name
  is the one the running app is reached at, so a failed release removes nothing; a failed
  removal is a warning the next apply retries. A user's run removes nothing.
- **Removal with more than one seedbox (open).** The names are read off the one Gateway,
  and every box's managers share one prefix, so a second seedbox's names would look
  unserved to the first one's run. `adduser` has to scope them per seedbox before it ships.
- **Tested without the internet.** The e2e runs Pebble (Let's Encrypt's test ACME
  server) in the kind cluster and points a test issuer at it, so HTTP-01 through the
  Gateway is proven on every run. The self-signed path gets its own assertion: the
  served certificate verifies against `s5n-host-ca` alone, still does after a forced
  renewal, and a box with host certificates from before the root is re-issued from it.

- **A failed renewal doesn't block `apply`.** It shows in `status` and `doctor`, which
  name the host and cert-manager's reason. `apply` changes the seedbox, not the
  certificate, so refusing it would only get in the way of the fix.

### install, remove and instances (decided)

The Arrs take multiple instances (an HD and a 4K Sonarr), Bazarr one per Arr pair and
Seerr one per media server (`mediaServer: plex|jellyfin`, at a name of its own). The chart
models each as `instances:` on the app's entry, and today the grant's list is edited by
hand. Proposed syntax: `box install sonarr --instance 4k`.

### apply and update (decided)

They change different things, so they stay separate commands:

- The **values file** holds what the user wants. `apply` keeps the current chart release
  and applies the edited file.
- The **chart release** holds the pins: image digests, platform versions, migrations.
  `update` moves to a newer release and keeps the values file as it is.

Both end in the same step: diff, confirm, apply. Using one never locks out the other.

One credential neither file has to carry is the Plex server token: a claimed server keeps it
in its own settings file, and apply reads it into the release Secret when the Secret has none
(docs above, "A claimed server's token reaches the Secret through apply too").

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

**No maximum box version (decided).** A chart can't know which future box will break
it, so it declares no ceiling; each side states only the floor it knows. The chart
states `min-box`. box states its own floor in code, the oldest release it still reads,
and raises it when it deletes support for something an old release relies on. The
platform pins are part of that promise: box's compiled-in manifests must work with the
platform version every release back to its floor pins. That isn't hypothetical:
`update` replaces box before it moves the chart, so a new box runs against root's old
release between those two steps, and for longer when the chart step fails. A release
older than box's floor is one box will only update.

**box only uses published charts (decided).** It pulls a released chart by version from
the OCI registry and reads the schema, manifest and annotations from that artifact. It
never loads `charts/` from its own tree, so a box built from main can't depend on
templates no user has. box's own tests run against the chart packaged from the same
commit, the same way: pushed to a local registry and pulled like any release.

**Where a release lives (decided).** The chart and box release separately, each from
its own tag family, so a new Sonarr never needs a new box and a box fix never
republishes the chart.

- An `s5n-v<version>` tag publishes the image at `ghcr.io/swizzin/s5n:<version>` and the
  chart at `oci://ghcr.io/swizzin/charts/s5n`, with `version` and `appVersion` both
  `<version>`. The chart is packaged after the image is pushed and names that image by
  digest, so a chart can never point at an image other than the one built from its
  commit. Its GitHub release lists the app images and platform pins that moved and is
  never marked latest.
- A `box-v<version>` tag publishes box's binaries with a `sha256sums.txt` as assets of a
  GitHub release, which is marked latest, so `releases/latest` always means box.

Because the platform pins ship in the chart (see "The platform layer"), a platform patch
is a chart release too, and a `box-v*` release only ever carries box's own code.

**A chart is signed like box (decided).** box applies a chart with cluster-admin, so a
chart release carries `chart.sig` as an asset of its GitHub release: the release key's
ed25519 signature, over a line naming the release (`s5n chart s5n-v<version>`) and then
the chart's manifest digest. Every command that pulls a chart resolves the tag to a
digest, checks that signature and pulls the digest it names, so a tag moved after the
release is not what box installs, and a chart nothing signed is refused by name. The
version is in the signed message, so neither another release's signature nor a box
release's verifies here. It is a release asset and not an OCI referrer: the registry that
serves the chart is the thing a signature exists to distrust.

**A release published before that signature is signed once, by hand (decided).** Such a
release has no `chart.sig` and box refuses it, so the maintainer dispatches the release
workflow with `sign` set to the version: it reads the digest the tag points at now, signs
it and uploads the asset, and cuts no release (with `sign` empty a dispatch does nothing).
Do them oldest first. box reads every release at or above the installed one, so an
unsigned release in that range stops an update there until it is signed.

**Branches (decided).** `main` is what has been released; work lands on `develop`. Both
move by ordinary pull-request merges, not fast-forward only: CI runs on every push to
`main`, and a release is built from its tagged commit, so a straight line adds nothing.
A feature release merges `develop` into `main`; once CI is green on `main`, the
maintainer tags it there. After every release, `main` is merged back into `develop`, so
`develop`'s e2e always tests the images and pins users are running.

**Automatic chart releases (decided).** Renovate bumps the app images in `variants.yaml`
on `main` only; `develop` gets them through the back-merge. The platform pins are bumped
on both branches, so work on `develop` is built and tested against the platform it will
ship on. On `main`, an app image's patch, minor or digest bump merges itself as soon as
the full e2e is green, a minor in a pull request of its own; a major waits for the
maintainer. Platform pins follow the same rule, except k0s (always the maintainer's) and a
0.x pin's minor. Renovate groups the patch and digest bumps that
land in one window into one pull request. There is no waiting period: the upstream projects
stage their own releases, and holding a build longer adds age, not testing. On every
push to `main`, if `main` has changed since the last `s5n-v*` tag and every change since
is one of those bumps, CI raises the chart's patch version and tags it. Any other change
on `main` waits for a maintainer's release, and the job names it — except Renovate's own
config (`.github/renovate.json`), which Renovate reads from `main` and nothing ships, so
tuning Renovate never holds a release (nor makes one). There is no off
switch in the repo: disabling the workflow in GitHub is the kill switch. Nothing is
released without a previous `s5n-v*` tag to build on, and nothing is published from a
push to `develop`.

A pull request that only bumps those files is tested by the tree it lands on. Renovate
opens them against `main`, and a pull request otherwise runs the tests of the branch it
targets: `main`'s, which lag `develop`'s between releases -- Tautulli 2.19.1 passed
`main`'s suite and broke on `develop`'s that way. So a pull request into `main` whose
whole change is `variants.yaml` and/or `platform.yaml` also runs `develop`'s suite with
its own change to those two files merged into `develop`'s (a three-way merge, as a
back-merge would make it), and a failure there fails the pull request. The
set is `release images`, which reads the same files `decide` releases on, never a second
list in the workflow; `release overlay` merges them, and refuses a bump that would
conflict with `develop`. Any other pull request runs what
it always ran. The check is a job like the rest, so a run cancelled by hand or a
branch protection rule that does not require it leaves the bump as untested as before.

What makes this safe is the e2e first: a bump that breaks on a clean box never becomes
a release. What the e2e can't see is one box's own data meeting a new image, and that
is what the per-app rollback under Updates is for. The Arrs migrate their databases
forward when they start and don't support running an older version on a newer
database, so putting an app back on its old image is not enough. `update` snapshots an
app's config before it moves the app's image, and a rollback restores the snapshot
together with the image the app had.

**What update does with it:**

- **A chart is verified before it is installed (decided).** `update`, `init`, `apply` and
  `migrate` resolve the release's tag to a manifest digest, check the signature its GitHub
  release publishes over it, and then pull that digest (docs above, "Where a release
  lives"). A missing, unreadable or wrong-version signature refuses by name before the
  archive is read, and there is no flag that skips the check; the digest a step installed
  is in `update.json` beside the cluster record.
- **box updates itself first, then re-runs as the new binary (decided).**
  `box update` checks for a newer box before it looks at charts, replaces itself, and
  execs the new binary with the same arguments, so the chart update always runs under
  the newest box. The timer does the same, which is why a release that raises
  `min-box` never leaves an unattended box stuck. A box that can't update itself (the
  release page is unreachable, the signature doesn't verify, a package owns the
  binary) says why in one line and runs the chart update itself, with the releases it
  meets `min-box` for. The one exception is a new binary that installed and then
  wouldn't start: that run stops, because carrying on would move the seedbox under the
  box it just replaced.
- Self-update rules (decided):
  - **Verified before it runs (decided).** The new binary is checked against the
    release's signed checksum before it replaces anything. It runs as root, so an
    unverified self-update would be remote code execution as root. The signature is
    ed25519 over a line naming the release's tag (`s5n box release box-v<version>`)
    followed by the exact bytes of `sha256sums.txt`, so an older release's signed files
    served under a newer tag don't verify. It is published beside them as
    `sha256sums.txt.sig`, and the public key is compiled into box. No keyserver, no
    transparency log, no new dependency: the standard library verifies it. A box built
    without a key refuses to update itself rather than skipping the check, and a
    release built without the signing secret, or with one whose public half isn't the
    key committed in the repository, fails instead of publishing.
  - **Atomic, with one step back.** Written beside the old binary and renamed over it.
    The previous binary is kept as `box.previous`, and `box update --rollback-box` puts
    it back.
  - **Root only.** The binary is root-owned. A user's `box update` that needs a newer
    box says so and stops: "box 0.4.0 is needed; root's next update brings it".
  - **Held like a pinned image.** `--hold-box` skips the self-update and says so on
    every run. `update.hold-box: true` in root's config does the same on every run,
    the timer's included (decided). A release whose `min-box` a held box doesn't meet
    is shown, not taken.
  - **Package-managed installs defer.** If box came from a distro package, it never
    overwrites itself. It names the package command instead.
- It picks the newest release this box meets `min-box` for. A newer release that needs
  a newer box is shown, not taken: "s5n 0.6.0 is out and needs a newer box".
- **It reads the repository from the installed release up (decided).** An update only
  ever steps forward, so the listing starts at the release the box runs: a version
  below it is never read. A repository keeps every chart ever published, and an old
  release is not always one today's box can read -- a chart from before `platform.yaml`
  shipped has no platform pins at all -- so a listing that read all of them would stop
  on history before it chose a target, and it costs one pull per version ever
  published. A version at or above the installed one that can't be read is still a
  broken release and fails the run. A box on the newest release, or on one ahead of
  everything published, has nothing to do, and that is `... is current`, not an error.
- It never skips past `upgrade-from`. If the installed release is older than the
  target's floor, update steps through the intermediate releases, each with its own
  health and wiring gate. This is what lets old migrations be deleted safely.
- `apply` re-reads the annotations of the release already installed. A box that's too
  old for the installed release refuses to apply rather than writing values it can't
  check.

**Testing a build on a real machine (proposed).** The e2e runs on kind, which is not a
host: no k0s, no disks, nobody answering a question. So before a release is cut, a
maintainer takes `develop` to a machine running a fresh OS and installs it there the way
a user does -- `box bootstrap`, then `box migrate apply` -- without tagging or
publishing anything. `hack/dev-box.sh` packs that build. Its three artifacts have to
agree on one version, so they are all `<next patch>-dev.<short sha>`:

- `box`, built with `-X main.version`. A build without one is `dev`, and skips the
  chart's `min-box` check instead of meeting it.
- the chart, packaged with `version` and `appVersion` both that version. That is also
  the s5n image's tag: `variants.yaml` leaves `s5n.default.tag` empty, so the chart
  names `appVersion`.
- the image, built from the repo's Dockerfile and carried to the test machine as a
  `docker save` tarball.

The next patch, not this one: `box.s5n/min-box` is the released version, and semver
puts `0.5.0-dev.x` *below* `0.5.0`, so a dev build of the released version would be
refused by the chart it had just built. `<next patch>-dev.<sha>` meets the floor and
still sorts below the release that follows it.

Nothing in a dev build is published, and box installs only published charts. So the
chart is served from a distribution container on the build machine's loopback -- the
one host box speaks plain HTTP to -- and reaches the test machine through `ssh -R`.
The image is not pulled at all: it is imported into the node's containerd
(`k0s ctr -n k8s.io images import`) and the chart's `IfNotPresent` policy uses it. There
is no containerd until box has installed k0s, so the build's `NEXT.txt` starts an
import loop in a second shell before the install: it retries until k0s's containerd
answers and imports the image ahead of the release that runs it.

This is a rehearsal on real hardware, not a replacement for the e2e: the cluster
decision, the platform layer and a real disk are what CI cannot reach.

**What CI proves:** box at HEAD against the newest released chart and the oldest
release back to its floor, each on the platform that release pins; the chart at HEAD
against the newest released box and against the box release its `min-box` names. A
chart change (a platform bump included) that needs a newer box but forgets to raise
`min-box` fails there and not on a user's server. The self-update path gets
its own test: the previous box release updates itself to HEAD and finishes a chart
update.

### Image choice: blessed variants (decided in principle)

A user can't run an arbitrary image. A different image can move config files, mounts or
users, and the chart can't promise anything about an image it hasn't seen.

Instead, the chart lists **blessed variants** per app, such as a qBittorrent build on
libtorrent 1.2 or 2.x (see `PLAN.md`, open decisions). Each variant is tested by the
e2e like the default. Proposed command: `box update qbittorrent --variant lt1.2`, which
writes the choice into values.

A digest pinned by hand in values is held: `update` leaves that app where it is and
says so ("radarr: held at 6.1 by your values"). Removing the pin releases it.

**Digests are box's promise; the chart accepts a tag (decided).** The chart's own pins
are always a tag and a digest, so a release runs the bytes its e2e tested. A pin in
values may be a tag alone, the way any chart takes one: the chart renders `repo:tag`
and refuses nothing, because asking a Helm user for a sha to pin `6.1` is hostile. Such
a pin is mutable. Each node runs whatever the tag pointed at when it first pulled, and a
reschedule or an image GC can pull different bytes with no release behind them; that is
the user's choice. Every pin box writes (`rollback --pin`, a hold) carries the tag and
the digest together, so box never creates a mutable pin itself. Proposed, not built:
`status` and `doctor` name a tag-only pin ("radarr: pinned to tag 6.1 without a
digest; it can change under you"), reading the running digest from the pod's
`imageID`, since box records none for it; and the chart's `NOTES.txt` names one on
`helm install`/`upgrade`, without failing.

**The s5n image is the chart's one tag-only pin (proposed).** `variants.yaml` ships
`s5n` with an empty digest because the image and the chart share a tag and the digest
does not exist until the image is built. The release workflow should push the image
first and write its digest into the chart before packaging, so our own pipeline is held
to the rule above. Due with the next change to `release.yaml`, not on its own.

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

**qBittorrent's own builds (decided).** qBittorrent is the one app where the
libtorrent line is a real choice, so its default is the project's own image,
`ghcr.io/swizzin/qbittorrent` on libtorrent 2.1, and the list beside it is six
more of those builds — `lt1.2`, `lt1.2-10g`, `lt2.1`, `lt2.1-10g`,
`lt1.2-10g-znver3` and `lt2.1-10g-znver3` — plus `home-operations`, the image
the default ran until the project built its own (a box on `default` moves to the
project's build; naming `home-operations` keeps that builder, and a migrated source
running it is planned onto it), and the one former name still listed beside them,
`lt1.2-10g-znver3-mimalloc` (see below).
`lt1.2` is the static build on libtorrent 1.2 (musl, baseline x86-64-v2 and
armv8-a) with the default's layout — `HOME` and `XDG_CONFIG_HOME` at `/config`, the
conf at `/config/qBittorrent/qBittorrent.conf`, ports from `QBT_WEBUI_PORT` and
`QBT_TORRENTING_PORT` — so the chart's one pod spec runs it unchanged and a box can
switch variants in place. A variant name is the libtorrent line plus the build
profile, never a version. The tag carries only qBittorrent's version and that line
(`5.3.1-lt1.2`) — the version is what lets Renovate propose a qBittorrent release —
and libtorrent's own release is pinned by the digest alone, so a later 1.2.21 is a
new digest on the same tag and the variant keeps its name.

`lt2.1` is the same build on libtorrent 2.1, which is the other axis a user can
care about: 2.0 maps files instead of caching them, 2.1 reads and writes through
pread/pwrite with a cache of its own (`src/session.cpp:492` and
`src/pread_disk_io.cpp:329` at v2.1.3), and the same pod spec runs it. `lt1.2-10g` and
`lt2.1-10g` are those two builds with two larger compiled-in peer buffers for 10
Gbit links — `max_peer_recv_buffer_size` 4 MiB against the base build's 2 MiB, and
`send_not_sent_low_watermark` 1 MiB against 512 KiB — and nothing else differs: the
base builds already carry five tuned defaults of their own, so `-10g` is not stock
libtorrent plus two values. On the two `lt2.1-10g` builds the chart also seeds
`Session\DiskQueueSize` at 512 MiB, the size a 10 Gbit link was measured with — the
queue libtorrent 2.1's writer is sized from — and only where `qBittorrent.conf` has
no value yet: a value there is the user's (or an earlier seed's), box never replaces
or removes one, and it stays when the variant changes. Every 10G build links
mimalloc for its allocator and the base builds keep musl's own, which is what the two
profiles differ in besides the buffers. All four are built for both architectures, and
`hack/e2e-kind.sh` switches through each one the way it runs `lt1.2`: the pod's own
image and digest, and qBittorrent's `buildInfo` reporting the libtorrent line, which is
what a tag alone cannot say.

The two Zen 3 builds, `lt1.2-10g-znver3` and `lt2.1-10g-znver3`, are the exemption
from "the e2e tests every variant". They are compiled for AMD Zen 3
(`-march=znver3`), so they die with SIGILL at start on CI's runners and most hosts,
and they are amd64 only, so an arm64 node cannot pull them. What tests them instead
is the `swizzin/qbittorrent` repository's own binary verify and smoke test, run on a
Zen 3 runner before the tag is pushed. Their layout is the one `lt1.2` has, which
`hack/e2e-kind.sh` runs on every push, so the pod spec is proven for both.

`lt1.2-10g-znver3-mimalloc` is the former name of `lt1.2-10g-znver3`, from when only
the Zen 3 build linked mimalloc. It stays in the list as a real entry pinned to the
same image, because a name the chart does not have fails the render, and a box whose
values still say it would otherwise stop updating: nothing chooses it anew.

### uninstall (decided)

- **Never deleted:** media and downloads, even when asked. The user removes those
  themselves.
- **Deleted only on opt-in:** app configs; k0s, and only if box installed it.
- **Always removed:** the seedbox releases, the platform layer box installed, box's own
  records and the box binary.

### migrate (decided in part, mechanism proposed)

`migrate` adopts an install box didn't make: its app configs, its torrent state and its
data. Everything it will change is written down before it changes anything.

- **Three verbs (decided).** `box migrate discover [--rehearsal]` reads a source and
  writes the plan, `box migrate cutover [--replace-cluster]` takes the final snapshot on
  the source host, and `box migrate apply [file]` installs or adopts from a snapshot.
  Each of them takes `--values`, and applies exactly that file's values.
- **One place for snapshots (decided).** They live in `/var/lib/s5n/migrate/`; discover
  writes there, or where `--out` names a file, and `box migrate apply` with no file
  takes the newest snapshot there that is not a survey. A snapshot from another host is
  the file to pass. A snapshot's name says what it is and when it was taken:
  `migrate-<UTC yyyymmddThhmmssZ>-<mode>.tar.gz`, so a directory listing reads as the
  history of a cutover. The modes are `survey` (a read with no configs in it),
  `rehearsal` (the configs copied from a running source) and `final` (from a stopped
  one); apply runs a final snapshot, refuses a survey because there is nothing in it to
  rewrite, and runs a rehearsal under the lock below. A box that applied a rehearsal
  stays locked, so it refuses a final snapshot: the real run starts from a final one on a
  clean host.
- **discover only reads, and the apps need not run (decided).** It reads each workload's
  pod template and the host's files, so it needs the cluster up, never the app: a
  stopped app is still there to read. Every workload kind with nothing owning it above
  it is read — Deployment, StatefulSet, DaemonSet, an ownerless ReplicaSet, a bare Pod
  (the goal is decided; the set of kinds is the mechanism).
- **Nothing unsettled reaches the install (decided).** discover and cutover run every
  rewriter's check pass, which writes nothing, against the draft plan. An address or a
  path that cannot be settled becomes a plan entry with a default and a one-line reason
  rather than a refusal after install; a guided run asks those questions, an unattended
  one writes the defaults and says so. apply refuses only for drift: a plan edited by
  hand, or an adoption into a box that already runs something the plan names.
- **box never stops or resets a source (decided).** It prints the command for the
  platform it found — `kubectl scale`, `docker stop`, `systemctl stop`, the reset recipe
  for a cluster it didn't install — shown as one to double-check, and the operator runs
  it. The reset recipe is for the distribution the source's own nodes name (the marker in
  each kubelet's version: `+k0s`, `+k3s`); a cluster box cannot place gets a line saying
  so rather than a teardown that would remove nothing.
- **cutover is the window (decided).** It refuses while any planned app still runs: the
  workload at replicas 0 and no pod left. It writes its read before printing any stop
  command, so following box's advice is always safe. With no earlier discover it lists what
  it found and asks whether that is everything, unattended refusing unless
  `--no-prior-discover` and naming `box migrate discover` as the better first step; with
  one, it diffs, and an app the earlier read had and this one cannot find stops the
  cutover. The earlier read is the newest snapshot in the directory whose plan names the
  same source -- the host in its `source` block and the namespaces it was read for -- and
  the run says which one it compared against. An app this read has and the earlier one did
  not is new, listed and kept. A read the cutover stops on is kept as a survey either way.
- **The source's cluster is kept by default (decided).** A kept cluster is self-managed:
  box installs onto it, manages none of its version, and the whole cutover runs in one
  go — platform, install, adoption — with the source's workloads left at 0. Before it
  installs, cutover checks three things and stops after the final snapshot naming the
  command that shows each one: bootstrap's own gates over the cluster (a managed control
  plane, more than one node, an API that does not answer), anything in any namespace
  already holding the node's 80/443 (a LoadBalancer Service, a node port, a hostPort, a
  pod on the node's own network), an auto-updater (keel's annotation on the source's
  workloads, or keel running anywhere), and a GitOps controller (Flux or Argo CD running
  anywhere, or their mark on the source's workloads), which would scale the stopped
  source back up beside box (decided). box changes nothing on a cluster it did
  not install: the operator clears them, or `--replace-cluster` asks for box's own k0s
  instead: cutover stops after the final snapshot with the reset recipe, and
  `box migrate apply` finishes on the clean host.
- **apply is the installer on a host with no box (decided).** It runs bootstrap and then
  init's steps from the plan: the data root and the owner are the plan's, and the apps
  go in off -- the migration switches on what it moved, because an app started before
  its config is moved into place would run on an empty one. Only the domain, the login,
  the password and the config path are asked, or taken from the flags an unattended run
  gives, which names every one it is missing. A guided run asks only the ones it does
  not have: an answer a flag or the plan already holds is not a question, and the app
  picker never appears, because the plan decides every app. On a host that already has
  a box it adopts into it, which is how one source is migrated per user.
  The install ends the way bootstrap's does: once init has installed the release, the
  run writes the update timer for the record, grant and values that run used, and says
  so -- a migrated box updates itself like any other. A box whose install never
  finished is never on a schedule, and a host that already had a box keeps the units
  its own bootstrap wrote, because that run adopts and writes none.
  The torrent port is decided for the box rather than for the app list this run writes,
  and the source's own peer port is kept when this cluster can use it.
  The install is what the run's one answer buys, so it comes after that answer: on a
  host with no box the run decides what it would install and what it would move first,
  asks, and only then installs. An answer of no leaves the host as it was -- no k0s, no
  platform, no firewall change, no grant and no values file, and nothing under the
  config root, because the pre-install pass stages into a directory it removes -- and
  a cluster record is written only once box actually owns the cluster, because a record
  is what makes a host "have a box".
- **One run, one confirm.** Everything a migration does is reviewed once, in one
  question: the platform the installer puts on a host with none (its plan first), then
  one row per app with what it carries and what changes, the data directories it
  renames, what needs the operator, the notes, and what is not coming. `--details`
  prints every rewrite column by column instead; the snapshot's `report.txt` holds it
  whole either way, and the wiring receipt keeps per-object detail behind the receipt
  itself.
  On a host with no box the question comes before anything is installed, so the
  platform plan is the plan this run would apply, decided from a read-only pass and
  shown first. Plex is the one app whose check runs its own image, and there is no box
  to run one in before the install: its row says its databases are checked once Plex's
  image runs, and after the install its check runs before any app is switched on. A
  blocker there stops the run with the box installed and nothing of the snapshot moved,
  saying so and what running it again does; a clean one carries on without asking
  again. The summary the adoption is held to is the one that was shown -- the same
  snapshot and plan read twice -- and a difference anywhere but Plex's filled-in row, or
  the app whose whole data the config root it lands on turns out not to hold (below), is
  refused rather than done.
- **One snapshot file, and it is the backup format (decided).** A snapshot is one tar +
  gzip archive with a manifest holding a sha256 per entry, so a truncated copy is
  refused rather than half-applied. `plan.yaml`, `source/`, `report.txt` and `config/`
  are inside it, and its mode says which of those apply may run. It holds secrets — API keys, the Plex token, qBittorrent's password
  hash — so it is root-only, 0600. `box backup` writes the same file from box itself,
  and `restore` is `migrate apply` of that snapshot: one format, one apply path.
- **A config travels as what the app cannot rebuild (decided, hers).** Every app's config
  directory is copied whole — apply rewrites the copy and a file a rewriter never reads
  still has to come across — except the one whose config is mostly a library: Plex's
  `Metadata` and `Media` trees are the artwork, the chapter thumbnails and the preview
  thumbnails it built from the media it is given, and on a real library they are tens of
  gigabytes. Rebuilding them is hours of the machine's time at full load, so a migration
  carries them too when there is room, and Plex's state alone when there is not.
  What a snapshot carries of Plex is the whole of `Library/Application Support/Plex Media
  Server` — preferences, both library databases, `Metadata`, `Media`, the agents' data —
  less what Plex rebuilds or downloads again by itself: `Cache`, `Plug-in Support/Caches`,
  `Codecs`, `Logs`, `Crash Reports`, `Diagnostics` and `Updates`. Nothing beside Plex's
  home in the config mount travels.
  The room is checked before anything is written, in both places a full carry is written
  to: the snapshot's own directory, and — on a host that is both ends of a migration — the
  seedbox's config path beside it, counted twice when they are one filesystem. The number
  is a fast walk of the tree (no reads, one stat per entry) plus a tenth of it, never less
  than 256 MiB; the free space is what an unprivileged writer has. A run without the room
  carries the state instead, in one line with both numbers
  ("plex: its whole data needs 62.4 GiB and 12.0 GiB is free at /var/lib/s5n/migrate; …"),
  and apply's confirm says Plex will build its artwork, thumbnails and detection again.
  `--plex-minimal` asks for that whatever the room. The choice is the plan's
  (`apps.plex.carry: full|minimal`), so cutover and apply agree; apply checks the config
  root again and, when the snapshot's tree does not fit there, carries the state and says
  so — the one difference between the pre-install summary and the run after it that is not
  a fault, because the config path is a question the install answers after the confirm.
  Plex's state alone is `Preferences.xml` and the two library databases with the `-wal` and
  `-shm` a writer leaves beside them. The two files are required (a snapshot without either
  is refused, naming the path); the companions are carried when they are there, because a
  database nobody has open has none. The read says which of the two it carries once, in its
  report and on the screen — "plex: carried its whole data -- metadata, artwork, thumbnails
  and its databases -- except the caches, codecs, logs and crash reports Plex rebuilds or
  downloads again", or "plex: carried Preferences.xml and its databases; Plex rebuilds its
  metadata, artwork and thumbnails (artwork uploaded by hand is not carried)" — and apply's
  confirm repeats it for the copy it staged. The list is box's own, not the catalog's
  `snapshot:` key, which is what an upgrade may rewrite and which stays Plex's state: the
  per-update copy does not grow a library.
  The tree is written once. apply reads the snapshot for what its checks and its rewrites
  read — the plan, the report, the sources, every app's config, and Plex's state alone — and
  the rest of a full carry is written straight from the snapshot into the instance's config
  directory, at the staging step, after the box is installed. The snapshot is verified whole
  first, by a pass that writes nothing, so a copy that lost a byte stops the run before its
  first write rather than halfway through its last. The numbers the confirm prints are that
  pass's, not a walk of a tree that is not on the host.
  A run's working directories are beside the snapshot it reads (`/var/lib/s5n/migrate` for
  one box names, the file's own directory otherwise), never in `$TMPDIR`: on a Fedora or an
  Arch host that is a tmpfs, and a snapshot's state is gigabytes — a real source's autobrr
  config alone was 8.5 GiB.
- **A migrated app account becomes box's own.** autobrr and qui keep their own user
  table and allow exactly one account there, and box owns the account the init container
  creates, sets the release Secret's password on and signs in as (`s5n.wiringAccount`);
  the person's way in is the login at the edge, never this account. So a source's one
  account row is renamed to it during apply, keeping its id — autobrr's schema refers to
  a user row nowhere (`api_key` is keyed by its own key, `sessions` by its own token),
  and qui's settings tables carry that id — and the app's first start already has the
  account its init container looks for. The confirm says so once per app, with the name
  the source used. A database with two rows is refused naming the count: neither app can
  hold two, so which one a source meant is not something box can decide.
- **A config from an older app is taken as it is.** box's own init containers run
  before an app's first start and write into configs a source wrote, which are not the
  shape the chart's own install makes. A PeerBanHelper config that predates its
  `privacy` block gets the block, and the app's own config migration — which writes that
  key from the template inside its jar, where it says `true` — is given a copy of that
  same template with `false` in it, so the start that migrates the file keeps box's
  answer instead of running once with analytics on. The copy is made from the jar on
  every start and every other byte of it is the image's, so no migration is skipped and
  nothing in the file is box's own invention.
- **A rehearsal cannot reach the internet (decided goal, mechanism proposed).**
  `discover --rehearsal` copies the configs into a snapshot that applies locked: no
  internet egress, proven from inside the namespace before any app starts, and every
  torrent started stopped. The lock is a grant value (`rehearsal`), so it is in box's
  own record and every later `box apply` and `box update` renders it; nothing unlocks
  it, because a rehearsal box is disposable. The proof is a control pod without the
  release's label that must reach an in-cluster address outside the namespace, and a
  probe pod carrying the release's label that must not; a control that cannot run is a
  refusal saying the proof could not happen, and a probe that succeeds is a refusal
  naming the cluster's CNI, because a NetworkPolicy is only an instruction to it. Both
  dial the cluster DNS Service's metrics port, which the lock denies, and keep dialling
  until the answer holds: a CNI programs a new pod's policy a moment after the pod
  starts (on kindnet a pod's first connection gets through), so one dial at start-up
  proves nothing. The same moment is why every app pod of a rehearsal box first runs
  a gate, an init container in the app's own network that waits until that address
  stops answering; the app never starts if it does not. The address is in the grant
  beside the flag (`rehearsalGate`), and a rehearsal box whose chart cannot lock it,
  or whose grant has no gate, is refused rather than rendered open. The
  torrents start stopped because the copy has no data to seed and would otherwise
  recheck from zero. A rehearsal renames nothing, refuses to apply on the host that
  holds the source's data, and never applies a final snapshot afterwards.
- **Every source is detected, and one plan can hold several (decided).** discover runs
  each reader that finds evidence — a cluster at a known kubeconfig, a docker socket
  with catalog images running, swizzin's units — and shows what it found. Each app
  records the source kind it came from, so one plan carries the apps of every platform.
  One reader per platform: a new platform adds a reader and its e2e, never a new plan
  shape. Readers ship Kubernetes, then swizzin classic, then docker.
- **Images come from the blessed variants (decided).** A source image maps to a variant
  only when it already is one of the chart's blessed variants, same repository and same
  variant, and discovery writes that variant into the plan; anything else gets the chart
  default and a note naming `apps.<app>.image.variant` and the list. The list is the
  chart's, so a read loads the release it is writing a plan for (`--chart-repo`,
  `--chart-version`, the flags the install takes): a read that cannot load it is refused
  rather than planning every app onto the default image silently. The same Ingress read
  that resolves an address gives the plan's domain default: the one name the source's hosts
  all are — box serves the apps under paths on it, as the source did — or the domain
  several hosts share. The source's own image is never carried, and the admission policy
  stays the guard. A swizzin qBittorrent tag names its variant by its suffix: the
  version in front (`5.3.1-`) is qBittorrent's and not part of the name, libtorrent's
  patch release folds back to its line (`5.3.1-lt1.2.20-10g` is `5.3.1-lt1.2-10g`,
  because a later 1.2.21 is a new digest on that same variant), and the rest is the
  variant's name — so a source on an older qBittorrent of a build box ships maps onto
  it, with a note saying the chart's version runs. The old Zen 3 name,
  `lt1.2-10g-znver3-mimalloc`, maps onto `lt1.2-10g-znver3`, which is the same image
  under its current name. The mapping is that one repository's, and a tag no variant
  has is reported as exactly that, naming the tags box compared.
- **One snapshot per owner (proposed).** discover groups the apps by the user that owns
  their config directories, so a source with several users yields one snapshot each. The
  plan records the owner's uid:gid, and every other snapshot is applied into that user's
  seedbox with `--user`.
- **Discovery comes first and writes a plan (decided).** migrate reads the source,
  running or stopped: each app's config directory, the paths the apps saw (container
  mounts, or the paths the configs themselves name), the categories, and how the apps
  reach each other. It writes this to a plan file and changes nothing. When the source
  has to be torn down before box can install, for example a cluster box will replace
  with its own, the operator does that between discovery and apply, and the plan
  carries across what box needs. box never removes a cluster it didn't install.
- **Configs are copied, data is adopted (proposed).** Configs are copied into box's
  config root and rewritten there, so the source stays as the rollback. Data never
  moves, so hardlinks survive.
- **Existing download category names are kept (decided).** The chart accepts the names a
  source already uses (`Movies`, `4K/Movies`), and qBittorrent's download tree is never
  touched: its categories, its torrents, their save paths and the category directories
  under `downloads` stay exactly as the source had them (only the root's `Downloads` →
  `downloads` rename, as before). A migrated instance's `downloadCategory` value is the
  category its torrents are in, so wiring wires that one and creates no other.
- **The library ends up in box's layout (decided).** What a person sees is box's own names, on a
  fresh box and on a migrated one alike: the source's library directories are mapped onto
  the categories the release lays out (`Movies` → `movies`, `4K/TV` → `tv-4k`), apply
  renames them inside the library, and the Arrs' root folders and everything under them,
  Plex's section locations and every other app that stored one are rewritten to follow.
  Discovery reads each Arr's root folders to build the mapping; one root folder under the
  library per instance is the normal case, and more than one — or one outside it — is a
  default with a one-line reason rather than a guess. A source already in box's layout
  maps each directory onto itself and the run renames nothing.
- **Ports go to the chart's defaults (decided).** Port changes are what broke moving a
  swizzin box. Copied configs get the chart's ports, bind addresses and URL bases, and
  references between the apps are rewritten to match.
- **Only local addresses are rewritten (decided).** This covers the connections the apps
  hold to each other: an Arr's download clients, Prowlarr's applications, Seerr's Arr
  servers, and the Arr, qBittorrent and Plex connections in any other catalog app. A
  connection is rewritten when it points at localhost (`localhost`, `127.0.0.0/8`, `::1`),
  at a name that is plainly inside the source cluster (a Service discovery found, by its
  short name or as `<svc>.<ns>`, `<svc>.<ns>.svc` or `<svc>.<ns>.svc.cluster.local`), at a
  Service's cluster address, or at a public host and path an Ingress of the source routes
  to one of its Services: those three resolve to that Service's app outright (decided Nov
  17). A host that serves several apps resolves by path — box's own shape is one name with
  each app under a path — so the app is the one whose Ingress path is the longest prefix of
  the address, on a path boundary, and an address with no path on such a host is left for
  the operator rather than pointed at one of them. A pod's address is a hint and never an
  answer, because it does not survive the move
  and may be another pod's by the time the plan is applied: an address that matches a pod
  at discovery becomes a plan entry with a proposed default and a one-line reason saying
  what box will do and why it is not certain, and every default is called out in the
  report, in the guided question and in the end-of-run summary, never applied silently. On
  localhost, the port says which app it was, from the ports the source's own configs set.
  Anything else (a LAN or public address, a domain, another machine) is left as it is and
  listed in the plan, because box can't know what it points at; the plan can map one by
  hand. Matching discovered Services rather than app names keeps box naming no apps.
- **Migrated connections are adopted by ID (decided).** Wiring adopts the objects it
  finds by name on its first run, so a migrated download client under a different
  name would get a second one created beside it. migrate records the IDs of the
  connections it rewrote (download clients, Prowlarr's applications) in wiring's
  ownership record, and wiring takes them over by ID: the client the source already
  had becomes the one box manages. With nothing recorded — the first run after a
  migration — an Arr's client is adopted by the qBittorrent it points at before its
  name is tried, because a source's client is named by its operator (`qBT`) and the
  host is what makes it the release's own; two enabled qBittorrent clients at that
  host are refused, naming both, rather than one picked.
- **The migrated server's Plex token is seeded (decided).** A claimed Plex keeps
  the account's token in its own `Preferences.xml` (`PlexOnlineToken`), and the
  release Secret's `plex-token` is what every Plex-dependent app reads: Tautulli for
  its PMS link, Maintainerr for its connection. `migrate apply` reads it out of the
  Plex it is copying and writes it into the release Secret, so a migrated box is
  wired rather than pending on a token nobody was asked for. It is never written over
  a `plex-token` the Secret already holds — that is the person's own, or one an
  earlier run of the same plan wrote — and the token itself is never printed, only
  the key's name on the confirm's row. A server that is not claimed refuses every
  server token, so the rows that use it are `pending` with the claim as the next
  thing to do, never a failed release and never a link written with a token the
  server would reject.
- **A claimed server's token reaches the Secret through apply too (decided).** The
  token a Plex keeps in its own `Preferences.xml` is read by every `box apply` — `init`'s
  re-run, `update`'s closing apply and a bare `apply` alike — and written into the release
  Secret's `plex-token` when the Secret holds none, so a box claimed after it was installed
  is wired by the next run rather than by a hand-patched Secret. That run installs the
  release again even when no value changed, because the wiring Job is a hook and re-reading
  the Secret is what turns the token into Tautulli's link. It reads and never writes Plex's
  file, and it keeps the person's answers: a token in the values (`secrets.plexToken`), a
  `secrets.existingSecret` box must not write into, and any key already in the Secret are
  left alone, and a first install has no Plex state to read. A file that does not parse is
  one line naming it and the apply goes on, and the token itself is never printed. The limit
  is rotation: a present token is never replaced, so a server that signs in again keeps the
  box on the old one until the key is deleted and `box apply` runs again.
- **Shown before it's done; unattended runs only what was shown (decided goal,
  mechanism proposed).** The plan holds the rules: the path rewrite table, the port
  map, every host reference marked rewritten or left alone, what is copied where, and
  what is not migrated (workloads outside the catalog). Guided, migrate prints the
  changes with counts per file and table and asks once. Unattended,
  `box migrate apply <snapshot> --yes` runs the plan and nothing else. The snapshot
  carries the configs apply will rewrite, so the checks discover and cutover ran are the
  checks apply repeats: what apply refuses for is drift — a plan or a config that
  changed since the snapshot was taken — and it refuses before its first write, naming
  the app, the file and the plan key that would settle it.
- **The path table is per app (proposed).** Each app saw its own paths: an Arr may see
  the library at `/library` while qBittorrent sees the same downloads at `/downloads`.
  So the plan maps paths per app, from the mounts discovery found:

  ```yaml
  version: 2
  source:                             # the install this plan was read from
    host: media-host                  # as discovery saw it, empty when it can't say
    taken: 2026-11-16T12:00:00Z       # when the source was read
    owner: {uid: 1000, gid: 1000}     # who owned the configs and the data
    kinds: [kubernetes, docker]       # every platform that contributed an app
  data:
    root: /srv/library                # becomes the seedbox's data path
    downloads: Downloads              # renamed to downloads
    media: media                      # already the contract's name
    folders:                          # the library's directories, and what each
      - from: Movies                  # becomes: one of the apps' categories
        to: movies
      - from: 4K/TV
        to: tv-4k
  apps:
    sonarr-4k:                        # box's instance name
      kind: kubernetes                # the platform it ran on
      source: sonarr4k                # the source's name for it
      image: lscr.io/linuxserver/sonarr:4.0.14  # what it ran, for the record
      config: /srv/apps/sonarr4k      # copied from, never written
      category: tv-4k                 # the directory box lays its library out in
      downloadCategory: 4K/TV         # the category its torrents are already in
      paths:
        - from: /library/Downloads
          to: /data/downloads
        - from: /library/media/4K/TV
          to: /data/media/tv-4k       # the folder rule, more specific than the
        - from: /library/media        # library's own
          to: /data/media
    qbittorrent:
      kind: docker
      source: qbittorrent
      image: ghcr.io/swizzin/qbittorrent:5.3.0-lt1.2  # what it ran, for the record
      variant: lt1.2                  # the chart's build of it, at its own version
      config: /srv/apps/qbittorrent
      paths:
        - from: /downloads
          to: /data/downloads
    seerr:
      kind: kubernetes
      source: jellyseerr
      config: /srv/apps/jellyseerr
      mediaServer: jellyfin           # the server its settings sign in to
      paths:
        - from: /app/config
          to: /config
  ```

  A Seerr's `mediaServer` is read from its settings (plex or jellyfin) and written on
  the instance it is switched on as, because the config it carries signs in to that
  server; a Seerr nobody set up has none and gets the release's default.

  A folder rule is a rename apply makes and a rule every app that stored the old
  directory follows, so the Arrs, Plex and Seerr all end up naming the one box lays
  out. `to` is a category one of the plan's apps has: two directories becoming one,
  one directory named twice, and a category no instance has are all refused.

  The longest `from` that matches whole path segments wins, so `/downloads2` is not
  under `/downloads`. A relative path follows its root and is left alone. An absolute
  path no rule covers stops apply and names the app, the file or table, and the path.
  Each app also records the namespace it ran in and the ports its own config set:
  a localhost address another app stored is resolved by its port, and a Service's
  short name by the namespace of the app that stored it. An app's `kind` is one of
  `kubernetes`, `swizzin` or `docker`, and one plan may hold apps from several.
- **Data is adopted by renaming, never by moving (decided).** The plan's `data` key
  names the directory that becomes the seedbox's data path and the two directories
  under it that held the downloads and the media. When their names differ from box's
  (`Downloads` for `downloads`), apply renames them, which keeps every hardlink. They
  must be single directories directly under the root; anything else is a move, and a
  move is the operator's to make before apply. The same holds one level down:
  `data.folders` names each library directory and the category it becomes, and apply
  renames those inside the library after the two above, removing a parent they emptied
  (`4K/`). A target that holds a file is refused before anything moves, naming both
  directories; a rename an earlier run made but did not record is taken as done; a
  rehearsal renames nothing.
- **Every other address is classified in the plan (proposed).** `hosts` lists the
  Services discovery found (`service: <name>.<namespace>`, matched in every form the
  cluster's DNS answers) and any other address the apps stored (`host:`), each either
  mapped to a box app (`app:`) or kept as it is (`leave: true`), optionally narrowed to
  one port. A rule discover wrote as a default says why, on one line, and marks itself
  (`why:`, `default: true`); a path rule written the same way carries the same two keys,
  so every defaulted decision reads as one.

  ```yaml
  data:
    root: /srv/library
    downloads: Downloads        # renamed to downloads
    media: media
  hosts:
    - service: sonarr.default
      app: sonarr
    - host: 10.244.0.56
      port: 32400
      app: plex
      why: a pod address that does not survive the move, and only plex answers on 32400
      default: true
    - host: 192.0.2.10
      leave: true
  ```
- **Every rewriter runs twice (proposed).** apply copies each app's config into a
  staging directory, then runs every app's check pass, which writes nothing and returns
  the changes and every gap. discover and cutover run that same check pass over the
  configs before they copy them, which is how a gap becomes a plan entry before the
  downtime. Only when no app has a gap does any app's write pass run, and it makes
  exactly the changes its check reported, in the staged copy. History (records of what
  happened, never read back to act on) is rewritten where a rule covers it and never
  stops apply.
- **A migrated config belongs to the seedbox's user, whatever box ran as (proposed).**
  apply stages each instance owned by `puid:pgid`, and every rewriter writes through an
  atomic rename — a new file beside the app's, renamed over it — so the files it
  replaced become the running user's, which on a real host is root's, often 0600. One
  pass after every rewriter and before anything is moved into place hands every path of
  every staged instance to `puid:pgid`, links chowned and never followed. It is a
  whole-tree rule rather than a list of the files today's rewriters write, so a rewriter
  that replaces or creates one cannot reintroduce it: an app that cannot read its own
  config either crash-loops (the Arrs' `config.xml`, qBittorrent's conf and its resume
  records, PeerBanHelper's, qui's and Tautulli's) or, like Seerr, starts anyway and
  writes its defaults over what the migration carried. A rewritten file keeps the mode
  its original had, which each rewriter reads before the write.
- **Every app's stored paths are rewritten, with no exceptions (decided).** Each app
  gets the `/data` contract and migrate rewrites what it holds: Arr databases and
  configs, qBittorrent's fastresume files and settings, and Plex's database through the
  `Plex SQLite` shipped in the pinned Plex image, never stock sqlite.

## Updates (decided goal, proposed mechanism)

Goal: everything stays current without anyone thinking about it, apps and k0s
included.

The chart pins every image by digest; there's no in-cluster image updater and no
`:latest` (`PLAN.md`, Chart). Freshness comes from the repo side instead:

1. Renovate opens a digest bump when an app image publishes.
2. CI runs the e2e suite on it; a passing bump cuts a chart release automatically.
3. Each box runs `update` on a timer (on by default). A health check and a wiring check
   gate each update. An app that crashloops on its new image is rolled back by box; any
   other failure stops the update there, is recorded and is announced.

New images arrive hours after upstream instead of minutes, and a broken upstream image
fails in CI instead of on a user's box.

**The timer (decided).** box checks for a newer release once an hour by default, with
jitter so a fleet doesn't arrive at once. The interval is a key in root's config, and
the timer can be turned off there. It reads the tag list from ghcr anonymously; no
token is needed to learn that something moved.

- **Root's config is `/etc/s5n/box.yaml` (decided).** It sits beside the cluster
  record, so `--record` moves both, and it is root's alone, mode 0600, because the
  notice address can carry a token. bootstrap writes it once, on the host that has
  none, with these keys and comments:

  ```yaml
  # box's settings for this host. box never rewrites this file; edit it by hand.
  update:
    timer: on        # off: no update checks and no health notices
    interval: 1h     # how often the timer checks for a newer release; 15m at least
    hold-box: false  # true: take no new box, the same as --hold-box on every run
  notices:
    url: ""          # box POSTs a JSON notice here after updates and on health changes
  ```

  An existing file is never rewritten. Every run parses it strictly: an unknown key, a
  malformed line or a bad value stops the command, naming the file, the line and the
  key, because a typo that is silently ignored is a box that quietly stopped updating.
  A missing file is the defaults. `update.timer: off` is read by the timer's own wake,
  so it takes effect on the next wake and systemd is never touched.
- **The unit wakes box every fifteen minutes; box decides whether to check (decided).**
  Two systemd units: `box-update.service` runs `box update --timer` as root with no
  timeout (an update can wait out an app's thirty-minute startup budget), for the
  record, grant and values that install used -- bootstrap's own, or the one a
  migration installs on a host with no box; `box-update.timer` wakes it 10 minutes
  after boot, every 15 minutes after that, with up to 10 minutes of jitter.
  `box update --timer` answers yes and asks nothing. The wake reads the clock
  `update.json` holds: a check is due when the last one is zero or a whole interval
  old, so most wakes read health and nothing else, and the check interval is one edit
  in `box.yaml`. A wake that is not due contacts no registry and replaces no binary.
  A wake that is due reads health after the check whatever it did, and once: a check
  that failed leaves the last one unwritten, so every wake would be due again and a
  registry outage would otherwise silence every health notice while it lasted.
- **Where box cannot schedule itself, it says so (decided).** The units are installed
  only when the run that installs a box -- bootstrap, or `box migrate apply` on a host
  with none -- is root on a host whose systemd is up. Anywhere else --
  a container, a non-root run -- that run prints one line: box will not check for
  updates by itself, and `box update --timer` from cron every 15 minutes is the same
  job. A seedbox that never updates on its own is the failure this exists to prevent,
  so it is said out loud rather than left to be noticed.

**When an app takes it (decided).** Each catalog app declares an apply policy, and the
default is to apply as soon as the release is taken. Plex defers while it has active
streams, and Jellyfin will do the same; the update applies at the first check with
none, so it can wait up to one interval after the last stream ends. Torrent activity
is never a reason to wait: a client is always busy, and there's no clean line between
busy and idle.

**Snapshot and rollback (decided for 0.5, mechanism proposed).** An unattended update
runs over forward-only database migrations, so 0.5 carries a narrow rollback ahead of
`box backup`. It covers one app over one image change box makes, in an `update` step or
an `apply`, not disaster recovery. A restart that pulls new bytes under a mutable tag is
not a change box made, and nothing snapshots it.

- **Only what moves is snapshotted.** A database migrates when its image changes, so a
  step snapshots the apps whose rendered image differs: the installed release under the
  installed values against what is about to be applied. That is the same comparison for
  `update` (the chart's pins move) and `apply` (a values pin moves, a tag or a digest;
  decided). A pin that is a tag alone moves when the tag does. A step that moves no image
  takes no snapshot. An `apply`'s copy is named like a step's, with the installed release
  on both sides (`<v>-<v>-<time>/`), so rollback finds the newest copy either way. An
  `apply` has no automatic rollback (decided): whoever runs it is there to run
  `box rollback`, and the apply ends by naming each copy it took and that command,
  whether it succeeded or not. Whether an app moves is read from what is deployed (its
  StatefulSet's image) against what this run renders, never from the values file. An
  app box can't name the exact bytes of (a tag whose pods report no digest, or
  instances that disagree) moves without a snapshot, and the plan says why before the
  confirm; an apply is never refused for it, because the apply that pins a digest is
  the way out (proposed).
- **What is copied is the chart's to say.** Each catalog entry may name a `snapshot:`
  list of globs relative to the app's config directory: the databases and the files
  that point at them (`*.db`, `*.db-wal`, `config.xml`), never caches, logs, media
  covers or Plex's metadata bundles. An app without the key has nothing to restore.
- **An app a hand pin holds doesn't move in an update.** Its image is the one values
  pin, whatever the chart's pin did, so an `update` step neither stops it nor copies it.
  An `apply` that changes the pin moves it like any other image change.
- **Snapshots can be turned off for an app (decided; where the switch lives is
  proposed).** A copy costs disk, and the app is down while it is made. With snapshots
  off for an app, a step that moves it takes no copy of it and the plan says so. That
  app can't be rolled back, by box or by hand: if its new image fails, it stays as it
  is and box says so.
- **Consistent, not live.** The moving apps are scaled to zero first, which the image
  change does anyway, so the copy is taken with no writer. It goes under the config
  root in `.box/snapshots/<from>-<to>-<time>/<app>/`, on the same filesystem as what it
  copies.
- **A snapshot records the image it was taken under (decided; the fields are
  proposed).** The copy and the image that wrote it belong together, and nothing else
  on the box remembers the image once the release has moved. Proposed: each snapshot's
  manifest names the image the app ran when it was copied and the image the step moved
  it to, both by digest. For an app pinned to a tag alone the chart names no digest, so
  box reads the one the pod ran from its `imageID` (proposed). A rollback reads the
  first to know what to hold the app at and the second to know what the hold avoids,
  whether box rolls back in the step or the user does a week later.
- **A rollback is per app: its snapshot and the image it had (decided).** Rolling an
  app back stops it, waits until its pods are gone, restores its snapshot (a file the
  globs match that the snapshot lacks, such as a new `-wal`, is removed) and holds the
  app at the image it ran before the step. The rest of the release stays where the
  step put it. An automatic chart release changes nothing but pins, so the app runs
  under the template it ran under before. After a maintainer's release the template
  may have changed, and the old image under it is a combination the e2e hasn't run.
- **There is no whole-release rollback in 0.5 (decided).** A failure that is the
  chart's own, in the wiring Job or a template, is the same on every box: the e2e's to
  catch and a new release's to fix.
- **An app that crashloops on its new image is rolled back by box (decided).** The
  health gate tells three cases apart. Every app is ready: the step passed. A moved
  app's container keeps exiting: box rolls that app back, and only that app. A moved app
  is running but not ready: box waits, because the Arrs open their web port only after
  their migration has finished; if the wait runs out, box stops, records it and
  announces it without touching the app. Only an update step rolls back: an app that
  fails later is announced and left alone, because a restore by then would cost
  everything it wrote since. Keeps exiting means the container has exited three times
  since the step.
- **Wiring runs once, after the gate (proposed).** The chart's wiring Job runs as a Helm
  hook and waits for every app, so it can't pass while a moved app keeps exiting, and
  Helm would wait on it until it failed. A step therefore installs the release with
  hooks off, the gate judges the moved apps, any that keep exiting are rolled back, and
  then one apply with hooks on runs the wiring against the images that stay. A wiring
  failure at that point is the chart's own, and stops the update.
- **Apps that migrate when they start get thirty minutes to start (decided).** The
  startup probe used to allow five minutes, after which the kubelet restarts the
  container, and a migration that outlasted it would count as a crashloop. Every app
  gets the thirty minutes, so neither box nor the chart keeps a list of which apps
  migrate: the probe only delays the liveness check, and a crash still shows at once.
- **A rollback holds the app until the chart moves past the image it left (decided).**
  The hold names the image the app was rolled back from. While the chart still pins that
  image, the app stays on the old one; when a release pins another, the hold lapses and
  the app moves with the release. Nothing is written into the values file. The hold
  lives with the seedbox, under `<config root>/.box/` beside the snapshots it refers to,
  so a user's own rollback can write it.
- **`box rollback <app>` does the same by hand (decided).** It is for a fault no gate
  sees. It uses the app's newest snapshot only and says how old that is before it
  changes anything, because everything the app wrote since is lost. By default it leaves
  the same hold, which lapses by itself. With `--pin` it writes an ordinary pin into
  values instead, which holds the app until the user removes it, like any hand pin. A
  pin is an override on whatever release is installed, so the release keeps updating
  around it.
- **A rollback is exact, and it never moves a user's pin forward (decided).** It puts the
  app on the tag and digest it ran, never a tag alone, so it can't drift. For an app the
  values pin, a hold can't apply (a pin outranks a hold), so `box rollback` refuses until
  the user passes `--pin`, which rewrites their pin to the exact image. It stays there
  until the user changes it: box doesn't
  know what "next" means for a version someone chose, and won't turn their choice into
  its own. `status` names the way out ("radarr: pinned to 6.1@sha256:… by a rollback; the
  chart ships 6.3, remove the pin to follow it"). Where the chart moved the app, the hold
  is what carries it past the failed version: the next release that pins another digest,
  a same-tag rebuild included, lapses it and the app tries again, once per release.
- **A failure box doesn't roll back stops the update (decided).** A failed apply or
  wiring gate, a failing app with snapshots off, a rollback that fails itself: box
  leaves things where they are, records the failed release and says where the box is.
  The timer won't take the failed release again; a newer release clears that. A step
  that fails before the release moves only starts the apps it stopped.
- **Two kept.** After a step passes, each app keeps its two newest snapshots.

**Notices (decided; what is posted is proposed).** A box that updates by itself has
to be able to tell someone who isn't looking. box can post to one address after every
update step (what moved, from what to what), when a step stops or an app is rolled
back, and when an app stops being healthy.

- **The address is root's, in `notices.url` (decided).** One URL, on the host: a user's
  own hook arrives with `adduser`, and values hold none in 0.5. `init` asks for it on
  root's first run -- optional, masked, last -- and writes it into that one line of
  root's file. No address means no notices, and no error.
- **What is posted is JSON (decided).** One body per event:
  `{"event", "box", "seedbox", "from", "to", "apps", "gate", "error", "command",
  "text", "content"}`. The optional fields are left out when they don't apply. `text`
  and `content` carry the same one sentence, so Slack and Mattermost (text) and Discord
  (content) each show box's words rather than a rendering of the fields, and a script
  reads the fields. An example: `box.example.com: s5n 0.5.1 → 0.5.2 (sonarr 4.0.14 →
  4.0.15)`. The events are `updated`, `rolled-back`, `stopped`, `unhealthy`, `healthy`.
- **Health is read on every wake, and only a change is posted (decided).** The first
  reading on a box that stores none is recorded silently; after that an app whose
  readiness flips, in either direction, is one notice. A changed reason is not a
  change. An unhealthy notice names `box rollback <app>` when the app moved in the last
  step and box has that step's copy of it, and the logs otherwise. There is no `box
  logs` yet, so the command it names is `kubectl -n <namespace> logs
  statefulset/<app>`.
- **Posting never fails an update (decided).** One attempt, ten seconds. A receiver that
  is down is one warning naming the host box posted to -- never the path or the query,
  which is the token -- and the update that already happened stays a success.

**What `status` shows in 0.5 (decided).** The first line only, from cheap reads -- no
registry, so it is quick from a shell prompt. One line, segments joined by ` · `:

1. the installed release, from Helm: `s5n 0.5.1`, or `s5n ? (cluster not reachable)`
   when the cluster can't be read (the rest of the line still prints). That read has
   ten seconds: an API server that does not answer in time is the same answer as one
   that refused, so the one-line command cannot hang;
2. the last update from `update.json`: `last update 0.5.0 → 0.5.1 Nov 15 03:12 ok`,
   `... failed (health)`, `... ok, radarr rolled back`, or `last update none`;
3. the next check: `timer off` (root's config), `timer not installed` (no unit on this
   host), `next check within 15m` (due or overdue), or `next check ~04:12`;
4. what's held, left out when nothing is: `held: box, release 0.5.2 (failed), radarr
   (rolled back from 6.3), sonarr (pinned)`.

`box status` takes `--record`, `--grant`, `--values` and `--kubeconfig`. The rest of
"status and doctor" stays proposed.

### The platform layer (decided)

Envoy Gateway, MetalLB, cert-manager, the login and k0s follow the same path as the
images. Their pins ship in the chart:

1. Renovate opens a pull request when an upstream release lands for a pinned component,
   on `main` and on `develop`.
2. CI runs the full e2e on it. On `main`, a patch or minor bump merges itself as soon as
   it is green, and the push cuts a chart patch release like any image bump. A major waits
   for the maintainer, and so do k0s (no CI job boots it, and its minor is a Kubernetes
   minor) and a 0.x pin's minor (MetalLB: semver lets a 0.x minor break).
3. Root's `update` takes the pins from the release it is moving to and upgrades the
   components box manages before it moves any seedbox, with doctor run before and
   after. A component box doesn't manage (`external`, or `auto` that found the admin's
   own) is never upgraded; it's probed against the release being moved to before that
   release is taken (`PLAN.md`, the probes). A failed probe is an error: the release is
   shown, not taken, and box names the component and what has to change first ("s5n
   0.5.0 needs Gateway API v1.3 CRDs; this cluster serves v1.2"). A version that passes
   the probes but differs from the pin is a warning in status and doctor: it works, but
   it's not what CI tested. A user's `update` never touches the platform and
   ignores the pins; a seedbox still on an older release runs on the newer platform
   until it moves.

Every version box installs lives in one file, `charts/s5n/platform.yaml`: k0s and each
platform component, with the chart or release it comes from. box reads it from the
release it pulled, the same way it reads `variants.yaml`, so bootstrap pulls a release
before it installs anything. The e2e scripts read the same file instead of carrying
their own copies, and Renovate watches it. That way the e2e always tests the versions a
release ships, one pull request moves one pin, and a cert-manager patch never needs a
new box. What box compiles in are the manifests it writes for those components, which
is why a pin that needs a manifest change raises `min-box` (see "How box versions
against the chart"). Versions that exist only for tests (the foreign Gateway scenario,
the local registry, Pebble) stay in the scripts, pinned by digest and watched by
Renovate too.

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
  intact, wiring green. `hack/e2e-box.sh upgrade` is that path from what the registry
  actually publishes: every published chart mirrored into the run's own registry, the
  published 0.6.0 box binary installed with chart 0.6.0, state put in (a setting in an
  Arr, a torrent in qBittorrent, the login), this tree's chart published above every
  published version, and `box update` from this tree's box. It takes exactly the steps the
  releases' `upgrade-from` floors require, and after them the release is this tree's
  chart, every app is ready, the wiring receipt is ok, and the setting, the torrent and
  the login are all still there.

## The catalog and box (decided)

Every seedbox-oriented app belongs in the catalog, and contributors are expected to
add most of them. Adding an app shouldn't need a box release.

box names no app today; wiring runs in the s5n image, which the chart pins. What's
still missing is the setup questions. The chart should ship a **catalog manifest**,
one entry per app:

- name and one-line description;
- whether it takes multiple instances;
- how a person gets into it: behind the box's one login (`login: portal`, the default),
  or signed in by the app itself (`login: own`), which exempts it from the login;
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

- Who assigns the torrent port with multiple users (see Scopes).
- **A rollback across a repository change.** A hold puts an app back on the tag and
  digest it ran, under the repository the release pins. A release that moves an app to
  a different repository therefore can't be rolled back that way: the old digest isn't
  in the new repository. Proposed: when the release still blesses the old repository
  as a variant, the hold selects that variant; when it doesn't, the plan says before
  the step that the app can't be rolled back.
- **A user's route under an Arr's `/api` or `/ping`.** Those paths skip the login because
  the Arr checks its own key, but a user's own HTTPRoute under one of them (a longer
  prefix, pointing at another app) is the more specific route and is served without a
  login. The chart's own routes use these paths, so refusing them the way `/auth` is
  refused would break it.
