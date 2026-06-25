# kluster

kluster provisions disposable, fully-configured Kubernetes clusters for integration testing and inner-loop development. It is the primary local testing substrate for [Signet](https://github.com/bytepunx/signet) and [AuthStar](https://github.com/bytepunx/authstar).

Rather than shipping raw Helm values and install scripts, kluster provides named **profiles** that install and configure an opinionated, ready-to-use cluster in a single command:

```
kluster up --profile signet --name dev-signet
```

Two cluster runtimes are supported:

| Provider | Flag | Best for |
|---|---|---|
| k3d (default) | `--provider k3d` | Local development — fast, lightweight, Docker-based |
| kind | `--provider kind` | CI pipelines — standard upstream Kubernetes |

Both providers are embedded as Go libraries. No runtime binaries need to be pre-installed beyond Docker.

---

## Prerequisites

- **Docker** — required by both providers

That's it. kluster uses the k3d and kind Go libraries directly and does not shell out to any external tools. Run `kluster setup` to verify your environment.

---

## Installation

**One-liner (macOS and Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/bytepunx/kluster/main/install.sh | bash
```

The script detects your platform, downloads the correct binary from the latest release, installs it to `/usr/local/bin`, and warns you if that directory is not in your `PATH`.

**From source:**

```bash
git clone https://github.com/bytepunx/kluster
cd kluster
make install       # go install ./kluster → $GOPATH/bin/kluster
```

**Verify prerequisites:**

```bash
kluster setup
```

---

## Local development (k3d)

k3d runs k3s inside Docker with no VM layer. It starts in seconds and is the default provider.

```bash
# Bring up a full Signet stack
kluster up --profile signet --name dev-signet

# Switch your local kubectl to the new cluster
kluster use dev-signet

# Check what's running
kluster status

# Tear it down when you're done
kluster down --name dev-signet
```

With optional addons:

```bash
kluster up --profile authstar --addon observability --addon tracing --name dev-authstar
```

---

## Project config file

Add a `kluster.yaml` to your project root and you never need to remember flags again:

```yaml
# kluster.yaml
name: dev-signet
profile: signet
provider: k3d
trust-domain: dev.cluster.local
addons:
  - observability
```

With this file in place, `kluster up`, `kluster down`, and `kluster use` all work without arguments. CLI flags always override the file.

kluster searches for the config file in this order:

1. Path given by `--config`
2. `./kluster.yaml` (current working directory)
3. `$XDG_CONFIG_HOME/kluster/kluster.yaml` (user-level defaults)

---

## CI (kind)

kind runs standard upstream Kubernetes inside Docker. Pass `--provider kind` to every command — it must match the provider used to create the cluster.

**GitHub Actions example:**

```yaml
jobs:
  integration:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Install kluster
        run: go install github.com/bytepunx/kluster/kluster@latest

      - name: Bring up cluster
        run: kluster --provider kind up --profile signet --name ci-signet

      - name: Export kubeconfig
        run: kluster --provider kind kubeconfig --name ci-signet --output ${{ runner.temp }}/ci-signet.yaml

      - name: Run tests
        run: go test ./...
        env:
          KUBECONFIG: ${{ runner.temp }}/ci-signet.yaml

      - name: Tear down cluster
        if: always()
        run: kluster --provider kind down --name ci-signet
```

---

## Profiles

Profiles declare a set of addons and any post-install configuration. They are composable — `authstar` builds on top of `signet`.

### `signet`

Installs everything required to run Signet, a SPIFFE-native configuration and secrets manager:

| Component | Purpose |
|---|---|
| **cert-manager** | TLS certificate lifecycle |
| **SPIRE Server + Agent** | SVID issuance and workload attestation |
| **SPIRE Controller Manager** | `ClusterSPIFFEID` CRD-driven registration |
| **Traefik** | Ingress with TLS termination via cert-manager |

SPIFFE trust domain defaults to `dev.cluster.local`. Override with `--trust-domain`.

```bash
kluster up --profile signet --name dev-signet --trust-domain myteam.local
```

### `authstar`

Composes the `signet` profile and additionally installs:

| Component | Purpose |
|---|---|
| **RabbitMQ** | Message broker (single-node + management UI) |
| **Dex** | Local OIDC provider for end-to-end auth flow testing |

```bash
kluster up --profile authstar --name dev-authstar
```

### Optional addons

Either profile accepts `--addon` flags for opt-in components:

| Addon | Components | Notes |
|---|---|---|
| `observability` | Prometheus + Grafana | |
| `tracing` | Loki + Tempo | |
| `argocd` | ArgoCD | UI at `argocd.<trust-domain>`; admin password: `kluster-admin` |

```bash
kluster up --profile signet --addon observability --addon tracing --name dev-full
kluster up --profile signet --addon argocd --name dev-gitops
```

---

## Command reference

### `kluster setup`

Checks for required tools and installs anything missing. Run this once after installing kluster.

```
kluster setup [--dry-run]
```

```
  ✓ docker 29.2.1
  ✓ kubectl v1.33.0
```

Docker is never auto-installed — clear instructions are printed based on your OS and distro. kubectl is installed via Homebrew on macOS or `curl` on Linux.

| Flag | Description |
|---|---|
| `--dry-run` | Print what would be installed without making changes |

---

### `kluster up`

Creates a new cluster and installs the requested profile.

```
kluster up [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--name` | *(from `kluster.yaml` or required)* | Cluster name |
| `--profile` | `signet` | Profile to install: `signet`, `authstar` |
| `--addon` | | Additional opt-in addons: `observability`, `tracing`, `argocd`. Repeatable. |
| `--trust-domain` | `dev.cluster.local` | SPIFFE trust domain |
| `--k3s-version` | latest stable | k3s version tag (k3d only; ignored by kind) |

Progress is displayed with per-step timing. On a warm machine with cached images, `signet` takes roughly 3 minutes.

---

### `kluster down`

Destroys a named cluster and removes its kubeconfig entry.

```
kluster down [--name <name>]
```

| Flag | Default | Description |
|---|---|---|
| `--name` | *(from `kluster.yaml` or required)* | Cluster name |

---

### `kluster status`

Lists all clusters managed by the active provider.

```
kluster status
```

Output:

```
NAME          RUNNING  AGE
dev-signet    yes      14m
dev-authstar  yes      2h
```

---

### `kluster use`

Merges the cluster's kubeconfig into `~/.kube/config` and switches the active context. This is the fastest way to point `kubectl` and other tooling at a kluster cluster.

```
kluster use <name>
```

```bash
kluster use dev-signet
# Switched to context "k3d-dev-signet".
```

To switch back to a different context afterwards:

```bash
kubectl config use-context <other-context>
```

---

### `kluster kubeconfig`

Outputs or merges the kubeconfig for a named cluster.

```
kluster kubeconfig --name <name> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--name` | *(from `kluster.yaml` or required)* | Cluster name |
| `--output` | stdout | Write kubeconfig to a file |
| `--merge` | false | Merge into `~/.kube/config` and switch context (same as `kluster use`) |

```bash
# Print to stdout
kluster kubeconfig --name dev-signet

# Write to file (useful in CI)
kluster kubeconfig --name ci-signet --output /tmp/ci-signet.yaml
```

---

### `kluster charts list`

Shows the currently pinned Helm chart versions.

```
kluster charts list
```

Output:

```
ADDON          VERSION
cert-manager   v1.16.3
spire          0.23.1
traefik        35.2.0
argocd         7.8.0
rabbitmq       16.3.4
dex            0.20.0
prometheus     27.5.0
grafana        8.10.3
loki           6.26.0
tempo          1.16.0

Updated 2026-06-24 21:30 UTC
```

If no versions file exists yet, kluster prints guidance and fetches automatically on the next `kluster up`.

---

### `kluster charts update`

Fetches the latest stable version of every chart from its upstream repository and saves the result. Changed versions are shown with `→` arrows.

```
kluster charts update
```

Output:

```
Checking for updated chart versions...
  cert-manager   v1.16.2  →  v1.16.3
  spire          0.23.1
  traefik        35.2.0
  ...

1 chart(s) updated.
Saved to ~/.config/kluster/chart-versions.yaml
```

---

## Chart version pinning

On first run, kluster fetches the latest stable version of each chart and caches it at:

```
~/.config/kluster/chart-versions.yaml
```

All subsequent `kluster up` invocations use the cached versions, ensuring reproducible installs across your team. The file is safe to commit to a dotfiles repo.

Run `kluster charts update` when you want to pull in newer chart versions.

---

## Global flags

These flags apply to every command:

| Flag | Default | Description |
|---|---|---|
| `--provider` | `k3d` | Cluster runtime: `k3d` (local) or `kind` (CI) |
| `--config` | `./kluster.yaml` | Config file path |

The `--provider` flag must be consistent across all commands for the same cluster. A cluster created with `--provider kind` must be managed with `--provider kind`.
