package addon

import (
	"context"
	"fmt"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// rabbitmqBaseImageTag is the upstream RabbitMQ version this addon
	// pins, kept in sync with images/rabbitmq-kickr/Dockerfile's own
	// RABBITMQ_BASE_TAG build arg default. Broadcom's 2025 restructuring
	// of Bitnami's Docker Hub distribution removed this pinned tag from
	// the free "bitnami/*" org (confirmed via `docker manifest inspect
	// docker.io/bitnami/rabbitmq:4.1.3-debian-12-r1` returning "not
	// found"); the same tag is still served from the free-but-frozen
	// "bitnamilegacy/*" org, which images/rabbitmq-kickr builds from.
	rabbitmqBaseImageTag = "4.1.3-debian-12-r1"

	// rabbitmqKickrImageRepo/Tag identify the composite image built from
	// images/rabbitmq-kickr/Dockerfile: bitnamilegacy/rabbitmq with kickr
	// (per ADR 0010) layered in as PID 1. Published to Docker Hub by
	// images/rabbitmq-kickr's own CI workflow, mirroring
	// portcullis/.github/workflows/docker.yml's pattern; not yet pushed as
	// of this writing; for a fully offline dev loop, `docker build` the
	// image locally with this exact tag and `k3d image import` it into the
	// target cluster before running `kluster up`, or the StatefulSet pod
	// will sit in ImagePullBackOff until it can reach Docker Hub.
	rabbitmqKickrImageRepo = "bytepunx/rabbitmq-kickr"
	rabbitmqKickrImageTag  = "4.1.3-kickr1.0.0"

	// rabbitmqKickrConfigMap holds the .kickr.yaml file — kickr's
	// namespace/service identity has no environment variable equivalent
	// (see ~/git/kickr/design/draft.md section 3.1's table), so it must
	// come from a mounted file. namespace/service here also double as the
	// signet bundle path RabbitMQ's bootstrap credential is seeded into
	// (see seedRabbitMQBootstrapCredential) and must match the SPIFFE
	// identity the RabbitMQ pod's own ServiceAccount is issued — which,
	// with release name == chart name, is "rabbitmq" per the bitnami
	// chart's serviceAccount.name default (rabbitmq.fullname template).
	rabbitmqKickrConfigMap  = "rabbitmq-kickr-config"
	rabbitmqSignetNamespace = "rabbitmq"
	rabbitmqSignetService   = "rabbitmq"

	// rabbitmqUsernameSecretName/rabbitmqPasswordSecretName are the signet
	// secret names kickr's env-var injection turns into RABBITMQ_USERNAME
	// / RABBITMQ_PASSWORD (hyphen -> underscore, upcase — see kickr's
	// design doc section 4.2's "db/password -> DB_PASSWORD" example).
	rabbitmqUsernameSecretName = "rabbitmq-username"
	rabbitmqPasswordSecretName = "rabbitmq-password"

	// rabbitmqManagementPort is the bitnami chart's default RabbitMQ
	// management-API port (distinct from 5672/amqp) — what the Messaging
	// Topology Operator's connectionSecret "uri" must point at.
	rabbitmqManagementPort = 15672
)

// RabbitMQNamespace, RabbitMQConnectionSecretName, and RabbitMQVhost are
// exported for profile/authstar.go, which wires the RabbitMQ Messaging
// Topology Operator's CRDs and provisioning Job against this same
// namespace/Secret/vhost — see ensureTopologyOperatorConnectionSecret below
// for what populates RabbitMQConnectionSecretName.
const (
	RabbitMQNamespace            = rabbitmqNamespace
	RabbitMQConnectionSecretName = "rabbitmq-topology-operator-connection"
	RabbitMQVhost                = "authstar"

	// rabbitmqValues is a Go format string, not a plain literal: %[1]s is
	// the cluster's SPIFFE trust domain, used both for the KICKR_SIGNET_*
	// env vars and to pin signet's own exact SPIFFE ID (ns/signet/sa/signet
	// — confirmed via `helm template` against signet's own chart: Service
	// "signet" in namespace "signet", ServiceAccount "signet"). Pinning the
	// exact ID rather than falling back to KICKR_SIGNET_TRUST_DOMAIN is
	// kickr's own documented recommendation (design/draft.md section 7.2).
	//
	// auth.username/auth.password are seeded with the SAME real, random
	// credential already pushed into signet (see
	// seedRabbitMQBootstrapCredential, called before this values string is
	// rendered) — not guest/guest — because of a real, non-obvious
	// interaction in bitnami's own setup.sh/librabbitmq.sh: with
	// RABBITMQ_LOAD_DEFINITIONS unset (our config), a *fresh* data volume's
	// first-boot path calls `rabbitmqctl change_password
	// "$RABBITMQ_USERNAME" ...` unconditionally — not `add_user` — which
	// assumes that user already exists. RabbitMQ's own native bootstrap
	// only ever auto-creates the literal "guest" account, so if
	// RABBITMQ_USERNAME is anything else (kickr's env-var override to the
	// real signet-sourced username, e.g. "rmq-..."), change_password fails
	// outright ("Couldn't change password for user 'rmq-...'") and the
	// container exits — confirmed for real against a live cluster, not
	// theoretical. Baking the real username into auth.username makes Helm
	// render it as RABBITMQ_USERNAME from the start, so RabbitMQ's own
	// bootstrap creates *that* exact account on first boot — no
	// add_user-vs-change_password chicken-and-egg problem, regardless of
	// what kickr injects at runtime.
	//
	// kickr's runtime env-var override (design doc 4.3: the bundle's
	// "secrets" tier wins over the base container environment) still
	// matters for the *rotation* path: MODE=rotate's CronJob (see
	// profile/authstar.go) pushes a fresh password into signet and expects
	// kickr's WatchServiceBundle-triggered rolling restart to pick it up
	// and call change_password against the *already-existing* user — which
	// works fine, since by then the account is real. This values-baked
	// credential only replaces relying on that same mechanism for the
	// very first boot, where the target account doesn't exist yet.
	//
	// image.repository/tag point at the composite rabbitmq-kickr image
	// (see rabbitmqKickrImageRepo/Tag above) instead of bitnamilegacy
	// directly. global.security.allowInsecureImages is still required:
	// the chart's own NOTES.txt refuses to complete install when the
	// resolved image isn't in its "recognized" list, which any
	// non-bitnami-org repository trips (see
	// https://github.com/bitnami/charts/issues/30850).
	rabbitmqValues = `
replicaCount: 1

global:
  security:
    allowInsecureImages: true

image:
  registry: docker.io
  repository: %[2]s
  tag: %[3]s

auth:
  username: %[5]s
  password: %[6]s

# usePasswordFiles defaults to true: the chart then mounts auth.password as
# a Secret-backed *file* and sets RABBITMQ_PASSWORD_FILE rather than a
# plain RABBITMQ_PASSWORD env var. Confirmed for real against a live
# cluster why that breaks kickr: bitnami's own rabbitmq-env.sh (sourced by
# every startup script) unconditionally overwrites RABBITMQ_PASSWORD from
# RABBITMQ_PASSWORD_FILE's contents whenever that _FILE variable is
# present — see the "*_FILE variables" precedence rule documented at the
# very top of that script. kickr can only inject plain environment
# variables into the child process (by design — see
# ~/git/kickr/design/draft.md's "Config: Environment variables only"), so
# with usePasswordFiles left at its default, kickr's signet-sourced
# RABBITMQ_PASSWORD injection is silently clobbered back to whatever the
# Helm-rendered Secret file contains, every single time — on the very
# first boot (masked by baking the real password into auth.password
# above, so the *file* itself is already correct) and, critically, on
# every later credential *rotation* too (not masked: kickr's runtime
# override is the only mechanism a rotation has, and this setting is what
# lets it actually work rather than being silently overwritten straight
# back). Setting this false makes the chart source RABBITMQ_PASSWORD from
# a plain secretKeyRef-backed env var instead, with no competing _FILE
# variable for rabbitmq-env.sh to prefer.
usePasswordFiles: false

extraEnvVars:
  - name: KICKR_SIGNET_ADDR
    value: "signet.signet.svc.cluster.local:8443"
  - name: KICKR_SIGNET_SPIFFE_ID
    value: "spiffe://%[1]s/ns/signet/sa/signet"

extraVolumes:
  - name: kickr-config
    configMap:
      name: %[4]s
  - name: spiffe-workload-api
    csi:
      driver: csi.spiffe.io
      readOnly: true

extraVolumeMounts:
  - name: kickr-config
    mountPath: /etc/kickr
    readOnly: true
  - name: spiffe-workload-api
    mountPath: /run/spire/sockets
    readOnly: true

persistence:
  enabled: false

metrics:
  enabled: false
`
)

type RabbitMQAddon struct{}

var _ Addon = (*RabbitMQAddon)(nil)

func init() { Register(&RabbitMQAddon{}) }

func (*RabbitMQAddon) Name() string       { return "rabbitmq" }
func (*RabbitMQAddon) Requires() []string { return nil }

func (*RabbitMQAddon) Install(ctx context.Context, h ClusterHandle) error {
	if err := ensureNamespace(ctx, h, rabbitmqNamespace); err != nil {
		return fmt.Errorf("rabbitmq: ensure namespace: %w", err)
	}

	if err := ensureKickrConfigMap(ctx, h); err != nil {
		return fmt.Errorf("rabbitmq: kickr ConfigMap: %w", err)
	}

	// Seed RabbitMQ's own bootstrap credential into signet *before*
	// installing the chart. The returned values are used two ways below:
	// baked directly into the Helm install's auth.username/password (see
	// rabbitmqValues' own doc comment for why first boot needs this, not
	// just kickr's runtime override) and pushed to signet so kickr's
	// runtime fetch has the same value available for the rotation path.
	// This is kluster's stand-in for the "human operator provisions this
	// once" step ADR 0011 describes for every other authstar service —
	// see SeedSignetSecret's own doc comment in signet_admin.go for the
	// full mechanism.
	username, password, err := seedRabbitMQBootstrapCredential(ctx, h)
	if err != nil {
		return fmt.Errorf("rabbitmq: seed bootstrap credential into signet: %w", err)
	}

	// The same admin credential doubles as the Messaging Topology
	// Operator's own means of reaching RabbitMQ's management API (see
	// ensureTopologyOperatorConnectionSecret) — kluster generated it, so
	// kluster is also the simplest place to hand it to the operator,
	// rather than having the operator or the provisioning Job re-derive or
	// re-fetch it from signet.
	if err := ensureTopologyOperatorConnectionSecret(ctx, h, username, password); err != nil {
		return fmt.Errorf("rabbitmq: topology operator connection secret: %w", err)
	}

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

	values := fmt.Sprintf(rabbitmqValues,
		h.Config.TrustDomainOrDefault(),
		rabbitmqKickrImageRepo,
		rabbitmqKickrImageTag,
		rabbitmqKickrConfigMap,
		username,
		password,
	)

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     rabbitmqRelease,
		ChartName:       rabbitmqChart,
		Namespace:       rabbitmqNamespace,
		Version:         versions.For("rabbitmq"),
		CreateNamespace: true,
		ValuesYaml:      values,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("rabbitmq: helm install: %w", err)
	}

	return nil
}

// ensureKickrConfigMap creates (or updates, in case namespace/service ever
// change) the ConfigMap kickr's KICKR_CONFIG_PATH points at.
func ensureKickrConfigMap(ctx context.Context, h ClusterHandle) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rabbitmqKickrConfigMap,
			Namespace: rabbitmqNamespace,
		},
		Data: map[string]string{
			".kickr.yaml": fmt.Sprintf("namespace: %s\nservice: %s\n", rabbitmqSignetNamespace, rabbitmqSignetService),
		},
	}
	_, err := h.K8sClient.CoreV1().ConfigMaps(rabbitmqNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = h.K8sClient.CoreV1().ConfigMaps(rabbitmqNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

// seedRabbitMQBootstrapCredential generates a fresh random username/password
// and pushes both into signet as RabbitMQ's own bundle secrets, returning
// the plaintext values so the caller can also hand them to the Messaging
// Topology Operator (see ensureTopologyOperatorConnectionSecret).
// Regenerating on every `kluster up` (rather than reusing a persisted
// value) matches every other addon's disposable-cluster posture — each
// cluster is a fresh install, not a long-lived one being reconfigured in
// place.
func seedRabbitMQBootstrapCredential(ctx context.Context, h ClusterHandle) (username, password string, err error) {
	usernameSuffix, err := randomHexKey(8)
	if err != nil {
		return "", "", fmt.Errorf("generate username: %w", err)
	}
	username = "rmq-" + usernameSuffix
	password, err = randomHexKey(20)
	if err != nil {
		return "", "", fmt.Errorf("generate password: %w", err)
	}

	if err := SeedSignetSecret(ctx, h, rabbitmqSignetNamespace, rabbitmqSignetService, rabbitmqUsernameSecretName, username); err != nil {
		return "", "", fmt.Errorf("seed username: %w", err)
	}
	if err := SeedSignetSecret(ctx, h, rabbitmqSignetNamespace, rabbitmqSignetService, rabbitmqPasswordSecretName, password); err != nil {
		return "", "", fmt.Errorf("seed password: %w", err)
	}
	return username, password, nil
}

// ensureTopologyOperatorConnectionSecret creates (or updates) the Secret
// every rabbitmqClusterReference.connectionSecret in this cluster points
// at: the Messaging Topology Operator's own means of reaching RabbitMQ's
// management API to actually create/update users, vhosts, and permissions.
// The v1.20.0 release of the operator requires this label on every
// user-provided Secret it reads (connectionSecret, importCredentialsSecret,
// ...), enforced by an admission webhook — see that release's notes.
func ensureTopologyOperatorConnectionSecret(ctx context.Context, h ClusterHandle, username, password string) error {
	uri := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", rabbitmqRelease, rabbitmqNamespace, rabbitmqManagementPort)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RabbitMQConnectionSecretName,
			Namespace: rabbitmqNamespace,
			Labels:    map[string]string{"rabbitmq.com/topology-operator": "true"},
		},
		StringData: map[string]string{
			"uri":      uri,
			"username": username,
			"password": password,
		},
	}
	_, err := h.K8sClient.CoreV1().Secrets(rabbitmqNamespace).Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = h.K8sClient.CoreV1().Secrets(rabbitmqNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	}
	return err
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
