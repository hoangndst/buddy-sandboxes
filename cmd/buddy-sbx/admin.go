package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Manage platform users and tokens (requires administrator role)",
	}
	cmd.AddCommand(newAdminUsersCmd(), newAdminTokensCmd(), newAdminSandboxesCmd())
	return cmd
}

func newAdminUsersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage platform users",
	}
	cmd.AddCommand(
		newAdminUsersListCmd(),
		newAdminUsersCreateCmd(),
		newAdminUsersDisableCmd(),
		newAdminUsersEnableCmd(),
		newAdminUsersSetRoleCmd(),
	)
	return cmd
}

func newAdminUsersListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List all users",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			users, err := c.ListUsers(context.Background())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "ID\tDISPLAY NAME\tNAMESPACE\tROLE\tSTATUS\tCREATED"); err != nil {
				return err
			}
			for _, u := range users {
				status := "active"
				if u.Disabled {
					status = "disabled"
				}
				created := u.Created.Local().Format("2006-01-02 15:04:05 MST")
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					u.ID, u.DisplayName, u.Namespace, u.Role, status, created); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
}

func newAdminUsersCreateCmd() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:   "create <display-name>",
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

func newAdminUsersDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <user-id>",
		Short: "Disable a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.SetUserDisabled(context.Background(), args[0], true); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Disabled user %s\n", args[0])
			return err
		},
	}
}

func newAdminUsersEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <user-id>",
		Short: "Enable a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.SetUserDisabled(context.Background(), args[0], false); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Enabled user %s\n", args[0])
			return err
		},
	}
}

func newAdminUsersSetRoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-role <user-id> <admin|user>",
		Short: "Update a user's role",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			role := strings.ToLower(args[1])
			if role != "admin" && role != "user" {
				return fmt.Errorf("invalid role %q; choose admin or user", args[1])
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.SetUserRole(context.Background(), args[0], role); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Updated role for user %s to %s\n", args[0], role)
			return err
		},
	}
}

func newAdminTokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage user login tokens",
	}
	cmd.AddCommand(newAdminTokensIssueCmd(), newAdminTokensRevokeCmd())
	return cmd
}

func newAdminTokensIssueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "issue <user-id>",
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

func newAdminTokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <user-id> <token-id>",
		Short: "Revoke an issued login token",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.RevokeUserToken(context.Background(), args[0], args[1]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Revoked token %s for user %s\n", args[1], args[0])
			return err
		},
	}
}

func newAdminSandboxesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandboxes",
		Short: "Inspect and manage sandboxes across tenant namespaces",
	}
	cmd.AddCommand(newAdminSandboxesListCmd(), newAdminSandboxesRemoveCmd())
	return cmd
}

func newAdminSandboxesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls <user-id>",
		Aliases: []string{"list"},
		Short:   "List sandboxes belonging to a specific user",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			sandboxes, err := c.ListUserSandboxes(context.Background(), args[0])
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "NAME\tPHASE\tIMAGE\tNAMESPACE\tAGE"); err != nil {
				return err
			}
			now := time.Now()
			for _, s := range sandboxes {
				age := now.Sub(s.CreatedAt).Round(time.Second).String()
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					s.Name, s.Phase, s.Image, s.Namespace, age); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
}

func newAdminSandboxesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <user-id> <sandbox-name>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a sandbox belonging to a specific user",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.DeleteUserSandbox(context.Background(), args[0], args[1]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed sandbox %s for user %s\n", args[1], args[0])
			return err
		},
	}
}
