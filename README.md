# kluster

kluster provisions disposable, fully-configured Kubernetes clusters for integration testing and inner-loop development. It is the primary local testing substrate for [Signet](https://github.com/bytepunx/signet) and [AuthStar](https://github.com/bytepunx/authstar).

Rather than shipping raw Helm values and install scripts, kluster provides named **profiles** that install and configure an opinionated, ready-to-use cluster in a single command:

```
kluster up --profile spire --name dev-spire
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

That's it. Cluster lifecycle and addon installation use the k3d/kind and Helm Go SDKs directly and never shell out. (The optional `kluster setup` helper below is the one exception — it shells out to check/install prerequisites like `kubectl`.) Run `kluster setup` to verify your environment.

---

## Installation

**One-liner (macOS and Linux):**

```bash
curl -fsSL https://raw.githubusercontent.com/bytepunx/kluster/main/install.sh | bash
```

The script detects your platform, downloads the correct binary from the latest release, verifies its checksum, installs it to `/usr/local/bin`, and warns you if that directory is not in your `PATH`.

Every release binary also carries a [build provenance attestation](https://github.com/bytepunx/kluster/attestations), verifiable independently of the checksum:

```bash
gh attestation verify dist/kluster-<os>-<arch> -R bytepunx/kluster
```

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
# Bring up SPIFFE/SPIRE workload identity (a prerequisite for Signet)
kluster up --profile spire --name dev-spire

# Switch your local kubectl to the new cluster
kluster use dev-spire

# Check what's running
kluster status

# Tear it down when you're done
kluster down --name dev-spire
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
name: dev-spire
profile: spire
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

**Host state note:** kind nodes are privileged containers that share the host kernel. On cluster creation, kluster raises `fs.inotify.max_user_instances`/`max_user_watches` inside the node — which, because the kernel is shared, raises them on the **host**, not just the container. This persists until reboot and is not reverted by `kluster down` (other kind clusters on the same machine may depend on the raised limit). A one-line notice is printed when this happens.

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
        run: kluster --provider kind up --profile spire --name ci-spire

      - name: Export kubeconfig
        run: kluster --provider kind kubeconfig --name ci-spire --output ${{ runner.temp }}/ci-spire.yaml

      - name: Run tests
        run: go test ./...
        env:
          KUBECONFIG: ${{ runner.temp }}/ci-spire.yaml

      - name: Tear down cluster
        if: always()
        run: kluster --provider kind down --name ci-spire
```

---

## Profiles

Profiles declare a set of addons and any post-install configuration. They are composable — `signet` builds on top of `spire`, and `authstar` builds on top of `signet`.

> **`spire` vs. `signet`:** the `spire` profile installs the SPIFFE/SPIRE workload
> identity substrate; the `signet` profile installs [Signet](https://github.com/bytepunx/signet)
> itself on top of it. `spire` was previously (and confusingly) named `signet`
> before this distinction existed.

### `spire`

Installs SPIFFE/SPIRE workload identity — a prerequisite for Signet, not Signet itself:

| Component | Purpose |
|---|---|
| **cert-manager** | TLS certificate lifecycle |
| **SPIRE Server + Agent** | SVID issuance and workload attestation |
| **SPIRE Controller Manager** | `ClusterSPIFFEID` CRD-driven registration |
| **Traefik** | Ingress with TLS termination via cert-manager |

SPIFFE trust domain defaults to `dev.cluster.local`. Override with `--trust-domain`.

**Dev-only identity scope:** by default every workload in every namespace (other than kluster's own infra: `kube-system`, `spire-system`, `cert-manager`, `monitoring`, `argocd`, `dex`, `rabbitmq`) automatically receives an SVID — no per-workload registration needed. No production trust domain issues identities this broadly; this exists purely so a test workload just works without extra setup.

```bash
kluster up --profile spire --name dev-spire --trust-domain myteam.local
```

### `signet`

Composes the `spire` profile and installs Signet itself, pulled directly from Signet's published OCI Helm chart:

| Component | Purpose |
|---|---|
| **Signet** | SPIFFE-native configuration and secrets management |
| **CockroachDB** | In-cluster, single-node (dev-only; disable for a real backend) |

Fully unattended: kluster generates Signet's master key and audit-chain key and pre-seeds the Kubernetes Secret Signet's auto-unseal mode reads, so the deployment comes up already unsealed — no `signet init` step required. Not for production use; it exists purely so a local test cluster is immediately usable.

**Dev-only posture:** CockroachDB runs single-node and insecure (root, no TLS); the audit-chain key is passed via Helm values, so it's readable via `helm get values signet -n signet`. The master key that actually gates access to encrypted secrets is never passed this way — it's pre-seeded directly as a Kubernetes Secret and never travels through Helm.

```bash
kluster up --profile signet --name dev-signet
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

Any profile accepts `--addon` flags for opt-in components. `observability` and `tracing` are addon *groups* (themselves backed by a profile) rather than a single addon — `--addon` resolves either kind by name:

| `--addon` value | Installs | Notes |
|---|---|---|
| `observability` | Prometheus + Grafana | addon group |
| `tracing` | Loki + Tempo | addon group |
| `argocd` | ArgoCD | UI at `argocd.<trust-domain>`; admin password: `kluster-admin` |

```bash
kluster up --profile spire --addon observability --addon tracing --name dev-full
kluster up --profile spire --addon argocd --name dev-gitops
```

**Reaching `argocd.<trust-domain>` from your host:** kluster doesn't map any host port to Traefik or touch `/etc/hosts`, so that hostname isn't resolvable or reachable out of the box. Port-forward Traefik directly and add a hosts entry pointing at loopback:

```bash
kubectl port-forward -n kube-system svc/traefik 8443:443 &
echo "127.0.0.1 argocd.dev.cluster.local" | sudo tee -a /etc/hosts
```

Then browse to `https://argocd.dev.cluster.local:8443`. This is a rough edge in the current tooling (no k3d port mapping, no hosts-file automation), called out here so the documented login flow is actually followable.

---

## Default credentials — dev only

Every addon below installs with a fixed, well-known credential. This is fine for a disposable local cluster where every service is `ClusterIP` (not reachable off-host — see the k3d API-server binding note above) and the only way in is through your own kubeconfig, but don't reuse these anywhere that matters:

| Service | Credential | Notes |
|---|---|---|
| RabbitMQ | `guest` / `guest` | Management UI |
| Grafana | `admin` / `admin` | |
| Dex | `admin@example.com` / `password` | Static user for OIDC flow testing |
| Dex OAuth client | `authstar` / `authstar-dev-secret` | Fixed — AuthStar's config depends on this exact value |
| ArgoCD | `admin` / `kluster-admin` | UI at `argocd.<trust-domain>` |

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
| `--profile` | `spire` | Profile to install: `spire`, `signet`, `authstar` |
| `--addon` | | Additional opt-in addons: `observability`, `tracing`, `argocd`. Repeatable. |
| `--trust-domain` | `dev.cluster.local` | SPIFFE trust domain |
| `--k3s-version` | latest stable | k3s version tag (k3d only; ignored by kind) |

Progress is displayed with per-step timing. On a warm machine with cached images, `spire` takes roughly 3 minutes.

---

### `kluster down`

Destroys a named cluster and removes its kubeconfig entry.

```
kluster down [--name <name>] [--yes]
```

If the cluster name was resolved from a `./kluster.yaml` in the current directory (rather than an explicit `--name`), `down` prompts for confirmation and shows where the name came from — cloning a repo containing a `kluster.yaml` shouldn't be able to silently steer a `down` at whatever cluster name it declares. Pass `--yes` to skip the prompt (CI/scripts); an explicit `--name` always skips it too.

| Flag | Default | Description |
|---|---|---|
| `--name` | *(from `kluster.yaml` or required)* | Cluster name |
| `--yes`, `-y` | `false` | Skip the confirmation prompt |

---

### `kluster status`

Lists all clusters managed by the active provider.

```
kluster status
```

Output:

```
NAME          RUNNING  AGE
dev-spire     yes      14m
dev-authstar  yes      2h
```

---

### `kluster use`

Merges the cluster's kubeconfig into `~/.kube/config` and switches the active context. This is the fastest way to point `kubectl` and other tooling at a kluster cluster.

```
kluster use <name>
```

```bash
kluster use dev-spire
# Switched to context "k3d-dev-spire".
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
kluster kubeconfig --name dev-spire

# Write to file (useful in CI)
kluster kubeconfig --name ci-spire --output /tmp/ci-spire.yaml
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

All subsequent `kluster up` invocations on that machine use the cached versions, ensuring reproducible installs over time — the file is per-machine, generated whenever it's first missing, so two teammates who each first ran `kluster up` a week apart will have different pins out of the box. To actually share pins across a team, commit `~/.config/kluster/chart-versions.yaml` somewhere your team pulls from (a dotfiles repo, or your project repo with `--config`-style tooling) and have everyone use that copy.

Note also that any addon not in `versions.Catalog` (currently just `signet`, since its chart is OCI-only — see the `signet` profile section above) is never pinned by this file at all; it always resolves to the latest published chart.

Run `kluster charts update` when you want to pull in newer chart versions.

---

## Global flags

These flags apply to every command:

| Flag | Default | Description |
|---|---|---|
| `--provider` | `k3d` | Cluster runtime: `k3d` (local) or `kind` (CI) |
| `--config` | `./kluster.yaml` | Config file path |

The `--provider` flag must be consistent across all commands for the same cluster. A cluster created with `--provider kind` must be managed with `--provider kind`.
