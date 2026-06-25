package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/profile"
	"github.com/bytepunx/kluster-lib/provider"
	"github.com/bytepunx/kluster-lib/versions"
)

type Cluster struct {
	provider provider.Provider
	addons   map[string]addon.Addon
	profiles map[string]profile.Profile
}

// NewDefault constructs a Cluster populated with every globally-registered addon
// and profile. Registration happens automatically via init() functions in each
// addon and profile package file when those packages are first imported.
func NewDefault(p provider.Provider) *Cluster {
	return New(p, addon.All(), profile.All())
}

func New(p provider.Provider, addons []addon.Addon, profiles []profile.Profile) *Cluster {
	c := &Cluster{
		provider: p,
		addons:   make(map[string]addon.Addon, len(addons)),
		profiles: make(map[string]profile.Profile, len(profiles)),
	}
	for _, a := range addons {
		c.addons[a.Name()] = a
	}
	for _, pr := range profiles {
		c.profiles[pr.Name()] = pr
	}
	return c
}

func (c *Cluster) Up(ctx context.Context, cfg provider.ClusterConfig, progress ProgressFunc) error {
	emit := func(e ProgressEvent) {
		if progress != nil {
			progress(e)
		}
	}

	// ── chart versions ──────────────────────────────────────────────────────────
	emit(ProgressEvent{Kind: EventStarted, Phase: PhaseSetup,
		Name: "chart versions", Label: "resolving chart versions"})
	t := time.Now()
	if _, err := versions.Ensure(ctx); err != nil {
		emit(ProgressEvent{Kind: EventFailed, Phase: PhaseSetup,
			Name: "chart versions", Elapsed: time.Since(t), Err: err})
		return fmt.Errorf("resolve chart versions: %w", err)
	}
	emit(ProgressEvent{Kind: EventDone, Phase: PhaseSetup,
		Name: "chart versions", Elapsed: time.Since(t)})

	// ── cluster creation ────────────────────────────────────────────────────────
	clusterLabel := fmt.Sprintf("cluster %q", cfg.Name)
	emit(ProgressEvent{Kind: EventStarted, Phase: PhaseSetup,
		Name: clusterLabel, Label: fmt.Sprintf("creating cluster %q", cfg.Name)})
	t = time.Now()
	if err := c.provider.Create(ctx, cfg); err != nil {
		emit(ProgressEvent{Kind: EventFailed, Phase: PhaseSetup,
			Name: clusterLabel, Elapsed: time.Since(t), Err: err})
		return fmt.Errorf("create cluster: %w", err)
	}
	emit(ProgressEvent{Kind: EventDone, Phase: PhaseSetup,
		Name: clusterLabel, Elapsed: time.Since(t)})

	// ── internal plumbing (no events — fast, invisible to the user) ─────────────
	restConfig, err := c.provider.RESTConfig(ctx, cfg.Name)
	if err != nil {
		return fmt.Errorf("get REST config: %w", err)
	}
	handle, err := addon.NewClusterHandle(restConfig, cfg)
	if err != nil {
		return fmt.Errorf("build cluster handle: %w", err)
	}
	profileOrder, err := c.resolveProfiles(cfg.Profiles)
	if err != nil {
		return fmt.Errorf("resolve profiles: %w", err)
	}
	addonNames := c.collectAddons(profileOrder, cfg.Addons)
	sortedAddons, err := c.topoSort(addonNames)
	if err != nil {
		return fmt.Errorf("resolve addon order: %w", err)
	}

	// ── addons ──────────────────────────────────────────────────────────────────
	for _, name := range sortedAddons {
		a := c.addons[name]

		emit(ProgressEvent{Kind: EventStarted, Phase: PhaseAddon,
			Name: name, Label: name + ": installing"})
		t = time.Now()
		if err := a.Install(ctx, handle); err != nil {
			emit(ProgressEvent{Kind: EventFailed, Phase: PhaseAddon,
				Name: name, Elapsed: time.Since(t), Err: err})
			return fmt.Errorf("install %s: %w", name, err)
		}

		emit(ProgressEvent{Kind: EventProgress, Phase: PhaseAddon,
			Name: name, Label: name + ": waiting for ready"})
		if err := a.Ready(ctx, handle); err != nil {
			emit(ProgressEvent{Kind: EventFailed, Phase: PhaseAddon,
				Name: name, Elapsed: time.Since(t), Err: err})
			return fmt.Errorf("wait for %s: %w", name, err)
		}
		emit(ProgressEvent{Kind: EventDone, Phase: PhaseAddon,
			Name: name, Elapsed: time.Since(t)})
	}

	// ── profiles ────────────────────────────────────────────────────────────────
	for _, name := range profileOrder {
		p := c.profiles[name]

		emit(ProgressEvent{Kind: EventStarted, Phase: PhaseProfile,
			Name: name, Label: name + ": configuring"})
		t = time.Now()
		if err := p.Configure(ctx, handle, cfg); err != nil {
			emit(ProgressEvent{Kind: EventFailed, Phase: PhaseProfile,
				Name: name, Elapsed: time.Since(t), Err: err})
			return fmt.Errorf("configure profile %s: %w", name, err)
		}
		emit(ProgressEvent{Kind: EventDone, Phase: PhaseProfile,
			Name: name, Elapsed: time.Since(t)})
	}

	return nil
}

func (c *Cluster) Down(ctx context.Context, name string) error {
	return c.provider.Delete(ctx, name)
}

func (c *Cluster) Status(ctx context.Context) ([]provider.ClusterInfo, error) {
	return c.provider.List(ctx)
}

func (c *Cluster) Kubeconfig(ctx context.Context, name string) ([]byte, error) {
	return c.provider.Kubeconfig(ctx, name)
}

// resolveProfiles returns profile names in dependency order (dependencies first).
func (c *Cluster) resolveProfiles(names []string) ([]string, error) {
	visited := make(map[string]bool)
	inStack := make(map[string]bool)
	var order []string

	var visit func(name string) error
	visit = func(name string) error {
		if inStack[name] {
			return fmt.Errorf("cyclic profile dependency at %q", name)
		}
		if visited[name] {
			return nil
		}
		inStack[name] = true
		defer func() { inStack[name] = false }()

		p, ok := c.profiles[name]
		if !ok {
			return fmt.Errorf("unknown profile %q", name)
		}
		for _, dep := range p.RequiresProfiles() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[name] = true
		order = append(order, name)
		return nil
	}

	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// collectAddons gathers the deduplicated list of addon names from resolved
// profiles (in profile order) followed by any explicitly requested extras.
func (c *Cluster) collectAddons(profileOrder []string, extra []string) []string {
	seen := make(map[string]bool)
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, profileName := range profileOrder {
		p, ok := c.profiles[profileName]
		if !ok {
			continue
		}
		for _, a := range p.Addons() {
			add(a)
		}
	}
	for _, name := range extra {
		add(name)
	}
	return names
}

// topoSort returns addon names in dependency-resolved installation order.
// Returns an error if an unknown addon is referenced or a cycle is detected.
func (c *Cluster) topoSort(addonNames []string) ([]string, error) {
	visited := make(map[string]bool)
	inStack := make(map[string]bool)
	var order []string

	var visit func(name string) error
	visit = func(name string) error {
		if inStack[name] {
			return fmt.Errorf("cyclic addon dependency at %q", name)
		}
		if visited[name] {
			return nil
		}
		inStack[name] = true
		defer func() { inStack[name] = false }()

		a, ok := c.addons[name]
		if !ok {
			return fmt.Errorf("unknown addon %q", name)
		}
		for _, dep := range a.Requires() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[name] = true
		order = append(order, name)
		return nil
	}

	for _, name := range addonNames {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}
