package profile

import (
	"context"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

type AuthStarProfile struct{}

var _ Profile = (*AuthStarProfile)(nil)

func init() { Register(&AuthStarProfile{}) }

func (*AuthStarProfile) Name() string { return "authstar" }

// RequiresProfiles depends on "spire" for workload identity. AuthStar's real
// dependency is on Signet for secrets/config, but the "signet" profile (built
// on Signet's own Helm chart) does not exist yet — swap this to "signet" once
// it does.
func (*AuthStarProfile) RequiresProfiles() []string { return []string{"spire"} }
func (*AuthStarProfile) Addons() []string {
	return []string{"rabbitmq", "dex"}
}

// Configure is intentionally minimal: RabbitMQ and Dex are pre-seeded via their
// Helm values (guest/guest credentials; static AuthStar OIDC client). Per-cluster
// provisioning (RabbitMQ vhosts, additional Dex connectors) should be added here
// once AuthStar's concrete requirements are known.
func (*AuthStarProfile) Configure(_ context.Context, _ addon.ClusterHandle, _ provider.ClusterConfig) error {
	return nil
}
