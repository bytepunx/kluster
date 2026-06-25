package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/bytepunx/kluster-lib/cluster"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "List all kluster-managed clusters",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, _ []string) error {
	p, err := resolveProvider()
	if err != nil {
		return err
	}
	c := cluster.New(p, nil, nil)
	clusters, err := c.Status(cmd.Context())
	if err != nil {
		return err
	}
	if len(clusters) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No clusters found.")
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tRUNNING\tAGE")
	for _, cl := range clusters {
		running := "yes"
		if !cl.Running {
			running = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", cl.Name, running, cl.Age)
	}
	return w.Flush()
}
