# kluster — Design Document

## Overview

`kluster` is a local Kubernetes cluster automation tool purpose-built to support development and integration testing of **Signet** and **AuthStar**. It provisions fully-configured, feature-complete local clusters in a single command, with all identity, secrets, TLS, and messaging infrastructure pre-installed and wired together.

The project is split into two Go modules:

- **`kluster-lib`** — the automation library containing all cluster and addon logic
- **`kluster`** — a thin CLI shell built with Cobra that delegates all work to `kluster-lib`

This separation is intentional and enforced: the CLI contains no automation logic. All cluster lifecycle operations, Helm installs, and Kubernetes resource management live exclusively in `kluster-lib`, making the library independently usable and testable.

---

## Runtime: k3d (k3s in Docker)

### Why k3d

k3d was selected as the primary cluster runtime for the following reasons:

- **Docker-native lifecycle.** k3d runs k3s inside Docker containers, requiring no VM layer on any platform. Clusters are created and destroyed in seconds.
- **Go library.** k3d is written in Go and exposes its cluster lifecycle as importable packages (`github.com/k3d-io/k3d/v5`). kluster-lib calls k3d directly — no `exec.Command` shelling out.
- **k3s compatibility.** k3d wraps k3s, so all k3s features (Traefik, Flannel CNI, local-path-provisioner, embedded SQLite) are available. SPIFFE/SPIRE, cert-manager, and all planned addons work without shimming.
- **Multi-cluster support.** Multiple named clusters can coexist on the same machine, each with independent kubeconfigs. This is native to k3d but requires manual VM setup in bare k3s.
- **CI portability.** The same Docker-based approach works in CI environments. A `kind` provider may be added later as a CI-optimized alternative.

### Why not bare k3s

Bare k3s requires a VM layer on macOS and Windows and does not support multiple clusters without significant manual configuration. Since kluster targets local developer machines as the primary use case, k3d is strictly superior.

### Why not kind

kind is designed for testing Kubernetes itself and produces the most vanilla Kubernetes experience. However, SPIFFE/SPIRE workload attestation has known surface area issues in Docker-in-Docker topologies that kind uses. k3d avoids these by running k3s in a more conventional container topology. kind may be added as a secondary provider for pure-CI scenarios where SPIRE is not required.

### k3d provider implementation notes

The `K3dProvider` in `provider/k3d.go` uses the k3d Go library (`github.com/k3d-io/k3d/v5`) directly. No `exec.Command` anywhere.

**Create flow:**
1. Build a `k3dconf.SimpleConfig` (v1alpha5) with name, image (`docker.io/rancher/k3s:<version>`), `Servers: 1`, and `Wait: true` with a 5-minute timeout.
2. `config.ProcessSimpleConfig` — handles host-network edge cases.
3. `config.TransformSimpleToClusterConfig` — expands nodes, attaches load balancer, resolves API exposure opts.
4. `config.ProcessClusterConfig` — final sanitisation (volume shortcut expansion, etc.).
5. `config.ValidateClusterConfig` — pre-flight checks against the Docker runtime.
6. `k3dclient.ClusterRun` — creates containers, waits for the server to be ready.
7. On any failure after node creation begins, rolls back with `ClusterDelete`.

**K3sVersion default:** uses `k3dversion.K3sVersion` (hardcoded in the k3d module at build time, e.g. `v1.32.5-k3s1`). Callers can override via `ClusterConfig.K3sVersion`.

**Logger:** k3d uses a package-global logrus logger. `K3dProvider` silences it to `WarnLevel` at package init so k3d's info-level chatter doesn't bleed into the CLI's output. Callers that want verbose k3d output can raise it: `k3dlogger.Logger.SetLevel(logrus.InfoLevel)`.

**Kubeconfig / RESTConfig:** `KubeconfigGet` fetches the kubeconfig from the server container's `/output` path, then `clientcmd.Write` serialises it to bytes. `RESTConfig` calls `Kubeconfig` and passes the bytes to `clientcmd.RESTConfigFromKubeConfig`.

---

## Language: Go

Both `kluster-lib` and `kluster` are written in Go. The decisive factors:

- **k3d is natively importable in Go.** Cluster lifecycle is a library call, not a subprocess.
- **Helm SDK is Go-native.** `helm.sh/helm/v3` exposes the full Helm engine as a library. `github.com/mittwald/go-helm-client` provides a clean wrapper. No shelling out to `helm` CLI.
- **client-go is the canonical Kubernetes client.** `k8s.io/client-go` gives direct, typed access to the Kubernetes API for applying manifests, managing CRDs, and watching resource readiness.
- **TypeScript lacks equivalents for the critical pieces.** There is no TypeScript-native k3d or kind library; cluster lifecycle would require subprocess calls. There is no Helm SDK for TypeScript; chart installation would require subprocess calls or REST. The TypeScript Kubernetes client (`@kubernetes/client-node`) is mature but narrower in scope.

The TypeScript ecosystem is strong for building workloads that run *inside* Kubernetes. Go is the correct choice for building tooling that *creates and manages* Kubernetes clusters.

---

## Architecture

### Layer model

```
┌─────────────────────────────────────────────────────┐
│                   Named Profiles                    │
│         signet | authstar | observability           │
├─────────────────────────────────────────────────────┤
│               Platform Addons (opt-in)              │
│   spire | cert-manager | traefik-tls | rabbitmq     │
│           dex | prometheus | grafana                │
│                   loki | tempo                      │
├─────────────────────────────────────────────────────┤
│                    Core (always)                    │
│     CNI (Flannel) | CoreDNS | metrics-server        │
│         local-path-provisioner | kubeconfig         │
├─────────────────────────────────────────────────────┤
│               Distribution Runtime                  │
│           k3d (primary) | kind (CI, future)         │
└─────────────────────────────────────────────────────┘
```

**Core** is installed unconditionally by the k3d provider as part of cluster bootstrap. k3s ships with Flannel, CoreDNS, metrics-server, and local-path-provisioner built in; no additional installation step is needed.

**Addons** are discrete installable units, each implementing the `Addon` interface. They declare prerequisites, own their Helm values and manifest templates, and report readiness.

**Profiles** are named compositions of addons. They declare which addons are required and provide profile-specific configuration (Helm values overrides, registration entries, scaffolded CRs). Profiles may depend on other profiles.

**Named Profiles** are the user-facing entrypoints: `signet`, `authstar`, `observability`, `tracing`.

### Module structure

```
kluster/
├── kluster-lib/
│   ├── go.mod
│   ├── provider/
│   │   ├── provider.go           # Provider interface
│   │   ├── k3d.go                # k3d lifecycle implementation
│   │   └── kind.go               # kind provider (future)
│   ├── addon/
│   │   ├── addon.go              # Addon interface + registry
│   │   ├── certmanager.go        # cert-manager Helm addon
│   │   ├── spire.go              # SPIRE Server + Agent + Controller Manager
│   │   ├── traefik.go            # Traefik TLS via cert-manager
│   │   ├── rabbitmq.go           # RabbitMQ + management UI
│   │   ├── dex.go                # Dex OIDC provider
│   │   ├── observability.go      # Prometheus + Grafana
│   │   └── tracing.go            # Loki + Tempo
│   ├── profile/
│   │   ├── profile.go            # Profile interface + dependency resolver
│   │   ├── signet.go             # Signet profile
│   │   ├── authstar.go           # AuthStar profile (depends on signet)
│   │   ├── observability.go      # Observability opt-in profile
│   │   └── tracing.go            # Tracing opt-in profile
│   └── cluster/
│       ├── cluster.go            # Cluster struct + orchestration
│       └── config.go             # ClusterConfig
└── kluster/
    ├── go.mod
    └── cmd/
        ├── root.go
        ├── up.go
        ├── down.go
        ├── status.go
        └── kubeconfig.go
```

---

## Core Interfaces

### Provider

The `Provider` interface abstracts the cluster runtime. The k3d implementation calls the k3d Go library directly; a future kind implementation would call `sigs.k8s.io/kind`.

```go
// provider/provider.go
type Provider interface {
    // Create provisions a new cluster with the given config.
    Create(ctx context.Context, cfg ClusterConfig) error

    // Delete tears down a named cluster and removes its kubeconfig entry.
    Delete(ctx context.Context, name string) error

    // List returns all clusters managed by this provider.
    List(ctx context.Context) ([]ClusterInfo, error)

    // Kubeconfig returns the kubeconfig bytes for a named cluster.
    Kubeconfig(ctx context.Context, name string) ([]byte, error)

    // RESTConfig returns a *rest.Config for use with client-go and Helm SDK.
    RESTConfig(ctx context.Context, name string) (*rest.Config, error)
}

type ClusterConfig struct {
    Name        string
    K3sVersion  string            // e.g. "v1.31.0-k3s1"
    TrustDomain string            // SPIFFE trust domain, default "dev.cluster.local"
    Profiles    []string          // profile names to activate
    Addons      []string          // additional addon names beyond profile defaults
}

type ClusterInfo struct {
    Name    string
    Running bool
    Age     time.Duration
}
```

### Addon

Each addon is a self-contained installable unit. It declares its prerequisites, so the orchestrator can resolve installation order.

```go
// addon/addon.go
type Addon interface {
    // Name returns the unique identifier for this addon (e.g. "cert-manager").
    Name() string

    // Requires returns the names of addons that must be installed before this one.
    Requires() []string

    // Install deploys this addon into the cluster.
    Install(ctx context.Context, cluster ClusterHandle) error

    // Uninstall removes this addon from the cluster.
    Uninstall(ctx context.Context, cluster ClusterHandle) error

    // Ready blocks until the addon is healthy, or the context is cancelled.
    Ready(ctx context.Context, cluster ClusterHandle) error
}

// ClusterHandle gives addons access to the Kubernetes and Helm clients
// without coupling them to the full Cluster struct.
type ClusterHandle struct {
    RESTConfig *rest.Config
    HelmClient helmclient.Client
    K8sClient  kubernetes.Interface
}
```

### Profile

A profile is a named composition of addons plus profile-specific post-install configuration.

```go
// profile/profile.go
type Profile interface {
    // Name returns the unique identifier for this profile (e.g. "signet").
    Name() string

    // Requires returns the names of other profiles that must be installed first.
    RequiresProfiles() []string

    // Addons returns the addon names this profile installs.
    Addons() []string

    // Configure runs profile-specific post-addon setup (e.g. creating
    // ClusterSPIFFEID resources, seeding OIDC clients, etc.).
    Configure(ctx context.Context, cluster ClusterHandle, cfg ClusterConfig) error
}
```

### Dependency resolution

The `cluster` package resolves addon installation order via topological sort over the combined dependency graph of all selected profiles and their addons. Cyclic dependencies are detected and returned as errors at cluster-creation time, before any installation begins.

---

## Profiles

### `signet` profile

**Purpose:** Bootstraps everything required for Signet to operate. Signet uses SPIFFE SVIDs for caller identity, so the SPIRE stack must be fully functional with real SVID issuance before Signet itself is deployed.

**Addon stack (in installation order):**

1. `cert-manager` — TLS certificate lifecycle. Required by SPIRE Controller Manager's webhook and by Traefik for TLS termination.
2. `spire` — SPIRE Server (StatefulSet), SPIRE Agent (DaemonSet), and SPIRE Controller Manager. Installs `ClusterSPIFFEID` CRD.
3. `traefik-tls` — Reconfigures the k3s-bundled Traefik to use cert-manager for TLS termination. No separate ingress controller is added.

**Post-install configuration (`Configure`):**

- Creates a default `ClusterSPIFFEID` resource for the trust domain `spiffe://<trustDomain>/workload` scoped to a configurable namespace selector, so workloads registered via service account annotations receive SVIDs automatically.
- Sets trust domain on the cluster record for use by downstream profiles.

**SPIRE design notes:**

- **Attestation method:** Kubernetes Service Account Token (K8s SAT) attestation. This is the standard local attestation approach and works correctly in k3d.
- **Registration model:** `ClusterSPIFFEID` CRDs managed by the SPIRE Controller Manager, not manual `spire-server entry create` calls. This mirrors production behavior and ensures SVID issuance responds to workload deployment events.
- **SVID extraction:** The SPIFFE Workload API socket is mounted into workload containers via the CSI driver (`spiffe.io/csi-driver`) or a SPIFFE helper sidecar. Both approaches are tested locally. Real SVID issuance is required — mocked SVIDs are not sufficient for Signet testing.
- **Trust domain default:** `dev.cluster.local` (overridable via `ClusterConfig.TrustDomain`).

### `authstar` profile

**Purpose:** Bootstraps everything required for AuthStar. AuthStar depends on Signet for secrets and configuration, so the `authstar` profile declares `signet` as a required profile.

**Required profiles:** `signet`

**Additional addon stack (installed after signet addons):**

1. `rabbitmq` — Single-node RabbitMQ with the management UI enabled. Deployed via the Bitnami Helm chart. Exposes management UI at a predictable local port.
2. `dex` — Local OIDC provider for end-to-end OIDC flow testing. Configured with a static client matching AuthStar's expected OIDC client credentials.

**Post-install configuration:**

- Seeds Dex with a static OIDC client ID/secret for AuthStar.
- Creates a RabbitMQ vhost and user for AuthStar's expected connection configuration.
- Applies AuthStar-specific Helm values (OIDC issuer URL pointing to local Dex, RabbitMQ connection string, Signet endpoint).

**Ingress note:** AuthStar provides its own reverse proxy, so no additional ingress controller is required. Traefik handles TLS termination only.

### `observability` profile (opt-in)

**Purpose:** Prometheus and Grafana for metrics visibility during local testing.

**Addons:** `prometheus`, `grafana`

**Required profiles:** none (can be stacked with any other profile)

### `tracing` profile (opt-in)

**Purpose:** Loki (log aggregation) and Tempo (distributed traces) for deeper observability during local testing.

**Addons:** `loki`, `tempo`

**Required profiles:** none (commonly used alongside `observability`)

---

## Addon Details

### `cert-manager`

- Installed via official Helm chart (`cert-manager/cert-manager`)
- CRDs installed as part of Helm release (`installCRDs: true`)
- Webhook readiness checked before returning from `Ready()`
- Creates a self-signed `ClusterIssuer` named `kluster-selfsigned` for local TLS

### `spire`

Components installed:
- **SPIRE Server** — StatefulSet, ClusterIP service, configmap with trust domain and K8s SAT attestation plugin config
- **SPIRE Agent** — DaemonSet, mounts hostPath socket at `/run/spire/sockets/agent.sock`
- **SPIRE Controller Manager** — Deployment, watches `ClusterSPIFFEID` CRDs, manages registration entry lifecycle
- **SPIFFE CSI Driver** — DaemonSet, mounts the Workload API socket into pods via CSI volume

Helm chart: `spiffe/spire` (official SPIFFE Helm chart repo)

`Ready()` implementation:
1. Wait for SPIRE Server pod to be Running
2. Wait for SPIRE Agent daemonset to have all nodes ready
3. Wait for SPIRE Controller Manager deployment to be available
4. Exec a `spire-server healthcheck` via the server pod to confirm the trust bundle is initialized

### `traefik-tls`

k3s ships with Traefik pre-installed. This addon:
- Patches the existing Traefik HelmChart resource (k3s-managed) to enable the cert-manager integration
- Creates a `Certificate` CR in the `kube-system` namespace for the cluster's default TLS certificate
- Configures Traefik's default TLS store to reference the generated certificate secret

No separate Traefik install — the k3s-bundled instance is configured in place.

### `rabbitmq`

- Installed via Bitnami Helm chart (`bitnami/rabbitmq`)
- Single-node deployment (no clustering)
- Management UI plugin enabled, exposed via NodePort or Traefik IngressRoute
- Default credentials configurable via `ClusterConfig`; defaults to `guest`/`guest` for local dev

### `dex`

- Installed via official Dex Helm chart (`dex/dex`)
- Configured as an in-cluster OIDC provider
- Static client pre-configured for AuthStar
- Exposed via Traefik IngressRoute with TLS

---

## CLI Design

The CLI is a thin Cobra application. It parses flags, constructs a `ClusterConfig`, and delegates to `kluster-lib`. No automation logic lives here.

### Commands

```
kluster up [flags]
  --name        string   Cluster name (required)
  --profile     string   Profile to activate: signet, authstar (default: signet)
  --addon       strings  Additional opt-in addons: observability, tracing
  --trust-domain string  SPIFFE trust domain (default: dev.cluster.local)
  --k3s-version string   k3s version tag (default: latest stable)

kluster down [flags]
  --name        string   Cluster name to destroy (required)

kluster status
  (lists all kluster-managed clusters with name, profile, age, and running state)

kluster kubeconfig [flags]
  --name        string   Cluster name (required)
  --output      string   Write kubeconfig to file path (default: stdout)
  --merge               Merge into ~/.kube/config and switch context
```

### Example invocations

```bash
# Signet development cluster
kluster up --name dev-signet --profile signet

# AuthStar development cluster with observability
kluster up --name dev-authstar --profile authstar --addon observability

# AuthStar + full observability + tracing
kluster up --name dev-full --profile authstar --addon observability --addon tracing

# Get kubeconfig merged into ~/.kube/config
kluster kubeconfig --name dev-signet --merge

# Tear down
kluster down --name dev-signet

# List all clusters
kluster status
```

---

## Go Dependency Stack

| Package | Version | Purpose |
|---|---|---|
| `github.com/k3d-io/k3d/v5` | v5.x | Cluster lifecycle (no subprocess) |
| `k8s.io/client-go` | v0.31.x | Kubernetes API client |
| `helm.sh/helm/v3` | v3.x | Helm engine SDK |
| `github.com/mittwald/go-helm-client` | latest | Helm SDK wrapper |
| `sigs.k8s.io/controller-runtime` | v0.19.x | Resource readiness utilities |
| `github.com/spf13/cobra` | v1.x | CLI framework (kluster only) |
| `github.com/spf13/viper` | v1.x | Config file support (kluster only) |

---

## Design Decisions and Rationale

| Decision | Choice | Rationale |
|---|---|---|
| Runtime | k3d | Go library, Docker-native, multi-cluster, k3s compatible |
| Language | Go | k3d, Helm SDK, client-go all natively importable; no subprocess calls |
| Helm install method | Helm SDK via go-helm-client | No `helm` binary dependency; fully programmatic |
| SPIRE registration | SPIRE Controller Manager + ClusterSPIFFEID CRDs | Mirrors production; responds to workload lifecycle events |
| SPIRE attestation | Kubernetes SAT attestation | Standard local approach; works in k3d without modification |
| SVID extraction | Real issuance via Workload API socket (CSI driver) | Required for Signet testing; mocks are insufficient |
| Ingress / TLS | k3s-bundled Traefik + cert-manager | Zero additional ingress controller; Traefik already present in k3s |
| Service mesh | None | SPIFFE/SPIRE + cert-manager covers mTLS; full mesh is out of scope |
| Policy enforcement | Deferred | OPA/Gatekeeper to be revisited when concrete need emerges |
| Multi-node clusters | Single-node | Sufficient for current testing needs; reduces resource requirements |
| Profile dependencies | Explicit + topological sort | Ensures deterministic install order; detects cycles at plan time |

---

## Out of Scope

The following are explicitly deferred or excluded:

- **OPA/Gatekeeper / Kyverno** — policy enforcement deferred until a concrete requirement emerges
- **Service mesh (Linkerd, Istio)** — SPIFFE/SPIRE + cert-manager satisfies mTLS needs without a full mesh
- **MetalLB / LoadBalancer emulation** — not currently required; NodePort or Traefik routing is sufficient
- **Multi-node clusters** — single-node is sufficient; DaemonSet behavior across multiple nodes is not a current testing target
- **Production deployment** — kluster is a local developer tool only
- **OpenBao standalone addon** — secrets management is Signet's responsibility; OpenBao would be an internal Signet concern