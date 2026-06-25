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
	certManagerNamespace = "cert-manager"
	certManagerRelease   = "cert-manager"
	certManagerChart     = "cert-manager/cert-manager"
	certManagerRepoName  = "cert-manager"
	certManagerRepoURL   = "https://charts.jetstack.io"

	certManagerValues = `crds:
  enabled: true
`
)

// deployments that must be ready before cert-manager is considered operational.
var certManagerDeployments = []string{
	"cert-manager",
	"cert-manager-webhook",
	"cert-manager-cainjector",
}

type CertManagerAddon struct{}

var _ Addon = (*CertManagerAddon)(nil)

func init() { Register(&CertManagerAddon{}) }

func (*CertManagerAddon) Name() string     { return "cert-manager" }
func (*CertManagerAddon) Requires() []string { return nil }

func (*CertManagerAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(certManagerNamespace)
	if err != nil {
		return fmt.Errorf("cert-manager: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: certManagerRepoName,
		URL:  certManagerRepoURL,
	}); err != nil {
		return fmt.Errorf("cert-manager: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     certManagerRelease,
		ChartName:       certManagerChart,
		Namespace:       certManagerNamespace,
		Version:         versions.For("cert-manager"),
		CreateNamespace: true,
		ValuesYaml:      certManagerValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("cert-manager: helm install: %w", err)
	}

	return nil
}

func (*CertManagerAddon) Ready(ctx context.Context, h ClusterHandle) error {
	for _, name := range certManagerDeployments {
		name := name
		if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				d, err := h.K8sClient.AppsV1().Deployments(certManagerNamespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				return d.Status.ReadyReplicas >= 1, nil
			},
		); err != nil {
			return fmt.Errorf("cert-manager: waiting for deployment %s: %w", name, err)
		}
	}
	return nil
}

func (*CertManagerAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(certManagerNamespace)
	if err != nil {
		return fmt.Errorf("cert-manager: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(certManagerRelease); err != nil {
		return fmt.Errorf("cert-manager: helm uninstall: %w", err)
	}
	return nil
}
