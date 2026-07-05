package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// initConfig sets up Viper to read from a kluster.yaml config file.
// Search order:
//  1. --config flag (explicit path)
//  2. ./kluster.yaml (current working directory — project-level)
//  3. $XDG_CONFIG_HOME/kluster/kluster.yaml (user-level defaults)
//
// Errors are silently ignored — the config file is optional.
func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("kluster")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		xdgCfg := os.Getenv("XDG_CONFIG_HOME")
		if xdgCfg == "" {
			home, _ := os.UserHomeDir()
			xdgCfg = filepath.Join(home, ".config")
		}
		viper.AddConfigPath(filepath.Join(xdgCfg, "kluster"))
	}
	_ = viper.ReadInConfig()
}

// stringFlag returns the value for a string flag, following the precedence:
//   flag (if explicitly set) > config file > flag default
func stringFlag(cmd *cobra.Command, flagName, configKey string) string {
	if cmd.Flags().Changed(flagName) {
		v, _ := cmd.Flags().GetString(flagName)
		return v
	}
	if v := viper.GetString(configKey); v != "" {
		return v
	}
	// Neither flag nor config — return the flag's own default.
	v, _ := cmd.Flags().GetString(flagName)
	return v
}

// stringSliceFlag returns the value for a string-array flag, following the
// same precedence. configKey should match the YAML key (e.g. "addons").
func stringSliceFlag(cmd *cobra.Command, flagName, configKey string) []string {
	if cmd.Flags().Changed(flagName) {
		v, _ := cmd.Flags().GetStringArray(flagName)
		return v
	}
	if viper.IsSet(configKey) {
		return viper.GetStringSlice(configKey)
	}
	v, _ := cmd.Flags().GetStringArray(flagName)
	return v
}

// requireName returns the cluster name from flags or config, and errors if
// neither source provides one.
func requireName(cmd *cobra.Command) (string, error) {
	name := stringFlag(cmd, "name", "name")
	if name == "" {
		return "", fmt.Errorf("cluster name is required: set --name or add 'name: <name>' to kluster.yaml")
	}
	return name, nil
}

// repoLocalConfigPath returns the absolute path ./kluster.yaml would resolve
// to from the current working directory.
func repoLocalConfigPath() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(wd, "kluster.yaml")
}

// nameFromRepoLocalConfig reports the config file path when the resolved
// cluster --name came from a "./kluster.yaml" in the current directory
// rather than an explicit --name flag or --config path. A repo you clone can
// plant such a file; silently acting on the name it supplies for a
// destructive command is the kind of mistake that's easy to make once and
// regret. Returns "" when --name was explicit, --config was explicit, or no
// config file was used at all.
func nameFromRepoLocalConfig(cmd *cobra.Command) string {
	if cmd.Flags().Changed("name") || cfgFile != "" {
		return ""
	}
	used := viper.ConfigFileUsed()
	if used == "" {
		return ""
	}
	abs, err := filepath.Abs(used)
	if err != nil || abs != repoLocalConfigPath() {
		return ""
	}
	return used
}

// repoLocalConfigNotice returns a one-line notice when viper loaded a
// "./kluster.yaml" from the current directory, so a cloned repo's config
// silently steering non-destructive commands (up's --trust-domain,
// --provider, etc.) is at least visible rather than invisible. Returns ""
// when no config file was used, or the one used isn't repo-local.
func repoLocalConfigNotice() string {
	used := viper.ConfigFileUsed()
	if used == "" {
		return ""
	}
	if abs, err := filepath.Abs(used); err != nil || abs != repoLocalConfigPath() {
		return ""
	}
	return fmt.Sprintf("Using config %s", used)
}

// confirm prompts on cmd's stdin/stdout and reports whether the user answered
// affirmatively. Any non-"y"/"yes" answer (including a read error or EOF,
// e.g. a non-interactive shell) is treated as "no" — a destructive command
// should never proceed on an ambiguous answer.
func confirm(cmd *cobra.Command, prompt string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}
