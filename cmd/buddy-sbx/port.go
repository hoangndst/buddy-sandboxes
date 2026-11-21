package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newPortCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "port <sandbox-id> [local:]remote",
		Short: "Forward a local workstation port to a sandbox port",
		Long: `Port opens a local TCP listener on your machine and bridges connections 
directly to the specified sandbox port over a secure WebSocket tunnel.

Examples:
  buddy-sbx port sbx-1234 3000:3000
  buddy-sbx port sbx-1234 8080`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			localPort, remotePort, err := parsePortSpec(args[1])
			if err != nil {
				return err
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigChan
				fmt.Println("\nStopping port forward...")
				cancel()
			}()

			readyChan := make(chan struct{})
			go func() {
				<-readyChan
				fmt.Printf("Forwarding 127.0.0.1:%d -> %d on sandbox %s (Ctrl+C to stop)\n", localPort, remotePort, sandboxID)
			}()

			return c.PortForward(ctx, sandboxID, client.PortForwardOptions{
				LocalPort:  localPort,
				RemotePort: remotePort,
				ReadyChan:  readyChan,
			})
		},
	}

	return cmd
}
