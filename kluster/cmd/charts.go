package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/bytepunx/kluster-lib/versions"
	"github.com/spf13/cobra"
)

var chartsCmd = &cobra.Command{
	Use:   "charts",
	Short: "Manage pinned Helm chart versions",
}

var chartsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pinned chart versions",
	RunE:  runChartsList,
}

var chartsUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Fetch the latest chart versions and update the versions file",
	RunE:  runChartsUpdate,
}

func init() {
	rootCmd.AddCommand(chartsCmd)
	chartsCmd.AddCommand(chartsListCmd)
	chartsCmd.AddCommand(chartsUpdateCmd)
}

func runChartsList(cmd *cobra.Command, _ []string) error {
	f, err := versions.Load()
	if os.IsNotExist(err) {
		fmt.Fprintln(cmd.OutOrStdout(), "No chart versions file found.")
		fmt.Fprintf(cmd.OutOrStdout(), "Run 'kluster charts update' to fetch versions, or 'kluster up' to resolve automatically.\n")
		return nil
	}
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ADDON\tVERSION")
	for _, e := range versions.Catalog {
		fmt.Fprintf(w, "%s\t%s\n", e.Addon, f.Charts[e.Addon])
	}
	_ = w.Flush()
	fmt.Fprintf(cmd.OutOrStdout(), "\nUpdated %s\n", f.Updated.Format("2006-01-02 15:04 UTC"))
	return nil
}

func runChartsUpdate(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Checking for updated chart versions...")

	existing := versions.File{Charts: make(map[string]string)}
	if f, err := versions.Load(); err == nil {
		existing = f
	}

	latest, err := versions.Fetch(cmd.Context())
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	updated := 0
	for _, e := range versions.Catalog {
		prev := existing.Charts[e.Addon]
		next := latest.Charts[e.Addon]
		if prev != "" && prev != next {
			fmt.Fprintf(w, "  %s\t%s  →  %s\n", e.Addon, prev, next)
			updated++
		} else {
			fmt.Fprintf(w, "  %s\t%s\n", e.Addon, next)
		}
	}
	_ = w.Flush()

	if err := versions.Save(latest); err != nil {
		return err
	}

	fmt.Fprintln(out)
	if updated == 0 {
		fmt.Fprintln(out, "All charts are up to date.")
	} else {
		fmt.Fprintf(out, "%d chart(s) updated.\n", updated)
	}
	fmt.Fprintf(out, "Saved to %s\n", versions.Path())
	return nil
}
