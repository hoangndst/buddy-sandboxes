package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List active sandboxes",
		Long:    "List displays all sandboxes provisioned in the active tenant namespace.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			resp, err := c.ListSandboxes(context.Background())
			if err != nil {
				return err
			}

			if len(resp.Items) == 0 {
				fmt.Println("No active sandboxes found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tIMAGE\tPHASE\tAGE\tNETWORK\tMCP\tLABELS")

			now := time.Now()
			for _, sbx := range resp.Items {
				age := now.Sub(sbx.CreatedAt).Round(time.Second).String()
				netStr := "Web"
				if sbx.Network != nil {
					if len(sbx.Network.AllowedDomains) > 0 {
						netStr = fmt.Sprintf("Allowlist (%d)", len(sbx.Network.AllowedDomains))
					} else if !sbx.Network.AllowInternetWeb {
						netStr = "Isolated"
					}
				}
				var mcpParts []string
				for _, s := range sbx.MCPServers {
					mcpParts = append(mcpParts, fmt.Sprintf("%s:%d", s.Name, s.Port))
				}
				mcpStr := strings.Join(mcpParts, ",")
				if mcpStr == "" {
					mcpStr = "-"
				}
				var labelParts []string
				for k, v := range sbx.Labels {
					if !strings.HasPrefix(k, "app.kubernetes.io/") && !strings.HasPrefix(k, "buddy.io/") {
						labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
					}
				}
				labelStr := strings.Join(labelParts, ",")
				if labelStr == "" {
					labelStr = "-"
				}

				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					sbx.ID,
					sbx.Image,
					sbx.Phase,
					age,
					netStr,
					mcpStr,
					labelStr,
				)
			}

			return w.Flush()
		},
	}

	return cmd
}
