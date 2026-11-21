package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newExecCmd() *cobra.Command {
	var (
		flagInteractive bool
		flagTTY         bool
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

			var resizeChan chan apiv1.ResizeMessage
			if isTTY && term.IsTerminal(int(os.Stdin.Fd())) {
				oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
				if err == nil {
					defer func() {
						_ = term.Restore(int(os.Stdin.Fd()), oldState)
					}()
				}

				resizeChan = make(chan apiv1.ResizeMessage, 8)
				if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					resizeChan <- apiv1.ResizeMessage{Cols: w, Rows: h, Width: uint16(w), Height: uint16(h)}
				}

				sigChan := make(chan os.Signal, 2)
				signal.Notify(sigChan, syscall.SIGWINCH)
				defer signal.Stop(sigChan)

				go func() {
					for range sigChan {
						if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
							select {
							case resizeChan <- apiv1.ResizeMessage{Cols: w, Rows: h, Width: uint16(w), Height: uint16(h)}:
							default:
							}
						}
					}
				}()
			}

			exitCode, execErr := c.Exec(context.Background(), sandboxID, client.ExecOptions{
				Cmd:        execCmd,
				TTY:        isTTY,
				Stdin:      os.Stdin,
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

	return cmd
}
