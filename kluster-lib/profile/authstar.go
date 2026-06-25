package profile

import (
	"context"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

type AuthStarProfile struct{}

var _ Profile = (*AuthStarProfile)(nil)

func init() { Register(&AuthStarProfile{}) }

func (*AuthStarProfile) Name() string               { return "authstar" }
func (*AuthStarProfile) RequiresProfiles() []string { return []string{"signet"} }
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
