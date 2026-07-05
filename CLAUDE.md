# kluster — Project Context for Claude

## What this project is

`kluster` is a developer tool for creating disposable, baseline Kubernetes clusters on a local machine for integration testing and inner-loop development. It is the primary local testing substrate for **Signet** (a SPIFFE-native config and secrets management platform) and **AuthStar** (a modular authentication, authorization, and subscription management platform).

The project has two Go modules:

- **`kluster-lib`** — the automation library. Contains all cluster lifecycle logic, addon management, and profile orchestration. Has no CLI dependency.
- **`kluster`** — a thin Cobra CLI that imports `kluster-lib` and handles flag parsing, user output, and command routing. Contains no automation logic of its own.

## Runtime

**k3d** (k3s in Docker) is the cluster runtime. k3d is written in Go and its cluster lifecycle is importable directly as `github.com/k3d-io/k3d/v5` — no shelling out. k3d wraps k3s, providing full k3s compatibility with Docker-based lifecycle management and no VM layer requirement.

A `kind` provider may be added later as a CI-optimized alternative.

## Key dependency projects

### Signet vs. the `spire` profile — read this before touching either

**Signet** and **SPIRE** are two different things that are easy to conflate, and kluster's profile naming has historically made that worse:

- **SPIRE** is the SPIFFE workload identity runtime (server, agent, controller manager). It issues SVIDs; it knows nothing about Signet.
- **Signet** is the product built on top of SPIRE — it's Kubernetes-native configuration and secrets management that uses **SPIFFE SVIDs** as the caller identity signal: a workload presents its SVID to Signet, which validates the SPIFFE ID and maps it to an access policy to determine what config/secrets that workload may receive.

kluster-lib's `spire` profile (`kluster-lib/profile/spire.go`) bootstraps the SPIFFE/SPIRE identity substrate — it does **not** install Signet itself:
- SPIRE Server + Agent (DaemonSet)
- SPIRE Controller Manager (for `ClusterSPIFFEID` CRD-driven registration)
- cert-manager (TLS lifecycle)
- Traefik with TLS termination via cert-manager integration
- Trust domain: `dev.cluster.local` (configurable)

The `signet` profile (`kluster-lib/profile/signet.go`, requires `spire`) installs Signet itself, via the `signet` addon (`kluster-lib/addon/signet.go`), which pulls Signet's own Helm chart directly from `oci://ghcr.io/bytepunx/charts/signet`. It pre-creates the Kubernetes Secret Signet's auto-unseal mode reads (a random 32-byte key, field `master.key`) so signetd comes up already unsealed with no `signet init` CLI/gRPC step, generates a fresh audit-chain key, and runs Signet's in-cluster CockroachDB (`cockroachdb.enabled: true`) — all dev-only conveniences, not something you'd do in production. It also overrides two Helm values that don't match between the `spire` and `signet` charts by default: `spire.socketHostPath` (the real SPIRE agent socket directory when installed via the `spiffe/spire` chart is `/run/spire/agent-sockets`, not Signet chart's default `/run/spire/sockets`) and `signet.spireSocket` (the real socket filename is `spire-agent.sock`, not the chart's default `agent.sock`).

### AuthStar
AuthStar is a modular authentication, authorization, and subscription management platform. It depends on Signet for secrets and configuration — the `authstar` profile composes the `signet` profile as a dependency, then additionally installs:
- RabbitMQ (single-node + management UI)
- Dex (local OIDC provider for end-to-end OIDC flow testing)
- AuthStar-specific Helm values and RBAC

AuthStar provides its own reverse proxy for ingress, so no additional ingress controller is needed beyond Traefik for TLS termination.

## Architecture layers

```
Named Profiles          (authstar, signet, spire, observability, tracing)
    ↓ composes
Platform Addons         (signet, spire, cert-manager, traefik-tls, rabbitmq, dex, ...)
    ↓ installs into
Core                    (CNI, CoreDNS, metrics-server, local-path-provisioner)
    ↓ runs on
Distribution Runtime    (k3d primary; kind secondary/CI)
```

## Go module layout

```
kluster/
├── kluster-lib/                  # automation library
│   ├── provider/
│   │   ├── provider.go           # Provider interface
│   │   ├── k3d.go                # k3d cluster lifecycle
│   │   └── kind.go               # kind provider (future/CI)
│   ├── addon/
│   │   ├── addon.go              # Addon interface + registry
│   │   ├── certmanager.go
│   │   ├── spire.go
│   │   ├── signet.go             # Signet itself (OCI chart, auto-unseal)
│   │   ├── traefik.go
│   │   ├── rabbitmq.go
│   │   ├── dex.go
│   │   ├── observability.go      # Prometheus + Grafana
│   │   └── tracing.go            # Loki + Tempo
│   ├── profile/
│   │   ├── profile.go            # Profile interface + dependency graph
│   │   ├── spire.go              # SPIFFE/SPIRE workload identity (not Signet itself)
│   │   ├── signet.go             # Signet profile (requires spire)
│   │   ├── authstar.go
│   │   ├── observability.go
│   │   └── tracing.go
│   └── cluster/
│       ├── cluster.go            # Cluster struct + lifecycle orchestration
│       └── config.go             # ClusterConfig
└── kluster/                      # CLI
    └── cmd/
        ├── root.go
        ├── up.go
        ├── down.go
        ├── status.go
        └── kubeconfig.go
```

## Key design constraints

1. **No automation logic in the CLI.** The CLI parses flags, builds a `ClusterConfig`, delegates to `kluster-lib`, and renders output. Period.
2. **No shelling out for core operations.** Cluster lifecycle uses the k3d Go library directly. Helm installs use the Helm Go SDK (`helm.sh/helm/v3`) via `go-helm-client`. Kubernetes resource management uses `client-go`.
3. **Ordered addon installation.** Profiles declare addon dependencies explicitly. The library resolves a deterministic installation order (topological sort). Addons declare their own prerequisites.
4. **SPIRE is not optional for Signet.** The `spire` profile unconditionally installs SPIRE with the SPIRE Controller Manager. Registration entries are managed via `ClusterSPIFFEID` CRDs, not manual `spire-server entry create` calls, to mirror production behavior.
5. **SVID extraction must be testable.** The local SPIRE setup must support real SVID issuance and extraction — not mocked. Workload attestation uses Kubernetes Service Account Tokens (the standard local attestation method).

## Primary Go dependencies

| Package | Purpose |
|---|---|
| `github.com/k3d-io/k3d/v5` | Cluster lifecycle (create, delete, list, kubeconfig) |
| `k8s.io/client-go` | Kubernetes API (apply manifests, watch readiness, manage CRDs) |
| `helm.sh/helm/v3` | Helm SDK (chart install/upgrade/uninstall) |
| `github.com/mittwald/go-helm-client` | Higher-level wrapper over Helm SDK |
| `sigs.k8s.io/controller-runtime` | Resource readiness waiting |
| `github.com/spf13/cobra` | CLI framework (kluster module only) |
| `github.com/spf13/viper` | Config file support (kluster module only) |

## CLI command surface

```bash
kluster up --profile spire --name dev-spire
kluster up --profile signet --name dev-signet
kluster up --profile authstar --addon observability --name dev-authstar
kluster down --name dev-authstar
kluster status
kluster kubeconfig --name dev-signet
```

## What is explicitly out of scope (for now)

- OPA/Gatekeeper or Kyverno policy enforcement (deferred; revisit when needed)
- Multi-node clusters (single-node is sufficient for current testing needs)
- Service mesh (Linkerd/Istio) — SPIFFE/SPIRE + cert-manager covers mTLS needs
- MetalLB or external load balancer emulation
- Production deployment tooling (this is local-only)