package sbx

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

const osWindows = "windows"

var (
	flagEndpoint string
	flagToken    string
	flagConfig   string

	cliConfig *CLIConfig
)

// ExitCodeError indicates that the CLI command finished with a specific process exit code.
type ExitCodeError struct {
	Code int
}

func (e ExitCodeError) Error() string {
	return ""
}

// NewRootCmd constructs and returns the root cobra.Command for buddy-sbx.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buddy-sbx",
		Short: "buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes",
		Long: `buddy-sbx provides an ergonomic command-line tool for provisioning, executing,
and managing containerized agent environments in Kubernetes.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(flagConfig)
			if err != nil {
				return err
			}
			cliConfig = cfg
			return nil
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&flagEndpoint, "endpoint", "", "Buddy Gateway endpoint URL (overrides context)")
	flags.StringVar(&flagToken, "token", "", "Authentication token (overrides context)")
	flags.StringVar(&flagConfig, "config", "", "Path to config file (default: ~/.buddy/config.yaml)")

	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newDescribeCmd())
	cmd.AddCommand(newDeleteCmd())
	cmd.AddCommand(newCreateCmd())
	cmd.AddCommand(newSetCmd())
	cmd.AddCommand(newRunCmd())
	cmd.AddCommand(newAttachCmd())
	cmd.AddCommand(newLogsCmd())
	cmd.AddCommand(newExecCmd())
	cmd.AddCommand(newPortForwardCmd())
	cmd.AddCommand(newCpCmd())
	cmd.AddCommand(newSuspendCmd())
	cmd.AddCommand(newResumeCmd())
	cmd.AddCommand(newConfigCmd())
	cmd.AddCommand(newLoginCmd())

	return cmd
}

var rootCmd *cobra.Command

func init() {
	rootCmd = NewRootCmd()
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
