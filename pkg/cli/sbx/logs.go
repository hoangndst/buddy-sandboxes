package sbx

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newLogsCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	var (
		flagFollow     bool
		flagTail       int
		flagTimestamps bool
		flagContainer  string
		flagSession    string
	)

	cmd := &cobra.Command{
		Use:   "logs <sandbox-id> [flags]",
		Short: "Fetch or stream logs of a sandbox or supervisor session",
		Long:  `Logs prints container output logs or supervisor session logs from the specified sandbox.`,
		Example: `  # Fetch logs from a sandbox
  buddy-sbx logs my-box

  # Follow logs in real time
  buddy-sbx logs my-box -f

  # Fetch logs from a supervisor session
  buddy-sbx logs my-box --session sess-9b9ad3488cc2

  # Tail the last 50 lines with timestamps
  buddy-sbx logs my-box --tail 50 --timestamps`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opt.GetClient()
			if err != nil {
				return err
			}

			target := args[0]
			for _, prefix := range []string{"executions/", "execution/", "execs/", "exec/"} {
				if strings.HasPrefix(target, prefix) {
					execID := strings.TrimPrefix(target, prefix)
					logText, err := c.GetExecutionLogs(cmd.Context(), execID)
					if err != nil {
						return err
					}
					_, err = fmt.Fprint(cmd.OutOrStdout(), logText)
					return err
				}
			}

			sandboxID := target
			sessionID := flagSession

			if slashIdx := strings.IndexByte(target, '/'); slashIdx >= 0 {
				sandboxID = target[:slashIdx]
				sessionID = target[slashIdx+1:]
			}

			if sessionID != "" {
				_, err := c.AttachSession(cmd.Context(), sandboxID, sessionID, client.AttachOptions{
					ReadOnly: true,
					NoFollow: !flagFollow,
					Stdout:   cmd.OutOrStdout(),
					Stderr:   cmd.ErrOrStderr(),
				})
				return err
			}

			opts := client.LogOptions{
				Follow:     flagFollow,
				TailLines:  flagTail,
				Timestamps: flagTimestamps,
				Container:  flagContainer,
			}

			return c.StreamLogs(cmd.Context(), sandboxID, opts, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "Stream live log updates")
	cmd.Flags().IntVar(&flagTail, "tail", 100, "Number of lines to show from the end of the logs")
	cmd.Flags().BoolVarP(&flagTimestamps, "timestamps", "t", false, "Show timestamps")
	cmd.Flags().StringVar(&flagContainer, "container", "", "Container to read logs from (agent or git-init)")
	cmd.Flags().StringVarP(&flagSession, "session", "s", "", "Read logs from a specific supervisor session")

	return cmd
}
