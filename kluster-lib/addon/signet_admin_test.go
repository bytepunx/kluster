package addon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	adminv1 "github.com/bytepunx/signet/gen/admin/v1"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/decrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"gopkg.in/yaml.v3"
)

// These tests exist because signet_admin.go hand-codes calls against
// bytepunx/signet's generated adminv1 gRPC client/message types — a
// dependency that moves fast (see its CHANGELOG) and has no compiler-visible
// contract with kluster beyond "does it still build". A method rename, a
// changed oneof shape, or a field rename here would only surface at runtime
// against a real cluster otherwise. Exercising the real generated client
// against a fake in-process server (rather than mocking kluster's own
// interfaces) means a breaking change in the adminv1 wire contract fails
// these tests the same way it would fail in production.

// fakeGitOpsServer implements just enough of adminv1.GitOpsServiceServer to
// drive pushSignetSecret end-to-end. Embedding UnimplementedGitOpsServiceServer
// keeps this satisfying the interface across signet versions that add RPCs.
type fakeGitOpsServer struct {
	adminv1.UnimplementedGitOpsServiceServer

	mu sync.Mutex

	publicKey    string
	notFoundOnce bool // if true, GetSOPSPublicKey returns NotFound exactly once

	gotHeader  *adminv1.SyncBundleHeader
	gotArchive []byte
	syncErrors []string // if set, SyncBundle response carries these errors
}

func (f *fakeGitOpsServer) GetSOPSPublicKey(_ context.Context, _ *adminv1.GetSOPSPublicKeyRequest) (*adminv1.GetSOPSPublicKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notFoundOnce {
		f.notFoundOnce = false
		return nil, status.Error(codes.NotFound, "no sops key yet")
	}
	if f.publicKey == "" {
		return nil, status.Error(codes.NotFound, "no sops key yet")
	}
	return &adminv1.GetSOPSPublicKeyResponse{PublicKey: f.publicKey}, nil
}

func (f *fakeGitOpsServer) RotateSOPSKey(_ context.Context, _ *adminv1.RotateSOPSKeyRequest) (*adminv1.RotateSOPSKeyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &adminv1.RotateSOPSKeyResponse{NewPublicKey: f.publicKey}, nil
}

func (f *fakeGitOpsServer) SyncBundle(stream grpc.ClientStreamingServer[adminv1.SyncBundleChunk, adminv1.SyncBundleResponse]) error {
	var archive bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch p := chunk.GetPayload().(type) {
		case *adminv1.SyncBundleChunk_Header:
			f.mu.Lock()
			f.gotHeader = p.Header
			f.mu.Unlock()
		case *adminv1.SyncBundleChunk_Data:
			archive.Write(p.Data)
		}
	}
	f.mu.Lock()
	f.gotArchive = archive.Bytes()
	errs := f.syncErrors
	f.mu.Unlock()
	return stream.SendAndClose(&adminv1.SyncBundleResponse{SecretsAdded: 1, Errors: errs})
}

// dialFakeGitOpsServer starts srv on an in-memory bufconn listener and
// returns a real adminv1.GitOpsServiceClient dialed against it, plus a
// cleanup func.
func dialFakeGitOpsServer(t *testing.T, srv *fakeGitOpsServer) adminv1.GitOpsServiceClient {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	adminv1.RegisterGitOpsServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return adminv1.NewGitOpsServiceClient(conn)
}

func generateAgeIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id
}

func TestSopsEncryptValue_RoundTrip(t *testing.T) {
	id := generateAgeIdentity(t)
	const plaintext = "s3cr3t-value"

	encrypted, err := sopsEncryptValue(plaintext, id.Recipient().String())
	if err != nil {
		t.Fatalf("sopsEncryptValue: %v", err)
	}

	t.Setenv("SOPS_AGE_KEY", id.String())
	decrypted, err := decrypt.DataWithFormat(encrypted, formats.Yaml)
	if err != nil {
		t.Fatalf("decrypt round-trip: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(decrypted, &doc); err != nil {
		t.Fatalf("unmarshal decrypted yaml: %v\n%s", err, decrypted)
	}
	if got, ok := doc["value"].(string); !ok || got != plaintext {
		t.Fatalf("decrypted value = %v, want %q", doc["value"], plaintext)
	}
}

func TestSopsEncryptValue_WrongKeyFailsToDecrypt(t *testing.T) {
	encryptID := generateAgeIdentity(t)
	wrongID := generateAgeIdentity(t)

	encrypted, err := sopsEncryptValue("top-secret", encryptID.Recipient().String())
	if err != nil {
		t.Fatalf("sopsEncryptValue: %v", err)
	}

	t.Setenv("SOPS_AGE_KEY", wrongID.String())
	if _, err := decrypt.DataWithFormat(encrypted, formats.Yaml); err == nil {
		t.Fatal("expected decrypt with wrong age identity to fail, got nil error")
	}
}

func TestTarGzSingleFile(t *testing.T) {
	const name = "secrets/foo/bar/baz.yaml"
	content := []byte("hello: world\n")

	archive, err := tarGzSingleFile(name, content)
	if err != nil {
		t.Fatalf("tarGzSingleFile: %v", err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if hdr.Name != name {
		t.Errorf("tar entry name = %q, want %q", hdr.Name, name)
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read tar entry: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("tar entry content = %q, want %q", got, content)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("expected exactly one entry, got extra entry (err=%v)", err)
	}
}

func TestBearerPerRPCCreds(t *testing.T) {
	creds := bearerPerRPCCreds{token: "abc123"}

	md, err := creds.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if want := "Bearer abc123"; md["authorization"] != want {
		t.Errorf("authorization header = %q, want %q", md["authorization"], want)
	}
	if creds.RequireTransportSecurity() {
		t.Error("RequireTransportSecurity() = true, want false (loopback port-forward tunnel)")
	}
}

func TestEnsureSOPSPublicKey_ExistingKey(t *testing.T) {
	id := generateAgeIdentity(t)
	srv := &fakeGitOpsServer{publicKey: id.Recipient().String()}
	client := dialFakeGitOpsServer(t, srv)

	got, err := ensureSOPSPublicKey(context.Background(), client)
	if err != nil {
		t.Fatalf("ensureSOPSPublicKey: %v", err)
	}
	if got != srv.publicKey {
		t.Errorf("public key = %q, want %q", got, srv.publicKey)
	}
}

func TestEnsureSOPSPublicKey_BootstrapsOnNotFound(t *testing.T) {
	id := generateAgeIdentity(t)
	srv := &fakeGitOpsServer{publicKey: id.Recipient().String(), notFoundOnce: true}
	client := dialFakeGitOpsServer(t, srv)

	got, err := ensureSOPSPublicKey(context.Background(), client)
	if err != nil {
		t.Fatalf("ensureSOPSPublicKey: %v", err)
	}
	if got != srv.publicKey {
		t.Errorf("public key after bootstrap = %q, want %q", got, srv.publicKey)
	}
}

func TestEnsureSOPSPublicKey_NonNotFoundErrorPropagates(t *testing.T) {
	srv := &fakeGitOpsServer{}
	client := dialFakeGitOpsServer(t, srv)

	// Simulate a non-NotFound failure by cancelling the context before the call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ensureSOPSPublicKey(ctx, client); err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestPushSignetSecret_EndToEnd(t *testing.T) {
	id := generateAgeIdentity(t)
	srv := &fakeGitOpsServer{publicKey: id.Recipient().String()}

	// pushSignetSecret dials by address string itself (it's the withSignetAdminPortForward
	// caller's job to hand it a local address), so use a real TCP listener rather than bufconn.
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	adminv1.RegisterGitOpsServiceServer(s, srv)
	go func() { _ = s.Serve(tl) }()
	t.Cleanup(s.Stop)

	const (
		namespace  = "authstar"
		service    = "rabbitmq"
		secretName = "bootstrap-password"
		value      = "sup3r-secret"
	)

	if err := pushSignetSecret(context.Background(), tl.Addr().String(), "test-token", namespace, service, secretName, value); err != nil {
		t.Fatalf("pushSignetSecret: %v", err)
	}

	if srv.gotHeader == nil {
		t.Fatal("server never received a SyncBundleHeader")
	}
	if srv.gotHeader.GetSecretsPath() != "secrets/" {
		t.Errorf("SecretsPath = %q, want %q", srv.gotHeader.GetSecretsPath(), "secrets/")
	}

	gz, err := gzip.NewReader(bytes.NewReader(srv.gotArchive))
	if err != nil {
		t.Fatalf("gzip.NewReader on received archive: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next on received archive: %v", err)
	}
	wantName := "secrets/" + strings.Join([]string{namespace, service, secretName + ".yaml"}, "/")
	if hdr.Name != wantName {
		t.Errorf("archived file name = %q, want %q", hdr.Name, wantName)
	}

	encrypted, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read archived file: %v", err)
	}
	t.Setenv("SOPS_AGE_KEY", id.String())
	decrypted, err := decrypt.DataWithFormat(encrypted, formats.Yaml)
	if err != nil {
		t.Fatalf("decrypt archived secret: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(decrypted, &doc); err != nil {
		t.Fatalf("unmarshal decrypted secret: %v", err)
	}
	if got, _ := doc["value"].(string); got != value {
		t.Errorf("pushed secret value = %q, want %q", got, value)
	}
}

func TestPushSignetSecret_SurfacesSyncErrors(t *testing.T) {
	id := generateAgeIdentity(t)
	srv := &fakeGitOpsServer{
		publicKey:  id.Recipient().String(),
		syncErrors: []string{"path-depth mismatch: secrets/bad"},
	}
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	adminv1.RegisterGitOpsServiceServer(s, srv)
	go func() { _ = s.Serve(tl) }()
	t.Cleanup(s.Stop)

	err = pushSignetSecret(context.Background(), tl.Addr().String(), "test-token", "ns", "svc", "secret", "value")
	if err == nil {
		t.Fatal("expected error when SyncBundle response carries errors, got nil")
	}
	if !strings.Contains(err.Error(), "path-depth mismatch") {
		t.Errorf("error %q does not contain the server-reported sync error", err.Error())
	}
}
