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
shares, so it lives in the grant with the Service type, and `box adduser` picks a free
one.

## Logins (decided, mechanism proposed)

**One login per seedbox user, checked at the edge (decided).** A person signs in once,
with their account name, and reaches every app that has an admin UI: qBittorrent, the
Arrs, Prowlarr, qui. One auth provider checks the login at the Gateway. It is part of the
platform layer: root's `bootstrap` installs it, and `box adduser` adds the user to it.
Each user's domain admits only that user. Behind it the apps have no login of their own
(the Arrs run with `External` authentication) and are reachable only through the Gateway.

**One auth mechanism, no fallback (decided).** Wiring a login into each app separately
would be a second auth system with its own reset path, tests and docs, so box doesn't
build one. That is why Envoy Gateway is the only Gateway implementation box supports.

**The password is never stored where it can be read back (decided).** The provider keeps
a hash and nothing else does. A forgotten password is reset, not recovered. `init` (and
`adduser`) asks for one; an empty answer means box generates a random one and shows it
once, at the end of the run, the way swizzin always has. Credentials
between apps (API keys, the qBittorrent account the Arrs use) are generated, kept in the
release Secret, and never the person's password.

**Plex, Seerr and Tautulli sign in through Plex (decided).** Players talk to Plex
directly, and Seerr's users are the Plex server's friends, so these three stay outside
the provider. Skipping the Plex claim at `init` stays allowed, with a warning that
until the server is claimed, whoever signs in to Seerr first becomes its owner. A
migrated server that is already claimed needs no warning.

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

**The login is on root's Gateway, and users can't take it off (decided).** The
SecurityPolicy that sends requests to Authelia targets the `s5n` Gateway itself and lives
beside it in `s5n-gateway`, so every listener is behind the login unless root says
otherwise: each host's HTTPS listener, the shared port-80 listener, and any listener added
later. Every route attached to them is covered, including any route the user adds later.
Envoy Gateway lets a policy on a route override the Gateway's, so the grant denies users
any write to SecurityPolicies, and to Gateways so a user can't open a listener of their
own. Users still create their own HTTPRoutes, so the chart is unaffected. Exemptions live
in Authelia's own rules, which root owns: paths that check their own key (the Arrs' `/api`,
and `/ping` for health checks), Plex, Seerr and Tautulli, and the sign-in page itself.

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
| `box migrate` | root | adopts an existing install | D |
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
release newer than itself. The sums are written in the pull request that moves the pin,
and CI checks them against the published assets; Renovate can't write them, so a k0s
bump never merges itself.
The download goes to a temporary file beside `/usr/local/bin/k0s` and is hashed while it
is written; only a match is renamed into place, so an interrupted or tampered download
never lands where the unit would exec it. Then `k0s install controller --single`, `k0s
start`, and box waits until the API answers `/readyz` *and* a node is Ready through
`/var/lib/k0s/pki/admin.conf`: readyz turns true a moment before the node registers, and
the decision that follows refuses a cluster that lists no nodes. Only then is the cluster
probed for its distribution and platform layer and recorded — a cluster box started but
cannot reach is never written down as one box manages. Any failure after the binary is in
place says exactly what is left on the host and the command that removes it
(`k0s stop; k0s reset`), then stops. box never runs that reset itself: it would also
destroy a k0s installation box did not make. With the cluster up, bootstrap goes on to the
platform layer and `init` exactly as it does on a host that already had one.

**A record is checked, not trusted (decided).** A re-run reads
`/etc/swizzin/cluster.yaml` for what box decided — which cluster, who owns its
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

### Certificates (decided)

PLAN settles the goal: cert-manager, HTTP-01 through the Gateway when the domain can be
validated, self-signed otherwise. This is how box would do it.

- **What bootstrap installs.** cert-manager from its OCI chart at a pinned version, with
  its CRDs and Gateway API support switched on (`config.gatewayAPI.enabled`), in
  `cert-manager`. An existing cert-manager is used when its webhook answers, its version
  has the Gateway HTTP-01 solver, and its controller runs with Gateway API enabled. box
  reads that from the controller's config and never changes another install's flags. One
  that is present but has it switched off stops bootstrap and names the setting.
- **Issuers are root's.** Two ClusterIssuers box owns: `s5n-letsencrypt` (ACME, HTTP-01
  solver whose HTTPRoute names the `s5n` Gateway but no listener, so it attaches to
  every listener that admits `s5n-gateway`) and
  `s5n-selfsigned`. No ACME email: Let's Encrypt no longer sends expiry mail, so there is
  nothing to ask.
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
  still validates. `tls.issuer` in root's values forces either one.
- **Tested without the internet.** The e2e runs Pebble (Let's Encrypt's test ACME
  server) in the kind cluster and points a test issuer at it, so HTTP-01 through the
  Gateway is proven on every run. The self-signed path gets its own assertion.

- **A failed renewal doesn't block `apply`.** It shows in `status` and `doctor`, which
  name the host and cert-manager's reason. `apply` changes the seedbox, not the
  certificate, so refusing it would only get in the way of the fix.

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

**No maximum box version (decided).** A chart can't know which future box will break
it, so it declares no ceiling; each side states only the floor it knows. The chart
states `min-box`. box states its own floor in code, the oldest release it still reads,
and raises it when it deletes support for something an old release relies on. The
platform pins are part of that promise: box's compiled-in manifests must work with the
platform version every release back to its floor pins. That isn't hypothetical:
`update` replaces box before it moves the chart, so a new box runs against root's old
release between those two steps, and for longer when an update rolls back. A release
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

**Branches (decided).** `main` is what has been released; work lands on `develop`. Both
move by ordinary pull-request merges, not fast-forward only: CI runs on every push to
`main`, and a release is built from its tagged commit, so a straight line adds nothing.
A feature release merges `develop` into `main`; once CI is green on `main`, the
maintainer tags it there. After every release, `main` is merged back into `develop`, so
`develop`'s e2e always tests the images and pins users are running.

**Automatic chart releases (decided).** Renovate bumps the app images in `variants.yaml`
on `main` only; `develop` gets them through the back-merge. The platform pins are bumped
on both branches, so work on `develop` is built and tested against the platform it will
ship on. On `main`, a patch or digest bump merges itself as soon as the full e2e is
green; anything larger waits for the maintainer. Renovate groups the bumps that land in
one window into one pull request. There is no waiting period: the upstream projects
stage their own releases, and holding a build longer adds age, not testing. On every
push to `main`, if `main` has changed since the last `s5n-v*` tag and every change since
is one of those bumps, CI raises the chart's patch version and tags it. Any other change
on `main` waits for a maintainer's release, and the job names it. There is no off
switch in the repo: disabling the workflow in GitHub is the kill switch. Nothing is
released without a previous `s5n-v*` tag to build on, and nothing is published from a
push to `develop`.

What makes this safe is the e2e plus a rollback that really works. The Arrs migrate
their databases forward when they start and don't support running an older version
on a newer database, so stepping a chart back is not enough. `update` snapshots each
app's config before it moves a release, and a rollback restores the snapshot together
with the previous chart.

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

### migrate (decided in part, mechanism proposed)

`migrate` adopts an install box didn't make: its app configs, its torrent state and its
data. Everything it will change is written down before it changes anything.

- **Discovery comes first and writes a plan (decided).** migrate reads the source while
  it is still running: each app's config directory, the paths the apps saw (container
  mounts, or the paths the configs themselves name), the categories, and how the apps
  reach each other. It writes this to a plan file and changes nothing. When the source
  has to be torn down before box can install, for example a cluster box will replace
  with its own, the operator does that between discovery and apply, and the plan
  carries across what box needs. box never removes a cluster it didn't install.
- **Configs are copied, data is adopted (proposed).** Configs are copied into box's
  config root and rewritten there, so the source stays as the rollback. Data never
  moves, so hardlinks survive.
- **Existing category names are kept (decided).** The chart accepts the names a source
  already uses (`Movies`, `4K/Movies`); migrate renames nothing.
- **Ports go to the chart's defaults (decided).** Port changes are what broke moving a
  swizzin box. Copied configs get the chart's ports, bind addresses and URL bases, and
  references between the apps are rewritten to match.
- **Only local addresses are rewritten (decided).** This covers the connections the apps
  hold to each other: an Arr's download clients, Prowlarr's applications, Seerr's Arr
  servers, and the Arr, qBittorrent and Plex connections in any other catalog app. A
  connection is rewritten only when it points at localhost (`localhost`, `127.0.0.0/8`, `::1`) or
  at a name that is plainly inside the source cluster: a Service discovery found there,
  by its short name or as `<svc>.<ns>`, `<svc>.<ns>.svc` or
  `<svc>.<ns>.svc.cluster.local`. On localhost, the port says which app it was, from
  the ports the source's own configs set. Anything else (a LAN or public address, a
  domain, another machine) is left as it is and listed in the plan, because box can't
  know what it points at; the plan can map one by hand. Matching discovered Services
  rather than app names keeps box naming no apps.
- **Migrated connections are adopted by ID (decided).** Wiring adopts the objects it
  finds by name on its first run, so a migrated download client under a different
  name would get a second one created beside it. migrate records the IDs of the
  connections it rewrote (download clients, Prowlarr's applications) in wiring's
  ownership record, and wiring takes them over by ID: the client the source already
  had becomes the one box manages.
- **Shown before it's done; unattended runs only what was shown (decided goal,
  mechanism proposed).** The plan holds the rules: the path rewrite table, the port
  map, every host reference marked rewritten or left alone, what is copied where, and
  what is not migrated (workloads outside the catalog). Guided, migrate prints the
  changes with counts per file and table and asks once. Unattended,
  `box migrate --apply <plan> --yes` runs the plan and nothing else. Apply works from
  the stopped source, which will have moved on since discovery (new torrents, new
  history), so it recomputes the changes and refuses before its first write if any fall
  outside the plan's rules: a path prefix the table doesn't cover, a host reference the
  plan never classified. Guided, those become questions; unattended, the refusal names
  each one and the plan key that would settle it.

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

**The timer (decided).** box checks for a newer release once an hour by default, with
jitter so a fleet doesn't arrive at once. The interval is a key in root's config, and
the timer can be turned off there. It reads the tag list from ghcr anonymously; no
token is needed to learn that something moved.

**When an app takes it (decided).** Each catalog app declares an apply policy, and the
default is to apply as soon as the release is taken. Plex defers while it has active
streams, and Jellyfin will do the same; the update applies at the first check with
none, so it can wait up to one interval after the last stream ends. Torrent activity
is never a reason to wait: a client is always busy, and there's no clean line between
busy and idle.

### The platform layer (decided)

Envoy Gateway, MetalLB, cert-manager, the login and k0s follow the same path as the
images. Their pins ship in the chart:

1. Renovate opens a pull request when an upstream release lands for a pinned component,
   on `main` and on `develop`.
2. CI runs the full e2e on it. On `main`, a patch bump merges itself as soon as it is
   green, and the push cuts a chart patch release like any image bump. Minor and major
   bumps (a k0s minor is a Kubernetes minor; MetalLB is still 0.x) wait for the
   maintainer.
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
- **A user's route under an Arr's `/api` or `/ping`.** Those paths skip the login because
  the Arr checks its own key, but a user's own HTTPRoute under one of them (a longer
  prefix, pointing at another app) is the more specific route and is served without a
  login. The chart's own routes use these paths, so refusing them the way `/auth` is
  refused would break it.
