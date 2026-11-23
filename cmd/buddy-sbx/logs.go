package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newLogsCmd() *cobra.Command {
	var (
		flagFollow     bool
		flagTail       int
		flagTimestamps bool
		flagContainer  string
	)

	cmd := &cobra.Command{
		Use:   "logs <sandbox-id> [flags]",
		Short: "Fetch or stream logs of a sandbox",
		Long:  "Logs prints container output logs from the specified sandbox.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			opts := client.LogOptions{
				Follow:     flagFollow,
				TailLines:  flagTail,
				Timestamps: flagTimestamps,
				Container:  flagContainer,
			}

			return c.StreamLogs(context.Background(), sandboxID, opts, os.Stdout)
		},
	}

	cmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "Stream live log updates")
	cmd.Flags().IntVar(&flagTail, "tail", 100, "Number of lines to show from the end of the logs")
	cmd.Flags().BoolVarP(&flagTimestamps, "timestamps", "t", false, "Show timestamps")
	cmd.Flags().StringVar(&flagContainer, "container", "", "Container to read logs from (agent or git-init)")

	return cmd
}
