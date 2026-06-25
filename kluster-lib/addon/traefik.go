package addon

import (
	"context"
	"fmt"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/bytepunx/kluster-lib/versions"
)

// traefik-tls patches the k3s-bundled Traefik instance to serve TLS using a
// cert-manager-issued certificate. No separate Traefik installation is done —
// the k3s-bundled instance is configured in place.
//
// Assumes Traefik v3 (bundled with k3s v1.31+) which uses the traefik.io/v1alpha1
// API group. Earlier k3s versions use traefik.containo.us/v1alpha1.

type TraefikTLSAddon struct{}

var _ Addon = (*TraefikTLSAddon)(nil)

func init() { Register(&TraefikTLSAddon{}) }

func (*TraefikTLSAddon) Name() string      { return "traefik-tls" }
func (*TraefikTLSAddon) Requires() []string { return []string{"cert-manager"} }

const (
	traefikNamespace  = "kube-system"
	traefikCertName   = "kluster-default-tls"
	traefikIssuerName = "kluster-selfsigned"

	// Helm install constants — used when Traefik is not pre-bundled (e.g. kind)
	traefikHelmRelease  = "traefik"
	traefikHelmChart    = "traefik/traefik"
	traefikHelmRepoName = "traefik"
	traefikHelmRepoURL  = "https://traefik.github.io/charts"

	// On kind there is no LoadBalancer controller; ClusterIP avoids a Service
	// stuck in <pending> and is sufficient for in-cluster use.
	traefikHelmValues = `service:
  type: ClusterIP
`
)

var certGVR = schema.GroupVersionResource{
	Group:    "cert-manager.io",
	Version:  "v1",
	Resource: "certificates",
}

// patchTypeApply is the patch type for server-side apply.
const patchTypeApply = types.ApplyPatchType

func (*TraefikTLSAddon) Install(ctx context.Context, h ClusterHandle) error {
	// 0. On k3d/k3s, Traefik is pre-bundled; on kind it is not. Install via
	//    Helm if the TLSStore CRD is absent, then refresh the REST mapper cache.
	tlsStoreGVK := schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "TLSStore"}
	if _, err := h.RESTMapper.RESTMapping(tlsStoreGVK.GroupKind(), tlsStoreGVK.Version); err != nil {
		if err := installTraefikHelm(ctx, h); err != nil {
			return err
		}
		// Invalidate the mapper's discovery cache so the new CRDs are visible.
		type resetter interface{ Reset() }
		if r, ok := h.RESTMapper.(resetter); ok {
			r.Reset()
		}
	}

	// 1. Self-signed ClusterIssuer (cluster-scoped, no namespace)
	if err := ApplyManifest(ctx, h, `
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: kluster-selfsigned
spec:
  selfSigned: {}`); err != nil {
		return fmt.Errorf("traefik-tls: apply ClusterIssuer: %w", err)
	}

	// 2. Certificate in kube-system — cert-manager issues the TLS secret here
	dnsNames := traefikDNSNames(h.Config.TrustDomain)
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: "Certificate",
	})
	cert.SetName(traefikCertName)
	cert.SetNamespace(traefikNamespace)
	if err := unstructured.SetNestedMap(cert.Object, map[string]interface{}{
		"secretName": traefikCertName,
		"issuerRef": map[string]interface{}{
			"name": traefikIssuerName,
			"kind": "ClusterIssuer",
		},
		"dnsNames": dnsNames,
	}, "spec"); err != nil {
		return fmt.Errorf("traefik-tls: build Certificate spec: %w", err)
	}
	data, err := marshalForApply(cert)
	if err != nil {
		return fmt.Errorf("traefik-tls: marshal Certificate: %w", err)
	}
	if _, err := h.DynClient.Resource(certGVR).Namespace(traefikNamespace).Patch(
		ctx, traefikCertName, patchTypeApply, data, metav1.PatchOptions{
			FieldManager: "kluster",
			Force:        boolPtr(true),
		},
	); err != nil {
		return fmt.Errorf("traefik-tls: apply Certificate: %w", err)
	}

	// 3. Traefik TLSStore — makes the cert-manager-issued secret the default
	// traefik.io/v1alpha1 is the Traefik v3 API group (k3s v1.31+).
	if err := ApplyManifest(ctx, h, fmt.Sprintf(`
apiVersion: traefik.io/v1alpha1
kind: TLSStore
metadata:
  name: default
  namespace: %s
spec:
  defaultCertificate:
    secretName: %s`, traefikNamespace, traefikCertName)); err != nil {
		return fmt.Errorf("traefik-tls: apply TLSStore: %w", err)
	}

	return nil
}

func (*TraefikTLSAddon) Ready(ctx context.Context, h ClusterHandle) error {
	// When Traefik was installed via Helm (not pre-bundled), wait for its pod.
	if _, err := h.K8sClient.AppsV1().Deployments(traefikNamespace).Get(
		ctx, traefikHelmRelease, metav1.GetOptions{},
	); err == nil {
		if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				d, err := h.K8sClient.AppsV1().Deployments(traefikNamespace).Get(
					ctx, traefikHelmRelease, metav1.GetOptions{},
				)
				if err != nil {
					return false, nil
				}
				return d.Status.ReadyReplicas >= 1, nil
			},
		); err != nil {
			return fmt.Errorf("traefik-tls: waiting for traefik deployment: %w", err)
		}
	}

	// Wait for the cert-manager Certificate to be issued.
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			obj, err := h.DynClient.Resource(certGVR).Namespace(traefikNamespace).Get(
				ctx, traefikCertName, metav1.GetOptions{},
			)
			if err != nil {
				return false, nil
			}
			return waitForCondition(obj, "Ready"), nil
		},
	)
}

func (*TraefikTLSAddon) Uninstall(ctx context.Context, h ClusterHandle) error {
	_ = h.DynClient.Resource(certGVR).Namespace(traefikNamespace).Delete(
		ctx, traefikCertName, metav1.DeleteOptions{},
	)
	return nil
}

// installTraefikHelm installs Traefik via its official Helm chart.
// Called only when Traefik is not pre-bundled with the cluster runtime (kind).
func installTraefikHelm(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(traefikNamespace)
	if err != nil {
		return fmt.Errorf("traefik-tls: helm client: %w", err)
	}
	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: traefikHelmRepoName,
		URL:  traefikHelmRepoURL,
	}); err != nil {
		return fmt.Errorf("traefik-tls: add helm repo: %w", err)
	}
	// "hookOnly" waits for chart hooks (CRD pre-install) but NOT for pod
	// readiness or service endpoints. This avoids a hang on kind where the
	// LoadBalancer service never gets an external IP. Pod readiness is
	// checked explicitly in Ready() instead.
	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     traefikHelmRelease,
		ChartName:       traefikHelmChart,
		Namespace:       traefikNamespace,
		Version:         versions.For("traefik"),
		CreateNamespace: true,
		WaitStrategy:    "hookOnly",
		DryRunStrategy:  helmaction.DryRunNone,
		Timeout:         2 * time.Minute,
	}, nil)
	if err != nil {
		return fmt.Errorf("traefik-tls: helm install traefik: %w", err)
	}
	return nil
}

func traefikDNSNames(trustDomain string) []interface{} {
	if trustDomain == "" {
		trustDomain = "dev.cluster.local"
	}
	return []interface{}{"*." + trustDomain, trustDomain, "localhost"}
}
