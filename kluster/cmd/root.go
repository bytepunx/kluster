package cmd

import (
	"fmt"
	"os"

	"github.com/bytepunx/kluster-lib/provider"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

// Version is set at build time via -ldflags "-X github.com/bytepunx/kluster/cmd.Version=vX.Y.Z".
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:     "kluster",
	Short:   "Manage disposable local Kubernetes clusters",
	Long:    "kluster provisions and manages k3d-based local Kubernetes clusters for integration testing.",
	Version: Version,
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "",
		"Config file path (default: ./kluster.yaml)")
	rootCmd.PersistentFlags().String("provider", "k3d",
		"Cluster provider: k3d (local) or kind (CI)")
	_ = viper.BindPFlag("provider", rootCmd.PersistentFlags().Lookup("provider"))
}

// resolveProvider returns the provider from --provider flag or config file.
// It must match the provider used when the cluster was created.
func resolveProvider() (provider.Provider, error) {
	p := viper.GetString("provider")
	if p == "" {
		p = "k3d"
	}
	switch p {
	case "k3d":
		return provider.NewK3dProvider(), nil
	case "kind":
		return provider.NewKindProvider(), nil
	default:
		return nil, fmt.Errorf("unknown provider %q: choose k3d or kind", p)
	}
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
