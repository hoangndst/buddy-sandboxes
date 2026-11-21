package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func newSuspendCmd() *cobra.Command {
	return newSuspensionCmd("suspend", true)
}

func newResumeCmd() *cobra.Command {
	return newSuspensionCmd("resume", false)
}

func newSuspensionCmd(operation string, suspend bool) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   operation + " <sandbox-id>",
		Short: operation + " a PVC-backed sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			var latest *apiv1.SandboxResponse
			if suspend {
				latest, err = c.SuspendSandbox(cmd.Context(), args[0])
			} else {
				latest, err = c.ResumeSandbox(cmd.Context(), args[0])
			}
			if err != nil {
				return err
			}

			waitCtx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return waitForSuspension(waitCtx, c, args[0], operation, suspend, latest, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "Maximum time to wait for the sandbox transition")
	return cmd
}

func waitForSuspension(
	ctx context.Context,
	c sandboxStateClient,
	id, operation string,
	suspend bool,
	latest *apiv1.SandboxResponse,
	stdout, stderr io.Writer,
) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, err := c.GetSandbox(ctx, id)
		if err == nil {
			latest = state
			if suspensionOperationComplete(state, suspend) {
				_, _ = fmt.Fprintf(stdout, "%s %s\n", id, state.SuspensionPhase)
				return nil
			}
			if state.Phase == "Succeeded" || state.Phase == "Failed" {
				return fmt.Errorf("sandbox %s reached terminal phase %q before %s completed", id, state.Phase, operation)
			}
		} else if ctx.Err() == nil {
			return err
		}

		select {
		case <-ctx.Done():
			if latest != nil {
				_, _ = fmt.Fprintf(stderr,
					"latest sandbox status: phase=%s suspensionPhase=%s suspend=%t pod=%s\n",
					latest.Phase, latest.SuspensionPhase, latest.Suspend, latest.PodName,
				)
			} else {
				_, _ = fmt.Fprintln(stderr, "sandbox state could not be read after the wait timed out")
			}
			return fmt.Errorf("timed out waiting for sandbox %s to %s; desired state remains recorded", id, operation)
		case <-ticker.C:
		}
	}
}

type sandboxStateClient interface {
	GetSandbox(context.Context, string) (*apiv1.SandboxResponse, error)
}

func suspensionOperationComplete(sandbox *apiv1.SandboxResponse, suspend bool) bool {
	if suspend {
		return sandbox.SuspensionPhase == "Suspended"
	}
	return sandbox.SuspensionPhase == "None" && sandbox.Phase == "Running"
}
