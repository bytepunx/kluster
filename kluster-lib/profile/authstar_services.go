package profile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	sigsyaml "sigs.k8s.io/yaml"
)

// This file deploys AuthStar's own 8 microservices (tower, keep, herald,
// portcullis, web, chronicle, steward, lookout) — the RabbitMQ/Dex/Postgres/
// ClickHouse *dependencies* those services need are handled elsewhere in
// this package (authstar.go) and in kluster-lib/addon. Everything here
// ports bytepunx/authstar-integration's own proven, golden-path-tested
// manifests and provisioning sequence into Go, so `kluster up --profile
// authstar` produces a fully working AuthStar stack in one command.
//
// All 8 images are public on Docker Hub (bytepunx/authstar-<name>:latest —
// no other tag exists yet upstream, so there's nothing to pin), so unlike
// authstar-integration's own deploy.sh, no imagePullSecret is needed here.
const (
	authStarNamespace = "authstar"

	// authStarDBInitServiceAccount is deliberately its own identity, not
	// one of the 8 services below — the DB-init Job never talks to
	// signet/SPIRE, so it shouldn't be handed a SPIFFE identity it has no
	// use for.
	authStarDBInitServiceAccount = "authstar-db-init"
	authStarDBInitJobName        = "authstar-db-init"

	authStarSignetAddr      = "signet.signet.svc.cluster.local:8443"
	authStarSignetAdminAddr = "signet.signet.svc.cluster.local:8444"
	authStarDexDiscoveryURL = "http://dex.dex.svc.cluster.local:5556/.well-known/openid-configuration"

	// authStarDexClientSecret must equal dex.go's own static OIDC client
	// secret exactly — tower's default-provider-client-secret and
	// portcullis's tenant-acme-provider-dex-client-secret both authenticate
	// against this same Dex client.
	authStarDexClientSecret = "authstar-dev-secret"

	// authStarBaseDomain is AuthStar's own dev app domain (tenant cookie
	// domains, per-tenant JWKS hosts, etc.) — unrelated to the cluster's
	// SPIFFE trust domain, and not user-configurable today; matches
	// authstar-integration's own committed dev config exactly.
	authStarBaseDomain = "authstar.app"
)

// authStarServiceNames lists every AuthStar service deployed here, used to
// provision one ServiceAccount per service (required for the SPIFFE ID
// template in profile/spire.go's ClusterSPIFFEID to resolve correctly).
var authStarServiceNames = []string{
	"tower", "keep", "herald", "portcullis", "web", "chronicle", "steward", "lookout",
}

// configureAuthStarServices deploys AuthStar itself. Called from
// AuthStarProfile.Configure after configureRabbitMQProvisioning, whose
// RabbitMQ credential this function reads back (see readRabbitMQAMQPURL).
func configureAuthStarServices(ctx context.Context, h addon.ClusterHandle, cfg provider.ClusterConfig) error {
	if err := ensureAuthStarNamespace(ctx, h); err != nil {
		return fmt.Errorf("namespace: %w", err)
	}
	if err := ensureAuthStarServiceAccounts(ctx, h); err != nil {
		return fmt.Errorf("service accounts: %w", err)
	}
	if err := runDBInitJob(ctx, h); err != nil {
		return fmt.Errorf("db init: %w", err)
	}

	amqpURL, err := readRabbitMQAMQPURL(ctx, h)
	if err != nil {
		return fmt.Errorf("read rabbitmq credential: %w", err)
	}

	trustDomain := cfg.TrustDomainOrDefault()

	towerOperatorToken, heraldOperatorToken, err := seedAuthStarBundles(ctx, h, amqpURL)
	if err != nil {
		return err
	}

	if err := ensureAuthStarCrossServicePolicies(ctx, h, trustDomain); err != nil {
		return fmt.Errorf("cross-service policies: %w", err)
	}

	if err := deployAuthStarServices(ctx, h, trustDomain, amqpURL); err != nil {
		return fmt.Errorf("deploy services: %w", err)
	}

	if err := writeAuthStarTokens(cfg.Name, map[string]string{
		"towerOperatorToken":  towerOperatorToken,
		"heraldOperatorToken": heraldOperatorToken,
	}); err != nil {
		return fmt.Errorf("write operator tokens: %w", err)
	}
	return nil
}

func ensureAuthStarNamespace(ctx context.Context, h addon.ClusterHandle) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: authStarNamespace}}
	_, err := h.K8sClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func ensureAuthStarServiceAccounts(ctx context.Context, h addon.ClusterHandle) error {
	names := append([]string{authStarDBInitServiceAccount}, authStarServiceNames...)
	for _, name := range names {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: authStarNamespace}}
		_, err := h.K8sClient.CoreV1().ServiceAccounts(authStarNamespace).Create(ctx, sa, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("service account %s: %w", name, err)
		}
	}
	return nil
}

// readRabbitMQAMQPURL reads back the broker-bootstrap credential
// addon/rabbitmq.go already generated and stored in
// addon.RabbitMQConnectionSecretName, and builds the amqp:// URL
// tower/keep/herald (via signet) and chronicle/lookout (as a plain env var)
// all connect with.
//
// This is a deliberate simplification matching authstar-integration's own
// current, real behavior: every one of those 5 services shares this single
// credential rather than getting a distinct least-privilege user from the
// rabbitmq-provisioner Job (see configureRabbitMQProvisioning) — that Job's
// provisioned credentials land at the wrong signet path
// (secrets/<service>/<service>/... instead of secrets/authstar/<service>/...)
// due to a confirmed bug in the external images/rabbitmq-provisioner image,
// out of scope to fix here. This is still a net improvement over
// authstar-integration today, which requires a manual `kubectl get secret |
// base64 -d` copy-paste per fresh cluster for this same value.
func readRabbitMQAMQPURL(ctx context.Context, h addon.ClusterHandle) (string, error) {
	secret, err := h.K8sClient.CoreV1().Secrets(addon.RabbitMQNamespace).Get(ctx, addon.RabbitMQConnectionSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s/%s: %w", addon.RabbitMQNamespace, addon.RabbitMQConnectionSecretName, err)
	}
	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	return fmt.Sprintf("amqp://%s:%s@rabbitmq.%s.svc.cluster.local:5672/%s",
		username, password, addon.RabbitMQNamespace, addon.RabbitMQVhost), nil
}

func authStarDBConnectionString(db string) string {
	return fmt.Sprintf("postgres://postgres:postgres@postgresql.postgres.svc.cluster.local:5432/%s?sslmode=disable", db)
}

// seedAuthStarBundles pushes every secret + config document the 4
// signet-wired services (tower, keep, herald, portcullis) need into signet
// ahead of their own pods starting, and returns the tower/herald operator
// tokens it generated so the caller can surface them to the user (see
// writeAuthStarTokens — unlike every other value seeded here, an operator
// token has no other recoverable copy: it goes straight into signet's SOPS
// store with no Kubernetes Secret fallback).
//
// web/chronicle/steward/lookout are deliberately NOT seeded here even
// though authstar-integration's own repo has secrets/config files
// committed for chronicle and lookout too — none of those 4 services'
// current code actually fetches a signet bundle (confirmed from that
// repo's own config file comments: each is "provisioned ahead of that
// wiring landing"), so seeding would be inert. They get plain hardcoded
// env vars instead, in deployAuthStarServices, matching what's actually
// exercised today.
func seedAuthStarBundles(ctx context.Context, h addon.ClusterHandle, amqpURL string) (towerOperatorToken, heraldOperatorToken string, err error) {
	contactHashPepper, err := randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate tower contactHashPepper: %w", err)
	}
	towerOperatorToken, err = randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate tower operatorToken: %w", err)
	}
	tenantAcmeContactHashPepper, err := randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate tower tenant-acme-contact-hash-pepper: %w", err)
	}
	towerConfig, err := yaml.Marshal(map[string]any{
		"provisioning": map[string]any{
			"selfServiceEnabled": false,
			"baseDomain":         authStarBaseDomain,
			"defaultProvider": map[string]any{
				"name":         "dex",
				"clientId":     "authstar",
				"discoveryUrl": authStarDexDiscoveryURL,
				// Must be an array -- tower's resolveProvisioningConfig
				// gates on Array.isArray(scope), rejecting a
				// space-separated string as "missing" even though
				// authstar-integration's own committed tower.yaml still
				// uses the (now-stale) string form.
				"scope": []string{"openid", "email"},
			},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal tower config: %w", err)
	}
	if err := addon.SeedSignetBundle(ctx, h, authStarNamespace, "tower", map[string]string{
		"dbConnectionString":              authStarDBConnectionString("tower"),
		"rabbitmqUrl":                     amqpURL,
		"contactHashPepper":               contactHashPepper,
		"operatorToken":                   towerOperatorToken,
		"default-provider-client-secret":  authStarDexClientSecret,
		"tenant-acme-contact-hash-pepper": tenantAcmeContactHashPepper,
	}, towerConfig); err != nil {
		return "", "", fmt.Errorf("seed tower bundle: %w", err)
	}

	// keep and herald both fail to start without towerBaseUrl in their own
	// signet config document — confirmed live, not from authstar-integration's
	// committed files (whose config/authstar/herald.yaml is comments-only;
	// its own config/authstar/keep.yaml comment already documents hitting
	// this exact "signet bundle missing towerBaseUrl" crash once before,
	// for keep specifically). Both services' currently-published images
	// need it, so both get it here regardless of what's committed upstream.
	towerBaseURLConfig, err := yaml.Marshal(map[string]any{
		"towerBaseUrl": "http://tower.authstar.svc.cluster.local:8080",
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal towerBaseUrl config: %w", err)
	}
	// credentialSalt: required (requireBundleSecret, no fallback) as of
	// keep's current image -- replaces an earlier hardcoded dev-default
	// salt in keep's own hash.ts (the "left as a follow-up" per-credential
	// random salting ADR 0079 flagged has now landed).
	credentialSalt, err := randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate keep credentialSalt: %w", err)
	}
	if err := addon.SeedSignetBundle(ctx, h, authStarNamespace, "keep", map[string]string{
		"dbConnectionString":         authStarDBConnectionString("keep"),
		"rabbitmqUrl":                amqpURL,
		"credentialSalt":             credentialSalt,
		"stripe-secret-key-acme":     "sk_test_51PlaceholderAcmeDevKeyDoNotUseInProd00000000",
		"stripe-webhook-secret-acme": "whsec_PlaceholderAcmeDevWebhookSigningSecret0000",
	}, towerBaseURLConfig); err != nil {
		return "", "", fmt.Errorf("seed keep bundle: %w", err)
	}

	heraldOperatorToken, err = randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate herald operatorToken: %w", err)
	}
	if err := addon.SeedSignetBundle(ctx, h, authStarNamespace, "herald", map[string]string{
		"dbConnectionString": authStarDBConnectionString("herald"),
		"rabbitmqUrl":        amqpURL,
		"operatorToken":      heraldOperatorToken,
	}, towerBaseURLConfig); err != nil {
		return "", "", fmt.Errorf("seed herald bundle: %w", err)
	}

	sessionKey, err := randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate portcullis session key: %w", err)
	}
	internalKey, err := randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate portcullis internal key: %w", err)
	}
	identityHashKey, err := randomHexKey(32)
	if err != nil {
		return "", "", fmt.Errorf("generate portcullis identity hash key: %w", err)
	}
	portcullisConfig, err := yaml.Marshal(portcullisConfigDoc())
	if err != nil {
		return "", "", fmt.Errorf("marshal portcullis config: %w", err)
	}
	if err := addon.SeedSignetBundle(ctx, h, authStarNamespace, "portcullis", map[string]string{
		"tenant-acme-session-key":                sessionKey,
		"tenant-acme-internal-key":               internalKey,
		"tenant-acme-identity-hash-key-1":        identityHashKey,
		"tenant-acme-provider-dex-client-secret": authStarDexClientSecret,
	}, portcullisConfig); err != nil {
		return "", "", fmt.Errorf("seed portcullis bundle: %w", err)
	}

	return towerOperatorToken, heraldOperatorToken, nil
}

// authStarPolicyGrant is one ADR 0103 cross-service write grant: sourceService
// (identified by its own SPIFFE identity) is authorized to write into
// targetService's bundle.
type authStarPolicyGrant struct {
	sourceService string
	targetService string
}

// authStarCrossServicePolicyGrants are the four grants authstar-integration's
// own provision.sh provisions (see its "ADR 0103 cross-service policy
// grants" step): herald pushes its payload-encryption public key into
// tower's, keep's, and portcullis's bundles; tower authors portcullis's
// bundle on its behalf. signet's convention-first auto-access rule only
// covers a caller writing to its OWN namespace/service, so each of these
// needs an explicit grant. keep needs none — it only ever writes its own
// bundle.
var authStarCrossServicePolicyGrants = []authStarPolicyGrant{
	{sourceService: "herald", targetService: "tower"},
	{sourceService: "herald", targetService: "keep"},
	{sourceService: "herald", targetService: "portcullis"},
	{sourceService: "tower", targetService: "portcullis"},
}

// ensureAuthStarCrossServicePolicies grants each authStarCrossServicePolicyGrants
// entry "put" access on the target service's bundle, ahead of any pod
// starting — must run before deployAuthStarServices, since herald/tower may
// attempt these writes as soon as their own process starts.
//
// This — not a bearer signetAdminToken — is how tower/keep/herald now
// authenticate their own signet writes: their own SPIFFE workload identity,
// presented on the same mTLS connection used to fetch their own bundle. See
// ADR 0103 ("spiffe-scoped-signet-writes-retire-admin-tokens") in
// authstar-design; signetAdminToken is retired entirely as of the images
// this deploys — kluster no longer seeds one (compare with earlier kluster
// versions, which minted a signetAdminToken via
// addon.SeedSignetAdminTokenSecret for each of tower/keep/herald; that
// secret is simply unused by the currently-published images now).
func ensureAuthStarCrossServicePolicies(ctx context.Context, h addon.ClusterHandle, trustDomain string) error {
	for _, grant := range authStarCrossServicePolicyGrants {
		spiffeID := fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, authStarNamespace, grant.sourceService)
		if err := addon.EnsureSignetPolicy(ctx, h, spiffeID, authStarNamespace, grant.targetService, []string{"put"}); err != nil {
			return fmt.Errorf("grant %s -> %s: %w", grant.sourceService, grant.targetService, err)
		}
	}
	return nil
}

// portcullisConfigDoc mirrors config/authstar/portcullis.yaml exactly,
// matching the Bundle struct in authstar-portcullis's own
// crates/authstar-config/src/bundle.rs (camelCase field names — confirmed
// from the Rust struct definitions, not guessed). A single dev tenant,
// "acme", is provisioned — enough to exercise the full stack end-to-end.
func portcullisConfigDoc() map[string]any {
	return map[string]any{
		"webUpstream": map[string]any{
			"address": "http://web.authstar.svc.cluster.local:3000",
			"timeout": 10,
		},
		// https, not http: tower always runs real SPIFFE mTLS.
		"towerUpstream": map[string]any{
			"address": "https://tower.authstar.svc.cluster.local:8080",
			"timeout": 10,
		},
		"tenants": map[string]any{
			"acme": map[string]any{
				"status": "active",
				"providers": map[string]any{
					"dex": map[string]any{
						"clientId":     "authstar",
						"discoveryUrl": authStarDexDiscoveryURL,
						"scope":        []string{"openid", "email"},
					},
				},
				"cookie": map[string]any{
					"domain":   "acme.authstar.app",
					"path":     "/",
					"maxAge":   604800,
					"httpOnly": true,
					"secure":   false,
				},
				"jwt": map[string]any{
					"issuer":    "https://acme.authstar.app",
					"audience":  "acme",
					"expiresIn": "168h",
				},
				"branding": map[string]any{
					"name":         "Acme Corp",
					"logoUrl":      "https://acme.authstar.app/logo.png",
					"primaryColor": "#1a73e8",
				},
				"identityHashKeyGenerations": []map[string]any{
					{"version": 1, "effectiveFrom": "2026-01-01T00:00:00Z"},
				},
				"applications": map[string]any{
					"web": map[string]any{
						// admin.acme.authstar.app: the per-tenant JWKS
						// convention every JWT-consuming service resolves
						// against — tower's own tenant provisioning never
						// patches this host in itself, so it's provisioned
						// here instead (see the upstream repo's own
						// comment on this exact field).
						"hosts": []string{"acme.authstar.app", "admin.acme.authstar.app"},
						"server": map[string]any{
							"publicUrl": "https://acme.authstar.app",
						},
						"upstream": map[string]any{
							"base":    "http://web.authstar.svc.cluster.local:3000",
							"routes":  map[string]any{},
							"timeout": 30,
						},
						"cors": map[string]any{
							"origin": "https://acme.authstar.app",
						},
						"exempt": []string{"/healthz"},
						"auth": map[string]any{
							"loginUrl": "/login",
						},
						"providers": []string{"dex"},
					},
				},
			},
		},
	}
}

func randomHexKey(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// --- DB init ---

// runDBInitJob creates the per-service Postgres databases (tower, keep,
// herald) and the ClickHouse database (chronicle) inside the already-running
// shared postgres/clickhouse addon instances — neither addon provisions
// more than its own single default database on its own.
func runDBInitJob(ctx context.Context, h addon.ClusterHandle) error {
	manifest, err := dbInitJobManifest()
	if err != nil {
		return err
	}
	if err := addon.ApplyManifest(ctx, h, manifest); err != nil {
		return fmt.Errorf("apply db-init job: %w", err)
	}
	return waitForJobComplete(ctx, h, authStarNamespace, authStarDBInitJobName)
}

// dbInitJobManifest mirrors authstar-integration's own working
// 01-db-init-job.yaml exactly: Postgres has no `CREATE DATABASE IF NOT
// EXISTS`, so the initContainer does its own existence check per database;
// ClickHouse's HTTP interface does support `IF NOT EXISTS` natively.
func dbInitJobManifest() (string, error) {
	const postgresInitScript = `set -e
for db in tower keep herald; do
  exists=$(psql -h postgresql.postgres.svc.cluster.local -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='${db}'")
  if [ "$exists" != "1" ]; then
    psql -h postgresql.postgres.svc.cluster.local -U postgres -d postgres -c "CREATE DATABASE ${db}"
  fi
done
`
	backoffLimit := int32(3)
	job := &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: authStarDBInitJobName, Namespace: authStarNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: authStarDBInitServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{{
						Name:    "postgres-databases",
						Image:   "postgres:16-alpine",
						Command: []string{"sh", "-c", postgresInitScript},
						Env:     []corev1.EnvVar{{Name: "PGPASSWORD", Value: "postgres"}},
					}},
					Containers: []corev1.Container{{
						Name:  "clickhouse-database",
						Image: "curlimages/curl:8.11.1",
						Command: []string{"sh", "-c",
							`curl -sf -u default:clickhouse http://clickhouse.clickhouse.svc.cluster.local:8123/ --data-binary "CREATE DATABASE IF NOT EXISTS chronicle"`,
						},
					}},
				},
			},
		},
	}
	return marshalManifest(job)
}

// --- Deployment/Service/CronJob manifest builders ---

// serviceSpec describes one AuthStar service's Deployment+Service shape.
// Per-service env assembly (signetEnvVars for the 4 signet-wired services,
// or a small hand-built list for the 4 plain ones) lives in
// deployAuthStarServices, not here — the 4 signet-wired and 4 plain
// services have different enough env shapes (different var names,
// portcullis's own bespoke set) that folding that into this struct would
// just move the branching, not remove it.
type serviceSpec struct {
	Name           string
	Image          string
	Port           int32
	ServiceAccount string
	Env            []corev1.EnvVar
	Volumes        []corev1.Volume
	VolumeMounts   []corev1.VolumeMount
}

// signetEnvVars is the env shape shared by tower/keep/herald (the 3
// Node-based signet-wired services) — portcullis (Rust) uses different
// variable names/shapes entirely and is built directly in
// deployAuthStarServices instead.
func signetEnvVars(trustDomain string, admin bool) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "SIGNET_NAMESPACE", Value: authStarNamespace},
		{Name: "SIGNET_ADDR", Value: authStarSignetAddr},
		{Name: "SIGNET_TRUST_DOMAIN", Value: trustDomain},
		{Name: "SPIFFE_ENDPOINT_SOCKET", Value: "unix:///run/spire/sockets/spire-agent.sock"},
	}
	if admin {
		env = append(env, corev1.EnvVar{Name: "SIGNET_ADMIN_ADDR", Value: authStarSignetAdminAddr})
	}
	return env
}

func spiffeCSIVolume() corev1.Volume {
	readOnly := true
	return corev1.Volume{
		Name: "spiffe-workload-api",
		VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{Driver: "csi.spiffe.io", ReadOnly: &readOnly},
		},
	}
}

func spiffeCSIVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "spiffe-workload-api", MountPath: "/run/spire/sockets", ReadOnly: true}
}

func deploymentManifest(spec serviceSpec) (string, error) {
	replicas := int32(1)
	d := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: authStarNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": spec.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": spec.Name}},
				Spec: corev1.PodSpec{
					ServiceAccountName: spec.ServiceAccount,
					Containers: []corev1.Container{{
						Name:           spec.Name,
						Image:          spec.Image,
						Ports:          []corev1.ContainerPort{{ContainerPort: spec.Port}},
						Env:            spec.Env,
						VolumeMounts:   spec.VolumeMounts,
						LivenessProbe:  tcpProbe(spec.Port),
						ReadinessProbe: tcpProbe(spec.Port),
					}},
					Volumes: spec.Volumes,
				},
			},
		},
	}
	return marshalManifest(d)
}

func serviceManifest(spec serviceSpec) (string, error) {
	s := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: authStarNamespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": spec.Name},
			Ports: []corev1.ServicePort{{
				Port:       spec.Port,
				TargetPort: intstr.FromInt32(spec.Port),
			}},
		},
	}
	return marshalManifest(s)
}

// cronJobManifest builds keep-pricing-sync/herald-retention-purge: same
// image/env/CSI-volume shape as that service's own Deployment, with a
// command override and restartPolicy: OnFailure — required (not just
// convention) because SPIRE registers a SPIFFE identity per-Pod-UID; a
// Never-restart Job would get a fresh Pod (and therefore a fresh
// registration race) on every retry, while OnFailure restarts the
// container in place inside the same already-registered Pod. Matches the
// exact reasoning already documented for rabbitmqProvisionerPodSpec in
// authstar.go.
func cronJobManifest(spec serviceSpec, name, schedule, timezone string, command []string) (string, error) {
	backoffLimit := int32(2)
	cj := &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: authStarNamespace},
		Spec: batchv1.CronJobSpec{
			Schedule: schedule,
			TimeZone: &timezone,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit: &backoffLimit,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": spec.Name}},
						Spec: corev1.PodSpec{
							ServiceAccountName: spec.ServiceAccount,
							RestartPolicy:      corev1.RestartPolicyOnFailure,
							Containers: []corev1.Container{{
								Name:         spec.Name,
								Image:        spec.Image,
								Command:      command,
								Env:          spec.Env,
								VolumeMounts: spec.VolumeMounts,
							}},
							Volumes: spec.Volumes,
						},
					},
				},
			},
		},
	}
	return marshalManifest(cj)
}

func tcpProbe(port int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}
}

func marshalManifest(obj any) (string, error) {
	data, err := sigsyaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	return string(data), nil
}

func waitForDeploymentReady(ctx context.Context, h addon.ClusterHandle, namespace, name string) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			d, err := h.K8sClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return d.Status.ReadyReplicas >= 1, nil
		},
	)
}

// --- deployment orchestration ---

func deployAuthStarServices(ctx context.Context, h addon.ClusterHandle, trustDomain, amqpURL string) error {
	specs := []serviceSpec{
		{
			Name: "tower", Image: "docker.io/bytepunx/authstar-tower:latest", Port: 8080, ServiceAccount: "tower",
			Env:          signetEnvVars(trustDomain, true),
			Volumes:      []corev1.Volume{spiffeCSIVolume()},
			VolumeMounts: []corev1.VolumeMount{spiffeCSIVolumeMount()},
		},
		{
			Name: "keep", Image: "docker.io/bytepunx/authstar-keep:latest", Port: 8080, ServiceAccount: "keep",
			// admin=true here: keep's own signet-admin wiring also calls
			// signet's GitOpsService on the admin port (8444), not the
			// workload port (8443) getServiceBundle uses.
			Env:          signetEnvVars(trustDomain, true),
			Volumes:      []corev1.Volume{spiffeCSIVolume()},
			VolumeMounts: []corev1.VolumeMount{spiffeCSIVolumeMount()},
		},
		{
			Name: "herald", Image: "docker.io/bytepunx/authstar-herald:latest", Port: 8080, ServiceAccount: "herald",
			// admin=true here: herald's own /operator/rotate-keys writes
			// payload/unsubscribe key generations back into signet.
			Env:          signetEnvVars(trustDomain, true),
			Volumes:      []corev1.Volume{spiffeCSIVolume()},
			VolumeMounts: []corev1.VolumeMount{spiffeCSIVolumeMount()},
		},
		{
			Name: "portcullis", Image: "docker.io/bytepunx/authstar-portcullis:latest", Port: 3710, ServiceAccount: "portcullis",
			Env: []corev1.EnvVar{
				{Name: "SIGNET_NAMESPACE", Value: authStarNamespace},
				{Name: "SIGNET_SERVICE", Value: "portcullis"},
				{Name: "SIGNET_ADDR", Value: authStarSignetAddr},
				{Name: "SIGNET_TRUST_DOMAIN", Value: trustDomain},
				// Bare path, not a unix:// URI, unlike the Node services'
				// SPIFFE_ENDPOINT_SOCKET — authstar-config/src/env.rs
				// normalizes it itself.
				{Name: "SPIFFE_SOCKET", Value: "/run/spire/sockets/spire-agent.sock"},
			},
			Volumes:      []corev1.Volume{spiffeCSIVolume()},
			VolumeMounts: []corev1.VolumeMount{spiffeCSIVolumeMount()},
		},
		{
			Name: "web", Image: "docker.io/bytepunx/authstar-web:latest", Port: 3000, ServiceAccount: "web",
			Env: []corev1.EnvVar{
				{Name: "TOWER_BASE_URL", Value: "http://tower.authstar.svc.cluster.local:8080"},
				{Name: "PUBLIC_TOWER_BASE_URL", Value: "http://localhost:8080"},
				// INTERNAL_JWT_JWKS_URL deliberately unset: portcullis has
				// no JWKS route yet. web falls back to treating every
				// request as unauthenticated — a known upstream gap, not
				// this deployment's bug to paper over.
			},
		},
		{
			Name: "chronicle", Image: "docker.io/bytepunx/authstar-chronicle:latest", Port: 3000, ServiceAccount: "chronicle",
			Env: []corev1.EnvVar{
				{Name: "CHRONICLE_CLICKHOUSE_URL", Value: "http://clickhouse.clickhouse.svc.cluster.local:8123"},
				{Name: "CHRONICLE_CLICKHOUSE_USERNAME", Value: "default"},
				{Name: "CHRONICLE_CLICKHOUSE_PASSWORD", Value: "clickhouse"},
				{Name: "CHRONICLE_CLICKHOUSE_DATABASE", Value: "chronicle"},
				// Reuses tower's own RabbitMQ credential directly as a
				// plain env var rather than via signet — chronicle has no
				// signet-client wiring yet (see seedAuthStarBundles's own
				// doc comment); matches authstar-integration's current,
				// working behavior, not an oversight.
				{Name: "CHRONICLE_RABBITMQ_URL", Value: amqpURL},
				{Name: "CHRONICLE_INTERNAL_JWT_SECRET", Value: "dev-only-insecure-secret"},
			},
		},
		{
			Name: "steward", Image: "docker.io/bytepunx/authstar-steward:latest", Port: 3000, ServiceAccount: "steward",
			Env: []corev1.EnvVar{
				{Name: "TOWER_URL", Value: "http://tower.authstar.svc.cluster.local:8080"},
				{Name: "HERALD_URL", Value: "http://herald.authstar.svc.cluster.local:8080"},
				// No operator token: per ADR 0045/0048, a human types it
				// into steward's UI each session — never provisioned.
			},
		},
		{
			Name: "lookout", Image: "docker.io/bytepunx/authstar-lookout:latest", Port: 3000, ServiceAccount: "lookout",
			Env: []corev1.EnvVar{
				{Name: "LOOKOUT_RABBITMQ_URL", Value: amqpURL},
			},
		},
	}

	for _, spec := range specs {
		dm, err := deploymentManifest(spec)
		if err != nil {
			return fmt.Errorf("%s: build deployment manifest: %w", spec.Name, err)
		}
		if err := addon.ApplyManifest(ctx, h, dm); err != nil {
			return fmt.Errorf("%s: apply deployment: %w", spec.Name, err)
		}

		sm, err := serviceManifest(spec)
		if err != nil {
			return fmt.Errorf("%s: build service manifest: %w", spec.Name, err)
		}
		if err := addon.ApplyManifest(ctx, h, sm); err != nil {
			return fmt.Errorf("%s: apply service: %w", spec.Name, err)
		}
	}

	// keep-pricing-sync / herald-retention-purge: applied but not waited
	// on, matching the existing rabbitmq-provisioner rotate CronJob's own
	// "applied so the shape exists, not exercised by `kluster up`" precedent
	// in authstar.go (bounded verification time).
	keepCron, err := cronJobManifest(specs[1], "keep-pricing-sync", "0 4 * * *", "America/New_York",
		[]string{"node", "dist/sync-pricing.js"})
	if err != nil {
		return fmt.Errorf("keep-pricing-sync: build manifest: %w", err)
	}
	if err := addon.ApplyManifest(ctx, h, keepCron); err != nil {
		return fmt.Errorf("keep-pricing-sync: apply: %w", err)
	}

	// herald-retention-purge doesn't write to signet (only the Deployment's
	// own /operator/rotate-keys does), so admin=false here unlike the
	// Deployment above.
	heraldPurgeSpec := serviceSpec{
		Name: "herald", Image: specs[2].Image, ServiceAccount: "herald",
		Env:          signetEnvVars(trustDomain, false),
		Volumes:      specs[2].Volumes,
		VolumeMounts: specs[2].VolumeMounts,
	}
	heraldCron, err := cronJobManifest(heraldPurgeSpec, "herald-retention-purge", "0 5 * * *", "America/New_York",
		[]string{"node", "dist/purge-retention.js"})
	if err != nil {
		return fmt.Errorf("herald-retention-purge: build manifest: %w", err)
	}
	if err := addon.ApplyManifest(ctx, h, heraldCron); err != nil {
		return fmt.Errorf("herald-retention-purge: apply: %w", err)
	}

	for _, spec := range specs {
		if err := waitForDeploymentReady(ctx, h, authStarNamespace, spec.Name); err != nil {
			return fmt.Errorf("%s: %w", spec.Name, err)
		}
	}
	return nil
}

// --- operator token surfacing ---

// writeAuthStarTokens writes the tower/herald operator bearer tokens
// generated in seedAuthStarBundles to a local file. This is the only copy
// of these values anywhere outside signet's own SOPS-encrypted store (no
// Kubernetes Secret holds them, unlike RabbitMQ's credential) — without
// this, they'd be unrecoverable, and nobody could exercise the deployed
// stack's operator-only endpoints at all. Path/atomicity mirrors
// versions.go's own ~/.config/kluster/chart-versions.yaml convention.
func writeAuthStarTokens(clusterName string, tokens map[string]string) error {
	path, err := authStarTokensPath(clusterName)
	if err != nil {
		return fmt.Errorf("resolve tokens path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp tokens file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename tokens file: %w", err)
	}
	return nil
}

// AuthStarTokensPath returns the path kluster writes AuthStar's operator
// tokens to for the named cluster, for kluster/cmd/up.go to check after
// `kluster up --profile authstar` succeeds.
func AuthStarTokensPath(clusterName string) (string, error) {
	return authStarTokensPath(clusterName)
}

func authStarTokensPath(clusterName string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "kluster", clusterName+"-authstar-tokens.yaml"), nil
}
