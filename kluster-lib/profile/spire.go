package profile

import (
	"context"
	"fmt"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

// SpireProfile bootstraps SPIFFE/SPIRE workload identity plus the cert-manager
// and Traefik TLS plumbing built on top of it. It does not install Signet
// itself — see the separate "signet" profile, which depends on this one for
// workload identity.
type SpireProfile struct{}

var _ Profile = (*SpireProfile)(nil)

func init() { Register(&SpireProfile{}) }

func (*SpireProfile) Name() string               { return "spire" }
func (*SpireProfile) RequiresProfiles() []string { return nil }
func (*SpireProfile) Addons() []string {
	return []string{"cert-manager", "spire", "traefik-tls"}
}

// Configure creates a ClusterSPIFFEID resource that instructs the SPIRE
// Controller Manager to issue SVIDs to all workloads except system namespaces
// and kluster's own non-SPIFFE-aware infrastructure. The SPIFFE ID follows
// the standard path:
//
//	spiffe://<trustDomain>/ns/<namespace>/sa/<serviceAccount>
//
// This is a dev convenience, not a production pattern: no real trust domain
// hands out identities to arbitrary workloads by default. It exists so a
// workload deployed into a test namespace gets an SVID with zero extra
// registration, matching this tool's goal of a ready-to-use cluster. The
// exclusion list below only trims addon-installed infrastructure that has no
// use for a SPIFFE identity (cert-manager, the observability/tracing stack,
// ArgoCD, Dex) — it must NOT include "signet", which dials the SPIRE
// workload API for its own identity.
//
// "rabbitmq" was removed from this list (ADR 0010/0012): RabbitMQ now runs
// under kickr (see addon/rabbitmq.go), which needs its own SVID to dial
// signet for RabbitMQ's bootstrap credential, and the RabbitMQ-namespace
// provisioning Job (see profile/authstar.go) needs one too, to fetch its own
// elevated signet admin token. Leaving the namespace registered is harmless
// for RabbitMQ's own broker process itself, which never requests an SVID —
// SPIRE only issues one to a workload that actually asks the Workload API
// for it.
func (*SpireProfile) Configure(ctx context.Context, h addon.ClusterHandle, cfg provider.ClusterConfig) error {
	trustDomain := cfg.TrustDomainOrDefault()

	if err := addon.ApplyManifest(ctx, h, fmt.Sprintf(`
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata:
  name: kluster-workload
spec:
  spiffeIDTemplate: "spiffe://%s/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodMeta.ServiceAccountName }}"
  podSelector: {}
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: NotIn
        values:
          - kube-system
          - spire-system
          - cert-manager
          - monitoring
          - argocd
          - dex
`, trustDomain)); err != nil {
		return fmt.Errorf("spire: apply ClusterSPIFFEID: %w", err)
	}

	if err := addon.ProbeSVID(ctx, h, trustDomain); err != nil {
		return fmt.Errorf("spire: SVID probe: %w", err)
	}

	return nil
}
