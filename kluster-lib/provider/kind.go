package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"
	kindlog "sigs.k8s.io/kind/pkg/log"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KindProvider uses kind (Kubernetes IN Docker) for CI-optimised clusters.
// It runs standard upstream Kubernetes, making it a better match for CI
// environments where exact version reproducibility matters.
// The container runtime (Docker, Podman, Nerdctl) is auto-detected.
type KindProvider struct {
	kind *kindcluster.Provider
}

var _ Provider = (*KindProvider)(nil)

func NewKindProvider() *KindProvider {
	// Auto-detect the available container runtime; fall back to Docker.
	runtimeOpt, err := kindcluster.DetectNodeProvider()
	if err != nil || runtimeOpt == nil {
		runtimeOpt = kindcluster.ProviderWithDocker()
	}
	return &KindProvider{
		kind: kindcluster.NewProvider(
			kindcluster.ProviderWithLogger(kindlog.NoopLogger{}),
			runtimeOpt,
		),
	}
}

func (p *KindProvider) Create(ctx context.Context, cfg ClusterConfig) error {
	existing, _ := p.kind.List()
	for _, name := range existing {
		if name == cfg.Name {
			return fmt.Errorf("cluster %q already exists", cfg.Name)
		}
	}

	opts := []kindcluster.CreateOption{
		kindcluster.CreateWithWaitForReady(5 * time.Minute),
		kindcluster.CreateWithDisplayUsage(false),
		kindcluster.CreateWithDisplaySalutation(false),
	}
	if img := kindNodeImage(cfg.K3sVersion); img != "" {
		opts = append(opts, kindcluster.CreateWithNodeImage(img))
	}

	// kind's Create does not accept a context; cancellation is not propagated.
	if err := p.kind.Create(cfg.Name, opts...); err != nil {
		return fmt.Errorf("create kind cluster %q: %w", cfg.Name, err)
	}
	// kind node containers are privileged and share the host kernel; raise
	// inotify limits before addons install to prevent "too many open files"
	// errors in controller-manager pods.
	raiseKindInotifyLimits(ctx, cfg.Name)
	return nil
}

func (p *KindProvider) Delete(_ context.Context, name string) error {
	existing, _ := p.kind.List()
	found := false
	for _, n := range existing {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("cluster %q not found", name)
	}
	// Empty explicitKubeconfigPath means kind cleans up its own kubeconfig entry.
	if err := p.kind.Delete(name, ""); err != nil {
		return fmt.Errorf("delete kind cluster %q: %w", name, err)
	}
	return nil
}

func (p *KindProvider) List(ctx context.Context) ([]ClusterInfo, error) {
	names, err := p.kind.List()
	if err != nil {
		return nil, fmt.Errorf("list kind clusters: %w", err)
	}
	result := make([]ClusterInfo, 0, len(names))
	for _, name := range names {
		nodes, _ := p.kind.ListNodes(name)
		result = append(result, ClusterInfo{
			Name:    name,
			Running: len(nodes) > 0,
			Age:     kindClusterAge(ctx, name),
		})
	}
	return result, nil
}

func (p *KindProvider) Kubeconfig(_ context.Context, name string) ([]byte, error) {
	// internal=false returns the external (host-accessible) server address.
	kc, err := p.kind.KubeConfig(name, false)
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig for %q: %w", name, err)
	}
	return []byte(kc), nil
}

func (p *KindProvider) RESTConfig(ctx context.Context, name string) (*rest.Config, error) {
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

// kindNodeImage maps a version string to a kind node image.
// k3s-specific versions (containing "+k3s") are ignored — kind uses its own
// default image in that case. A plain Kubernetes version like "v1.32.0"
// becomes "kindest/node:v1.32.0".
func kindNodeImage(version string) string {
	if version == "" || strings.Contains(version, "+k3s") {
		return "" // let kind pick its own pinned default
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return "kindest/node:" + version
}

// raiseKindInotifyLimits execs sysctl inside the kind node container to raise
// inotify limits. kind nodes are privileged containers that share the host
// kernel, so this affects the host. Errors are silently ignored (best-effort).
func raiseKindInotifyLimits(ctx context.Context, clusterName string) {
	dc, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return
	}
	defer dc.Close()

	container := clusterName + "-control-plane"
	for _, kv := range []string{
		"fs.inotify.max_user_instances=512",
		"fs.inotify.max_user_watches=524288",
	} {
		resp, err := dc.ContainerExecCreate(ctx, container, dockercontainer.ExecOptions{
			Cmd: []string{"sysctl", "-w", kv},
		})
		if err != nil {
			continue
		}
		_ = dc.ContainerExecStart(ctx, resp.ID, dockercontainer.ExecStartOptions{})
	}
}

// kindClusterAge returns the elapsed time since the kind control-plane
// container was created, using the Docker API. Returns zero on any error.
func kindClusterAge(ctx context.Context, clusterName string) time.Duration {
	dc, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return 0
	}
	defer dc.Close()

	// kind names the control-plane container "{clusterName}-control-plane".
	info, err := dc.ContainerInspect(ctx, clusterName+"-control-plane")
	if err != nil {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, info.Created)
	if err != nil {
		return 0
	}
	return time.Since(t).Truncate(time.Second)
}
