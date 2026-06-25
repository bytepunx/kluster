package profile

import (
	"context"

	"github.com/bytepunx/kluster-lib/addon"
	"github.com/bytepunx/kluster-lib/provider"
)

type Profile interface {
	Name() string
	RequiresProfiles() []string
	Addons() []string
	Configure(ctx context.Context, cluster addon.ClusterHandle, cfg provider.ClusterConfig) error
}

var registry = map[string]Profile{}

func Register(p Profile) {
	registry[p.Name()] = p
}

func Get(name string) (Profile, bool) {
	p, ok := registry[name]
	return p, ok
}

func All() []Profile {
	result := make([]Profile, 0, len(registry))
	for _, p := range registry {
		result = append(result, p)
	}
	return result
}
