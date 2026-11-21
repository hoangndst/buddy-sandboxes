package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newLoginCmd() *cobra.Command {
	var (
		flagLoginToken   string
		flagLoginLocal   bool
		flagLoginContext string
	)

	cmd := &cobra.Command{
		Use:   "login [flags] [endpoint]",
		Short: "Authenticate with a Buddy Sandboxes gateway and configure local context",
		Long: `Login authenticates against a Buddy Sandboxes gateway daemon using a pre-shared token,
verifies identity and assigned namespace via GET /api/v1/auth/whoami, and saves the connection
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
				if token == "" {
					token = "dev-token-secret"
				}
				if endpoint == "" {
					endpoint = defaultEndpoint
				}
			} else if token == "" {
				if term.IsTerminal(int(os.Stdin.Fd())) {
					fmt.Print("Enter token: ")
					byteToken, err := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Println()
					if err != nil {
						return fmt.Errorf("failed to read token: %w", err)
					}
					token = strings.TrimSpace(string(byteToken))
				} else {
					return fmt.Errorf("token is required (use --token, --local, or run in an interactive terminal)")
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
				if whoami.Namespace != "" && whoami.Namespace != defaultNamespaceName {
					ctxName = whoami.Namespace
				} else {
					ctxName = defaultContextName
				}
			}

			if cliConfig == nil {
				cliConfig = &CLIConfig{}
			}

			found := false
			for i := range cliConfig.Contexts {
				if cliConfig.Contexts[i].Name == ctxName {
					cliConfig.Contexts[i].Endpoint = endpoint
					cliConfig.Contexts[i].Token = token
					cliConfig.Contexts[i].DefaultNamespace = whoami.Namespace
					found = true
					break
				}
			}
			if !found {
				cliConfig.Contexts = append(cliConfig.Contexts, ContextConfig{
					Name:             ctxName,
					Endpoint:         endpoint,
					Token:            token,
					DefaultNamespace: whoami.Namespace,
				})
			}
			cliConfig.CurrentContext = ctxName

			if err := saveConfig(flagConfig, cliConfig); err != nil {
				return fmt.Errorf("failed to save configuration: %w", err)
			}

			fmt.Printf("Successfully logged in as %s (namespace: %s) to %s\n", whoami.User, whoami.Namespace, endpoint)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagLoginToken, "token", "", "Authentication bearer token")
	cmd.Flags().BoolVar(&flagLoginLocal, "local", false,
		"Authenticate against local development gateway with default credentials")
	cmd.Flags().StringVar(&flagLoginContext, "context", "", "Context name to assign (defaults to namespace or default)")

	return cmd
}
