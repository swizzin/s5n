# AGENTS.md — working on s5n

Read `PLAN.md` first. It is the architecture; this file is how to work in the repo.

## What exists (Phase 0 done, Phase 1 in progress)

- `charts/s5n/` — the chart. qBittorrent, Prowlarr, Sonarr, Radarr (HD + optional 4K
  instance per Arr). Storage claims, generated credentials Secret, StatefulSets,
  Services, HTTPRoutes, NetworkPolicies, and `values.schema.json`, which rejects unknown
  keys, floating image tags and malformed values before anything renders.
- `cmd/s5n/` — the one binary the image ships. Subcommands: `wiring` (the Job),
  `qbt-config` (qBittorrent's init container), `version`. It stays stdlib-only: `box`
  gets its own binary, `cmd/box/`, in the same module, so the Helm SDK and client-go
  never reach the distroless image the Job and the init container run.
- `internal/apps/` — schema-driven HTTP clients for qBittorrent (`/api/v2`), Sonarr and
  Radarr (`/api/v3`) and Prowlarr (`/api/v1`). Every `Ensure…` reads live state, writes
  only what differs, and returns `Created`/`Updated`/`Unchanged`.
- `internal/wiring/` — the reconciler: config, steps, the hardlink proof on `/data` (one
  per distinct download/library pair, since a nested mount can break just one), and
  the receipt published into the `<release>-wiring` ConfigMap (`status.json`,
  `summary.txt`).
- `internal/qbtconf/` — qBittorrent 5 credential hashing and `qBittorrent.conf` editing.
- `internal/seedbox/` — the grant/values contract from `docs/box.md`: loads both files,
  merges them with the grant winning, validates against a chart release's `values.yaml`
  + `values.schema.json`, diffs for `apply`, and edits by splicing one key's bytes so
  comments and indentation survive. `grant.go` is the only list of what the grant owns:
  a new values key that decides isolation needs a rule there, and
  `TestGrantKeysAreRealKeys` fails if a rule names a key the chart does not have. Only
  this package, `internal/admission` and `internal/catalog` may import `go.yaml.in/yaml/v3`, and only this
  package the JSON-schema library; `cmd/s5n` must stay stdlib-only.
- `internal/admission/` — the admission contract root writes. `Render` turns one grant
  (namespace, torrent nodePort, Service type) into the params ConfigMap in `s5n-system`
  and the two bindings, reading the blessed list from a chart's `variants.yaml`;
  `UnblessedImages` applies the policies' image rule to pods that already exist, because
  a binding never re-checks a running pod. Pure: no client-go. Its goldens are the
  output of the Python renderer it replaced, and one renders the real `variants.yaml`,
  so re-blessing an image fails a test on purpose.
- `charts/s5n/catalog.yaml` — the catalog manifest: one entry per app under `apps:`
  (description, whether it takes instances, whether init preselects it, its setup
  questions). Chart-owned beside `variants.yaml`; values cannot override it. A question
  names a user-owned values path only, never a grant key, and its kind (`string`,
  `secret`, `bool`, `transient`) must match `values.schema.json`.
- `internal/catalog/` — reads that manifest as untrusted input: strict decode, 256 KiB
  cap, exactly one entry per app in `values.yaml`, every question key a schema leaf the
  grant does not own. Errors name the entry.
- `internal/setup/` — `box init`'s decision over `Prompter` (the UI seam) and `PathHost`
  (stat, statfs, mountinfo, mkdir, chown). A re-run asks with the current answers filled
  in and splices only changed keys; nothing is written until the last question; a cancel
  writes nothing and says "oh heck, nvm". `LinePrompt` is init's plain implementation.
- `internal/say/` — box's one output seam. The flow packages emit events (a phase, a
  plan item, a step that starts and finishes, a timed wait with its budget, a confirm,
  a wiring receipt, a final card) and a reporter decides how they look. `say.Plain` is
  the byte-for-byte transcript every non-TTY, `--plain` and `TERM=dumb` run reads;
  `internal/tui` is the live one. Dependency-free on purpose: bootstrap and platform
  may not import a TUI.
- `internal/tui/` — init's dialogs and the live reporter: huh behind `setup.Prompter`,
  with box's masthead, accent and step header drawn by box, and the same events above
  drawn with a spinner, wrapped plans and the success card. The only package that
  imports huh, lipgloss or bubbletea. `tui.Decide` picks the UI: no terminal on stdin is
  unattended, `TERM=dumb`, `--plain` or a piped stdout get `LinePrompt` and `say.Plain`,
  `NO_COLOR` keeps the form and the live layout without colour.
- `internal/release/` — `box apply`'s decision over `ChartSource`, `ReleaseClient` and
  `Confirmer`, with the Helm v4 + client-go implementation in `helm.go`. With
  `internal/platform` and `internal/bootstrap` (clientcmd for the prober and the k0s
  readiness check), the only packages that import Helm or client-go; only `cmd/box`
  links them. It creates a missing
  seedbox namespace (Pod Security `restricted`, managed-by `box`) before the first
  volume, and removes only the volumes it created if a run fails before the release.
  box pulls its chart by version from `oci://ghcr.io/swizzin/charts/s5n` (`oci.go`,
  `chart.go`); `minbox.go` is the chart's `box.s5n/min-box` rule, refused before any
  cluster client opens.
- `internal/bootstrap/` — the cluster decision, and on a clean host the k0s
  installer: the release and its sha256 per architecture are pinned in `policy.go`, the
  download is verified and renamed into `/usr/local/bin/k0s`, then `k0s install
  controller --single` and `k0s start`, and bootstrap waits for `/readyz` *and* a Ready
  node before it records the cluster. After a failure it names what it left and the
  undo (`k0s stop; k0s reset`); it never runs the reset. A record is re-probed, never
  trusted: the full probe chain and every gate run again on the cluster it names. An
  explicit path never replaces it: `--kubeconfig`/`KUBECONFIG` naming the record's own
  kubeconfig is the recorded re-run, and any other file stops before a probe or write,
  naming the record file (`RecordStore.RecordPath()`). Candidates carry
  `DistributionOwned` (`source.go`): a distribution-owned kubeconfig that exists but
  fails `/version` or the SSAR blocks the install, while a dead `~/.kube/config` or a
  dry run's remote skip does not. The host scan includes the kubelet binary and unit.
  The prober and the readiness check load kubeconfigs through clientcmd (`kubeClient`),
  so tokens, tokenFile, client certificates and exec plugins work as under kubectl and
  as for box's writers; a kubeconfig with no current-context is refused by name, and
  box never picks a context.
- `internal/platform/` — bootstrap's write half. It applies only what the decision says
  to install: Envoy Gateway from its OCI chart, MetalLB (the node's InternalIP as a /32
  pool, L2 advertisement), the GatewayClass and Gateway, the admission policy. It prints
  the plan, checks RBAC with SelfSubjectAccessReview, asks once, then writes with
  bounded readiness waits. A release already at its pin is verified, never upgraded, so
  a re-run makes no Helm revision. cert-manager is not installed yet and bootstrap says
  so. Every chart fetch (platform or apply) runs Helm on a private, empty home: settings
  built as a literal inside a 0700 temp dir removed when the fetch ends. Never
  `cli.New()`, never `repo.FindChartInRepoURL` (it writes into helmpath's
  environment-derived cache), so root's Helm environment can't redirect, cache or break
  what box installs.
- `internal/fetch/` — the retrying transport for every bootstrap download: GET/HEAD
  only, on 5xx/429 or a refused, reset or timed-out connection, four attempts at
  1s/2s/4s, ctx honoured. A 404 or a certificate error is the answer on the first try.
- `install/platform/` — the manifests box installs (`gateway/`, `admission/`), each
  with its `embed.go` beside the file, because go:embed can't reach a sibling tree. The
  bytes box compiles in are the bytes the e2e applies; product code never imports
  `hack/`.
- `cmd/box/` — the host CLI. `bootstrap` decides the cluster, installs k0s when there
  is none, applies the platform layer, then runs init for root in the same process
  with init's flags (`--record` moves the record off `/etc/swizzin/cluster.yaml` for the
  non-root e2e); `init` asks a seedbox's questions, writes the two files and hands over
  to `apply`; `apply` loads root's grant and values, checks them against the chart's
  schema, diffs against the deployed release, confirms, installs or upgrades release
  `s5n` in the grant's namespace, and prints the wiring receipt.
- `Dockerfile` — static binary on distroless; the image the wiring Job and the
  qBittorrent init container run.
- `hack/grab-test.sh` — the end-to-end grab: a local mock indexer (`hack/mock-indexer.py`,
  `hack/make-torrent.py`) serves one synthetic torrent, Prowlarr syncs it to Sonarr,
  Sonarr grabs it into qBittorrent, and the test completes the download on disk so
  Sonarr does a real import. It asserts `nlink == 2` on the imported file. Opt-in, not
  part of CI: it needs TMDB/TVDB metadata, and everything it handles is synthetic
  (ffmpeg noise filed under a public-domain work).
- `hack/e2e-kind.sh` — the acceptance test. Creates a kind cluster, installs Envoy
  Gateway (which brings the Gateway API CRDs) and MetalLB (L2, with a small pool carved
  from the kind docker network), enforces Pod Security `restricted`, builds and loads the
  image, installs the chart, then asserts: pods Ready, routes Accepted, every app answers
  through the Gateway's LoadBalancer address, the peers Service gets its own
  LoadBalancer address and accepts a TCP connect, `/data` hardlinks work, NetworkPolicy
  allows Prowlarr→Sonarr and blocks an outside pod, the wiring Job connects everything,
  and a second `helm upgrade` performs **zero writes** with app state identical before
  and after. The 4K Radarr runs on category `4K/Movies`, so a nested category meets real
  qBittorrent and a real Arr root folder on every run. `--keep` leaves the cluster up.
- `hack/e2e-box.sh` — the same install done by box instead of helm, through `box
  bootstrap` on three kind clusters: `fresh` (box installs the whole platform),
  `preinstalled` (MetalLB already there, so box adds none) and `existing` (Gateway API
  v1.6.3 + NGINX Gateway Fabric 2.8.1, used, not replaced, an app reached through the
  rewrite with `Host: box.example.com`). Each asserts a second `--yes` run changes no
  Helm revision, spec or label (spec and labels, not resourceVersion: controllers
  rewrite status). `fresh` also runs init's no-op re-run, a one-key `box apply` edit
  and a `helm uninstall` that keeps the claims. `all` (the default) runs the three and
  prints each wall time. The chart is packaged from the same commit, pushed to a
  distribution registry container on 127.0.0.1, and pulled by box through
  `--chart-repo`/`--chart-version`; there is no local-chart flag. box under test is
  built with `-X main.version=<chart version>`, and the run fails on the dev-build
  notice. Not in CI yet.
- `hack/render-grant/` — `internal/admission` as a command, the e2e's caller until
  `box adduser` exists.
- `.github/workflows/ci.yaml` — every PR and push to main: gofmt/vet/`go test -race`,
  `hack/validate-chart.sh` on 1.30/1.34/1.37, then the kind e2e.
  Actions are pinned by commit SHA.
- `.github/workflows/release.yaml` — a `v*` tag publishes the image (amd64 + arm64,
  digest captured), the chart with that digest stamped into the packaged copy and
  validated before `helm push`, then box's binaries and `sha256sums.txt` as a GitHub
  release. It fails unless Chart.yaml's version and appVersion equal the tag. Nothing
  publishes from a branch.

## Commands

```sh
gofmt -l . && go vet ./... && go test ./...
hack/validate-chart.sh 1.30.0  # helm lint + kubeconform for every charts/s5n/ci/*-values.yaml
docker build -t s5n:e2e .     # what the e2e uses
hack/e2e-kind.sh              # full smoke; needs docker, kind, helm, kubectl
hack/e2e-box.sh [fresh|preinstalled|existing]  # through box bootstrap; same needs
```

Run lint + kubeconform against 1.30, 1.34 and 1.37 before every commit. Run the e2e before
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
   (`/data/downloads/<category>`, `/data/media/<category>`) and its config at `/config`
   via `subPath: <instance-name>`. Never add a second data mount, a remote path mapping,
   or a per-app data volume. That is what keeps hardlinks working. The claims are not
   the chart's: `storage.{data,config}.existingClaim` name claims that already exist, and
   `storage` accepts nothing else (no hostPath, storage class or size). box creates the
   static PV and claim from the grant's `storage.<kind>.path`; a plain `helm install`
   brings its own. No CSI or provisioner: a volume per claim splits `/data`. A category
   is one qBittorrent category and one directory name in each tree: letters, digits,
   `.`, `_`, `-`, and at most one `/` for a subcategory (`4K/Movies`). It lives only in
   `wiring.json`, never in an object name, label or port name; the wiring Job creates
   the intermediate directory in both trees, owned like the leaf. The schema accepts
   the names an existing seedbox already uses, because migrate renames nothing.
4. **Pod Security `restricted` must admit every pod.** No hostPath in pod specs (the only
   hostPath is in the static PV box creates), no hostPort, no hostNetwork, no privileged,
   no root. Security contexts come only from `s5n.podSecurity` / `s5n.containerSecurity`
   in `_helpers.tpl`; do not write ad-hoc ones in templates.
5. **One pod spec.** `workloads.yaml` is the only place an app pod spec is written.
   Per-app differences are values (`apps.<name>`), or a small `if eq $inst.app` branch when
   unavoidable. Adding an app = a values entry plus its `apps.<name>` definition in
   `values.schema.json`. `s5n.instances` derives the workload list from those keys, so
   no template edit is needed for a generic app; an app that acts like an Arr also
   needs its kind in `internal/wiring` and its peers in `networkpolicy.yaml`. The wiring Job and the qBittorrent init container are separate
   templates because they are not app replicas.
6. **Images are blessed variants, pinned by tag + digest.** Each app's image comes from
   `charts/s5n/variants.yaml` (chart-owned, read with `.Files.Get`), never from values.
   `apps.<app>.image.variant` names one and may pin a tag or digest inside that variant's
   repository; `values.schema.json` rejects `repository`. No `:latest`, no `:rolling`,
   no keel, no arbitrary registry.
7. **No secrets in values, ConfigMaps, the repo, or logs.** Credentials live in the Secret
   (`secret.yaml`, preserved across upgrades via `lookup`). The wiring config points at
   *file paths* in the mounted Secret, never at values. One exception, by design: a
   *transient* answer (the Plex claim token, which expires minutes after issue) reaches
   one apply through `release.Options.AfterConfirm`, is never written to either file and
   is dropped from diffs by `Options.TransientKeys`. Helm stores the values it renders,
   so it sits in the release Secret until the next apply.
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
11. **The wiring Job owns narrowly.** It owns only objects it created, and on them only
    the data-contract fields (host, port, credentials, category, save paths, hardlink
    import). An update is the *live* object with those fields overlaid, never a fresh
    schema template, so a user's other settings survive every upgrade. Objects the user
    added are never touched. Ownership is the object id recorded in the `<release>-wiring`
    ConfigMap (`owned.json`): on the first run an existing object with the expected name
    is adopted, and after that the id is authoritative, so a rename is the user's
    business. A drifted owned field is restored and the receipt names the old and new
    value; a deleted owned object is recreated and reported. If the ConfigMap is gone,
    fall back to adoption by name. A failed *read* of it is not "gone": the run stops
    and the existing record is left alone, because adopting by name after an API blip
    would duplicate a renamed client and then record the duplicate.
    **Never use tags as the ownership marker.** In Sonarr and Radarr a tag on a download
    client restricts it to series/films carrying that tag, and in Prowlarr a tag on an
    application restricts which indexers sync to it. A marker tag would silently stop
    grabs and indexer sync.
12. **The grant is enforced in the cluster, not only by box.** Root installs
    `install/platform/admission/policy.yaml` at bootstrap and binds both policies per user
    namespace at adduser, with params in a ConfigMap in the root-owned `s5n-system`
    namespace: never the user's namespace, never a global binding (root's own seedbox is
    unbound). The blessed repository list is generated from `variants.yaml`; there is no
    second list in Go, `install/` or docs. Changing a policy or its params means changing
    the e2e admission asserts in the same commit.
13. **box apply writes only what it showed.** The files are loaded, merged and checked
    before a cluster client exists, so a refusal (grant key in values, schema violation,
    no chart, no cluster) opens no connection, and an empty diff returns before Helm is
    called. The diff compares the effective values (chart defaults, then values, then
    grant) with the values Helm stored on the deployed release. Tests assert the client
    was never opened, not just the message. A first install's plan names the namespace
    box will create; a volume this run wrote is removed if the run fails before the
    release, and a pre-existing volume never is.

## Gotchas already hit

- box trusts the kubeconfig path `box bootstrap` recorded, not the cluster behind it:
  changing that file's current-context retargets `apply`. No cluster identity check yet.
- A release named `s5n` that box did not install is adopted, and Helm stored only the
  flags it was given, so the first apply shows box's whole model as added. A release
  left `failed` makes `apply` refuse (`exists but is not deployed`): recover with
  `helm rollback`, never by deleting release Secrets.
- The chart's `lookup "v1" "Endpoints"` (`templates/networkpolicy.yaml`) makes client-go
  print a deprecation line through klog. `internal/release` installs `rest.NoWarnings`
  so box's output stays its own; `hack/e2e-box.sh` greps apply output for klog lines.
- A ValidatingAdmissionPolicy takes effect a moment after its binding is created and
  never re-checks running objects. An e2e that asserts a denial right after `kubectl apply`
  races the binding: poll for a dry-run denial. A pod admitted before the binding keeps
  running, but its next replacement is refused.
- A VAP resource rule for `pods` does not match `pods/ephemeralcontainers` (`kubectl
  debug`); name the subresource. On it, `object` is the whole Pod, so a pre-binding pod
  still running an unblessed image cannot be updated or debugged until it is replaced.
- CEL in a VAP is type-checked against every kind in `matchConstraints`; one policy can't
  read Service and Pod fields behind a ternary without permanent warnings. One policy per
  resource shape.
- `--set apps.sonarr.instances[1].enabled=true` replaces the whole list, so the render
  fails the schema; use a values file (`charts/s5n/ci/`). And kubeconform reports
  "0 resources" as success, including for a path without a `.yaml` extension.
  `hack/validate-chart.sh` fails unless every rendered object was validated.
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
- qBittorrent's image re-applies its own save path at runtime, so seeding
  `qBittorrent.conf` is not enough: a fresh install still reports `/config/Downloads`,
  which is on the config volume and can never be hardlinked. The wiring Job sets
  `save_path`, `temp_path` and `use_category_paths_in_manual_mode` through
  `app/setPreferences` after the app is up, then reads them back and fails if they did
  not stick (`setPreferences` answers 200 either way). Paths are compared exactly, not
  by prefix: `/data/media` is under `/data` and still wrong. That step is what makes
  the data contract true rather than aspirational.
- The session keys are QSettings-escaped in the file: `[Preferences]` with
  `Session\DefaultSavePath` (two backslashes on disk), not a `[Session]` section.
- Never retry a rejected qBittorrent login: five failures ban the client IP for an hour,
  and the ban answers 403 on everything, which looks like a routing fault.
- NetworkPolicy is evaluated *after* kube-proxy DNATs a Service, so the wiring Job's
  API-server rule needs the `kubernetes` Endpoints IPs and the endpoint port (6443 on
  k0s/k3s/kind, 443 behind a load balancer), not the ClusterIP alone. The chart looks up
  both and falls back to `networkPolicy.apiServerCIDRs`.
- Secrets mounted into a pod are owned root:fsGroup, so `defaultMode: 0440` is what a
  non-root process can read. `0400` locks it out.
- Prowlarr does not default an indexer's `enable` to true on create, and a disabled
  indexer is silently absent from the Arr's searches — no error, no results.
- Sonarr and Radarr reject a release whose parsed title is not the series/film being
  searched for, and refuse to import a file with no audio track or a runtime far below
  the episode's (it reads as a sample). A test fixture has to look like the real thing.
- The wiring Job is a Helm hook, so it is not a tracked release resource:
  `helm upgrade --wait` waits for it, `helm rollback` does not revert its ConfigMap
  changes, and `helm uninstall` leaves `<release>-wiring` behind.
- Pod template labels must not duplicate keys from `s5n.labels`
  (`app.kubernetes.io/instance` is already there).
- Host directories must exist and be owned by puid:pgid *before* install; the PV uses
  `type: Directory`. `box apply` stats both paths and refuses before any write if one is
  missing; `--paths-in-node` skips that only for the kind e2e, whose paths are inside the
  node. `box init` creates them on real hosts: it asks first in guided mode (a disk that
  didn't mount leaves an empty directory on `/`) and refuses a missing path unattended.
- A kubelet creating a missing `subPath` makes it root-owned, so the app starts and can't
  save its settings. `box init` creates `<config>/<instance>` for every enabled instance,
  owned puid:pgid; the data categories stay the wiring Job's, which creates them as puid
  before qBittorrent or any Arr is touched (`internal/wiring/dirs.go`), because an Arr
  refuses a root folder whose directory doesn't exist. One receipt line per directory,
  `ok (created)` or `ok (exists)`.
- The hardlink proof doesn't create the directories it checks. A proof that prepared its
  own subject is what hid the empty-data-path first install. `hack/e2e-*.sh` pre-create
  only what `box init` makes on a real host (the data and config roots,
  `config/<instance>`), and both fail if the category trees exist before install.
- `box init --puid/--pgid` track whether the flag was given: the default comes from the
  invoking user, and a re-run keeps the grant's ids instead of re-chowning to whoever ran
  it.
- A fresh cluster has no seedbox namespace. `apply` checks with a SelfSubjectAccessReview
  and refuses before any write when the kubeconfig can't create one (a user's is
  namespace-scoped; root makes it at `adduser`). An existing namespace is never
  relabelled; one whose `enforce` isn't `restricted` gets a warning, not a change.
- huh's accessible mode silently returns an empty secret when its reader isn't a real
  terminal, so box never uses it. A huh form knows nothing of the flow: box prints the
  masthead, step counter and breadcrumb itself, and Esc has to be added to
  `huh.KeyMap.Quit`.
- `os.File.ReadDir(n)` with n > 0 reports the end of a directory as `io.EOF`, not nil;
  a probe that doesn't swallow it reads every empty directory as a failure.
- The e2e scripts own their kubeconfig (`$S5N_E2E_KUBECONFIG`, default
  `~/.cache/s5n-e2e/kubeconfig`): kind rewrites the file it's given and leaves no
  current-context after a delete, so they never touch `~/.kube/config`. Point
  `KUBECONFIG` at that file to reach a `--keep` cluster.
- MetalLB's webhook endpoint can be Ready while the server still refuses connections, so
  the e2e retries the pool apply rather than trusting the endpoint.
- The LoadBalancer probe is the Gateway's own status. After applying the Gateway, box
  waits up to 60s for Programmed=True with an address and decides only at the deadline:
  Envoy reports NoResources while its proxy starts, with the address already assigned,
  so an intermediate reason is never acted on. No address at the deadline
  (AddressNotAssigned) installs MetalLB; anything else stops bootstrap.
- An address isn't a working platform. box sends an HTTP GET to `http://<address>:80/`
  from the host and stops, naming the address, if nothing answers; an empty Envoy's 404
  passes.
- The feature the chart's routes need is `HTTPRoutePathRewrite` (URLRewrite with
  ReplacePrefixMatch); there is no `HTTPRouteURLRewrite`. A class whose feature list
  lacks it stops bootstrap; a class with no list stops unless
  `platform.gateway.assumeFeatures` / `--gateway-assume-features` is set. Envoy Gateway
  1.9.3 publishes no list, so box trusts only the class it labelled managed-by=box.
- box always applies its own Gateway `s5n-gateway/s5n` on the class it chose. It writes
  GatewayClass `eg` only when it installs Envoy Gateway and never rewrites another
  controller's class. Platform Helm releases carry `app.kubernetes.io/managed-by=box`;
  an unmarked release of the same name is the operator's and is never upgraded.
- A recorded GatewayClass is kept only while the cluster still serves it (Accepted, with
  `HTTPRoutePathRewrite`). A vanished non-`eg` class, or a missing class while another
  implementation is accepted, stops rather than moving the routes; a vanished `eg` with
  no accepted class is box's own implementation missing, and is reinstalled. A record
  whose kubeconfig no longer answers stops naming it, and never falls through to the
  k0s installer. The record path never rewrites the record.
- A fresh k0s node is Ready only after its kube-router CNI pod starts, so readiness is
  `/readyz` plus a Ready node; `/readyz` alone is early.
- k0s in a container whose root is overlayfs needs `/var/lib/k0s` on a real filesystem:
  containerd's overlay snapshotter can't nest (`mount source: overlay … invalid
  argument`).
- box never changes a static PV in place. A PV of box's name that backs another path or
  claim, or a claim pinned to another volume, is refused with both sides named. The
  pre-claims chart named its PV `<ns>-<fullname>-<kind>`, the same as box's for release
  `s5n`, but bound to the old claim, so an existing install refuses until `migrate`
  adopts it.
- The torrent port is a LoadBalancer Service by default (31111): with MetalLB on the
  platform layer it gets its own address, no hostPort. NodePort stays available (it must
  then sit in the cluster's NodePort range, 30000–32767 by default); `nodePort` is only
  rendered for NodePort.
- The Gateway's proxy runs wherever its implementation puts it: Envoy Gateway in the
  controller's namespace (`envoy-gateway-system`), NGINX Gateway Fabric v2 in the
  Gateway's own. So `networkPolicy.gatewayFrom` is box's (a key no file may set),
  derived on every apply from the Service holding the Gateway's address; a node address
  becomes an ipBlock /32. Never assume `envoy-gateway-system`.
- MetalLB `allow-shared-ip` cannot share one address between the Gateway's Service and
  the peers Service the way PLAN once hoped. Envoy Gateway's proxy Service and the peers
  Service are both `externalTrafficPolicy: Local` (needed to keep real peer source IPs),
  and MetalLB only shares an IP between Services that are both `Cluster` or select the
  exact same pods. Both-Local with different selectors → the second Service gets
  `AllocationFailed` ("can't change sharing key … address also in use by …") and no IP.
  Both-`Cluster` shares the IP, but kube-proxy SNATs peer traffic at the node (conntrack
  shows the pod replying to the node's IP, not the peer's), so qBittorrent loses real
  peer IPs. The e2e gives the two Services separate pool addresses; a single-IP VPS must
  solve this some other way.
- Sonarr/Radarr/Prowlarr's `PUT`/`POST` on `downloadclient`/`applications` run the same
  validation as the UI's save button — a warning (e.g. an unused but still-present field,
  or a value the schema flags but does not hard-reject) can fail the request outright. The
  real API accepts `?forceSave=true` on those calls to save anyway, same as the UI's own
  "Save Anyway" prompt; the e2e's hand-edits through the app APIs use it so a test
  fixture's quirks do not block the thing actually under test.
- Overlaying owned fields onto the *live* object (instead of a fresh schema template) means
  a masked secret (`********`) is never read as the live value — `MatchesFields`'s masked-value
  skip still matters, but the overlay itself goes one step further and never even compares a
  secret field; it always writes the spec's real value over whatever was there, masked or not.
  The one failure mode to watch for is the reverse: never write the *masked placeholder* back
  as if it were the secret. Since the overlay source of truth for a secret field is always
  `spec.Password`/`spec.APIKey`, not the live value, this cannot happen by construction — but
  it is exactly the invariant the apps-level tests assert directly (`internal/apps/arr_test.go`'s
  `TestEnsureDownloadClientNeverLeaksMaskedSecret`), because the overlay is new code and it
  would have been easy to accidentally route the live (masked) value back into the PUT body.
- Helm's chart-repo getter takes only a concrete `*http.Transport` and builds its own
  client, so its retry wraps `Getter.Get` and reads the status from Helm's error text.
  `TestGetterStatusReadsTheStatusHelmReturns` pins that wording, so a Helm bump fails a
  test instead of silently disabling retries. `Retry-After` is honoured only where box
  sees headers (k0s download, OCI); on the chart-repo path the backoff is box's own.
- Helm's registry client still opens `$DOCKER_CONFIG/config.json` when it is built (no
  option disables it); the anonymous authorizer means nothing from it is ever sent.
- `go.mod` carries `distribution/v3` for the in-process OCI registry in
  `TestOCIRoundTrip` only. Its ~45 indirect requirements must never reach
  `go list -deps ./cmd/...`.
- `--dry-run` refuses a non-loopback host in the prober's `get`/`post`, before the
  first request and therefore before client-go runs an exec plugin. Keep the guard
  there, not in a transport wrapper.
- clientcmd sends no credentials (token, certificate, exec) to a plain `http://`
  server, exactly as kubectl does. A fake API over HTTP proves reachability, not
  authentication; auth tests need TLS.
- Which kubeconfigs are distribution-owned is `source.go`'s answer. Don't add path
  matching in `decision.go`.

## Next work

The proof of concept: box takes a fresh host from nothing to a running, wired stack.
The code path exists (k0s, platform layer, init, wiring on an empty data path). What's
left:

1. A run on a clean VM. The image, the chart and box's binaries are published from
   `v*` tags and pull anonymously, so a host needs no checkout and no side-loaded image.

Then: cert-manager on the platform layer, `hack/e2e-box.sh` in CI, the upgrade e2e,
and `adduser`. The product decisions are
in `docs/box.md`, marked decided, proposed or open.

In parallel, outside this repo: `swizzin/qbittorrent`, the userdocs static recipe plus
`build/libtorrent/` patches, published as `<qbit>-lt<libtorrent>` for amd64 and arm64.

Keep commits small. Each commit: lint + kubeconform green; chart-changing commits also
e2e green.
