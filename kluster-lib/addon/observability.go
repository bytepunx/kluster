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

const monitoringNamespace = "monitoring"

// ── Prometheus ────────────────────────────────────────────────────────────────

const (
	prometheusRelease  = "prometheus"
	prometheusChart    = "prometheus-community/prometheus"
	prometheusRepoName = "prometheus-community"
	prometheusRepoURL  = "https://prometheus-community.github.io/helm-charts"

	prometheusValues = `
alertmanager:
  enabled: false
kube-state-metrics:
  enabled: false
prometheus-node-exporter:
  enabled: false
prometheus-pushgateway:
  enabled: false
server:
  retention: 24h
  persistentVolume:
    enabled: false
`
)

type PrometheusAddon struct{}

var _ Addon = (*PrometheusAddon)(nil)

func init() { Register(&PrometheusAddon{}) }

func (*PrometheusAddon) Name() string      { return "prometheus" }
func (*PrometheusAddon) Requires() []string { return nil }

func (*PrometheusAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("prometheus: helm client: %w", err)
	}
	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: prometheusRepoName,
		URL:  prometheusRepoURL,
	}); err != nil {
		return fmt.Errorf("prometheus: add repo: %w", err)
	}
	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     prometheusRelease,
		ChartName:       prometheusChart,
		Namespace:       monitoringNamespace,
		Version:         versions.For("prometheus"),
		CreateNamespace: true,
		ValuesYaml:      prometheusValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("prometheus: helm install: %w", err)
	}
	return nil
}

func (*PrometheusAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			d, err := h.K8sClient.AppsV1().Deployments(monitoringNamespace).Get(ctx, "prometheus-server", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*PrometheusAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("prometheus: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(prometheusRelease); err != nil {
		return fmt.Errorf("prometheus: helm uninstall: %w", err)
	}
	return nil
}

// ── Grafana ───────────────────────────────────────────────────────────────────

const (
	grafanaRelease  = "grafana"
	grafanaChart    = "grafana/grafana"
	grafanaRepoName = "grafana"
	grafanaRepoURL  = "https://grafana.github.io/helm-charts"

	// Loki and Tempo datasources are pre-wired. They show offline in Grafana
	// when those addons aren't installed, which is acceptable for local dev.
	grafanaValues = `
adminUser: admin
adminPassword: admin
persistence:
  enabled: false
datasources:
  datasources.yaml:
    apiVersion: 1
    datasources:
      - name: Prometheus
        type: prometheus
        url: http://prometheus-server.monitoring.svc.cluster.local
        isDefault: true
        access: proxy
      - name: Loki
        type: loki
        url: http://loki.monitoring.svc.cluster.local:3100
        access: proxy
      - name: Tempo
        type: tempo
        url: http://tempo.monitoring.svc.cluster.local:3100
        access: proxy
`
)

type GrafanaAddon struct{}

var _ Addon = (*GrafanaAddon)(nil)

func init() { Register(&GrafanaAddon{}) }

func (*GrafanaAddon) Name() string      { return "grafana" }
func (*GrafanaAddon) Requires() []string { return []string{"prometheus"} }

func (*GrafanaAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("grafana: helm client: %w", err)
	}
	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: grafanaRepoName,
		URL:  grafanaRepoURL,
	}); err != nil {
		return fmt.Errorf("grafana: add repo: %w", err)
	}
	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     grafanaRelease,
		ChartName:       grafanaChart,
		Namespace:       monitoringNamespace,
		Version:         versions.For("grafana"),
		CreateNamespace: true,
		ValuesYaml:      grafanaValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("grafana: helm install: %w", err)
	}
	return nil
}

func (*GrafanaAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			d, err := h.K8sClient.AppsV1().Deployments(monitoringNamespace).Get(ctx, grafanaRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*GrafanaAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(monitoringNamespace)
	if err != nil {
		return fmt.Errorf("grafana: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(grafanaRelease); err != nil {
		return fmt.Errorf("grafana: helm uninstall: %w", err)
	}
	return nil
}
