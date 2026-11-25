package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <subcommand>",
		Short: "Configure specific features on resources",
		Long: `Set modifies properties of platform resources.

Supported subcommands:
  - role  (update a user's platform role)
  - user  (enable or disable a user account)`,
	}

	cmd.AddCommand(newSetRoleCmd())
	cmd.AddCommand(newSetUserCmd())

	return cmd
}

func newSetRoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "role <user-id> <admin|user>",
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

func newSetUserCmd() *cobra.Command {
	var (
		flagDisable bool
		flagEnable  bool
	)

	cmd := &cobra.Command{
		Use:   "user <user-id>",
		Short: "Enable or disable a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !flagDisable && !flagEnable {
				return fmt.Errorf("must specify either --disable or --enable")
			}
			if flagDisable && flagEnable {
				return fmt.Errorf("cannot specify both --disable and --enable")
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			disabled := flagDisable
			if err := c.SetUserDisabled(context.Background(), args[0], disabled); err != nil {
				return err
			}
			action := "Enabled"
			if disabled {
				action = "Disabled"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s user %s\n", action, args[0])
			return err
		},
	}

	cmd.Flags().BoolVar(&flagDisable, "disable", false, "Disable the user account")
	cmd.Flags().BoolVar(&flagEnable, "enable", false, "Enable the user account")

	return cmd
}
