package cmd

import (
	"fmt"

	"github.com/bytepunx/kluster-lib/cluster"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Destroy a named cluster",
	RunE:  runDown,
}

func init() {
	rootCmd.AddCommand(downCmd)
	downCmd.Flags().String("name", "", "Cluster name")
}

func runDown(cmd *cobra.Command, _ []string) error {
	name, err := requireName(cmd)
	if err != nil {
		return err
	}
	p, err := resolveProvider()
	if err != nil {
		return err
	}
	c := cluster.New(p, nil, nil)
	fmt.Fprintf(cmd.OutOrStdout(), "Destroying cluster %q...\n", name)
	return c.Down(cmd.Context(), name)
}
