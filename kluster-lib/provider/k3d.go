package provider

import (
	"context"
	"fmt"
	"net/url"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	k3dclient "github.com/k3d-io/k3d/v5/pkg/client"
	"github.com/k3d-io/k3d/v5/pkg/config"
	k3dconfigtypes "github.com/k3d-io/k3d/v5/pkg/config/types"
	k3dconf "github.com/k3d-io/k3d/v5/pkg/config/v1alpha5"
	k3dlogger "github.com/k3d-io/k3d/v5/pkg/logger"
	"github.com/k3d-io/k3d/v5/pkg/runtimes"
	k3d "github.com/k3d-io/k3d/v5/pkg/types"
	k3dversion "github.com/k3d-io/k3d/v5/version"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const createTimeout = 5 * time.Minute

func init() {
	// Suppress k3d's own info-level chatter; callers can raise it if needed.
	k3dlogger.Logger.SetLevel(logrus.WarnLevel)
}

type K3dProvider struct{}

var _ Provider = (*K3dProvider)(nil)

func NewK3dProvider() *K3dProvider { return &K3dProvider{} }

func (p *K3dProvider) Create(ctx context.Context, cfg ClusterConfig) error {
	k3sVersion := cfg.K3sVersion
	if k3sVersion == "" {
		k3sVersion = k3dversion.K3sVersion
	}
	image := fmt.Sprintf("%s:%s", k3d.DefaultK3sImageRepo, k3sVersion)

	simpleCfg := k3dconf.SimpleConfig{
		TypeMeta:   k3dconfigtypes.TypeMeta{APIVersion: k3dconf.ApiVersion, Kind: "Simple"},
		ObjectMeta: k3dconfigtypes.ObjectMeta{Name: cfg.Name},
		Servers:    1,
		Image:      image,
		Options: k3dconf.SimpleConfigOptions{
			K3dOptions: k3dconf.SimpleConfigOptionsK3d{
				Wait:    true,
				Timeout: createTimeout,
			},
		},
	}

	if err := config.ProcessSimpleConfig(&simpleCfg); err != nil {
		return fmt.Errorf("process simple config: %w", err)
	}

	clusterConfig, err := config.TransformSimpleToClusterConfig(ctx, runtimes.SelectedRuntime, simpleCfg, "")
	if err != nil {
		return fmt.Errorf("transform cluster config: %w", err)
	}

	clusterConfig, err = config.ProcessClusterConfig(*clusterConfig)
	if err != nil {
		return fmt.Errorf("process cluster config: %w", err)
	}

	if err := config.ValidateClusterConfig(ctx, runtimes.SelectedRuntime, *clusterConfig); err != nil {
		return fmt.Errorf("validate cluster config: %w", err)
	}

	if _, err := k3dclient.ClusterGet(ctx, runtimes.SelectedRuntime, &clusterConfig.Cluster); err == nil {
		return fmt.Errorf("cluster %q already exists", cfg.Name)
	}

	clusterConfig.ClusterCreateOpts.WaitForServer = true

	if err := k3dclient.ClusterRun(ctx, runtimes.SelectedRuntime, clusterConfig); err != nil {
		_ = k3dclient.ClusterDelete(ctx, runtimes.SelectedRuntime, &clusterConfig.Cluster, k3d.ClusterDeleteOpts{SkipRegistryCheck: true})
		return fmt.Errorf("run cluster: %w", err)
	}

	return nil
}

func (p *K3dProvider) Delete(ctx context.Context, name string) error {
	cluster, err := k3dclient.ClusterGet(ctx, runtimes.SelectedRuntime, &k3d.Cluster{Name: name})
	if err != nil {
		return fmt.Errorf("cluster %q not found: %w", name, err)
	}

	if err := k3dclient.ClusterDelete(ctx, runtimes.SelectedRuntime, cluster, k3d.ClusterDeleteOpts{}); err != nil {
		return fmt.Errorf("delete cluster %q: %w", name, err)
	}

	// Best-effort kubeconfig cleanup; not fatal if it fails.
	_ = k3dclient.KubeconfigRemoveClusterFromDefaultConfig(ctx, cluster)
	return nil
}

func (p *K3dProvider) List(ctx context.Context) ([]ClusterInfo, error) {
	clusters, err := k3dclient.ClusterList(ctx, runtimes.SelectedRuntime)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}

	result := make([]ClusterInfo, 0, len(clusters))
	for _, c := range clusters {
		_, serversRunning := c.ServerCountRunning()
		result = append(result, ClusterInfo{
			Name:    c.Name,
			Running: serversRunning > 0,
			Age:     clusterAge(c),
		})
	}
	return result, nil
}

func (p *K3dProvider) Kubeconfig(ctx context.Context, name string) ([]byte, error) {
	cluster, err := k3dclient.ClusterGet(ctx, runtimes.SelectedRuntime, &k3d.Cluster{Name: name})
	if err != nil {
		return nil, fmt.Errorf("cluster %q not found: %w", name, err)
	}

	kubeconfig, err := k3dclient.KubeconfigGet(ctx, runtimes.SelectedRuntime, cluster)
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig for %q: %w", name, err)
	}

	// k3d leaves the API server port empty in the server node label when the
	// cluster is created without an explicit ExposeAPI.HostPort.  Fix it by
	// reading the actual host-port binding from the load balancer container.
	if err := fixKubeconfigServerPort(ctx, name, kubeconfig); err != nil {
		return nil, fmt.Errorf("fix kubeconfig server port: %w", err)
	}

	data, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("serialize kubeconfig: %w", err)
	}
	return data, nil
}

// fixKubeconfigServerPort corrects the server URL when k3d leaves the API port
// empty.  k3d stores the configured host-port (which may be empty for random
// assignment) in the server node label; the actual Docker-assigned port is
// only available in NetworkSettings.Ports after the container starts.
// We call ContainerInspect on the load balancer directly to get that value.
func fixKubeconfigServerPort(ctx context.Context, clusterName string, kc *clientcmdapi.Config) error {
	needsFix := false
	for _, ci := range kc.Clusters {
		u, err := url.Parse(ci.Server)
		if err != nil || u.Port() == "" || u.Port() == "0" {
			needsFix = true
			break
		}
	}
	if !needsFix {
		return nil
	}

	dc, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dc.Close()

	lbName := fmt.Sprintf("%s-%s-serverlb", k3d.DefaultObjectNamePrefix, clusterName)
	info, err := dc.ContainerInspect(ctx, lbName)
	if err != nil {
		return fmt.Errorf("inspect LB container %q: %w", lbName, err)
	}

	apiNatPort := nat.Port(k3d.DefaultAPIPort + "/tcp")
	bindings, ok := info.NetworkSettings.Ports[apiNatPort]
	if !ok || len(bindings) == 0 {
		return fmt.Errorf("no port binding for %s on %s", apiNatPort, lbName)
	}
	hostIP := bindings[0].HostIP
	if hostIP == "" {
		hostIP = k3d.DefaultAPIHost
	}
	serverURL := fmt.Sprintf("https://%s:%s", hostIP, bindings[0].HostPort)
	for _, ci := range kc.Clusters {
		ci.Server = serverURL
	}
	return nil
}

func (p *K3dProvider) RESTConfig(ctx context.Context, name string) (*rest.Config, error) {
	data, err := p.Kubeconfig(ctx, name)
	if err != nil {
		return nil, err
	}
	rc, err := clientcmd.RESTConfigFromKubeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("build REST config for %q: %w", name, err)
	}
	return rc, nil
}

// clusterAge returns the time elapsed since the server node was created.
// Returns zero if no creation timestamp is available or parseable.
func clusterAge(c *k3d.Cluster) time.Duration {
	for _, node := range c.Nodes {
		if node.Role == k3d.ServerRole && node.Created != "" {
			if t, err := time.Parse(time.RFC3339, node.Created); err == nil {
				return time.Since(t).Truncate(time.Second)
			}
		}
	}
	return 0
}
