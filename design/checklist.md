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
| `traefik-tls` | ✅ | On k3d: configures bundled Traefik; on kind: Helm-installs Traefik first (hookOnly wait), then applies ClusterIssuer + Certificate + TLSStore |
| `rabbitmq` | ✅ | bitnami/rabbitmq, single-node, no persistence, polls StatefulSet |
| `dex` | ✅ | dex/dex chart, in-memory storage, static AuthStar client + admin user |
| `prometheus` | ✅ | prometheus-community/prometheus; polls prometheus-server Deployment |
| `grafana` | ✅ | grafana/grafana; pre-wired Prometheus + Loki + Tempo datasources |
| `loki` | ✅ | grafana/loki single-binary, no persistence, polls StatefulSet |
| `tempo` | ✅ | grafana/tempo single-binary, local storage, polls StatefulSet |
| `argocd` | ✅ | Helm install (argoproj.github.io/argo-helm); bcrypt admin password generated at install time; Traefik IngressRoute at `argocd.<trust-domain>`; disables built-in Dex; opt-in addon |

---

## kluster-lib: Profile

| Profile | Status | Notes |
|---|---|---|
| `Profile` interface | ✅ | `profile/profile.go` |
| Registry (`Register` / `Get` / `All`) | ✅ | `profile/profile.go` |
| `signet` | ✅ | Applies ClusterSPIFFEID `kluster-workload` via SSA; trust domain from config |
| `authstar` | ✅ | no-op Configure (pre-seeded via Helm values); note for future RabbitMQ/Dex wiring |
| `observability` | ✅ | prometheus + grafana; registered via init() |
| `tracing` | ✅ | loki + tempo; registered via init() |

---

## kluster-lib: Cluster Orchestration

| Item | Status | Notes |
|---|---|---|
| `Cluster` struct + `New` | ✅ | `cluster/cluster.go` |
| `topoSort` (DFS, cycle detection) | ✅ | Tested: linear chain, diamond dedup, cycle, unknown addon |
| `resolveProfiles` | ✅ | DFS topo over RequiresProfiles; cycle detection |
| `collectAddons` | ✅ | Deduped union from profiles (in order) + explicit extras |
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
| `versions.For(addon)` | ✅ | Returns pinned version or `"> 0.0.0"` fallback |
| `versions.Save` / `Load` | ✅ | YAML at `~/.config/kluster/chart-versions.yaml` |

---

## kluster (CLI)

| Command | Status | Notes |
|---|---|---|
| `root` | ✅ | Cobra root; `--provider k3d\|kind` persistent flag; `resolveProvider()` helper |
| `up` | ✅ | Uses `NewDefault`; addons/profiles auto-registered via import side-effects |
| `down` | ✅ | Delegates to provider.Delete |
| `status` | ✅ | Tabwriter table with NAME / RUNNING / AGE columns |
| `kubeconfig` | ✅ | `--output` file write; `--merge` merges into ~/.kube/config and switches context |
| `use` | ✅ | Merges kubeconfig into ~/.kube/config, switches context, prints confirmation |
| `charts list` | ✅ | Reads cached versions file; tabwriter table; shows updated timestamp |
| `charts update` | ✅ | Fetches live; shows `prev → next` diff; saves file |
| Progress renderer | ✅ | TTY: braille spinner + ANSI color + elapsed times; non-TTY: plain lines |
| Config file (`kluster.yaml`) | ✅ | Viper-backed; search order: `--config` flag → `./kluster.yaml` → XDG; flag > file > default precedence |
| `setup` (prerequisite installer) | ✅ | Checks Docker + kubectl; installs kubectl via curl/Homebrew; Docker never auto-installed (instructions by distro); `--dry-run` flag |

---

## Upcoming Features

### Config file (`kluster.yaml`)

A project-level YAML file users commit alongside their code. Eliminates the need to memorize or copy-paste flags across team members and CI scripts.

**File search order:**
1. Path from `--config` flag
2. `./kluster.yaml` (current working directory)
3. `$XDG_CONFIG_HOME/kluster/kluster.yaml` (user-level defaults)

**Schema:**

```yaml
# kluster.yaml
name: dev-signet
profile: signet
provider: k3d
trust-domain: dev.cluster.local
addons:
  - observability
k3s-version: ""           # optional pin; empty = latest stable
```

**Behavior:**
- CLI flags override file values: `flag > file > built-in default`
- `kluster up` with no flags and a valid `kluster.yaml` should just work
- `kluster down` reads `name` from config so `kluster down` alone tears down the project cluster
- Viper is already a dependency in the CLI module — use it for file loading + flag binding

**Implementation touches:**
- `kluster/cmd/root.go` — add `--config` flag; call `viper.SetConfigFile` / `viper.AutomaticEnv`; `viper.SetConfigName("kluster")` + search paths
- `kluster/cmd/up.go` — bind each flag to a Viper key with `viper.GetString` / `viper.GetStringSlice`; read defaults from Viper when flag not set
- `kluster/cmd/down.go` — read `name` from Viper when `--name` not provided (mark flag as not required when Viper is wired)

---

### ArgoCD addon (`argocd`)

Installs ArgoCD as an opt-in addon, pre-configured for local GitOps workflow testing.

**Chart:** `argo/argo-cd` from `https://argoproj.github.io/argo-helm`

**What it does:**
- Helm-installs ArgoCD into the `argocd` namespace
- Sets a deterministic admin password (e.g. `kluster-admin`) via bcrypt hash in Helm values — no secret extraction needed
- Creates a Traefik `IngressRoute` to expose the ArgoCD UI at `argocd.<trust-domain>`
- Waits for the `argocd-server` Deployment to have ≥1 ready replica

**Usage:**
```bash
kluster up --profile signet --addon argocd --name dev-gitops
```

**Implementation:**
- New file `kluster-lib/addon/argocd.go` implementing the `Addon` interface
- `Requires() []string` → `["traefik-tls"]` (needs Traefik IngressRoute CRDs + the default cert)
- Add `argocd` entry to `versions.Catalog` (repo: `https://argoproj.github.io/argo-helm`, chart: `argo-cd`)
- Helm values: disable built-in Dex (OIDC handled by the `dex` addon if present), single-replica server, pre-set bcrypt admin password hash
- `Ready()`: poll `argocd-server` Deployment for `ReadyReplicas >= 1`

---

### `kluster setup` (prerequisite installer)

A one-shot command that checks for required tools and installs anything missing. Lowers the barrier to first use — especially useful when onboarding new team members or configuring a fresh CI runner.

**What it checks / installs:**

| Tool | Check | Install method |
|---|---|---|
| Docker | `docker info` | Print distro-specific instructions only (too varied to auto-install) |
| k3d | `k3d version` | `curl` install script on Linux; `brew install k3d` on macOS |
| kubectl | `kubectl version --client` | `curl` from dl.k8s.io on Linux; `brew install kubectl` on macOS |

**OS detection:**
- Linux: read `/etc/os-release` for distro name; detect WSL2 via `/proc/version` containing `microsoft`
- macOS: `runtime.GOOS == "darwin"`; check for Homebrew at `/opt/homebrew/bin/brew` or `/usr/local/bin/brew`

**Behavior:**
- Prints a status line per tool: `✓ k3d v5.8.3` or `✗ k3d — not found, installing...`
- Skips anything already on `$PATH` at the required version
- Docker is never auto-installed — prints a clear message with distro-specific install links
- `--dry-run` flag: prints what would be installed without executing anything

**Implementation:**
- New file `kluster/cmd/setup.go` — all logic is CLI-side; no library changes needed
- Tool checks via `exec.LookPath` + version subprocess
- Install execution via `exec.Command` (curl pipe to bash, or brew)
- Platform detection via `runtime.GOOS` + `os.ReadFile("/etc/os-release")`
