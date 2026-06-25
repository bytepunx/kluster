package versions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

// Entry describes a single chart managed by kluster.
type Entry struct {
	Addon     string // addon name in the kluster registry
	RepoURL   string // Helm repository base URL
	ChartName string // chart name within the repository index
}

// Catalog lists every chart kluster manages, in display order.
// Each unique RepoURL is fetched only once during a Fetch call.
var Catalog = []Entry{
	{Addon: "cert-manager", RepoURL: "https://charts.jetstack.io", ChartName: "cert-manager"},
	{Addon: "spire", RepoURL: "https://spiffe.github.io/helm-charts", ChartName: "spire"},
	{Addon: "traefik", RepoURL: "https://traefik.github.io/charts", ChartName: "traefik"},
	{Addon: "argocd", RepoURL: "https://argoproj.github.io/argo-helm", ChartName: "argo-cd"},
	{Addon: "rabbitmq", RepoURL: "https://charts.bitnami.com/bitnami", ChartName: "rabbitmq"},
	{Addon: "dex", RepoURL: "https://charts.dexidp.io", ChartName: "dex"},
	{Addon: "prometheus", RepoURL: "https://prometheus-community.github.io/helm-charts", ChartName: "prometheus"},
	{Addon: "grafana", RepoURL: "https://grafana.github.io/helm-charts", ChartName: "grafana"},
	{Addon: "loki", RepoURL: "https://grafana.github.io/helm-charts", ChartName: "loki"},
	{Addon: "tempo", RepoURL: "https://grafana.github.io/helm-charts", ChartName: "tempo"},
}

// File is the on-disk representation at Path().
type File struct {
	Updated time.Time         `yaml:"updated"`
	Charts  map[string]string `yaml:"charts"` // addon name → version string
}

// current is the in-process chart version state, populated by Ensure().
var current File

// Path returns the canonical location of the versions file.
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kluster", "chart-versions.yaml")
}

// Load reads and parses the versions file from disk.
func Load() (File, error) {
	data, err := os.ReadFile(Path())
	if err != nil {
		return File{}, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return File{}, fmt.Errorf("parse chart versions: %w", err)
	}
	if f.Charts == nil {
		f.Charts = make(map[string]string)
	}
	return f, nil
}

// Save writes the versions file to disk, creating the parent directory if needed.
func Save(f File) error {
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal chart versions: %w", err)
	}
	return os.WriteFile(Path(), data, 0o600)
}

// Fetch contacts each repo in Catalog, resolves the latest stable version for
// each chart, and returns a populated File ready for saving.
// Each unique RepoURL is fetched exactly once regardless of how many charts share it.
func Fetch(ctx context.Context) (File, error) {
	byRepo := make(map[string][]Entry)
	for _, e := range Catalog {
		byRepo[e.RepoURL] = append(byRepo[e.RepoURL], e)
	}

	f := File{
		Updated: time.Now().UTC(),
		Charts:  make(map[string]string, len(Catalog)),
	}
	for repoURL, entries := range byRepo {
		idx, err := fetchIndex(ctx, repoURL)
		if err != nil {
			return File{}, fmt.Errorf("fetch %s: %w", repoURL, err)
		}
		for _, e := range entries {
			v, err := latestStable(idx, e.ChartName)
			if err != nil {
				return File{}, fmt.Errorf("resolve %s: %w", e.ChartName, err)
			}
			f.Charts[e.Addon] = v
		}
	}
	return f, nil
}

// Ensure loads the versions file if it exists; otherwise fetches from the upstream
// repos and saves it. It also populates the package-level state used by For().
// Returns true if a network fetch was performed.
func Ensure(ctx context.Context) (bool, error) {
	f, err := Load()
	if err == nil {
		current = f
		return false, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("load chart versions: %w", err)
	}
	f, err = Fetch(ctx)
	if err != nil {
		return true, err
	}
	if err := Save(f); err != nil {
		return true, fmt.Errorf("save chart versions: %w", err)
	}
	current = f
	return true, nil
}

// For returns the pinned version string for the named addon.
// Falls back to "> 0.0.0" when the addon is absent from the loaded file,
// which preserves Helm's latest-available behaviour as a safety net.
func For(addon string) string {
	if v, ok := current.Charts[addon]; ok && v != "" {
		return v
	}
	return "> 0.0.0"
}

// --- internal ---

type repoIndex struct {
	Entries map[string][]repoEntry `yaml:"entries"`
}

type repoEntry struct {
	Version string `yaml:"version"`
}

func fetchIndex(ctx context.Context, repoURL string) (repoIndex, error) {
	url := strings.TrimRight(repoURL, "/") + "/index.yaml"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return repoIndex{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return repoIndex{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return repoIndex{}, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return repoIndex{}, err
	}
	var idx repoIndex
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return repoIndex{}, fmt.Errorf("parse index from %s: %w", repoURL, err)
	}
	return idx, nil
}

func latestStable(idx repoIndex, chartName string) (string, error) {
	entries, ok := idx.Entries[chartName]
	if !ok || len(entries) == 0 {
		return "", fmt.Errorf("chart %q not found in repository index", chartName)
	}
	var vs []*semver.Version
	for _, e := range entries {
		v, err := semver.NewVersion(e.Version)
		if err != nil {
			continue
		}
		if v.Prerelease() != "" {
			continue
		}
		vs = append(vs, v)
	}
	if len(vs) == 0 {
		return "", fmt.Errorf("no stable version found for chart %q", chartName)
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].GreaterThan(vs[j]) })
	return vs[0].Original(), nil
}
