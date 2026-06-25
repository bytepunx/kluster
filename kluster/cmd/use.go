package cmd

import (
	"fmt"

	"github.com/bytepunx/kluster-lib/cluster"
	"github.com/spf13/cobra"
)

var useCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Merge cluster kubeconfig into ~/.kube/config and switch context",
	Long: `Merges the cluster's kubeconfig into ~/.kube/config and sets it as the
active context so that kubectl and other Kubernetes tooling work immediately
without any additional configuration.

To switch back to a different context afterwards:
  kubectl config use-context <other-context>`,
	Args: cobra.ExactArgs(1),
	RunE: runUse,
}

func init() {
	rootCmd.AddCommand(useCmd)
}

func runUse(cmd *cobra.Command, args []string) error {
	name := args[0]

	p, err := resolveProvider()
	if err != nil {
		return err
	}
	c := cluster.New(p, nil, nil)

	data, err := c.Kubeconfig(cmd.Context(), name)
	if err != nil {
		return err
	}

	contextName, err := mergeKubeconfig(data)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Switched to context %q.\n", contextName)
	return nil
}
