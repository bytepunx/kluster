package addon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	helmclient "github.com/mittwald/go-helm-client"
	"github.com/bytepunx/kluster-lib/provider"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

type Addon interface {
	Name() string
	Requires() []string
	Install(ctx context.Context, cluster ClusterHandle) error
	Uninstall(ctx context.Context, cluster ClusterHandle) error
	Ready(ctx context.Context, cluster ClusterHandle) error
}

// ClusterHandle gives addons access to all cluster clients they need.
//
// HelmClientFor creates a namespace-scoped Helm client on demand. A separate
// scoped client per addon is required because Helm stores release state in the
// namespace used to initialise the action config.
type ClusterHandle struct {
	RESTConfig    *rest.Config
	HelmClientFor func(namespace string) (helmclient.Client, error)
	K8sClient     kubernetes.Interface
	DynClient     dynamic.Interface
	RESTMapper    apimeta.RESTMapper
	Config        provider.ClusterConfig
}

// NewClusterHandle builds a ClusterHandle from a REST config and the cluster
// config (for addons that need config values such as the trust domain).
func NewClusterHandle(rc *rest.Config, cfg provider.ClusterConfig) (ClusterHandle, error) {
	k8sClient, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return ClusterHandle{}, fmt.Errorf("build kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(rc)
	if err != nil {
		return ClusterHandle{}, fmt.Errorf("build dynamic client: %w", err)
	}

	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return ClusterHandle{}, fmt.Errorf("build discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	helmCacheDir := filepath.Join(os.TempDir(), "kluster", "helm-cache")
	helmReposFile := filepath.Join(os.TempDir(), "kluster", "helm-repos.yaml")
	if err := os.MkdirAll(helmCacheDir, 0o755); err != nil {
		return ClusterHandle{}, fmt.Errorf("create helm cache dir: %w", err)
	}

	factory := func(namespace string) (helmclient.Client, error) {
		return helmclient.NewClientFromRestConf(&helmclient.RestConfClientOptions{
			Options: &helmclient.Options{
				Namespace:        namespace,
				RepositoryCache:  helmCacheDir,
				RepositoryConfig: helmReposFile,
			},
			RestConfig: rc,
		})
	}

	return ClusterHandle{
		RESTConfig:    rc,
		HelmClientFor: factory,
		K8sClient:     k8sClient,
		DynClient:     dynClient,
		RESTMapper:    mapper,
		Config:        cfg,
	}, nil
}

var registry = map[string]Addon{}

func Register(a Addon) {
	registry[a.Name()] = a
}

func Get(name string) (Addon, bool) {
	a, ok := registry[name]
	return a, ok
}

func All() []Addon {
	result := make([]Addon, 0, len(registry))
	for _, a := range registry {
		result = append(result, a)
	}
	return result
}
