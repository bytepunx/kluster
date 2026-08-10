package addon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bytepunx/kluster-lib/provider"
	helmclient "github.com/mittwald/go-helm-client"
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
//
// ResetRESTMapper forces RESTMapper's next lookup to re-run API discovery —
// see its own doc comment for why callers (specifically ApplyManifest) must
// invoke this explicitly rather than relying on RESTMapper to notice new CRDs
// installed earlier in the same `kluster up` run on its own.
type ClusterHandle struct {
	RESTConfig      *rest.Config
	HelmClientFor   func(namespace string) (helmclient.Client, error)
	K8sClient       kubernetes.Interface
	DynClient       dynamic.Interface
	RESTMapper      apimeta.RESTMapper
	ResetRESTMapper func()
	Config          provider.ClusterConfig
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

	// DeferredDiscoveryRESTMapper does have a built-in "re-discover on
	// miss" path (RESTMapping retries once after calling d.Reset() -- see
	// k8s.io/client-go/restmapper/discovery.go), but it is gated on
	// !d.cl.Fresh(), and memory.NewMemCacheClient's Fresh() returns
	// whether discovery has EVER succeeded, not whether it's still
	// current. Once any single call anywhere in this process populates
	// the cache, Fresh() returns true for the remaining lifetime of this
	// ClusterHandle (nothing flips it back to false except an explicit
	// Invalidate()/Reset() call) -- so that built-in retry silently never
	// fires again, even for a real new CRD installed later in the same
	// `kluster up` run. This bit ApplyManifest directly: the
	// rabbitmq-topology-operator addon installs the Vhost CRD, and the
	// authstar profile's Configure() -- which runs after every addon in
	// the profile, specifically so newly-installed CRDs are available --
	// still got "no matches for kind Vhost" from a REST mapper that had
	// already cached a pre-CRD discovery snapshot from some earlier
	// ApplyManifest/Helm call in the same run.
	//
	// ResetRESTMapper is ApplyManifest's explicit way to force the actual
	// re-discovery this scenario needs, on a genuine NoKindMatchError,
	// instead of depending on a Fresh() gate that doesn't distinguish
	// "never discovered" from "discovered a while ago, might be stale."

	// os.TempDir() (/tmp) is world-writable: another local user could win the
	// race to create "kluster/helm-cache" first and plant a poisoned chart
	// cache. User-private cache/config dirs avoid that entirely.
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return ClusterHandle{}, fmt.Errorf("resolve user cache dir: %w", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ClusterHandle{}, fmt.Errorf("resolve user config dir: %w", err)
	}
	helmCacheDir := filepath.Join(cacheDir, "kluster", "helm-cache")
	helmReposFile := filepath.Join(configDir, "kluster", "helm-repos.yaml")
	if err := os.MkdirAll(helmCacheDir, 0o700); err != nil {
		return ClusterHandle{}, fmt.Errorf("create helm cache dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(helmReposFile), 0o700); err != nil {
		return ClusterHandle{}, fmt.Errorf("create helm config dir: %w", err)
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
		RESTConfig:      rc,
		HelmClientFor:   factory,
		K8sClient:       k8sClient,
		DynClient:       dynClient,
		RESTMapper:      mapper,
		ResetRESTMapper: mapper.Reset,
		Config:          cfg,
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
