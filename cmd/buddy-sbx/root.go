package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

var (
	flagEndpoint string
	flagToken    string
	flagConfig   string

	cliConfig *CLIConfig
)

var rootCmd = &cobra.Command{
	Use:   "buddy-sbx",
	Short: "buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes",
	Long: `buddy-sbx provides an ergonomic command-line tool for provisioning, executing,
and managing containerized agent environments in Kubernetes.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig(flagConfig)
		if err != nil {
			return err
		}
		cliConfig = cfg
		return nil
	},
}

func init() {
	flags := rootCmd.PersistentFlags()
	flags.StringVar(&flagEndpoint, "endpoint", "", "Buddy Gateway endpoint URL (overrides context)")
	flags.StringVar(&flagToken, "token", "", "Authentication token (overrides context)")
	flags.StringVar(&flagConfig, "config", "", "Path to config file (default: ~/.buddy/config.yaml)")

	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newExecCmd())
	rootCmd.AddCommand(newLogsCmd())
	rootCmd.AddCommand(newListCmd())
	rootCmd.AddCommand(newRmCmd())
	rootCmd.AddCommand(newPortCmd())
	rootCmd.AddCommand(newConfigCmd())
	rootCmd.AddCommand(newLoginCmd())
	rootCmd.AddCommand(newCpCmd())
	rootCmd.AddCommand(newSuspendCmd())
	rootCmd.AddCommand(newResumeCmd())
	rootCmd.AddCommand(newSecretCmd())
	rootCmd.AddCommand(newAdminCmd())
}

func getClient() (*client.Client, error) {
	endpoint := flagEndpoint
	token := flagToken

	if cliConfig != nil {
		if active := cliConfig.ActiveContext(); active != nil {
			if endpoint == "" {
				endpoint = active.Endpoint
			}
			if token == "" {
				token = active.Token
			}
		}
	}

	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	c, err := client.NewClient(client.Config{
		Endpoint: endpoint,
		Token:    token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize client: %w", err)
	}

	return c, nil
}
