package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newExecCmd() *cobra.Command {
	var (
		flagInteractive bool
		flagTTY         bool
		flagDetach      bool
	)

	cmd := &cobra.Command{
		Use:   "exec <sandbox-id> [flags] -- <command...>",
		Short: "Execute a command inside an active sandbox",
		Long:  "Exec runs a command inside an active sandbox, supporting interactive TTY streaming and signal forwarding.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			var execCmd []string
			if len(args) > 1 {
				execCmd = args[1:]
			} else {
				execCmd = []string{"/bin/sh"}
			}

			isTTY := flagTTY || (flagInteractive && term.IsTerminal(int(os.Stdin.Fd())))

			if flagDetach {
				sess, err := c.CreateSession(context.Background(), sandboxID, &apiv1.CreateSessionRequest{
					Command: execCmd,
					TTY:     &isTTY,
				})
				if err != nil {
					return fmt.Errorf("failed to create detached session: %w", err)
				}
				_, _ = fmt.Fprintf(
					os.Stdout,
					"Session %s started in background.\nTo attach: buddy-sbx attach %s %s\n",
					sess.ID,
					sandboxID,
					sess.ID,
				)
				return nil
			}

			var resizeChan chan apiv1.ResizeMessage
			if isTTY {
				rc, cleanup, err := setupTTY()
				if err == nil {
					defer cleanup()
					resizeChan = rc
				}
			}

			var stdin io.Reader
			if flagInteractive {
				stdin = os.Stdin
			}

			exitCode, execErr := c.Exec(context.Background(), sandboxID, client.ExecOptions{
				Cmd:        execCmd,
				TTY:        isTTY,
				Stdin:      stdin,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
				ResizeChan: resizeChan,
			})

			if execErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Exec error: %v\n", execErr)
			}

			// Propagate exit code to main so all deferred cleanup runs first (Finding 8)
			if exitCode != 0 {
				return ExitCodeError{Code: exitCode}
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&flagInteractive, "interactive", "i", false, "Keep STDIN open even if not attached")
	cmd.Flags().BoolVarP(&flagTTY, "tty", "t", false, "Allocate a pseudo-TTY")
	cmd.Flags().BoolVarP(&flagDetach, "detach", "d", false, "Run command in background supervisor session")

	return cmd
}
