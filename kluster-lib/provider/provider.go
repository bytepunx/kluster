package provider

import (
	"context"
	"time"

	"k8s.io/client-go/rest"
)

type Provider interface {
	Create(ctx context.Context, cfg ClusterConfig) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]ClusterInfo, error)
	Kubeconfig(ctx context.Context, name string) ([]byte, error)
	RESTConfig(ctx context.Context, name string) (*rest.Config, error)
}

type ClusterConfig struct {
	Name        string
	K3sVersion  string
	TrustDomain string
	Profiles    []string
	Addons      []string
}

// DefaultTrustDomain is used wherever a caller doesn't set
// ClusterConfig.TrustDomain. Defined once here — rather than re-hardcoded at
// each addon/profile call site — since addon and profile packages can't
// import kluster-lib/cluster (which imports them) to share a constant
// defined there instead.
const DefaultTrustDomain = "dev.cluster.local"

// TrustDomainOrDefault returns c.TrustDomain, falling back to
// DefaultTrustDomain when unset.
func (c ClusterConfig) TrustDomainOrDefault() string {
	if c.TrustDomain == "" {
		return DefaultTrustDomain
	}
	return c.TrustDomain
}

type ClusterInfo struct {
	Name    string
	Running bool
	Age     time.Duration
}
