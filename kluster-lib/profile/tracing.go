package profile

import (
	"context"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

type TracingProfile struct{}

var _ Profile = (*TracingProfile)(nil)

func init() { Register(&TracingProfile{}) }

func (*TracingProfile) Name() string               { return "tracing" }
func (*TracingProfile) RequiresProfiles() []string { return nil }
func (*TracingProfile) Addons() []string {
	return []string{"loki", "tempo"}
}
func (*TracingProfile) Configure(_ context.Context, _ addon.ClusterHandle, _ provider.ClusterConfig) error {
	return nil
}
