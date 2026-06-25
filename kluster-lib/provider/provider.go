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

type ClusterInfo struct {
	Name    string
	Running bool
	Age     time.Duration
}
