# kluster — Implementation Checklist

Track what is scaffolded (stub), implemented (real logic), or not yet started.

Legend: ✅ done · 🔧 in progress · ⬜ not started · 🏗 stub only

---

## Scaffolding & Tooling

| Item | Status | Notes |
|---|---|---|
| Go workspace (`go.work`) | ✅ | `./kluster-lib`, `./kluster` |
| `kluster-lib` module init | ✅ | `github.com/bytepunx/kluster-lib` |
| `kluster` CLI module init | ✅ | `github.com/bytepunx/kluster` |
| Makefile (`build/test/vet/lint/tidy/install`) | ✅ | `make tools` installs golangci-lint |
| `.golangci.yml` | ✅ | errcheck, staticcheck, revive, govet, gofmt |
| `.gitignore` | ✅ | |
| `README.md` | ✅ | Install, local dev, CI/GitHub Actions, all commands documented |

---

## kluster-lib: Provider

| Method | Status | Notes |
|---|---|---|
| `Provider` interface | ✅ | `provider/provider.go` |
| `ClusterConfig` / `ClusterInfo` types | ✅ | `provider/provider.go` |
| `K3dProvider.Create` | ✅ | SimpleConfig → transform → validate → ClusterRun; rollback on failure |
| `K3dProvider.Delete` | ✅ | ClusterGet → ClusterDelete → kubeconfig cleanup (best-effort) |
| `K3dProvider.List` | ✅ | ClusterList; derives Running from ServerCountRunning; Age from node.Created |
| `K3dProvider.Kubeconfig` | ✅ | KubeconfigGet → clientcmd.Write |
| `K3dProvider.RESTConfig` | ✅ | Kubeconfig → RESTConfigFromKubeConfig |
| `KindProvider.Create` | ✅ | Auto-detect runtime (Docker/Podman); raises inotify limits via Docker exec after create |
| `KindProvider.Delete` | ✅ | Checks existence; delegates to kind.Delete |
| `KindProvider.List` | ✅ | ListNodes + Docker inspect for age |
| `KindProvider.Kubeconfig` | ✅ | kind.KubeConfig(name, false) → []byte |
| `KindProvider.RESTConfig` | ✅ | Kubeconfig → RESTConfigFromKubeConfig |

---

## kluster-lib: Addon

| Addon | Status | Notes |
|---|---|---|
| `Addon` interface | ✅ | `addon/addon.go` |
| `ClusterHandle` type | ✅ | RESTConfig, HelmClientFor, K8sClient, DynClient, RESTMapper, Config |
| `NewClusterHandle` constructor | ✅ | Wires k8s, dynamic, discovery, deferred REST mapper, helm factory |
| `ApplyManifest` helper | ✅ | `addon/apply.go` — SSA via dynamic client + deferred REST mapper |
| Registry (`Register` / `Get` / `All`) | ✅ | `addon/addon.go` |
| `cert-manager` | ✅ | Helm install (charts.jetstack.io), CRD enable, poll 3 deployments |
| `spire` | ✅ | spiffe/spire umbrella chart; polls StatefulSet + DaemonSet + Deployment; version-pinned |
| `signet` | ✅ | Signet's own OCI chart (`oci://ghcr.io/bytepunx/charts/signet`); pre-seeds auto-unseal master-key Secret + in-cluster CockroachDB; overrides SPIRE socket path/filename to match the `spire` addon's actual layout; `Ready()` confirms unseal via pod logs, not just Deployment-ready. Not in `versions.Catalog` (OCI, no index.yaml) |
| `traefik-tls` | ✅ | On k3d: configures bundled Traefik; on kind: Helm-installs Traefik first (hookOnly wait), then applies ClusterIssuer + Certificate + TLSStore |
| `rabbitmq` | ✅ | bitnami/rabbitmq, single-node, no persistence, polls StatefulSet |
| `dex` | ✅ | dex/dex chart, in-memory storage, static AuthStar client + admin user |
| `prometheus` | ✅ | prometheus-community/prometheus; polls prometheus-server Deployment |
| `grafana` | ✅ | grafana/grafana; pre-wired Prometheus + Loki + Tempo datasources |
| `loki` | ✅ | grafana/loki single-binary, no persistence, polls StatefulSet |
| `tempo` | ✅ | grafana/tempo single-binary, local storage, polls StatefulSet |
| `argocd` | ✅ | Helm install (argoproj.github.io/argo-helm); bcrypt admin password generated at install time; Traefik IngressRoute at `argocd.<trust-domain>`; disables built-in Dex; opt-in addon. `Ready()` polls `argocd-server` + `argocd-repo-server` Deployments and `argocd-application-controller` StatefulSet. Helm install timeout 15 min. **Known limitation:** IngressRoute exposes HTTP port 80 only; ArgoCD CLI gRPC requires `--plaintext --port-forward` workaround. |

---

## kluster-lib: Profile

| Profile | Status | Notes |
|---|---|---|
| `Profile` interface | ✅ | `profile/profile.go` |
| Registry (`Register` / `Get` / `All`) | ✅ | `profile/profile.go` |
| `spire` | ✅ | SPIFFE/SPIRE workload identity (not Signet itself). Applies ClusterSPIFFEID `kluster-workload` via SSA; trust domain from config |
| `signet` | ✅ | Requires `spire`; installs the `signet` addon; no-op Configure (spire profile's catch-all ClusterSPIFFEID already covers Signet's own workload) |
| `authstar` | ✅ | Requires `signet`; no-op Configure (pre-seeded via Helm values); note for future RabbitMQ/Dex wiring |
| `observability` | ✅ | prometheus + grafana; registered via init() |
| `tracing` | ✅ | loki + tempo; registered via init() |

---

## kluster-lib: Cluster Orchestration

| Item | Status | Notes |
|---|---|---|
| `Cluster` struct + `New` | ✅ | `cluster/cluster.go` |
| `topoSort` (DFS, cycle detection) | ✅ | Tested: linear chain, diamond dedup, cycle, unknown addon |
| `resolveProfiles` | ✅ | DFS topo over RequiresProfiles; cycle detection |
| `collectAddons` | ✅ | Deduped union from profiles (in order) + explicit extras; an extra name resolves as an addon first, only expanding as a profile's addon-group if no addon of that name exists (avoids the "spire"/"signet" addon-vs-profile name collision) |
| `Cluster.Up` — full flow | ✅ | Create → RESTConfig → handle → profiles → addons → Install/Ready → Configure |
| `Cluster.Up` — progress events | ✅ | `ProgressFunc` / `ProgressEvent`; PhaseSetup / PhaseAddon / PhaseProfile; elapsed times |
| `Cluster.Down` | ✅ | Delegates to provider.Delete; k3d removes Docker containers + all state |
| `Cluster.NewDefault` | ✅ | Populates from global registries populated by addon/profile init() |
| `Cluster.Status` | ✅ | Delegates to provider.List |
| `Cluster.Kubeconfig` | ✅ | Delegates to provider.Kubeconfig |

---

## kluster-lib: Chart Versions

| Item | Status | Notes |
|---|---|---|
| `versions.Catalog` | ✅ | cert-manager, spire, traefik, rabbitmq, dex, prometheus, grafana, loki, tempo |
| `versions.Fetch` | ✅ | HTTP GET index.yaml per repo; deduped by URL; latest stable via semver sort |
| `versions.Ensure` | ✅ | Load from disk or fetch+save; populates `For()` package state |
| `versions.For(addon)` | ✅ | Returns pinned version or `"> 0.0.0"` fallback; prints a warning every time it falls back, since an unpinned install is a silent supply-chain-window widening otherwise. `signet` always falls back — its chart is OCI-only (ghcr.io), which doesn't expose the index.yaml this package's `Fetch()` speaks; Helm's own registry client still resolves the install correctly against live tags, it's just not pinned by kluster's own file. |
| `versions.Save` / `Load` | ✅ | YAML at `~/.config/kluster/chart-versions.yaml` |

---

## kluster (CLI)

| Command | Status | Notes |
|---|---|---|
| `root` | ✅ | Cobra root; `--provider k3d\|kind` persistent flag; `resolveProvider()` helper |
| `up` | ✅ | Uses `NewDefault`; addons/profiles auto-registered via import side-effects |
| `down` | ✅ | Delegates to provider.Delete; prompts for confirmation (shows config source) when the cluster name came from a repo-local `./kluster.yaml` rather than `--name`; `--yes`/`-y` skips |
| `status` | ✅ | Tabwriter table with NAME / RUNNING / AGE columns |
| `kubeconfig` | ✅ | `--output` file write; `--merge` merges into ~/.kube/config and switches context |
| `use` | ✅ | Merges kubeconfig into ~/.kube/config, switches context, prints confirmation |
| `charts list` | ✅ | Reads cached versions file; tabwriter table; shows updated timestamp |
| `charts update` | ✅ | Fetches live; shows `prev → next` diff; saves file |
| Progress renderer | ✅ | TTY: braille spinner + ANSI color + elapsed times; non-TTY: plain lines |
| Config file (`kluster.yaml`) | ✅ | Viper-backed; search order: `--config` flag → `./kluster.yaml` → XDG; flag > file > default precedence |
| `setup` (prerequisite installer) | ✅ | Checks Docker + kubectl; installs kubectl via curl/Homebrew; Docker never auto-installed (instructions by distro); `--dry-run` flag |

---

## CI / Supply Chain

| Item | Status | Notes |
|---|---|---|
| `ci.yml` | ✅ | Runs on PRs + push to main: build/vet/test (`make build`, `make check`), `golangci-lint` (both modules, pinned to v1.64.8 — this repo's `.golangci.yml` is v1-schema, incompatible with golangci-lint v2's default config format), `govulncheck` (both modules) |
| `release-build.yml` / `release-please.yml` | ✅ | Actions pinned by commit SHA (not mutable tags); `release-build.yml` attests build provenance for `dist/*` via `actions/attest-build-provenance` |
| `install.sh` | ✅ | Checksum verification is now mandatory (aborts if unavailable, undownloadable, or missing an entry — previously silently skipped); installs via `install -o root -g root -m 0755` instead of `mv`, so the binary isn't left owned by the invoking user |
| `govulncheck` findings (run 2026-07-05) | 🔧 | **GO-2026-5746** (`github.com/docker/docker` — `PUT /containers/{id}/archive` executes container binary on host) in both modules, pulled in transitively via k3d/kind's Docker client use. **Fixed in: N/A** — no patched version exists upstream yet. `ci.yml`'s govulncheck step is `continue-on-error: true` for this reason (a permanently-red required check trains people to ignore CI); revisit once a fix ships. |

