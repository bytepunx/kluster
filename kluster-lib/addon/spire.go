package addon

import (
	"context"
	"fmt"
	"net/http"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"

	"github.com/bytepunx/kluster-lib/versions"
)

const (
	spireNamespace = "spire-system"
	spireRelease   = "spire"
	spireChart     = "spiffe/spire"
	spireRepoName  = "spiffe"
	spireRepoURL   = "https://spiffe.github.io/helm-charts/"
)

type SpireAddon struct{}

var _ Addon = (*SpireAddon)(nil)

func init() { Register(&SpireAddon{}) }

func (*SpireAddon) Name() string      { return "spire" }
func (*SpireAddon) Requires() []string { return []string{"cert-manager"} }

func (*SpireAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(spireNamespace)
	if err != nil {
		return fmt.Errorf("spire: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: spireRepoName,
		URL:  spireRepoURL,
	}); err != nil {
		return fmt.Errorf("spire: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     spireRelease,
		ChartName:       spireChart,
		Namespace:       spireNamespace,
		Version:         versions.For("spire"),
		CreateNamespace: true,
		ValuesYaml:      spireValues(h.Config.TrustDomain),
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("spire: helm install: %w", err)
	}

	return nil
}

func (*SpireAddon) Ready(ctx context.Context, h ClusterHandle) error {
	// SPIRE Server — StatefulSet
	if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(spireNamespace).Get(ctx, "spire-server", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	); err != nil {
		return fmt.Errorf("spire: waiting for spire-server statefulset: %w", err)
	}

	// SPIRE Agent — DaemonSet
	if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ds, err := h.K8sClient.AppsV1().DaemonSets(spireNamespace).Get(ctx, "spire-agent", metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ds.Status.NumberReady >= 1, nil
		},
	); err != nil {
		return fmt.Errorf("spire: waiting for spire-agent daemonset: %w", err)
	}

	// SPIRE Server health endpoint — confirms control plane is serving
	if err := spireProxyHealth(ctx, h, "spire-server-0", 8080, "/ready", "server"); err != nil {
		return fmt.Errorf("spire: server health endpoint: %w", err)
	}

	// SPIRE Agent health endpoint — find DaemonSet pod then probe liveness
	agentPods, err := h.K8sClient.CoreV1().Pods(spireNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=agent,app.kubernetes.io/instance=spire",
	})
	if err != nil || len(agentPods.Items) == 0 {
		return fmt.Errorf("spire: no agent pods found")
	}
	if err := spireProxyHealth(ctx, h, agentPods.Items[0].Name, 9980, "/live", "agent"); err != nil {
		return fmt.Errorf("spire: agent health endpoint: %w", err)
	}

	return nil
}

// spireProxyHealth polls the Kubernetes API server proxy to reach a named pod's
// HTTP health endpoint without requiring a port-forward or cluster ingress.
func spireProxyHealth(ctx context.Context, h ClusterHandle, pod string, port int, path, component string) error {
	transport, err := rest.TransportFor(h.RESTConfig)
	if err != nil {
		return fmt.Errorf("build transport: %w", err)
	}
	httpClient := &http.Client{Transport: transport}
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s:%d/proxy%s",
		h.RESTConfig.Host, spireNamespace, pod, port, path)

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return false, nil
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return false, nil
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK, nil
		},
	)
}

func (*SpireAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(spireNamespace)
	if err != nil {
		return fmt.Errorf("spire: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(spireRelease); err != nil {
		return fmt.Errorf("spire: helm uninstall: %w", err)
	}
	return nil
}

func spireValues(trustDomain string) string {
	if trustDomain == "" {
		trustDomain = "dev.cluster.local"
	}
	return fmt.Sprintf(`
global:
  spire:
    trustDomain: %s
  installAndUpgradeHooks:
    enabled: false
  deleteHooks:
    enabled: false

spire-server:
  replicaCount: 1
  trustDomain: %s

spire-agent:
  logLevel: WARN

spiffe-csi-driver:
  enabled: true

spire-controller-manager:
  enabled: true
`, trustDomain, trustDomain)
}
