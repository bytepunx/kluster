package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bytepunx/kluster-lib/cluster"
	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var kubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Output or merge kubeconfig for a cluster",
	RunE:  runKubeconfig,
}

func init() {
	rootCmd.AddCommand(kubeconfigCmd)
	kubeconfigCmd.Flags().String("name", "", "Cluster name")
	kubeconfigCmd.Flags().String("output", "", "Write kubeconfig to file path (default: stdout)")
	kubeconfigCmd.Flags().Bool("merge", false, "Merge into ~/.kube/config and switch context")
}

func runKubeconfig(cmd *cobra.Command, _ []string) error {
	name, err := requireName(cmd)
	if err != nil {
		return err
	}
	p, err := resolveProvider()
	if err != nil {
		return err
	}
	c := cluster.New(p, nil, nil)
	data, err := c.Kubeconfig(cmd.Context(), name)
	if err != nil {
		return err
	}

	merge, _ := cmd.Flags().GetBool("merge")
	if merge {
		contextName, err := mergeKubeconfig(data)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Switched to context %q.\n", contextName)
		return nil
	}

	output, _ := cmd.Flags().GetString("output")
	if output != "" {
		return os.WriteFile(output, data, 0o600)
	}
	fmt.Fprint(cmd.OutOrStdout(), string(data))
	return nil
}

// mergeKubeconfig merges the given kubeconfig bytes into ~/.kube/config,
// switches the current context to the incoming cluster's context, and returns
// the context name that was activated.
func mergeKubeconfig(data []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	kubeconfigPath := filepath.Join(home, ".kube", "config")

	existing := clientcmdapi.NewConfig()
	if _, statErr := os.Stat(kubeconfigPath); statErr == nil {
		existing, err = clientcmd.LoadFromFile(kubeconfigPath)
		if err != nil {
			return "", fmt.Errorf("load existing kubeconfig: %w", err)
		}
	}

	incoming, err := clientcmd.Load(data)
	if err != nil {
		return "", fmt.Errorf("parse cluster kubeconfig: %w", err)
	}

	for k, v := range incoming.Clusters {
		existing.Clusters[k] = v
	}
	for k, v := range incoming.AuthInfos {
		existing.AuthInfos[k] = v
	}
	for k, v := range incoming.Contexts {
		existing.Contexts[k] = v
	}
	existing.CurrentContext = incoming.CurrentContext

	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
		return "", fmt.Errorf("create .kube directory: %w", err)
	}
	return incoming.CurrentContext, clientcmd.WriteToFile(*existing, kubeconfigPath)
}
