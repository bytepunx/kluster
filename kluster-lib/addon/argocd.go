package addon

import (
	"context"
	"fmt"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/bytepunx/kluster-lib/versions"
)

const (
	argoCDNamespace = "argocd"
	argoCDRelease   = "argocd"
	argoCDChart     = "argo/argo-cd"
	argoCDRepoName  = "argo"
	argoCDRepoURL   = "https://argoproj.github.io/argo-helm"

	// AdminPassword is the plaintext password pre-configured for the ArgoCD
	// admin user. It is bcrypt-hashed at install time and set via Helm values.
	// Intended for dev/CI only — never use this in production.
	ArgoCDAdminPassword = "kluster-admin"
)

// argoCDValues returns the Helm values for ArgoCD.
// passwordHash is a bcrypt hash of ArgoCDAdminPassword.
func argoCDValues(trustDomain, passwordHash string) string {
	if trustDomain == "" {
		trustDomain = "dev.cluster.local"
	}
	return fmt.Sprintf(`
global:
  domain: argocd.%s

server:
  replicas: 1

configs:
  params:
    server.insecure: "true"
  secret:
    argocdServerAdminPassword: %q
    argocdServerAdminPasswordMtime: "2024-01-01T00:00:00Z"

# Use the kluster Dex addon for SSO instead of the bundled one.
dex:
  enabled: false

# Notifications are not needed in dev/CI clusters.
notifications:
  enabled: false

redis:
  enabled: true

repoServer:
  replicas: 1

applicationSet:
  replicas: 1
`, trustDomain, passwordHash)
}

type ArgoCDAddon struct{}

var _ Addon = (*ArgoCDAddon)(nil)

func init() { Register(&ArgoCDAddon{}) }

func (*ArgoCDAddon) Name() string      { return "argocd" }
func (*ArgoCDAddon) Requires() []string { return []string{"traefik-tls"} }

func (*ArgoCDAddon) Install(ctx context.Context, h ClusterHandle) error {
	// Hash the admin password at install time so it never appears in plaintext
	// in a Kubernetes secret.
	hash, err := bcrypt.GenerateFromPassword([]byte(ArgoCDAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("argocd: hash admin password: %w", err)
	}

	hc, err := h.HelmClientFor(argoCDNamespace)
	if err != nil {
		return fmt.Errorf("argocd: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: argoCDRepoName,
		URL:  argoCDRepoURL,
	}); err != nil {
		return fmt.Errorf("argocd: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     argoCDRelease,
		ChartName:       argoCDChart,
		Namespace:       argoCDNamespace,
		Version:         versions.For("argocd"),
		CreateNamespace: true,
		ValuesYaml:      argoCDValues(h.Config.TrustDomain, string(hash)),
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
		Timeout:         15 * time.Minute,
	}, nil)
	if err != nil {
		return fmt.Errorf("argocd: helm install: %w", err)
	}

	// Expose ArgoCD UI via Traefik IngressRoute using the cluster's default
	// TLS certificate (issued by cert-manager via the traefik-tls addon).
	domain := h.Config.TrustDomain
	if domain == "" {
		domain = "dev.cluster.local"
	}
	if err := ApplyManifest(ctx, h, fmt.Sprintf(`
apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata:
  name: argocd
  namespace: %s
spec:
  entryPoints:
    - websecure
  routes:
    - match: Host(%q)
      kind: Rule
      services:
        - name: argocd-server
          port: 80
  tls: {}`, argoCDNamespace, "argocd."+domain)); err != nil {
		return fmt.Errorf("argocd: apply IngressRoute: %w", err)
	}

	return nil
}

func (*ArgoCDAddon) Ready(ctx context.Context, h ClusterHandle) error {
	deployments := []string{
		argoCDRelease + "-server",
		argoCDRelease + "-repo-server",
	}
	for _, name := range deployments {
		name := name
		if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				d, err := h.K8sClient.AppsV1().Deployments(argoCDNamespace).Get(
					ctx, name, metav1.GetOptions{},
				)
				if err != nil {
					return false, nil
				}
				return d.Status.ReadyReplicas >= 1, nil
			},
		); err != nil {
			return fmt.Errorf("argocd: waiting for deployment %s: %w", name, err)
		}
	}

	// application-controller is a StatefulSet, not a Deployment.
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(argoCDNamespace).Get(
				ctx, argoCDRelease+"-application-controller", metav1.GetOptions{},
			)
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*ArgoCDAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(argoCDNamespace)
	if err != nil {
		return fmt.Errorf("argocd: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(argoCDRelease); err != nil {
		return fmt.Errorf("argocd: helm uninstall: %w", err)
	}
	return nil
}
