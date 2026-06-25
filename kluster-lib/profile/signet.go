package profile

import (
	"context"
	"fmt"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

type SignetProfile struct{}

var _ Profile = (*SignetProfile)(nil)

func init() { Register(&SignetProfile{}) }

func (*SignetProfile) Name() string               { return "signet" }
func (*SignetProfile) RequiresProfiles() []string { return nil }
func (*SignetProfile) Addons() []string {
	return []string{"cert-manager", "spire", "traefik-tls"}
}

// Configure creates a ClusterSPIFFEID resource that instructs the SPIRE
// Controller Manager to issue SVIDs to all workloads except system namespaces.
// The SPIFFE ID follows the standard path:
//
//	spiffe://<trustDomain>/ns/<namespace>/sa/<serviceAccount>
func (*SignetProfile) Configure(ctx context.Context, h addon.ClusterHandle, cfg provider.ClusterConfig) error {
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
		return fmt.Errorf("signet: apply ClusterSPIFFEID: %w", err)
	}

	if err := addon.ProbeSVID(ctx, h, trustDomain); err != nil {
		return fmt.Errorf("signet: SVID probe: %w", err)
	}

	return nil
}
