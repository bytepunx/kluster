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
	dexNamespace = "dex"
	dexRelease   = "dex"
	dexChart     = "dex/dex"
	dexRepoName  = "dex"
	dexRepoURL   = "https://charts.dexidp.io"
)

// dexValues configures Dex with in-memory storage and a static OIDC client for
// AuthStar. The password hash is bcrypt("password") from the Dex example config.
const dexValues = `
config:
  issuer: http://dex.dex.svc.cluster.local:5556
  storage:
    type: memory
  web:
    http: 0.0.0.0:5556
  oauth2:
    skipApprovalScreen: true
  staticClients:
    - id: authstar
      redirectURIs:
        - http://localhost:8080/auth/callback
        - http://localhost:3000/callback
      name: AuthStar Dev Client
      secret: authstar-dev-secret
  enablePasswordDB: true
  staticPasswords:
    - email: admin@example.com
      hash: "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYxx5/XVBDNX5lWdD."
      username: admin
      userID: "08a8684b-db88-4b73-90a9-3cd1661f5466"

service:
  type: ClusterIP
  ports:
    http:
      port: 5556
`

type DexAddon struct{}

var _ Addon = (*DexAddon)(nil)

func init() { Register(&DexAddon{}) }

func (*DexAddon) Name() string      { return "dex" }
func (*DexAddon) Requires() []string { return []string{"traefik-tls"} }

func (*DexAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(dexNamespace)
	if err != nil {
		return fmt.Errorf("dex: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: dexRepoName,
		URL:  dexRepoURL,
	}); err != nil {
		return fmt.Errorf("dex: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     dexRelease,
		ChartName:       dexChart,
		Namespace:       dexNamespace,
		Version:         versions.For("dex"),
		CreateNamespace: true,
		ValuesYaml:      dexValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("dex: helm install: %w", err)
	}

	return nil
}

func (*DexAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			d, err := h.K8sClient.AppsV1().Deployments(dexNamespace).Get(ctx, dexRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*DexAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(dexNamespace)
	if err != nil {
		return fmt.Errorf("dex: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(dexRelease); err != nil {
		return fmt.Errorf("dex: helm uninstall: %w", err)
	}
	return nil
}
