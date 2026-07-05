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
// Controller Manager to issue SVIDs to all workloads except system namespaces.
// The SPIFFE ID follows the standard path:
//
//	spiffe://<trustDomain>/ns/<namespace>/sa/<serviceAccount>
func (*SpireProfile) Configure(ctx context.Context, h addon.ClusterHandle, cfg provider.ClusterConfig) error {
	trustDomain := cfg.TrustDomain
	if trustDomain == "" {
		trustDomain = "dev.cluster.local"
	}

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
`, trustDomain)); err != nil {
		return fmt.Errorf("spire: apply ClusterSPIFFEID: %w", err)
	}

	if err := addon.ProbeSVID(ctx, h, trustDomain); err != nil {
		return fmt.Errorf("spire: SVID probe: %w", err)
	}

	return nil
}
