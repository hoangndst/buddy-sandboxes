package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <sandbox-id>...",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete one or more sandboxes",
		Long:    "Rm deletes the specified sandboxes and triggers cascading resource teardown in Kubernetes.",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			hasErrors := false
			for _, id := range args {
				if err := c.DeleteSandbox(context.Background(), id); err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "Error deleting sandbox %s: %v\n", id, err)
					hasErrors = true
				} else {
					fmt.Println(id)
				}
			}

			if hasErrors {
				os.Exit(1)
			}
			return nil
		},
	}

	return cmd
}
