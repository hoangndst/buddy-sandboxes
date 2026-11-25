package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage detached supervisor sessions in sandboxes",
		Long:  "Commands to inspect, list, and terminate background supervisor sessions.",
	}

	cmd.AddCommand(newSessionListCmd())
	cmd.AddCommand(newSessionInspectCmd())
	cmd.AddCommand(newSessionRmCmd())

	return cmd
}

func newSessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <sandbox-id>",
		Short: "List all sessions inside a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			resp, err := c.ListSessions(context.Background(), sandboxID)
			if err != nil {
				return err
			}

			if len(resp.Sessions) == 0 {
				fmt.Println("No active or historical sessions found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "SESSION ID\tSTATE\tEXIT CODE\tCOMMAND\tCREATED")
			for _, s := range resp.Sessions {
				exitCodeStr := "-"
				if s.ExitCode != nil {
					exitCodeStr = fmt.Sprintf("%d", *s.ExitCode)
				}
				cmdStr := strings.Join(s.Command, " ")
				if len(cmdStr) > 40 {
					cmdStr = cmdStr[:37] + "..."
				}
				createdStr := s.CreatedAt.Format(time.RFC3339)
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.State, exitCodeStr, cmdStr, createdStr)
			}
			return w.Flush()
		},
	}
}

func newSessionInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <sandbox-id> <session-id>",
		Short: "Inspect session details and exit metadata",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			sessionID := args[1]
			resp, err := c.GetSession(context.Background(), sandboxID, sessionID)
			if err != nil {
				return err
			}

			data, err := json.MarshalIndent(resp, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

func newSessionRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <sandbox-id> <session-id>",
		Short: "Terminate and remove a supervisor session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			sessionID := args[1]
			if err := c.DeleteSession(context.Background(), sandboxID, sessionID); err != nil {
				return err
			}

			fmt.Printf("Session %s terminated.\n", sessionID)
			return nil
		},
	}
}

func newAttachCmd() *cobra.Command {
	var (
		flagTakeLease bool
		flagReadOnly  bool
	)

	cmd := &cobra.Command{
		Use:   "attach <sandbox-id> <session-id>",
		Short: "Attach to a detached supervisor session",
		Long: `Attach connects interactive terminal stdio to a background supervisor session.
Press 'Ctrl+]' at any time to detach without terminating the remote command.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			sessionID := args[1]

			isTTY := term.IsTerminal(int(os.Stdin.Fd()))
			var resizeChan chan apiv1.ResizeMessage
			if isTTY && !flagReadOnly {
				rc, cleanup, err := setupTTY()
				if err == nil {
					defer cleanup()
					resizeChan = rc
				}
			}

			exitCode, err := c.AttachSession(context.Background(), sandboxID, sessionID, client.AttachOptions{
				TakeLease:  flagTakeLease,
				ReadOnly:   flagReadOnly,
				Stdin:      os.Stdin,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
				ResizeChan: resizeChan,
			})

			if err != nil {
				if context.Canceled == err || strings.Contains(err.Error(), "context canceled") {
					fmt.Fprintf(os.Stderr, "\n[Detached from session %s]\n", sessionID)
					return nil
				}
				return err
			}

			if exitCode != 0 {
				return ExitCodeError{Code: exitCode}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&flagTakeLease, "take-lease", false, "Preempt existing write leaseholder and claim input focus")
	cmd.Flags().BoolVar(&flagReadOnly, "ro", false, "Attach in read-only observer mode")

	return cmd
}
