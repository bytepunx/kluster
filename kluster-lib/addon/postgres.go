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

// postgres provisions a single-node bitnami/postgresql instance for the
// authstar services that use consequent-postgres (tower, keep, herald).
// It shares the "bitnami" chart repo already registered by the rabbitmq
// addon (see rabbitmqRepoName/rabbitmqRepoURL in rabbitmq.go) — repo
// registration is idempotent, so re-adding it here is safe regardless of
// addon install order.
const (
	postgresNamespace = "postgres"
	postgresRelease   = "postgresql"
	postgresChart     = "bitnami/postgresql"

	// postgresValues runs a single standalone primary with persistence
	// disabled (ephemeral dev cluster) and fixed dev credentials, matching
	// rabbitmq's guest/guest bluntness: superuser "postgres", password
	// "postgres", default database "postgres".
	postgresValues = `
architecture: standalone

auth:
  postgresPassword: postgres
  database: postgres

primary:
  persistence:
    enabled: false

metrics:
  enabled: false
`
)

type PostgresAddon struct{}

var _ Addon = (*PostgresAddon)(nil)

func init() { Register(&PostgresAddon{}) }

func (*PostgresAddon) Name() string       { return "postgres" }
func (*PostgresAddon) Requires() []string { return nil }

func (*PostgresAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(postgresNamespace)
	if err != nil {
		return fmt.Errorf("postgres: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: rabbitmqRepoName,
		URL:  rabbitmqRepoURL,
	}); err != nil {
		return fmt.Errorf("postgres: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     postgresRelease,
		ChartName:       postgresChart,
		Namespace:       postgresNamespace,
		Version:         versions.For("postgres"),
		CreateNamespace: true,
		ValuesYaml:      postgresValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("postgres: helm install: %w", err)
	}

	return nil
}

func (*PostgresAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(postgresNamespace).Get(ctx, postgresRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*PostgresAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(postgresNamespace)
	if err != nil {
		return fmt.Errorf("postgres: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(postgresRelease); err != nil {
		return fmt.Errorf("postgres: helm uninstall: %w", err)
	}
	return nil
}
