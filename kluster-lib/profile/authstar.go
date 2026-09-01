package profile

import (
	"context"
	"fmt"
	"time"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

type AuthStarProfile struct{}

var _ Profile = (*AuthStarProfile)(nil)

func init() { Register(&AuthStarProfile{}) }

func (*AuthStarProfile) Name() string { return "authstar" }

// RequiresProfiles depends on "signet" for secrets/config, which transitively
// depends on "spire" for workload identity.
func (*AuthStarProfile) RequiresProfiles() []string { return []string{"signet"} }
func (*AuthStarProfile) Addons() []string {
	return []string{"rabbitmq", "rabbitmq-topology-operator", "dex", "postgres", "clickhouse"}
}

// Configure runs after every addon above has finished installing (Install +
// Ready). Dex/Postgres/ClickHouse are still pre-seeded entirely via their
// Helm values (static dev credentials, a static OIDC client) and need
// nothing here. RabbitMQ is different, per ADR 0006/0008/0010/0012: this is
// where the Messaging Topology Operator's CRD instances (a Vhost, and the
// RBAC + one-shot Job that provisions tower's/keep's own least-privilege
// Users/Permissions) get applied — RabbitMQ's own bootstrap credential and
// the connectionSecret the operator uses to reach it are already handled
// earlier, inside the "rabbitmq" addon's own Install() (see
// addon/rabbitmq.go), since kluster generates that credential and there's
// no reason to thread it back out to this layer.
//
// configureAuthStarServices (authstar_services.go) runs after RabbitMQ
// provisioning specifically because it reads back the amqp:// URL that
// step's own RabbitMQ credential produces — see readRabbitMQAMQPURL.
func (*AuthStarProfile) Configure(ctx context.Context, h addon.ClusterHandle, cfg provider.ClusterConfig) error {
	if err := configureRabbitMQProvisioning(ctx, h, cfg); err != nil {
		return fmt.Errorf("authstar: rabbitmq provisioning: %w", err)
	}
	if err := configureAuthStarServices(ctx, h, cfg); err != nil {
		return fmt.Errorf("authstar: services: %w", err)
	}
	return nil
}

const (
	rabbitmqProvisionerServiceAccount = "rabbitmq-provisioner"
	rabbitmqProvisionerOwnService     = "rabbitmq-provisioner"
	rabbitmqProvisionerAdminSecret    = "signet-admin-token"

	// rabbitmqProvisionerAdminTokenTTLSeconds: see
	// addon.SeedSignetAdminTokenSecret's own doc comment on why a bounded
	// TTL is an acceptable dev-cluster stand-in for ADR 0012's
	// "provisioned once by a human operator" assumption. 24h comfortably
	// outlives a single `kluster up` session, including at least one
	// same-day firing of the daily rotate CronJob applied below.
	rabbitmqProvisionerAdminTokenTTLSeconds = 24 * 60 * 60

	// rabbitmqProvisionerImageRepo/Tag: see images/rabbitmq-provisioner.
	// Not yet published to Docker Hub as of this writing — see
	// rabbitmqKickrImageRepo's own comment in addon/rabbitmq.go for the
	// same caveat and the `k3d image import` workaround for a fully
	// offline dev loop.
	rabbitmqProvisionerImageRepo = "bytepunx/rabbitmq-provisioner"
	rabbitmqProvisionerImageTag  = "0.1.0"

	// rabbitmqTargetServices is the authoritative list of services needing
	// their own least-privilege RabbitMQ credential, per ADR 0008/0012
	// ("today: tower, keep") — not the broader list earlier, unconfirmed
	// speculation suggested; both ADRs explicitly name only these two.
	rabbitmqTargetServices = "tower,keep"

	// rabbitmqRotateSchedule/TimeZone match ADR 0012's CronJob example
	// exactly ("0 3 * * *", spec.timeZone rather than a hardcoded UTC
	// offset).
	rabbitmqRotateSchedule = "0 3 * * *"
	rabbitmqRotateTimeZone = "America/New_York"

	rabbitmqProvisionerJobName     = "rabbitmq-provisioner-bootstrap"
	rabbitmqProvisionerCronJobName = "rabbitmq-provisioner-rotate"
)

// configureRabbitMQProvisioning wires ADR 0012's provisioning Job: RBAC for
// its ServiceAccount (namespace-scoped Roles, not ClusterRoles, split
// across the "rabbitmq" namespace it manages Secrets/Users/Permissions in
// and the "signet" namespace it needs pods/portforward against — see
// rabbitmq-provisioner's own src/lib.js for why the port-forward is
// needed at all), its own elevated signet admin token (seeded the same way
// RabbitMQ's own bootstrap credential is), a Vhost CRD instance, and the
// Job (bootstrap, run now and waited on) + CronJob (rotate, applied but not
// exercised by `kluster up` itself — see this function's own doc comment
// at the bottom on what's deferred).
func configureRabbitMQProvisioning(ctx context.Context, h addon.ClusterHandle, cfg provider.ClusterConfig) error {
	trustDomain := cfg.TrustDomainOrDefault()

	if err := ensureRabbitMQProvisionerServiceAccount(ctx, h); err != nil {
		return fmt.Errorf("service account: %w", err)
	}
	if err := ensureRabbitMQProvisionerRBAC(ctx, h); err != nil {
		return fmt.Errorf("rbac: %w", err)
	}

	if err := addon.SeedSignetAdminTokenSecret(
		ctx, h, addon.RabbitMQNamespace, rabbitmqProvisionerOwnService,
		rabbitmqProvisionerAdminSecret, rabbitmqProvisionerAdminTokenTTLSeconds,
	); err != nil {
		return fmt.Errorf("seed provisioner admin token: %w", err)
	}

	if err := addon.ApplyManifest(ctx, h, fmt.Sprintf(`
apiVersion: rabbitmq.com/v1beta1
kind: Vhost
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  name: %[1]s
  rabbitmqClusterReference:
    connectionSecret:
      name: %[3]s
`, addon.RabbitMQVhost, addon.RabbitMQNamespace, addon.RabbitMQConnectionSecretName)); err != nil {
		return fmt.Errorf("apply Vhost: %w", err)
	}

	jobEnv := rabbitmqProvisionerEnv(trustDomain, "bootstrap")
	if err := addon.ApplyManifest(ctx, h, rabbitmqProvisionerJobManifest(jobEnv)); err != nil {
		return fmt.Errorf("apply bootstrap Job: %w", err)
	}
	if err := waitForJobComplete(ctx, h, addon.RabbitMQNamespace, rabbitmqProvisionerJobName); err != nil {
		return fmt.Errorf("wait for bootstrap Job: %w", err)
	}

	// The CronJob is applied so the rotation path ADR 0012 designs exists
	// in every cluster, matching production shape — but its "0 3 * * *"
	// daily firing is intentionally not triggered or waited on here.
	// Exercising MODE=rotate live is deferred: see this project's overall
	// report for why (bounded verification time; the bootstrap path
	// already exercises every piece rotate reuses — signet push, K8s
	// object upserts — the only genuinely untested part of rotate is its
	// ordering-sensitive cutover sequence, which needs a already-bootstrapped
	// cluster to observe meaningfully anyway).
	cronEnv := rabbitmqProvisionerEnv(trustDomain, "rotate")
	if err := addon.ApplyManifest(ctx, h, rabbitmqProvisionerCronJobManifest(cronEnv)); err != nil {
		return fmt.Errorf("apply rotate CronJob: %w", err)
	}

	return nil
}

func ensureRabbitMQProvisionerServiceAccount(ctx context.Context, h addon.ClusterHandle) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rabbitmqProvisionerServiceAccount,
			Namespace: addon.RabbitMQNamespace,
		},
	}
	_, err := h.K8sClient.CoreV1().ServiceAccounts(addon.RabbitMQNamespace).Create(ctx, sa, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// ensureRabbitMQProvisionerRBAC grants exactly what ADR 0012 specifies: "a
// dedicated ServiceAccount and a namespace-scoped Role (not ClusterRole)...
// within the namespace RabbitMQ runs in".
//
// An earlier version also granted a second namespace-scoped Role, in the
// "signet" namespace, for pods/portforward against signet's own pod —
// needed when this Job reached signet's admin API via a Kubernetes
// port-forward tunnel. That path is gone (see addon/signet_admin.go's
// package doc comment and addon/signet.go's signetAdminPort doc comment):
// the admin port is now reachable directly over signet's own in-cluster
// Service (bytepunx/signet#19's admin.clusterAccess flag), which needs no
// Kubernetes RBAC at all (signetd's own bearer-token check is the access
// control). One less moving part, not just a different one.
func ensureRabbitMQProvisionerRBAC(ctx context.Context, h addon.ClusterHandle) error {
	rabbitmqRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: rabbitmqProvisionerServiceAccount, Namespace: addon.RabbitMQNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create", "update"}},
			{APIGroups: []string{"rabbitmq.com"}, Resources: []string{"users", "permissions"}, Verbs: []string{"get", "list", "create", "update"}},
		},
	}
	if err := applyRole(ctx, h, rabbitmqRole); err != nil {
		return fmt.Errorf("rabbitmq namespace role: %w", err)
	}

	rabbitmqBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: rabbitmqProvisionerServiceAccount, Namespace: addon.RabbitMQNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: rabbitmqProvisionerServiceAccount},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: rabbitmqProvisionerServiceAccount, Namespace: addon.RabbitMQNamespace,
		}},
	}
	return applyRoleBinding(ctx, h, rabbitmqBinding)
}

func applyRole(ctx context.Context, h addon.ClusterHandle, role *rbacv1.Role) error {
	_, err := h.K8sClient.RbacV1().Roles(role.Namespace).Create(ctx, role, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func applyRoleBinding(ctx context.Context, h addon.ClusterHandle, rb *rbacv1.RoleBinding) error {
	_, err := h.K8sClient.RbacV1().RoleBindings(rb.Namespace).Create(ctx, rb, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func waitForJobComplete(ctx context.Context, h addon.ClusterHandle, namespace, name string) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			job, err := h.K8sClient.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			for _, c := range job.Status.Conditions {
				if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
					return false, fmt.Errorf("job %s/%s failed: %s", namespace, name, c.Message)
				}
				if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		},
	)
}

type rabbitmqProvisionerEnvVars struct {
	TrustDomain string
	Mode        string
}

func rabbitmqProvisionerEnv(trustDomain, mode string) rabbitmqProvisionerEnvVars {
	return rabbitmqProvisionerEnvVars{TrustDomain: trustDomain, Mode: mode}
}

// rabbitmqProvisionerPodSpec is shared between the bootstrap Job and the
// rotate CronJob: same image, same ServiceAccount, same env shape (only
// MODE differs), same CSI SPIFFE volume kickr's own RabbitMQ pod uses.
//
// restartPolicy: OnFailure (not the more common Job default of Never) is
// deliberate, not a copy-paste default: SPIRE's controller-manager
// registers a workload's SPIFFE identity per *Pod* (reacting to pod-create
// events), and that registration needs a few seconds to propagate to the
// node-local SPIRE agent this pod dials over the CSI socket. A brand-new
// ServiceAccount's very first pod can lose that race — observed for real,
// consistently, as "no identity issued" from the workload API. With
// restartPolicy: Never, a Job's retries (up to backoffLimit) each get an
// entirely new Pod object from the Job controller, so every retry hits the
// identical fresh-registration race and none of them are any likelier to
// win it than the first. With OnFailure, a failed container is restarted
// in place by the kubelet, inside the *same* Pod object (same UID, same
// already-registered SPIFFE identity) with Kubernetes' own crash-loop
// backoff — exactly the mechanism that already makes kickr's RabbitMQ pod
// self-heal from the same race in this same cluster (a StatefulSet's
// default restartPolicy is Always, which restarts in-place the same way).
func rabbitmqProvisionerPodSpec(env rabbitmqProvisionerEnvVars) string {
	return fmt.Sprintf(`serviceAccountName: %[1]s
restartPolicy: OnFailure
containers:
  - name: rabbitmq-provisioner
    image: docker.io/%[2]s:%[3]s
    env:
      - name: MODE
        value: %[4]s
      - name: OWN_NAMESPACE
        value: %[5]s
      - name: OWN_SERVICE
        value: %[6]s
      - name: TRUST_DOMAIN
        value: %[7]s
      - name: SIGNET_WORKLOAD_ADDR
        value: "signet.signet.svc.cluster.local:8443"
      - name: SIGNET_ADMIN_ADDR
        value: "signet.signet.svc.cluster.local:8444"
      - name: RABBITMQ_NAMESPACE
        value: %[5]s
      - name: RABBITMQ_CONNECTION_SECRET
        value: %[8]s
      - name: RABBITMQ_VHOST
        value: %[9]s
      - name: TARGET_SERVICES
        value: %[10]s
      - name: SPIFFE_SOCKET
        value: /run/spire/sockets/spire-agent.sock
    volumeMounts:
      - name: spiffe-workload-api
        mountPath: /run/spire/sockets
        readOnly: true
volumes:
  - name: spiffe-workload-api
    csi:
      driver: csi.spiffe.io
      readOnly: true
`,
		rabbitmqProvisionerServiceAccount,
		rabbitmqProvisionerImageRepo, rabbitmqProvisionerImageTag,
		env.Mode,
		addon.RabbitMQNamespace, rabbitmqProvisionerOwnService,
		env.TrustDomain,
		addon.RabbitMQConnectionSecretName, addon.RabbitMQVhost,
		rabbitmqTargetServices,
	)
}

func rabbitmqProvisionerJobManifest(env rabbitmqProvisionerEnvVars) string {
	return fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  backoffLimit: 2
  template:
    metadata:
      labels:
        app: rabbitmq-provisioner
    spec:
%[3]s
`, rabbitmqProvisionerJobName, addon.RabbitMQNamespace, indent(rabbitmqProvisionerPodSpec(env), 6))
}

func rabbitmqProvisionerCronJobManifest(env rabbitmqProvisionerEnvVars) string {
	return fmt.Sprintf(`
apiVersion: batch/v1
kind: CronJob
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  schedule: %[3]q
  timeZone: %[4]q
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        metadata:
          labels:
            app: rabbitmq-provisioner
        spec:
%[5]s
`, rabbitmqProvisionerCronJobName, addon.RabbitMQNamespace, rabbitmqRotateSchedule, rabbitmqRotateTimeZone,
		indent(rabbitmqProvisionerPodSpec(env), 10))
}

// indent prefixes every non-empty line of s with n spaces, for splicing a
// shared YAML fragment (rabbitmqProvisionerPodSpec, itself indented for the
// Job's shallower nesting) into the CronJob's deeper jobTemplate.spec.template.spec.
func indent(s string, n int) string {
	prefix := ""
	for i := 0; i < n; i++ {
		prefix += " "
	}
	out := ""
	for _, line := range splitLines(s) {
		if line == "" {
			out += "\n"
			continue
		}
		out += prefix + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
