package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

var (
	flagEndpoint  string
	flagToken     string
	flagNamespace string
	flagConfig    string

	cliConfig *CLIConfig
)

var rootCmd = &cobra.Command{
	Use:   "buddy-sbx",
	Short: "buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes",
	Long:  "buddy-sbx provides an ergonomic command-line tool for provisioning, executing, and managing containerized agent environments in Kubernetes.",
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
	rootCmd.PersistentFlags().StringVar(&flagEndpoint, "endpoint", "", "Buddy Gateway endpoint URL (overrides context)")
	rootCmd.PersistentFlags().StringVar(&flagToken, "token", "", "Authentication token (overrides context)")
	rootCmd.PersistentFlags().StringVar(&flagNamespace, "namespace", "", "Target Kubernetes namespace (overrides context)")
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "", "Path to buddy config file (default: ~/.buddy/config.yaml)")

	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newExecCmd())
	rootCmd.AddCommand(newLogsCmd())
	rootCmd.AddCommand(newListCmd())
	rootCmd.AddCommand(newRmCmd())
	rootCmd.AddCommand(newPortCmd())
	rootCmd.AddCommand(newConfigCmd())
}

func getClient() (*client.Client, error) {
	endpoint := flagEndpoint
	token := flagToken
	namespace := flagNamespace

	if cliConfig != nil {
		if active := cliConfig.ActiveContext(); active != nil {
			if endpoint == "" {
				endpoint = active.Endpoint
			}
			if token == "" {
				token = active.Token
			}
			if namespace == "" {
				namespace = active.DefaultNamespace
			}
		}
	}

	if endpoint == "" {
		endpoint = "http://localhost:8080"
	}
	if namespace == "" {
		namespace = "default"
	}

	c, err := client.NewClient(client.Config{
		Endpoint:  endpoint,
		Token:     token,
		Namespace: namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize client: %w", err)
	}

	return c, nil
}
