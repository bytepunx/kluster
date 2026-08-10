package addon

import (
	"context"
	"fmt"
	"time"

	helmclient "github.com/mittwald/go-helm-client"
	helmaction "helm.sh/helm/v4/pkg/action"
	helmrepo "helm.sh/helm/v4/pkg/repo/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/bytepunx/kluster-lib/versions"
)

// clickhouse provisions a single-node bitnami/clickhouse instance for
// chronicle, the only authstar service that talks to ClickHouse directly.
//
// bitnami/clickhouse is served from the same classic "bitnami" chart repo
// index already registered by the rabbitmq addon (see
// rabbitmqRepoName/rabbitmqRepoURL in rabbitmq.go), so it is pinned via
// versions.go's normal Catalog/Fetch path rather than the unpinned OCI
// fallback that "signet" needs. (The repo's index.yaml entries point at OCI
// artifact refs under registry-1.docker.io/bitnamicharts — Bitnami's current
// distribution model — but the index itself is still a standard index.yaml
// that versions.go's Fetch() can parse for version pinning, and Helm
// transparently pulls the OCI artifact the index points at on install.)
const (
	clickhouseNamespace = "clickhouse"
	clickhouseRelease   = "clickhouse"
	clickhouseChart     = "bitnami/clickhouse"

	// clickhouseStatefulSet is the name of the single shard's StatefulSet.
	// The chart templates one StatefulSet per shard, named
	// "<fullname>-shard<index>" (0-based) — see templates/statefulset.yaml.
	// With release name == chart name, common.names.fullname collapses to
	// just "clickhouse", giving "clickhouse-shard0" for shards[0].
	clickhouseStatefulSet = "clickhouse-shard0"

	// clickhouseValues runs a single shard with no replication (shards: 1,
	// replicaCount: 1) and disables ClickHouse Keeper entirely: Keeper only
	// exists to coordinate replicated/sharded topologies, and running a
	// single unreplicated node needs no coordination service — cutting it
	// removes an extra StatefulSet from an already-disposable dev cluster.
	// Persistence is disabled (ephemeral dev cluster) and credentials are
	// fixed dev values, matching rabbitmq's guest/guest bluntness.
	//
	// image.repository overrides the chart's default "bitnami/clickhouse":
	// Broadcom's 2025 restructuring of Bitnami's Docker Hub distribution
	// removed this chart's pinned image tag (25.7.5-debian-12-r0, matching
	// clickhouse chart 9.4.4) from the free "bitnami/*" org — confirmed via
	// `docker manifest inspect docker.io/bitnami/clickhouse:25.7.5-debian-12-r0`
	// returning "not found". The same tag is still served from the
	// free-but-frozen "bitnamilegacy/*" org, so installs use that instead.
	// global.security.allowInsecureImages is required alongside it: the
	// chart's own NOTES.txt refuses to complete install when the resolved
	// image isn't in its "recognized" list, which the bitnamilegacy
	// substitution trips (see https://github.com/bitnami/charts/issues/30850).
	clickhouseValues = `
shards: 1
replicaCount: 1

keeper:
  enabled: false

global:
  security:
    allowInsecureImages: true

image:
  registry: docker.io
  repository: bitnamilegacy/clickhouse

auth:
  username: default
  password: clickhouse

persistence:
  enabled: false

metrics:
  enabled: false
`
)

type ClickHouseAddon struct{}

var _ Addon = (*ClickHouseAddon)(nil)

func init() { Register(&ClickHouseAddon{}) }

func (*ClickHouseAddon) Name() string       { return "clickhouse" }
func (*ClickHouseAddon) Requires() []string { return nil }

func (*ClickHouseAddon) Install(ctx context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(clickhouseNamespace)
	if err != nil {
		return fmt.Errorf("clickhouse: helm client: %w", err)
	}

	if err := hc.AddOrUpdateChartRepo(helmrepo.Entry{
		Name: rabbitmqRepoName,
		URL:  rabbitmqRepoURL,
	}); err != nil {
		return fmt.Errorf("clickhouse: add repo: %w", err)
	}

	_, err = hc.InstallOrUpgradeChart(ctx, &helmclient.ChartSpec{
		ReleaseName:     clickhouseRelease,
		ChartName:       clickhouseChart,
		Namespace:       clickhouseNamespace,
		Version:         versions.For("clickhouse"),
		CreateNamespace: true,
		ValuesYaml:      clickhouseValues,
		WaitStrategy:    "legacy",
		DryRunStrategy:  helmaction.DryRunNone,
	}, nil)
	if err != nil {
		return fmt.Errorf("clickhouse: helm install: %w", err)
	}

	return nil
}

func (*ClickHouseAddon) Ready(ctx context.Context, h ClusterHandle) error {
	return wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			ss, err := h.K8sClient.AppsV1().StatefulSets(clickhouseNamespace).Get(ctx, clickhouseStatefulSet, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			return ss.Status.ReadyReplicas >= 1, nil
		},
	)
}

func (*ClickHouseAddon) Uninstall(_ context.Context, h ClusterHandle) error {
	hc, err := h.HelmClientFor(clickhouseNamespace)
	if err != nil {
		return fmt.Errorf("clickhouse: helm client: %w", err)
	}
	if err := hc.UninstallReleaseByName(clickhouseRelease); err != nil {
		return fmt.Errorf("clickhouse: helm uninstall: %w", err)
	}
	return nil
}
