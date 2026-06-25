package cluster

import (
	"context"
	"testing"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/profile"
	"github.com/bytepunx/kluster-lib/provider"
)

// ── test doubles ────────────────────────────────────────────────────────────

type fakeAddon struct {
	name     string
	requires []string
}

func (f *fakeAddon) Name() string                                        { return f.name }
func (f *fakeAddon) Requires() []string                                  { return f.requires }
func (f *fakeAddon) Install(_ context.Context, _ addon.ClusterHandle) error   { return nil }
func (f *fakeAddon) Uninstall(_ context.Context, _ addon.ClusterHandle) error { return nil }
func (f *fakeAddon) Ready(_ context.Context, _ addon.ClusterHandle) error     { return nil }

type fakeProfile struct {
	name     string
	requires []string
	addons   []string
}

func (f *fakeProfile) Name() string               { return f.name }
func (f *fakeProfile) RequiresProfiles() []string { return f.requires }
func (f *fakeProfile) Addons() []string           { return f.addons }
func (f *fakeProfile) Configure(_ context.Context, _ addon.ClusterHandle, _ provider.ClusterConfig) error {
	return nil
}

func clusterWithAddons(addons ...*fakeAddon) *Cluster {
	c := &Cluster{
		addons:   make(map[string]addon.Addon),
		profiles: make(map[string]profile.Profile),
	}
	for _, a := range addons {
		c.addons[a.name] = a
	}
	return c
}

func clusterWithProfiles(profiles ...*fakeProfile) *Cluster {
	c := &Cluster{
		addons:   make(map[string]addon.Addon),
		profiles: make(map[string]profile.Profile),
	}
	for _, p := range profiles {
		c.profiles[p.name] = p
	}
	return c
}

// ── topoSort ────────────────────────────────────────────────────────────────

func TestTopoSort_LinearChain(t *testing.T) {
	// a requires b requires c → order must be c, b, a
	c := clusterWithAddons(
		&fakeAddon{name: "a", requires: []string{"b"}},
		&fakeAddon{name: "b", requires: []string{"c"}},
		&fakeAddon{name: "c"},
	)
	order, err := c.topoSort([]string{"a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"c", "b", "a"}
	for i, got := range order {
		if got != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got, want[i])
		}
	}
}

func TestTopoSort_DiamondDedup(t *testing.T) {
	// a → {b, c}; b → d; c → d  →  d must appear exactly once and before b, c, a
	c := clusterWithAddons(
		&fakeAddon{name: "a", requires: []string{"b", "c"}},
		&fakeAddon{name: "b", requires: []string{"d"}},
		&fakeAddon{name: "c", requires: []string{"d"}},
		&fakeAddon{name: "d"},
	)
	order, err := c.topoSort([]string{"a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pos := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}
	if pos("d") >= pos("b") || pos("d") >= pos("c") {
		t.Errorf("d must precede b and c; order=%v", order)
	}
	if pos("b") >= pos("a") || pos("c") >= pos("a") {
		t.Errorf("b and c must precede a; order=%v", order)
	}
	count := 0
	for _, n := range order {
		if n == "d" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("d appears %d times, want 1; order=%v", count, order)
	}
}

func TestTopoSort_CycleDetected(t *testing.T) {
	c := clusterWithAddons(
		&fakeAddon{name: "a", requires: []string{"b"}},
		&fakeAddon{name: "b", requires: []string{"a"}},
	)
	_, err := c.topoSort([]string{"a"})
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

func TestTopoSort_UnknownAddon(t *testing.T) {
	c := clusterWithAddons(
		&fakeAddon{name: "a", requires: []string{"missing"}},
	)
	_, err := c.topoSort([]string{"a"})
	if err == nil {
		t.Fatal("expected unknown addon error, got nil")
	}
}

// ── resolveProfiles ─────────────────────────────────────────────────────────

func TestResolveProfiles_DependencyOrder(t *testing.T) {
	// authstar requires signet → signet must come first
	c := clusterWithProfiles(
		&fakeProfile{name: "signet"},
		&fakeProfile{name: "authstar", requires: []string{"signet"}},
	)
	order, err := c.resolveProfiles([]string{"authstar"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 2 || order[0] != "signet" || order[1] != "authstar" {
		t.Errorf("unexpected order %v", order)
	}
}

func TestResolveProfiles_CycleDetected(t *testing.T) {
	c := clusterWithProfiles(
		&fakeProfile{name: "a", requires: []string{"b"}},
		&fakeProfile{name: "b", requires: []string{"a"}},
	)
	_, err := c.resolveProfiles([]string{"a"})
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
}

// ── collectAddons ───────────────────────────────────────────────────────────

func TestCollectAddons_Deduplication(t *testing.T) {
	// signet: [cert-manager, spire]
	// authstar: [rabbitmq]  (cert-manager already pulled in by signet)
	c := clusterWithProfiles(
		&fakeProfile{name: "signet", addons: []string{"cert-manager", "spire"}},
		&fakeProfile{name: "authstar", addons: []string{"rabbitmq"}},
	)
	names := c.collectAddons([]string{"signet", "authstar"}, []string{"cert-manager"})
	want := []string{"cert-manager", "spire", "rabbitmq"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("[%d] got %q, want %q", i, n, want[i])
		}
	}
}
