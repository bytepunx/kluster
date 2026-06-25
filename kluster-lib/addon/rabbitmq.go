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

const (
	rabbitmqNamespace = "rabbitmq"
	rabbitmqRelease   = "rabbitmq"
	rabbitmqChart     = "bitnami/rabbitmq"
	rabbitmqRepoName  = "bitnami"
	rabbitmqRepoURL   = "https://charts.bitnami.com/bitnami"

	rabbitmqValues = `
replicaCount: 1

auth:
  username: guest
  password: guest

persistence:
  enabled: false

metrics:
  enabled: false
`
)

type RabbitMQAddon struct{}

var _ Addon = (*RabbitMQAddon)(nil)

func init() { Register(&RabbitMQAddon{}) }

func (*RabbitMQAddon) Name() string      { return "rabbitmq" }
func (*RabbitMQAddon) Requires() []string { return nil }

func (*RabbitMQAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(rabbitmqNamespace)
	if err != nil {
		return fmt.Errorf("rabbitmq: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: rabbitmqRepoName,
		URL:  rabbitmqRepoURL,
	}); err != nil {
		return fmt.Errorf("rabbitmq: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     rabbitmqRelease,
		ChartName:       rabbitmqChart,
		Namespace:       rabbitmqNamespace,
		Version:         versions.For("rabbitmq"),
		CreateNamespace: true,
		ValuesYaml:      rabbitmqValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("rabbitmq: helm install: %w", err)
	}

	return nil
}

func (*RabbitMQAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(rabbitmqNamespace).Get(ctx, rabbitmqRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*RabbitMQAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(rabbitmqNamespace)
	if err != nil {
		return fmt.Errorf("rabbitmq: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(rabbitmqRelease); err != nil {
		return fmt.Errorf("rabbitmq: helm uninstall: %w", err)
	}
	return nil
}
