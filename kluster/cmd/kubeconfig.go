package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

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
		contextName, err := mergeKubeconfig(data, cmd.ErrOrStderr())
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
// the context name that was activated. warn receives a notice whenever a
// same-named entry already in ~/.kube/config is being replaced with a
// different one — silently overwriting another cluster's identically-named
// context is the kind of mistake that's easy to make and hard to notice.
func mergeKubeconfig(data []byte, warn io.Writer) (string, error) {
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

	warnOnCollision(warn, existing.Clusters, incoming.Clusters, "cluster")
	warnOnCollision(warn, existing.AuthInfos, incoming.AuthInfos, "user")
	warnOnCollision(warn, existing.Contexts, incoming.Contexts, "context")

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
	if err := writeKubeconfigAtomic(*existing, kubeconfigPath); err != nil {
		return "", err
	}
	return incoming.CurrentContext, nil
}

func warnOnCollision[T any](w io.Writer, existing, incoming map[string]*T, kind string) {
	for name, inc := range incoming {
		if old, ok := existing[name]; ok && !reflect.DeepEqual(old, inc) {
			fmt.Fprintf(w, "warning: replacing existing %s %q in ~/.kube/config (it differed from the incoming one)\n", kind, name)
		}
	}
}

// writeKubeconfigAtomic writes to a temp file in the destination's directory
// and renames it into place, so a concurrent reader (or a crash mid-write)
// never observes a partially-written kubeconfig.
func writeKubeconfigAtomic(config clientcmdapi.Config, path string) error {
	content, err := clientcmd.Write(config)
	if err != nil {
		return fmt.Errorf("serialize kubeconfig: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kubeconfig-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp kubeconfig: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp kubeconfig: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp kubeconfig: %w", err)
	}
	return os.Rename(tmpPath, path)
}
