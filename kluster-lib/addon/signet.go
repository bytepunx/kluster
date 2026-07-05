package addon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/bytepunx/kluster-lib/versions"
)

const (
	signetNamespace = "signet"
	signetRelease   = "signet"
	signetChart     = "oci://ghcr.io/bytepunx/charts/signet"

	// signetMasterKeySecret holds the 32-byte master key signetd reads at
	// startup via Kubernetes auto-unseal (field "master.key"). Pre-creating
	// it before install lets signetd come up already unsealed with no
	// `signet init` / admin-gRPC step required.
	signetMasterKeySecret = "signet-master-key"

	// signetSpireSocketHostPath is the node-side directory holding the SPIRE
	// agent's workload API socket when SPIRE is installed via the spiffe/spire
	// chart with spiffe-csi-driver enabled (see addon/spire.go). This differs
	// from signet's own chart default ("/run/spire/sockets"), so it must be
	// passed as an explicit override.
	signetSpireSocketHostPath = "/run/spire/agent-sockets"

	// signetSpireSocketPath is the unix socket signetd dials, inside its own
	// container. The mountPath is hardcoded to /run/spire/sockets by the
	// chart; the socket file itself is named "spire-agent.sock" (not the
	// chart default "agent.sock"), matching the spiffe/spire chart's agent
	// socket filename.
	signetSpireSocketPath = "unix:///run/spire/sockets/spire-agent.sock"

	// signetUnsealLogLine is written by signetd on a successful Kubernetes
	// auto-unseal. Ready() greps for it because the readiness probe is a bare
	// TCP check on the gRPC port and does not reflect seal state.
	signetUnsealLogLine = "unsealed via Kubernetes Secret"
)

type SignetAddon struct{}

var _ Addon = (*SignetAddon)(nil)

func init() { Register(&SignetAddon{}) }

func (*SignetAddon) Name() string      { return "signet" }
func (*SignetAddon) Requires() []string { return []string{"spire"} }

func (*SignetAddon) Install(ctx context.Context, h ClusterHandle) error {
	if err := ensureNamespace(ctx, h, signetNamespace); err != nil {
		return fmt.Errorf("signet: ensure namespace: %w", err)
	}

	if err := ensureSignetMasterKeySecret(ctx, h); err != nil {
		return fmt.Errorf("signet: master key secret: %w", err)
	}

	auditChainKey, err := randomHexKey(32)
	if err != nil {
		return fmt.Errorf("signet: generate audit chain key: %w", err)
	}

	hc, err := h.HelmClientFor(signetNamespace)
	if err != nil {
		return fmt.Errorf("signet: helm client: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     signetRelease,
		ChartName:       signetChart,
		Namespace:       signetNamespace,
		Version:         versions.For("signet"),
		CreateNamespace: true,
		ValuesYaml:      signetValues(h.Config.TrustDomainOrDefault(), auditChainKey),
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
		Timeout:         10 * time.Minute,
	}, nil)
	if err != nil {
		return fmt.Errorf("signet: helm install: %w", err)
	}

	return nil
}

// signetValues has a dev-only security posture, matching every other addon
// in this package: CockroachDB runs single-node, insecure (root, no TLS —
// deploy/helm/signet's cockroachdb.enabled convenience mode), and
// auditChainKey travels through Helm values, so it's readable via
// `helm get values signet -n signet` and lands in the Helm release Secret.
// Same-namespace secret-read access either way, and the master key (the one
// that actually gates access to encrypted data) never takes this path — see
// ensureSignetMasterKeySecret — but this is not a production configuration.
func signetValues(trustDomain, auditChainKey string) string {
	dbConnString := fmt.Sprintf(
		"postgresql://root@%s-cockroachdb.%s.svc.cluster.local:26257/signet?sslmode=disable",
		signetRelease, signetNamespace,
	)
	return fmt.Sprintf(`
signet:
  trustDomain: %s
  auditChainKey: %q
  dbConnString: %q
  spireSocket: %q

spire:
  socketHostPath: %q

autoUnseal:
  enabled: true
  secretName: %q

cockroachdb:
  enabled: true
`, trustDomain, auditChainKey, dbConnString, signetSpireSocketPath,
		signetSpireSocketHostPath, signetMasterKeySecret)
}

// ensureSignetMasterKeySecret creates the auto-unseal Secret if it does not
// already exist. It never overwrites an existing key: regenerating it would
// orphan every secret already encrypted under the old master key.
func ensureSignetMasterKeySecret(ctx context.Context, h ClusterHandle) error {
	_, err := h.K8sClient.CoreV1().Secrets(signetNamespace).Get(ctx, signetMasterKeySecret, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get existing secret: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate master key: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      signetMasterKeySecret,
			Namespace: signetNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"master.key": key,
		},
	}
	if _, err := h.K8sClient.CoreV1().Secrets(signetNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create secret: %w", err)
	}
	return nil
}

// ensureNamespace creates the namespace if absent. Needed here (rather than
// relying on Helm's CreateNamespace) because the master key Secret must exist
// in-namespace before the Helm install runs.
func ensureNamespace(ctx context.Context, h ClusterHandle, name string) error {
	_, err := h.K8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func randomHexKey(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (*SignetAddon) Ready(ctx context.Context, h ClusterHandle) error {
	if err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			d, err := h.K8sClient.AppsV1().Deployments(signetNamespace).Get(ctx, signetRelease, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	); err != nil {
		return fmt.Errorf("signet: waiting for deployment: %w", err)
	}

	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.K8sClient.CoreV1().Pods(signetNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s,app.kubernetes.io/instance=%s", signetRelease, signetRelease),
			})
			if err != nil || len(pods.Items) == 0 {
				return false, nil
			}
			logs, err := readPodLogs(ctx, h, signetNamespace, pods.Items[0].Name, "signetd")
			if err != nil {
				return false, nil
			}
			return strings.Contains(logs, signetUnsealLogLine), nil
		},
	)
}

func (*SignetAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(signetNamespace)
	if err != nil {
		return fmt.Errorf("signet: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(signetRelease); err != nil {
		return fmt.Errorf("signet: helm uninstall: %w", err)
	}
	return nil
}
