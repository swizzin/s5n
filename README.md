# s5n

swizzin on Kubernetes. A free, self-hosted seedbox stack: qBittorrent, Prowlarr,
Sonarr, Radarr — installed as one Helm release, wired together, hardlink-safe, and
locked down by default.

Status: Phase 1. The chart installs, and its wiring Job connects qBittorrent, Sonarr,
Radarr and Prowlarr on every install or upgrade — then proves the `/data` hardlink
contract and records what it verified. The e2e runs that Job twice and asserts the second
run changes nothing. Seerr, the media servers, the `box` CLI and the installer are next.
See `PLAN.md` for the design and `AGENTS.md` for how to work here.

```sh
hack/e2e-kind.sh   # build, install everything on kind, assert, tear down
```

The e2e keeps its kubeconfig in `${XDG_CACHE_HOME:-~/.cache}/s5n-e2e/kubeconfig`
(override with `S5N_E2E_KUBECONFIG`) and never reads or writes `~/.kube/config`, so
your current context survives a run; after `--keep`, reach the cluster with
`KUBECONFIG=${XDG_CACHE_HOME:-~/.cache}/s5n-e2e/kubeconfig kubectl --context kind-s5n ...`.

## AI disclosure

s5n is built with substantial help from AI coding assistants (Anthropic's Claude and
DeepSeek). The maintainer directs the design, reviews the work, and is responsible for
every commit. `AGENTS.md` is the brief those assistants work from, and it's public on
purpose.

## Licence

GPL-3.0, like swizzin. See `LICENSE`.
