package addon

import (
	"context"
	"fmt"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/bytepunx/kluster-lib/versions"
)

// Loki and Tempo run in single-binary mode with no persistence for local dev.
// Both use the grafana Helm repo (already registered if grafana addon ran first).

// ── Loki ──────────────────────────────────────────────────────────────────────

const (
	lokiRelease = "loki"
	lokiChart   = "grafana/loki"

	lokiValues = `
deploymentMode: SingleBinary

loki:
  auth_enabled: false
  commonConfig:
    replication_factor: 1
  storage:
    type: filesystem

singleBinary:
  replicas: 1

chunksCache:
  enabled: false
resultsCache:
  enabled: false

minio:
  enabled: false
`
)

type LokiAddon struct{}

var _ Addon = (*LokiAddon)(nil)

func init() { Register(&LokiAddon{}) }

func (*LokiAddon) Name() string      { return "loki" }
func (*LokiAddon) Requires() []string { return nil }

func (*LokiAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("loki: helm client: %w", err)
	}
	// grafana repo may be registered already by the grafana addon; idempotent
	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: grafanaRepoName,
		URL:  grafanaRepoURL,
	}); err != nil {
		return fmt.Errorf("loki: add repo: %w", err)
	}
	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     lokiRelease,
		ChartName:       lokiChart,
		Namespace:       monitoringNamespace,
		Version:         versions.For("loki"),
		CreateNamespace: true,
		ValuesYaml:      lokiValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("loki: helm install: %w", err)
	}
	return nil
}

func (*LokiAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(monitoringNamespace).Get(ctx, lokiRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*LokiAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("loki: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(lokiRelease); err != nil {
		return fmt.Errorf("loki: helm uninstall: %w", err)
	}
	return nil
}

// ── Tempo ─────────────────────────────────────────────────────────────────────

const (
	tempoRelease = "tempo"
	tempoChart   = "grafana/tempo"

	tempoValues = `
replicas: 1

tempo:
  storage:
    trace:
      backend: local
      local:
        path: /var/tempo/traces

persistence:
  enabled: false
`
)

type TempoAddon struct{}

var _ Addon = (*TempoAddon)(nil)

func init() { Register(&TempoAddon{}) }

func (*TempoAddon) Name() string      { return "tempo" }
func (*TempoAddon) Requires() []string { return nil }

func (*TempoAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("tempo: helm client: %w", err)
	}
	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: grafanaRepoName,
		URL:  grafanaRepoURL,
	}); err != nil {
		return fmt.Errorf("tempo: add repo: %w", err)
	}
	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     tempoRelease,
		ChartName:       tempoChart,
		Namespace:       monitoringNamespace,
		Version:         versions.For("tempo"),
		CreateNamespace: true,
		ValuesYaml:      tempoValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("tempo: helm install: %w", err)
	}
	return nil
}

func (*TempoAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(monitoringNamespace).Get(ctx, tempoRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*TempoAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("tempo: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(tempoRelease); err != nil {
		return fmt.Errorf("tempo: helm uninstall: %w", err)
	}
	return nil
}
