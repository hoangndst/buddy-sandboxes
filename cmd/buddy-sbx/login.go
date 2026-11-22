package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

//nolint:gocyclo // Login validates one credential source and persists it through the command lifecycle.
func newLoginCmd() *cobra.Command {
	var (
		flagLoginToken   string
		flagTokenFile    string
		flagLoginLocal   bool
		flagLoginContext string
	)

	cmd := &cobra.Command{
		Use:   "login [flags] [endpoint]",
		Short: "Authenticate with a Buddy Sandboxes gateway and configure local context",
		Long: `Login authenticates against a Buddy Sandboxes API using an administrator-issued token,
verifies identity via GET /api/v1/auth/whoami, and saves the connection
context to ~/.buddy/config.yaml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint := ""
			if len(args) > 0 {
				endpoint = args[0]
			} else if flagEndpoint != "" {
				endpoint = flagEndpoint
			} else if cliConfig != nil && cliConfig.ActiveContext() != nil && cliConfig.ActiveContext().Endpoint != "" {
				endpoint = cliConfig.ActiveContext().Endpoint
			} else {
				endpoint = defaultEndpoint
			}

			token := flagLoginToken
			if flagLoginLocal {
				endpoint = defaultEndpoint
			}
			if flagLoginLocal && flagTokenFile == "" && token == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("cannot locate token file: %w", err)
				}
				flagTokenFile = filepath.Join(home, ".buddy", "api-token")
			}
			if flagTokenFile != "" {
				if token != "" {
					return fmt.Errorf("--token-file cannot be combined with --token")
				}
				info, err := os.Stat(flagTokenFile)
				if err != nil {
					return fmt.Errorf("cannot read token file: %w", err)
				}
				if runtime.GOOS != osWindows && info.Mode().Perm()&0o077 != 0 {
					return fmt.Errorf("token file permissions must not allow group or other access")
				}
				data, err := os.ReadFile(flagTokenFile)
				if err != nil {
					return fmt.Errorf("cannot read token file: %w", err)
				}
				token = strings.TrimSpace(string(data))
			}

			if token == "" {
				if term.IsTerminal(int(os.Stdin.Fd())) {
					fmt.Print("Enter token: ")
					byteToken, err := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Println()
					if err != nil {
						return fmt.Errorf("failed to read token: %w", err)
					}
					token = strings.TrimSpace(string(byteToken))
				} else {
					return fmt.Errorf("token is required (use --token, --token-file, --local, or run in an interactive terminal)")
				}
			}

			if token == "" {
				return fmt.Errorf("token cannot be empty")
			}

			c, err := client.NewClient(client.Config{
				Endpoint: endpoint,
				Token:    token,
			})
			if err != nil {
				return fmt.Errorf("failed to initialize client: %w", err)
			}

			whoami, err := c.WhoAmI(cmd.Context())
			if err != nil {
				return fmt.Errorf("authentication verification failed: %w", err)
			}

			ctxName := flagLoginContext
			if ctxName == "" {
				ctxName = defaultContextName
			}

			if cliConfig == nil {
				cliConfig = &CLIConfig{}
			}

			found := false
			for i := range cliConfig.Contexts {
				if cliConfig.Contexts[i].Name == ctxName {
					cliConfig.Contexts[i].Endpoint = endpoint
					cliConfig.Contexts[i].Token = token
					found = true
					break
				}
			}
			if !found {
				cliConfig.Contexts = append(cliConfig.Contexts, ContextConfig{
					Name:     ctxName,
					Endpoint: endpoint,
					Token:    token,
				})
			}
			cliConfig.CurrentContext = ctxName

			if err := saveConfig(flagConfig, cliConfig); err != nil {
				return fmt.Errorf("failed to save configuration: %w", err)
			}

			fmt.Printf("Successfully logged in as %s to %s\n", whoami.User, endpoint)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagLoginToken, "token", "", "Authentication bearer token")
	cmd.Flags().StringVar(&flagTokenFile, "token-file", "", "Read the authentication bearer token from a private file")
	cmd.Flags().BoolVar(&flagLoginLocal, "local", false,
		"Use the loopback gateway and ~/.buddy/api-token")
	cmd.Flags().StringVar(&flagLoginContext, "context", "", "Context name to assign")

	return cmd
}
