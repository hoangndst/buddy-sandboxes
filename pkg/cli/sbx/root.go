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

// RootOptions configures root-level options and dependencies for buddy-sbx CLI commands.
type RootOptions struct {
	Endpoint   string
	Token      string
	ConfigFile string
	Config     *CLIConfig
}

// NewRootOptions returns a new RootOptions instance.
func NewRootOptions() *RootOptions {
	return &RootOptions{}
}

// GetClient initializes and returns an API client using the instance options and configuration.
func (o *RootOptions) GetClient() (*client.Client, error) {
	endpoint := o.Endpoint
	token := o.Token

	if o.Config != nil {
		if active := o.Config.ActiveContext(); active != nil {
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

func resolveOptions(opts []*RootOptions) *RootOptions {
	if len(opts) > 0 && opts[0] != nil {
		return opts[0]
	}
	return &RootOptions{
		Endpoint:   flagEndpoint,
		Token:      flagToken,
		ConfigFile: flagConfig,
		Config:     cliConfig,
	}
}

// ExitCodeError indicates that the CLI command finished with a specific process exit code.
type ExitCodeError struct {
	Code int
}

func (e ExitCodeError) Error() string {
	return ""
}

// NewRootCmd constructs and returns the root cobra.Command for buddy-sbx.
func NewRootCmd() *cobra.Command {
	return NewRootCmdWithOptions(NewRootOptions())
}

// NewRootCmdWithOptions constructs the root cobra.Command bound to the specified RootOptions.
func NewRootCmdWithOptions(opts *RootOptions) *cobra.Command {
	if opts == nil {
		opts = NewRootOptions()
	}

	cmd := &cobra.Command{
		Use:   "buddy-sbx",
		Short: "buddy-sbx is a developer CLI for managing isolated AI coding-agent sandboxes",
		Long: `buddy-sbx provides an ergonomic command-line tool for provisioning, executing,
and managing containerized agent environments in Kubernetes.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(opts.ConfigFile)
			if err != nil {
				return err
			}
			opts.Config = cfg
			cliConfig = cfg
			return nil
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.Endpoint, "endpoint", opts.Endpoint, "Buddy Gateway endpoint URL (overrides context)")
	flags.StringVar(&opts.Token, "token", opts.Token, "Authentication token (overrides context)")
	flags.StringVar(&opts.ConfigFile, "config", opts.ConfigFile, "Path to config file (default: ~/.buddy/config.yaml)")

	cmd.AddCommand(newGetCmd(opts))
	cmd.AddCommand(newDescribeCmd(opts))
	cmd.AddCommand(newDeleteCmd(opts))
	cmd.AddCommand(newCreateCmd(opts))
	cmd.AddCommand(newSetCmd(opts))
	cmd.AddCommand(newRunCmd(opts))
	cmd.AddCommand(newAttachCmd(opts))
	cmd.AddCommand(newLogsCmd(opts))
	cmd.AddCommand(newExecCmd(opts))
	cmd.AddCommand(newPortForwardCmd(opts))
	cmd.AddCommand(newCpCmd(opts))
	cmd.AddCommand(newSuspendCmd(opts))
	cmd.AddCommand(newResumeCmd(opts))
	cmd.AddCommand(newConfigCmd(opts))
	cmd.AddCommand(newLoginCmd(opts))

	return cmd
}
