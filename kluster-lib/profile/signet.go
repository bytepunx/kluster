package profile

import (
	"context"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

// SignetProfile installs Signet itself, on top of SPIFFE/SPIRE workload
// identity (the "spire" profile). See addon/signet.go for the install.
type SignetProfile struct{}

var _ Profile = (*SignetProfile)(nil)

func init() { Register(&SignetProfile{}) }

func (*SignetProfile) Name() string               { return "signet" }
func (*SignetProfile) RequiresProfiles() []string { return []string{"spire"} }
func (*SignetProfile) Addons() []string {
	return []string{"signet"}
}

// Configure is a no-op: the "spire" profile's catch-all ClusterSPIFFEID
// already covers Signet's own workload (it selects every namespace outside
// kube-system/spire-system), so no additional SPIFFE registration is needed
// here.
func (*SignetProfile) Configure(_ context.Context, _ addon.ClusterHandle, _ provider.ClusterConfig) error {
	return nil
}
