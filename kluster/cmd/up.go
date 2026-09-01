package cmd

import (
	"fmt"
	"os"

	"github.com/bytepunx/kluster-lib/cluster"
	klusterprofile "github.com/bytepunx/kluster-lib/profile"
	"github.com/bytepunx/kluster-lib/provider"
	"github.com/spf13/cobra"

	// Trigger init() registrations in every addon and profile.
	_ "github.com/bytepunx/kluster-lib/addon"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Create and configure a new cluster",
	RunE:  runUp,
}

func init() {
	rootCmd.AddCommand(upCmd)
	upCmd.Flags().String("name", "", "Cluster name")
	upCmd.Flags().String("profile", "spire", "Profile to activate: spire, signet, authstar")
	upCmd.Flags().StringArray("addon", nil, "Additional opt-in addons: argocd, or addon groups observability, tracing. Repeatable.")
	upCmd.Flags().String("trust-domain", provider.DefaultTrustDomain, "SPIFFE trust domain")
	upCmd.Flags().String("k3s-version", "", "k3s version tag (default: latest stable)")
}

func runUp(cmd *cobra.Command, _ []string) error {
	if notice := repoLocalConfigNotice(); notice != "" {
		fmt.Fprintln(cmd.OutOrStdout(), notice)
	}

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
		Profiles:    []string{profile},
		Addons:      addons,
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

	// Only the authstar profile writes this file (tower/herald's operator
	// bearer tokens — see profile.AuthStarTokensPath's own doc comment for
	// why these specifically have no other recoverable copy). Checking for
	// the file rather than the --profile flag value covers `--addon`-style
	// composition too, without this command needing to know which profiles
	// generate credentials.
	if tokensPath, err := klusterprofile.AuthStarTokensPath(name); err == nil {
		if _, statErr := os.Stat(tokensPath); statErr == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "AuthStar operator tokens written to %s\n", tokensPath)
		}
	}
	return nil
}
