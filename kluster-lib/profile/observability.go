package profile

import (
	"context"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

type ObservabilityProfile struct{}

var _ Profile = (*ObservabilityProfile)(nil)

func init() { Register(&ObservabilityProfile{}) }

func (*ObservabilityProfile) Name() string               { return "observability" }
func (*ObservabilityProfile) RequiresProfiles() []string { return nil }
func (*ObservabilityProfile) Addons() []string {
	return []string{"prometheus", "grafana"}
}
func (*ObservabilityProfile) Configure(_ context.Context, _ addon.ClusterHandle, _ provider.ClusterConfig) error {
	return nil
}
