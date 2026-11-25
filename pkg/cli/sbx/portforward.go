package sbx

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newPortForwardCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	cmd := &cobra.Command{
		Use:   "port-forward <sandbox-id> [local:]remote",
		Short: "Forward a local workstation port to a sandbox port",
		Long: `Port-forward opens a local TCP listener on your machine and bridges connections 
directly to the specified sandbox port over a secure WebSocket tunnel.`,
		Example: `  # Forward local port 3000 to remote port 3000
  buddy-sbx port-forward sbx-1234 3000:3000

  # Forward a local port to remote port 8080
  buddy-sbx port-forward sbx-1234 8080`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opt.GetClient()
			if err != nil {
				return err
			}

			sandboxID := args[0]
			localPort, remotePort, err := parsePortSpec(args[1])
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			readyChan := make(chan struct{})
			go func() {
				select {
				case <-readyChan:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(),
						"Forwarding 127.0.0.1:%d -> %d on sandbox %s (Ctrl+C to stop)\n", localPort, remotePort, sandboxID)
				case <-ctx.Done():
				}
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
