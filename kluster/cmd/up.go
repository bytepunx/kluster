package cmd

import (
	"github.com/bytepunx/kluster-lib/cluster"
	"github.com/bytepunx/kluster-lib/provider"
	"github.com/spf13/cobra"

	// Trigger init() registrations in every addon and profile.
	_ "github.com/bytepunx/kluster-lib/addon"
	_ "github.com/bytepunx/kluster-lib/profile"
)


var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Create and configure a new cluster",
	RunE:  runUp,
}

func init() {
	rootCmd.AddCommand(upCmd)
	upCmd.Flags().String("name", "", "Cluster name")
	upCmd.Flags().String("profile", "signet", "Profile to activate: signet, authstar")
	upCmd.Flags().StringArray("addon", nil, "Additional opt-in addons: observability, tracing. Repeatable.")
	upCmd.Flags().String("trust-domain", "dev.cluster.local", "SPIFFE trust domain")
	upCmd.Flags().String("k3s-version", "", "k3s version tag (default: latest stable)")
}

func runUp(cmd *cobra.Command, _ []string) error {
	name, err := requireName(cmd)
	if err != nil {
		return err
	}
	profile := stringFlag(cmd, "profile", "profile")
	addons := stringSliceFlag(cmd, "addon", "addons")
	cfg := provider.ClusterConfig{
		Name:        name,
		K3sVersion:  stringFlag(cmd, "k3s-version", "k3s-version"),
		TrustDomain: stringFlag(cmd, "trust-domain", "trust-domain"),
		Profiles:    append([]string{profile}, addons...),
	}
	p, err := resolveProvider()
	if err != nil {
		return err
	}
	c := cluster.NewDefault(p)
	r := newRenderer(cmd.OutOrStdout())
	if err := c.Up(cmd.Context(), cfg, r.Handle); err != nil {
		return err
	}
	r.Done(name)
	return nil
}
