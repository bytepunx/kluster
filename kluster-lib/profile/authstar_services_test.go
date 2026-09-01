package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"
)

// These tests cover the pure, deterministic manifest-building logic in
// authstar_services.go — no cluster, no signet, no network. They exist
// because this code has no existing precedent in this package (zero prior
// test coverage) and produces YAML that's easy to get subtly wrong (field
// name typos, wrong path depth, a spec accidentally reused between the
// Deployment and CronJob variants of the same service). Round-tripping the
// generated YAML back through the real k8s API types is a cheap way to
// catch a wiring mistake here instead of only against a real cluster.

func TestDeploymentManifest_SignetWiredService(t *testing.T) {
	spec := serviceSpec{
		Name: "tower", Image: "docker.io/bytepunx/authstar-tower:latest", Port: 8080, ServiceAccount: "tower",
		Env:          signetEnvVars("dev.cluster.local", true),
		Volumes:      []corev1.Volume{spiffeCSIVolume()},
		VolumeMounts: []corev1.VolumeMount{spiffeCSIVolumeMount()},
	}

	raw, err := deploymentManifest(spec)
	if err != nil {
		t.Fatalf("deploymentManifest: %v", err)
	}

	var d appsv1.Deployment
	if err := sigsyaml.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal generated manifest: %v\n%s", err, raw)
	}

	if d.Name != "tower" || d.Namespace != authStarNamespace {
		t.Errorf("name/namespace = %s/%s, want tower/%s", d.Name, d.Namespace, authStarNamespace)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", d.Spec.Replicas)
	}
	if got := d.Spec.Template.Labels["app"]; got != "tower" {
		t.Errorf("pod template label app = %q, want tower", got)
	}
	if d.Spec.Template.Spec.ServiceAccountName != "tower" {
		t.Errorf("service account = %q, want tower", d.Spec.Template.Spec.ServiceAccountName)
	}

	containers := d.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected exactly one container, got %d", len(containers))
	}
	c := containers[0]
	if c.Image != spec.Image {
		t.Errorf("image = %q, want %q", c.Image, spec.Image)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("container ports = %v, want [8080]", c.Ports)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil || c.LivenessProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("liveness probe = %+v, want a TCP probe on 8080", c.LivenessProbe)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for _, want := range []string{"SIGNET_NAMESPACE", "SIGNET_ADDR", "SIGNET_ADMIN_ADDR", "SIGNET_TRUST_DOMAIN", "SPIFFE_ENDPOINT_SOCKET"} {
		if _, ok := env[want]; !ok {
			t.Errorf("env missing %s (got %v)", want, env)
		}
	}
	if env["SIGNET_NAMESPACE"] != authStarNamespace {
		t.Errorf("SIGNET_NAMESPACE = %q, want %q", env["SIGNET_NAMESPACE"], authStarNamespace)
	}
	if env["SIGNET_TRUST_DOMAIN"] != "dev.cluster.local" {
		t.Errorf("SIGNET_TRUST_DOMAIN = %q, want dev.cluster.local", env["SIGNET_TRUST_DOMAIN"])
	}

	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/run/spire/sockets" {
		t.Errorf("volume mounts = %v, want one at /run/spire/sockets", c.VolumeMounts)
	}
	if len(d.Spec.Template.Spec.Volumes) != 1 || d.Spec.Template.Spec.Volumes[0].CSI == nil ||
		d.Spec.Template.Spec.Volumes[0].CSI.Driver != "csi.spiffe.io" {
		t.Errorf("volumes = %v, want one csi.spiffe.io volume", d.Spec.Template.Spec.Volumes)
	}
}

func TestDeploymentManifest_HeraldNoAdminAddr(t *testing.T) {
	spec := serviceSpec{Name: "herald", Env: signetEnvVars("dev.cluster.local", false)}
	raw, err := deploymentManifest(spec)
	if err != nil {
		t.Fatalf("deploymentManifest: %v", err)
	}
	var d appsv1.Deployment
	if err := sigsyaml.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SIGNET_ADMIN_ADDR" {
			t.Error("SIGNET_ADMIN_ADDR present with admin=false, want absent")
		}
	}
}

func TestDeploymentManifest_PlainServiceHasNoSpiffeVolume(t *testing.T) {
	spec := serviceSpec{
		Name: "web", Image: "docker.io/bytepunx/authstar-web:latest", Port: 3000, ServiceAccount: "web",
		Env: []corev1.EnvVar{{Name: "TOWER_BASE_URL", Value: "http://tower.authstar.svc.cluster.local:8080"}},
	}
	raw, err := deploymentManifest(spec)
	if err != nil {
		t.Fatalf("deploymentManifest: %v", err)
	}
	var d appsv1.Deployment
	if err := sigsyaml.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(d.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("volumes = %v, want none for a plain (non-signet-wired) service", d.Spec.Template.Spec.Volumes)
	}
	if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) != 0 {
		t.Errorf("volume mounts = %v, want none", d.Spec.Template.Spec.Containers[0].VolumeMounts)
	}
}

func TestServiceManifest(t *testing.T) {
	spec := serviceSpec{Name: "portcullis", Port: 3710}
	raw, err := serviceManifest(spec)
	if err != nil {
		t.Fatalf("serviceManifest: %v", err)
	}
	var s corev1.Service
	if err := sigsyaml.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Name != "portcullis" || s.Namespace != authStarNamespace {
		t.Errorf("name/namespace = %s/%s, want portcullis/%s", s.Name, s.Namespace, authStarNamespace)
	}
	if s.Spec.Selector["app"] != "portcullis" {
		t.Errorf("selector = %v, want app=portcullis", s.Spec.Selector)
	}
	if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 3710 || s.Spec.Ports[0].TargetPort.IntValue() != 3710 {
		t.Errorf("ports = %v, want port==targetPort==3710", s.Spec.Ports)
	}
}

func TestCronJobManifest_RestartPolicyOnFailure(t *testing.T) {
	spec := serviceSpec{
		Name: "keep", Image: "docker.io/bytepunx/authstar-keep:latest", ServiceAccount: "keep",
		Env:          signetEnvVars("dev.cluster.local", false),
		Volumes:      []corev1.Volume{spiffeCSIVolume()},
		VolumeMounts: []corev1.VolumeMount{spiffeCSIVolumeMount()},
	}
	raw, err := cronJobManifest(spec, "keep-pricing-sync", "0 4 * * *", "America/New_York", []string{"node", "dist/sync-pricing.js"})
	if err != nil {
		t.Fatalf("cronJobManifest: %v", err)
	}
	var cj batchv1.CronJob
	if err := sigsyaml.Unmarshal([]byte(raw), &cj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cj.Name != "keep-pricing-sync" || cj.Namespace != authStarNamespace {
		t.Errorf("name/namespace = %s/%s", cj.Name, cj.Namespace)
	}
	if cj.Spec.Schedule != "0 4 * * *" {
		t.Errorf("schedule = %q, want 0 4 * * *", cj.Spec.Schedule)
	}
	if cj.Spec.TimeZone == nil || *cj.Spec.TimeZone != "America/New_York" {
		t.Errorf("timezone = %v, want America/New_York", cj.Spec.TimeZone)
	}
	podSpec := cj.Spec.JobTemplate.Spec.Template.Spec
	if podSpec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("restartPolicy = %q, want OnFailure (SPIRE per-Pod-UID registration race)", podSpec.RestartPolicy)
	}
	if got := podSpec.Containers[0].Command; len(got) != 2 || got[1] != "dist/sync-pricing.js" {
		t.Errorf("command = %v, want [node dist/sync-pricing.js]", got)
	}
}

func TestDBInitJobManifest(t *testing.T) {
	raw, err := dbInitJobManifest()
	if err != nil {
		t.Fatalf("dbInitJobManifest: %v", err)
	}
	var job batchv1.Job
	if err := sigsyaml.Unmarshal([]byte(raw), &job); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if job.Name != authStarDBInitJobName || job.Namespace != authStarNamespace {
		t.Errorf("name/namespace = %s/%s", job.Name, job.Namespace)
	}
	podSpec := job.Spec.Template.Spec
	if podSpec.ServiceAccountName != authStarDBInitServiceAccount {
		t.Errorf("service account = %q, want %q", podSpec.ServiceAccountName, authStarDBInitServiceAccount)
	}
	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Image != "postgres:16-alpine" {
		t.Errorf("init containers = %v, want one postgres:16-alpine container", podSpec.InitContainers)
	}
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].Image != "curlimages/curl:8.11.1" {
		t.Errorf("containers = %v, want one curlimages/curl container", podSpec.Containers)
	}
	// Every database this Job is responsible for creating must actually be
	// named in its command — a cheap guard against silently dropping one
	// when editing the shared shell script.
	initCmd := podSpec.InitContainers[0].Command
	if len(initCmd) < 3 {
		t.Fatalf("init container command = %v, too short", initCmd)
	}
	script := initCmd[2]
	if !strings.Contains(script, "for db in tower keep herald") {
		t.Errorf("init script does not loop over tower/keep/herald: %s", script)
	}
	mainCmd := podSpec.Containers[0].Command
	if len(mainCmd) < 3 || !strings.Contains(mainCmd[2], "chronicle") {
		t.Errorf("clickhouse-database command does not reference the chronicle database: %v", mainCmd)
	}
}

func TestPortcullisConfigDoc_MarshalsToExpectedShape(t *testing.T) {
	data, err := sigsyaml.Marshal(portcullisConfigDoc())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var doc map[string]any
	if err := sigsyaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tenants, ok := doc["tenants"].(map[string]any)
	if !ok {
		t.Fatalf("tenants = %#v, want a map", doc["tenants"])
	}
	acme, ok := tenants["acme"].(map[string]any)
	if !ok {
		t.Fatalf("tenants.acme = %#v, want a map", tenants["acme"])
	}
	if acme["status"] != "active" {
		t.Errorf("tenants.acme.status = %v, want active", acme["status"])
	}

	webUpstream, ok := doc["webUpstream"].(map[string]any)
	if !ok || webUpstream["address"] != "http://web.authstar.svc.cluster.local:3000" {
		t.Errorf("webUpstream = %#v", doc["webUpstream"])
	}
	towerUpstream, ok := doc["towerUpstream"].(map[string]any)
	if !ok || towerUpstream["address"] != "https://tower.authstar.svc.cluster.local:8080" {
		t.Errorf("towerUpstream = %#v, want https (tower always runs real SPIFFE mTLS)", doc["towerUpstream"])
	}
}

func TestAuthStarDBConnectionString(t *testing.T) {
	got := authStarDBConnectionString("tower")
	want := "postgres://postgres:postgres@postgresql.postgres.svc.cluster.local:5432/tower?sslmode=disable"
	if got != want {
		t.Errorf("authStarDBConnectionString(tower) = %q, want %q", got, want)
	}
}

func TestWriteAuthStarTokens_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	tokens := map[string]string{"towerOperatorToken": "abc", "heraldOperatorToken": "def"}
	if err := writeAuthStarTokens("my-cluster", tokens); err != nil {
		t.Fatalf("writeAuthStarTokens: %v", err)
	}

	path, err := authStarTokensPath("my-cluster")
	if err != nil {
		t.Fatalf("authStarTokensPath: %v", err)
	}
	if filepath.Base(path) != "my-cluster-authstar-tokens.yaml" {
		t.Errorf("path = %q, unexpected filename", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	var got map[string]string
	if err := sigsyaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal written file: %v", err)
	}
	if got["towerOperatorToken"] != "abc" || got["heraldOperatorToken"] != "def" {
		t.Errorf("round-tripped tokens = %v, want %v", got, tokens)
	}
}
