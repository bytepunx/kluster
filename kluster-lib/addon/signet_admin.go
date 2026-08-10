package addon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"time"

	adminv1 "github.com/bytepunx/signet/gen/admin/v1"
	sops "github.com/getsops/sops/v3"
	sopsaes "github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	sopscommon "github.com/getsops/sops/v3/cmd/sops/common"
	sopsconfig "github.com/getsops/sops/v3/config"
	sopsyaml "github.com/getsops/sops/v3/stores/yaml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	authnv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// This file lets kluster act as the "human operator" signet's own docs
// assume for admin-API access (see signet/docs/cli.md's "signet bundle
// push" and deploy/helm/signet/values.yaml's adminServiceAccount comment:
// "kubectl create token signet-admin --duration=1h"). kluster automates
// that exact workflow — mint a short-lived token for signet's own
// "signet-admin" ServiceAccount via the TokenRequest API (the programmatic
// equivalent of `kubectl create token`), grant it the admin RBAC signet's
// chart deliberately leaves for the operator to create (see
// deploy/helm/signet/templates/clusterrole.yaml), port-forward to the
// admin gRPC listener, and push a secret through the same SyncBundle RPC
// the `signet bundle push` CLI command uses. This is what makes RabbitMQ's
// own bootstrap credential (see rabbitmq.go) arrive via signet for real,
// instead of a static Helm value, in a fully unattended `kluster up`.
//
// kluster itself is a native host binary, not something running inside the
// cluster — confirmed the hard way: an earlier version of this code tried
// dialing signet's own in-cluster Service directly by its
// *.svc.cluster.local DNS name, which only resolves from inside the
// cluster's own pod network. From the host, every attempt failed
// identically (not a propagation blip — genuinely unresolvable), no matter
// how many retries. A Kubernetes pods/portforward tunnel (client-go's SPDY
// tooling, the same mechanism `kubectl port-forward` uses) is the
// actually-correct way to cross that boundary from here, and it works
// cleanly: this package's own kluster process authenticates with the
// cluster's own admin-level kubeconfig (not a narrowly-scoped
// ServiceAccount), so it never hits the RBAC wall the *other* consumer of
// signet's admin port does.
//
// That other consumer — the RabbitMQ provisioning Job (images/rabbitmq-provisioner,
// wired in profile/authstar.go) — runs *inside* the cluster as an ordinary
// pod under its own narrowly-scoped rabbitmq-provisioner ServiceAccount, and
// dials that same in-cluster Service's DNS name directly (see that image's
// own src/index.js and signet.go's signetAdminPort doc comment for how
// signet's own admin.clusterAccess chart flag makes that Service reachable
// at all — see bytepunx/signet#19). For that caller, a Kubernetes-level
// port-forward tunnel would need its own pods/portforward RBAC grant, which
// turned out to hit a persistent 403 on the WebSocket upgrade
// @kubernetes/client-node's PortForward class speaks (not SPDY) — a
// completely different problem from the DNS one here, since that pod
// actually can resolve the in-cluster DNS name once it's dialing a real
// Service address instead of tunneling through the Kubernetes API server.
// So: two different callers, in two different network positions, each
// using the mechanism that actually fits where it runs — port-forward here
// (dialSignetAdmin below), direct Service dial in the provisioning Job's
// own src/index.js.
const (
	signetAdminServiceAccount = "signet-admin"

	// signetAdminOperatorClusterRole grants the synthetic RBAC permission
	// signetd's admin endpoint checks via SubjectAccessReview when a caller
	// isn't in its (empty-by-default) SIGNET_ADMIN_SUBJECTS allowlist. This
	// ClusterRole/ClusterRoleBinding pair is exactly the example given in
	// signet's own clusterrole.yaml comment, applied by kluster on behalf
	// of the "operator" (a human, elsewhere) since kluster's dev clusters
	// have none.
	signetAdminOperatorClusterRole = "kluster-signet-admin-operator"

	// signetAdminTokenAudience must match signet.kubeAudiences (chart
	// default: "signet"). signetd refuses to start with an empty audience
	// list specifically to prevent a token bearing ANY audience being
	// accepted, so this must match exactly.
	signetAdminTokenAudience = "signet"

	// signetAdminTokenTTLSeconds is deliberately short: this token is
	// minted, used for one SyncBundle push (or a handful, back-to-back
	// during a single `kluster up`), and discarded. It is never persisted.
	signetAdminTokenTTLSeconds = int64(600)

	// signetBundleSOPSVersion is stamped into the SOPS metadata of every
	// secret file kluster encrypts, matching the sops library version
	// pinned in this module's go.mod (see the getsops/sops/v3 import
	// below) — cosmetic (signet's decrypt path doesn't gate on it) but
	// kept accurate rather than a copy-pasted placeholder.
	signetBundleSOPSVersion = "3.12.1"
)

// ensureSignetAdminRBAC creates the ClusterRole/ClusterRoleBinding pair
// signet's own chart comments describe as the operator's responsibility:
// granting the "signet-admin" ServiceAccount verb "administer" on the
// synthetic resource "adminoperations" in API group "signet.io". Idempotent
// — safe to call on every `kluster up`.
func ensureSignetAdminRBAC(ctx context.Context, h ClusterHandle) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: signetAdminOperatorClusterRole},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"signet.io"},
			Resources: []string{"adminoperations"},
			Verbs:     []string{"administer"},
		}},
	}
	if _, err := h.K8sClient.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ClusterRole %s: %w", signetAdminOperatorClusterRole, err)
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: signetAdminOperatorClusterRole},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     signetAdminOperatorClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      signetAdminServiceAccount,
			Namespace: signetNamespace,
		}},
	}
	if _, err := h.K8sClient.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ClusterRoleBinding %s: %w", signetAdminOperatorClusterRole, err)
	}
	return nil
}

// mintSignetAdminToken is the TokenRequest-API equivalent of
// `kubectl create token signet-admin --duration=10m`.
func mintSignetAdminToken(ctx context.Context, h ClusterHandle) (string, error) {
	return mintSignetAdminTokenWithTTL(ctx, h, signetAdminTokenTTLSeconds)
}

// mintSignetAdminTokenWithTTL is mintSignetAdminToken with a caller-chosen
// TTL. Used for tokens that will be *stored* (as a signet secret value) for
// later use by another workload — e.g. the RabbitMQ provisioning Job's own
// admin token (see profile/authstar.go) — rather than consumed immediately
// within the same function call.
//
// ADR 0012 assumes this token is "provisioned once by a human operator" as a
// genuinely durable credential in a real deployment; a Kubernetes
// TokenRequest token is fundamentally short-lived (bounded TTL) and is not
// that. This is a dev-cluster-appropriate stand-in, not a production
// pattern: kluster's clusters are disposable and short-lived by design (see
// this project's own "tear the test cluster down when done" convention), so
// a generous-but-bounded TTL comfortably outlives a single `kluster up`
// session without needing real rotation machinery kluster itself doesn't
// have a use for.
func mintSignetAdminTokenWithTTL(ctx context.Context, h ClusterHandle, ttlSeconds int64) (string, error) {
	ttl := ttlSeconds
	tr := &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{signetAdminTokenAudience},
			ExpirationSeconds: &ttl,
		},
	}
	resp, err := h.K8sClient.CoreV1().ServiceAccounts(signetNamespace).
		CreateToken(ctx, signetAdminServiceAccount, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create token for %s/%s: %w", signetNamespace, signetAdminServiceAccount, err)
	}
	return resp.Status.Token, nil
}

// withSignetAdminPortForward opens a port-forward to signet's admin gRPC
// port on the running signet pod (client-go's SPDY tooling — the same
// underlying mechanism `kubectl port-forward` uses, invoked as a library
// rather than a shelled-out subprocess) and invokes fn with the local
// "127.0.0.1:<port>" address for the duration of the call. This is the
// only way for kluster's own process — running on the host, outside the
// cluster's pod network — to reach signet's admin port; see this file's
// package doc comment for the full story of why this differs from how the
// (in-cluster) RabbitMQ provisioning Job reaches the same Service.
func withSignetAdminPortForward(ctx context.Context, h ClusterHandle, fn func(addr string) error) error {
	pods, err := h.K8sClient.CoreV1().Pods(signetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s,app.kubernetes.io/instance=%s", signetRelease, signetRelease),
	})
	if err != nil {
		return fmt.Errorf("list signet pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no signet pods found in namespace %s", signetNamespace)
	}
	podName := pods.Items[0].Name

	transport, upgrader, err := spdy.RoundTripperFor(h.RESTConfig)
	if err != nil {
		return fmt.Errorf("build spdy transport: %w", err)
	}

	req := h.K8sClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(signetNamespace).
		Name(podName).
		SubResource("portforward")
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	var outBuf, errBuf bytes.Buffer

	fw, err := portforward.New(dialer, []string{fmt.Sprintf(":%d", signetAdminPort)}, stopCh, readyCh, &outBuf, &errBuf)
	if err != nil {
		return fmt.Errorf("create port forwarder: %w", err)
	}

	fwErrCh := make(chan error, 1)
	go func() { fwErrCh <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-fwErrCh:
		return fmt.Errorf("port-forward to signet admin port failed: %w (stderr: %s)", err, errBuf.String())
	case <-time.After(30 * time.Second):
		close(stopCh)
		return fmt.Errorf("port-forward to signet admin port did not become ready in time")
	}
	defer close(stopCh)

	ports, err := fw.GetPorts()
	if err != nil {
		return fmt.Errorf("get forwarded ports: %w", err)
	}
	if len(ports) == 0 {
		return fmt.Errorf("no forwarded ports returned")
	}

	return fn(fmt.Sprintf("127.0.0.1:%d", ports[0].Local))
}

// bearerPerRPCCreds injects "authorization: Bearer <token>" into every
// outgoing gRPC call, mirroring the TypeScript client's authInterceptor.
// RequireTransportSecurity is false because the underlying connection here
// is an insecure local loopback tunnel already carried inside the
// port-forward's own authenticated/encrypted SPDY connection to the API
// server — the same trust boundary the "kubectl port-forward" admin
// workflow documented in signet/docs/cli.md relies on.
type bearerPerRPCCreds struct{ token string }

func (b bearerPerRPCCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearerPerRPCCreds) RequireTransportSecurity() bool { return false }

// sopsEncryptValue encrypts plaintext into a SOPS-format YAML document
// (top-level "value" key, matching every hand-authored secret file
// elsewhere in this ecosystem — see signet-smoke-test/secrets/*/*.yaml)
// under the given age recipient, following the exact same
// GenerateDataKey -> EncryptTree -> EmitEncryptedFile sequence sops'
// own CLI uses (cmd/sops/common.EncryptTree), so the ciphertext is
// byte-for-byte the same format signet's own SOPS-decrypt path
// (internal/gitops/sops.go) already round-trips.
func sopsEncryptValue(plaintext, ageRecipient string) ([]byte, error) {
	key, err := sopsage.MasterKeyFromRecipient(ageRecipient)
	if err != nil {
		return nil, fmt.Errorf("parse age recipient: %w", err)
	}

	tree := sops.Tree{
		Branches: sops.TreeBranches{
			sops.TreeBranch{
				sops.TreeItem{Key: "value", Value: plaintext},
			},
		},
		Metadata: sops.Metadata{
			KeyGroups: []sops.KeyGroup{{key}},
			Version:   signetBundleSOPSVersion,
		},
	}

	dataKey, errs := tree.GenerateDataKey()
	if len(errs) > 0 {
		return nil, fmt.Errorf("generate sops data key: %v", errs)
	}

	if err := sopscommon.EncryptTree(sopscommon.EncryptTreeOpts{
		Tree:    &tree,
		Cipher:  sopsaes.NewCipher(),
		DataKey: dataKey,
	}); err != nil {
		return nil, fmt.Errorf("encrypt sops tree: %w", err)
	}

	store := sopsyaml.NewStore(&sopsconfig.YAMLStoreConfig{})
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		return nil, fmt.Errorf("emit encrypted sops file: %w", err)
	}
	return out, nil
}

// tarGzSingleFile builds an in-memory tar.gz archive containing exactly one
// file, matching the shape SyncBundle expects (see bundle_cmd.go's
// bundlePushCmd for the reference archive-building sequence this mirrors).
func tarGzSingleFile(name string, content []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, fmt.Errorf("tar header: %w", err)
	}
	if _, err := tw.Write(content); err != nil {
		return nil, fmt.Errorf("tar write: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("finalize tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("finalize gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// ensureSOPSPublicKey returns signet's active SOPS age public key,
// bootstrapping the very first one via RotateSOPSKey if none exists yet.
//
// A brand-new signet install has *no* SOPS key at all — nothing in
// signet's own install path creates one automatically. Every existing
// worked example (signet-smoke-test's provision.sh, signet's own
// docs/cli.md) treats `signet sops-key rotate` as an explicit, separate,
// human-run provisioning step performed once after signet comes up, before
// anything tries to push a GitOps-synced secret. Since kluster automates
// that entire human-operator role for RabbitMQ's own bootstrap credential
// (see SeedSignetSecret's doc comment), it must automate this step too —
// otherwise the very first SeedSignetSecret call in a fresh cluster fails
// with GetSOPSPublicKey returning NotFound, exactly as RotateSOPSKey's own
// server-side implementation expects on a first call (internal/api/gitops.go
// explicitly comments "ignore not-found on first rotation" when looking up
// the key it's about to deactivate).
func ensureSOPSPublicKey(ctx context.Context, client adminv1.GitOpsServiceClient) (string, error) {
	resp, err := client.GetSOPSPublicKey(ctx, &adminv1.GetSOPSPublicKeyRequest{})
	if err == nil {
		return resp.GetPublicKey(), nil
	}
	if status.Code(err) != codes.NotFound {
		return "", fmt.Errorf("get sops public key: %w", err)
	}

	rotateResp, rotateErr := client.RotateSOPSKey(ctx, &adminv1.RotateSOPSKeyRequest{})
	if rotateErr != nil {
		return "", fmt.Errorf("bootstrap first sops key (get returned NotFound): %w", rotateErr)
	}
	return rotateResp.GetNewPublicKey(), nil
}

// pushSignetSecret dials signet's admin API (already tunneled to addr by
// withSignetAdminPortForward), fetches the active SOPS age public key,
// encrypts value, and syncs it to
// secrets/<namespace>/<service>/<secretName>.yaml via the SyncBundle RPC —
// the same RPC and directory convention "signet bundle push" and every
// GitOps-synced secret in this ecosystem already use.
func pushSignetSecret(ctx context.Context, addr, token, namespace, service, secretName, value string) error {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerPerRPCCreds{token: token}),
	)
	if err != nil {
		return fmt.Errorf("dial signet admin API: %w", err)
	}
	defer conn.Close()

	client := adminv1.NewGitOpsServiceClient(conn)

	publicKey, err := ensureSOPSPublicKey(ctx, client)
	if err != nil {
		return fmt.Errorf("ensure sops public key: %w", err)
	}

	encrypted, err := sopsEncryptValue(value, publicKey)
	if err != nil {
		return fmt.Errorf("sops-encrypt secret: %w", err)
	}

	archive, err := tarGzSingleFile(
		fmt.Sprintf("secrets/%s/%s/%s.yaml", namespace, service, secretName),
		encrypted,
	)
	if err != nil {
		return fmt.Errorf("build bundle archive: %w", err)
	}

	stream, err := client.SyncBundle(ctx)
	if err != nil {
		return fmt.Errorf("open SyncBundle stream: %w", err)
	}

	if err := stream.Send(&adminv1.SyncBundleChunk{
		Payload: &adminv1.SyncBundleChunk_Header{
			Header: &adminv1.SyncBundleHeader{SecretsPath: "secrets/"},
		},
	}); err != nil {
		return fmt.Errorf("send SyncBundle header: %w", err)
	}

	const chunkSize = 64 << 10
	data := archive
	for len(data) > 0 {
		n := chunkSize
		if n > len(data) {
			n = len(data)
		}
		if err := stream.Send(&adminv1.SyncBundleChunk{
			Payload: &adminv1.SyncBundleChunk_Data{Data: data[:n]},
		}); err != nil {
			return fmt.Errorf("send SyncBundle chunk: %w", err)
		}
		data = data[n:]
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("SyncBundle: %w", err)
	}
	if len(resp.GetErrors()) > 0 {
		return fmt.Errorf("SyncBundle reported errors for %s/%s/%s: %v", namespace, service, secretName, resp.GetErrors())
	}
	return nil
}

// SeedSignetAdminTokenSecret mints a signet-admin Kubernetes ServiceAccount
// token with retainedTTLSeconds lifetime and pushes *that token itself* into
// signet as a secret value — used to give another workload (the RabbitMQ
// provisioning Job's own elevated GitOpsService access; see
// profile/authstar.go) a durable-enough admin credential of its own,
// fetched later via its own SPIFFE workload identity rather than kluster's.
// See mintSignetAdminTokenWithTTL's doc comment for why a bounded TTL is an
// acceptable dev-cluster stand-in for ADR 0012's "provisioned once by a
// human operator" real-world assumption.
func SeedSignetAdminTokenSecret(ctx context.Context, h ClusterHandle, namespace, service, secretName string, retainedTTLSeconds int64) error {
	retainedToken, err := mintSignetAdminTokenWithTTL(ctx, h, retainedTTLSeconds)
	if err != nil {
		return fmt.Errorf("mint retained admin token: %w", err)
	}
	return SeedSignetSecret(ctx, h, namespace, service, secretName, retainedToken)
}

// SeedSignetSecret is the entry point addons use to push a single secret
// value into signet ahead of the workload that will consume it starting up
// (e.g. RabbitMQ's own bootstrap credential in rabbitmq.go, or the
// RabbitMQ provisioning Job's own elevated admin token). It stands in for
// the one manual step ADR 0011 says every self-hosted install still needs
// ("an operator provisions tower's own secrets... once") — kluster performs
// it itself so `kluster up --profile authstar` stays a single command.
//
// namespace/service must match the SPIFFE ns/sa the consuming workload
// authenticates as, per signet's convention-first access policy.
func SeedSignetSecret(ctx context.Context, h ClusterHandle, namespace, service, secretName, value string) error {
	if err := ensureSignetAdminRBAC(ctx, h); err != nil {
		return fmt.Errorf("ensure signet admin RBAC: %w", err)
	}
	token, err := mintSignetAdminToken(ctx, h)
	if err != nil {
		return fmt.Errorf("mint signet admin token: %w", err)
	}

	// A handful of retries absorbs the small window where the
	// ClusterRoleBinding created above hasn't yet been observed by the API
	// server's authorizer, or signet's own pod isn't quite accepting
	// connections yet on a freshly-created cluster.
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(3 * time.Second)
		}
		lastErr = withSignetAdminPortForward(ctx, h, func(addr string) error {
			return pushSignetSecret(ctx, addr, token, namespace, service, secretName, value)
		})
		if lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("seed signet secret %s/%s/%s: %w", namespace, service, secretName, lastErr)
}
