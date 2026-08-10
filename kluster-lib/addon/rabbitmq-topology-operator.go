package addon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/bytepunx/kluster-lib/versions"
)

// RabbitMQTopologyOperatorAddon installs the upstream RabbitMQ Messaging
// Topology Operator (github.com/rabbitmq/messaging-topology-operator) —
// NOT an authstar-specific component. Unlike every other addon in this
// package, it isn't a Helm chart: upstream distributes it as a single
// static "kubectl apply -f" release manifest (see versions.go's
// SourceGithubRelease resolution path), so Install() fetches and applies
// that manifest directly via ApplyManifest instead of going through
// go-helm-client.
//
// The "-with-certmanager" manifest variant is used deliberately: it wires
// its own webhook TLS via cert-manager Certificate/Issuer resources rather
// than a self-signed-cert init job, reusing the cert-manager addon this
// addon Requires().
//
// Once installed, the operator's CRDs (Queue, Exchange, Binding, User,
// Vhost, Policy, Permission, Federation, Shovel, ...) are consumed two
// ways: every CRD kind supports a plain `connectionSecret` under
// spec.rabbitmqClusterReference pointing at any externally-managed
// RabbitMQ with a reachable management API — so this operator manages the
// existing bitnami-chart RabbitMQ installed by rabbitmq.go directly; no
// migration to the separate RabbitMQ Cluster Operator is needed.
//
// It does, however, need one single piece of that separate operator: the
// RabbitmqCluster CRD *definition* (not its controller). Confirmed by
// reading the Topology Operator's actual startup logs on a cluster that
// never installed it:
//
//	ERROR controller-runtime.source.Kind if kind is a CRD, it should be
//	  installed before calling Start {"kind": "RabbitmqCluster.rabbitmq.com",
//	  "error": "no matches for kind \"RabbitmqCluster\" in version
//	  \"rabbitmq.com/v1beta1\""}
//	ERROR setup problem running manager {"error": "failed to wait for
//	  policy caches to sync kind source: *v1beta1.RabbitmqCluster: timed
//	  out waiting for cache to be synced..."}
//
// The manager unconditionally starts a watch/informer on RabbitmqCluster
// (to auto-resolve connection details when a CR uses
// rabbitmqClusterReference.name instead of connectionSecret — a mode
// authstar never uses), and controller-runtime refuses to start any
// informer for a CRD kind the API server doesn't recognize at all, so the
// whole manager — and therefore this operator's pod — fails to start
// without it. Since every CR this addon's consumers create always uses
// connectionSecret (see rabbitmq.go's RabbitMQConnectionSecretName and
// profile/authstar.go), no RabbitmqCluster object is ever created, so an
// always-empty watch on a CRD whose controller doesn't exist is
// functionally a no-op — it just has to exist for the informer to start
// successfully. installRabbitmqClusterCRD extracts exactly that one CRD
// document (confirmed via `rabbitmq/cluster-operator`'s own release
// manifest: it's the sole "kind: CustomResourceDefinition" document,
// ~5500 lines on its own of the ~6000-line file, followed by that
// project's own RBAC/webhooks/Deployment which are deliberately never
// applied here) rather than installing the full separate operator.
type RabbitMQTopologyOperatorAddon struct{}

var _ Addon = (*RabbitMQTopologyOperatorAddon)(nil)

func init() { Register(&RabbitMQTopologyOperatorAddon{}) }

const (
	rabbitmqTopologyOperatorAddonName      = "rabbitmq-topology-operator"
	rabbitmqTopologyOperatorNamespace      = "rabbitmq-system"
	rabbitmqTopologyOperatorDeployment     = "messaging-topology-operator"
	rabbitmqTopologyOperatorWebhookService = "messaging-topology-webhook-service"
	rabbitmqTopologyOperatorGithubRepo     = "rabbitmq/messaging-topology-operator"

	// rabbitmqClusterCRDAddonName/GithubRepo: see the RabbitmqCluster CRD
	// doc comment above this type — pinned independently via versions.go
	// since it comes from an entirely different upstream project/release
	// cadence than the Topology Operator itself.
	rabbitmqClusterCRDAddonName  = "rabbitmq-cluster-operator-crds"
	rabbitmqClusterCRDGithubRepo = "rabbitmq/cluster-operator"
	rabbitmqClusterCRDName       = "rabbitmqclusters.rabbitmq.com"

	// manifestFetchTimeout bounds the release-manifest download so a hung
	// GitHub Releases CDN response can't stall "kluster up" indefinitely —
	// matching the same defensive posture versions.go's own index.yaml
	// fetch uses.
	manifestFetchTimeout = 30 * time.Second

	// maxManifestSize bounds the response body read; the real manifest is
	// well under 1 MB (13 CRDs + RBAC + Deployment + webhook config).
	maxManifestSize = 10 * 1024 * 1024
)

func (*RabbitMQTopologyOperatorAddon) Name() string { return rabbitmqTopologyOperatorAddonName }
func (*RabbitMQTopologyOperatorAddon) Requires() []string {
	return []string{"cert-manager", "rabbitmq"}
}

func (*RabbitMQTopologyOperatorAddon) Install(ctx context.Context, h ClusterHandle) error {
	// Must land before the Topology Operator's own Deployment starts —
	// see this type's doc comment for why its manager can't even start
	// without this CRD registered.
	if err := installRabbitmqClusterCRD(ctx, h); err != nil {
		return fmt.Errorf("rabbitmq-topology-operator: install RabbitmqCluster CRD: %w", err)
	}

	version := versions.For(rabbitmqTopologyOperatorAddonName)
	url := fmt.Sprintf(
		"https://github.com/%s/releases/download/%s/messaging-topology-operator-with-certmanager.yaml",
		rabbitmqTopologyOperatorGithubRepo, version,
	)

	manifest, err := fetchManifest(ctx, url)
	if err != nil {
		return fmt.Errorf("rabbitmq-topology-operator: fetch release manifest: %w", err)
	}

	// The release manifest is a single multi-document YAML file (Namespace,
	// 13 CRDs, RBAC, Service, Deployment, cert-manager Certificate/Issuer,
	// webhook configs — ~30 documents). addon.ApplyManifest applies exactly
	// one object per call, so split on "---" document separators and apply
	// each in the file's own order — which already places the Namespace
	// and CRDs before anything that depends on them.
	for i, doc := range splitYAMLDocuments(manifest) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		if err := ApplyManifest(ctx, h, doc); err != nil {
			return fmt.Errorf("rabbitmq-topology-operator: apply document %d: %w", i, err)
		}
	}

	return nil
}

// installRabbitmqClusterCRD fetches the RabbitMQ *Cluster* Operator's own
// release manifest and applies only its RabbitmqCluster CRD definition —
// deliberately not the rest of that manifest (its own controller
// Deployment, RBAC, webhooks), which would mean actually running a second
// operator this addon's own design avoids needing. See this file's type
// doc comment for the full why.
func installRabbitmqClusterCRD(ctx context.Context, h ClusterHandle) error {
	version := versions.For(rabbitmqClusterCRDAddonName)
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/cluster-operator.yml", rabbitmqClusterCRDGithubRepo, version)

	manifest, err := fetchManifest(ctx, url)
	if err != nil {
		return fmt.Errorf("fetch cluster-operator release manifest: %w", err)
	}

	doc, err := extractDocument(manifest, "CustomResourceDefinition", rabbitmqClusterCRDName)
	if err != nil {
		return fmt.Errorf("extract %s CRD from cluster-operator manifest: %w", rabbitmqClusterCRDName, err)
	}

	return ApplyManifest(ctx, h, doc)
}

// extractDocument finds the single YAML document within a multi-document
// manifest whose "kind:" and "metadata.name:" match, and returns it alone.
// A substring scan (rather than a full YAML parse) is deliberate and
// sufficient here — same reasoning as splitYAMLDocuments — since
// ApplyManifest performs the real parse once this returns exactly one
// document.
func extractDocument(manifest, kind, name string) (string, error) {
	kindLine := "kind: " + kind
	nameLine := "name: " + name
	for _, doc := range splitYAMLDocuments(manifest) {
		if strings.Contains(doc, kindLine) && strings.Contains(doc, nameLine) {
			return doc, nil
		}
	}
	return "", fmt.Errorf("no document found with kind %q and metadata.name %q", kind, name)
}

func (*RabbitMQTopologyOperatorAddon) Ready(ctx context.Context, h ClusterHandle) error {
	if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			d, err := h.K8sClient.AppsV1().Deployments(rabbitmqTopologyOperatorNamespace).
				Get(ctx, rabbitmqTopologyOperatorDeployment, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	); err != nil {
		return fmt.Errorf("rabbitmq-topology-operator: waiting for deployment: %w", err)
	}

	// The Deployment reporting ReadyReplicas >= 1 does not mean its
	// ValidatingWebhookConfiguration is actually reachable yet: the
	// webhook Service's Endpoints are populated by a separate
	// controller/kube-proxy path that can lag a few seconds behind pod
	// readiness (the same class of eventual-consistency gap SPIRE's own
	// addon in this package guards against with its own health-endpoint
	// probes in Ready(), rather than trusting Deployment/DaemonSet status
	// alone — see spireProxyHealth in spire.go). Every CR this operator
	// manages (Vhost, User, Permission, ...) goes through this webhook
	// for validation, so applying one before its Service has a ready
	// endpoint fails with "no endpoints available for service
	// messaging-topology-webhook-service" — observed for real applying
	// the authstar profile's Vhost instance immediately after this
	// addon's Ready() returned.
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 1*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ep, err := h.K8sClient.CoreV1().Endpoints(rabbitmqTopologyOperatorNamespace).
				Get(ctx, rabbitmqTopologyOperatorWebhookService, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			for _, subset := range ep.Subsets {
				if len(subset.Addresses) > 0 {
					return true, nil
				}
			}
			return false, nil
		},
	)
}

func (*RabbitMQTopologyOperatorAddon) Uninstall(_ context.Context, _ ClusterHandle) error {
	// The release manifest was applied document-by-document via
	// server-side apply rather than a Helm release, so there's no single
	// "uninstall" handle. kluster's disposable-cluster model (the whole
	// cluster is torn down via `kluster down`, not individual addons in
	// practice) makes this an acceptable gap — matching how apply.go's own
	// ApplyManifest doesn't track applied objects for cleanup either.
	return nil
}

func fetchManifest(ctx context.Context, url string) (string, error) {
	client := &http.Client{Timeout: manifestFetchTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// splitYAMLDocuments splits a multi-document YAML file on "---" separator
// lines. A plain string split (rather than a YAML-aware stream decoder) is
// enough here since ApplyManifest itself does the real YAML parsing per
// document; this only needs to find the boundaries.
func splitYAMLDocuments(manifest string) []string {
	var docs []string
	docs = append(docs, strings.Split(manifest, "\n---")...)
	return docs
}
