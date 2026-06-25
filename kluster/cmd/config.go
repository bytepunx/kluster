package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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
