package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newAttachCmd() *cobra.Command {
	var (
		flagTakeLease bool
		flagReadOnly  bool
		flagSession   string
	)

	cmd := &cobra.Command{
		Use:   "attach <sandbox-id> [session-id]",
		Short: "Attach to a detached supervisor session",
		Long: `Attach connects interactive terminal stdio to a background supervisor session.
If the sandbox has only one active session, the session ID is automatically selected.

Press 'Ctrl+]' at any time to detach without terminating the remote command.

Examples:
  buddy-sbx attach my-box
  buddy-sbx attach my-box sess-9b9ad3488cc2
  buddy-sbx attach my-box -s sess-9b9ad3488cc2
  buddy-sbx attach my-box/sess-9b9ad3488cc2`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			var sandboxID, sessionID string

			if len(args) == 2 {
				sandboxID = args[0]
				sessionID = args[1]
			} else {
				target := args[0]
				if slashIdx := strings.IndexByte(target, '/'); slashIdx >= 0 {
					sandboxID = target[:slashIdx]
					sessionID = target[slashIdx+1:]
				} else {
					sandboxID = target
					sessionID = flagSession
				}
			}

			if sessionID == "" {
				listResp, err := c.ListSessions(context.Background(), sandboxID)
				if err != nil {
					return fmt.Errorf("lookup sessions for sandbox %q: %w", sandboxID, err)
				}
				if len(listResp.Sessions) == 0 {
					return fmt.Errorf("no active sessions found in sandbox %q", sandboxID)
				}
				if len(listResp.Sessions) == 1 {
					sessionID = listResp.Sessions[0].ID
				} else {
					var active []apiv1.SessionResponse
					for _, s := range listResp.Sessions {
						if s.State == apiv1.SessionStateRunning || s.State == apiv1.SessionStatePending {
							active = append(active, s)
						}
					}
					if len(active) == 1 {
						sessionID = active[0].ID
					} else {
						sb := &strings.Builder{}
						fmt.Fprintf(sb, "multiple sessions found in sandbox %q:\n", sandboxID)
						for _, s := range listResp.Sessions {
							fmt.Fprintf(sb, "  - %s (%s): %s\n", s.ID, s.State, strings.Join(s.Command, " "))
						}
						fmt.Fprintf(sb, "Specify session with --session <id> or buddy-sbx attach %s <session-id>", sandboxID)
						return fmt.Errorf("%s", sb.String())
					}
				}
			}

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

	cmd.Flags().StringVarP(&flagSession, "session", "s", "", "Session ID to attach to")
	cmd.Flags().BoolVar(&flagTakeLease, "take-lease", false, "Preempt existing write leaseholder and claim input focus")
	cmd.Flags().BoolVar(&flagReadOnly, "read-only", false, "Attach in read-only observer mode")

	return cmd
}
