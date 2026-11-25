package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <resource> [args]",
		Short: "Create a resource from flags or standard input",
		Long: `Create provisions new platform resources.

Supported resource types:
  - secret  (store a provider credential or Git token)
  - user    (create a platform user with a dedicated namespace)
  - token   (issue a login token for a user)

Examples:
  buddy-sbx create secret openai
  buddy-sbx create secret openai --url https://example.com/v1 --model o3-mini
  buddy-sbx create secret github --repo https://github.com/org/repo.git
  buddy-sbx create user "Alice Smith" --namespace ns-alice
  buddy-sbx create token usr_alice`,
	}

	cmd.AddCommand(newCreateSecretCmd())
	cmd.AddCommand(newCreateUserCmd())
	cmd.AddCommand(newCreateTokenCmd())

	return cmd
}

func newCreateSecretCmd() *cobra.Command {
	options := &secretSetOptions{}

	cmd := &cobra.Command{
		Use:   "secret <service>",
		Short: "Store a credential for a supported service",
		Long: `Create secret stores an encrypted provider API key or Git token.

Supported services:
  openai, anthropic, github`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretSet(cmd, args[0], options)
		},
	}

	cmd.Flags().StringVar(&options.sandbox, "sandbox", "", "Limit the key to one of your sandboxes")
	cmd.Flags().StringVar(&options.repository, "repo", "", "GitHub HTTPS repository URL for GitHub credentials")
	cmd.Flags().StringVar(&options.baseURL, "url", "", "Custom provider base URL")
	cmd.Flags().StringVar(&options.baseURL, "base-url", "", "Alias for --url")
	cmd.Flags().StringVar(&options.model, "model", "", "Default model identifier (e.g. gpt-4o, o3-mini)")
	cmd.Flags().StringVar(&options.authType, "auth-type", "", "Authentication type ('bearer' or 'api-key')")

	return cmd
}

func newCreateUserCmd() *cobra.Command {
	var namespace string

	cmd := &cobra.Command{
		Use:   "user <display-name>",
		Short: "Create a new user with an assigned namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(namespace) == "" {
				return fmt.Errorf("--namespace is required")
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			user, err := c.CreateUser(context.Background(), args[0], namespace)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created user %s (ID: %s, Namespace: %s, Role: %s)\n",
				user.DisplayName, user.ID, user.Namespace, user.Role)
			return err
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", "", "Target Kubernetes namespace for user sandboxes")
	_ = cmd.MarkFlagRequired("namespace")

	return cmd
}

func newCreateTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "token <user-id>",
		Short: "Issue a new login token for a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			token, err := c.IssueUserToken(context.Background(), args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Issued token for user %s.\nSave the token now; it is shown only once:\n\n%s\n\nToken ID: %s\nExpires:  %s\n",
				args[0], token.Token, token.ID, token.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST"))
			return nil
		},
	}
}
