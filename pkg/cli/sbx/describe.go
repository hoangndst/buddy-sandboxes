package sbx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newDescribeCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	var (
		flagSandbox string
		flagOutput  string
	)

	cmd := &cobra.Command{
		Use:   "describe <resource> <name>",
		Short: "Show details of a specific resource",
		Long: `Describe displays detailed metadata, configuration, and runtime status of a resource.

Supported resource types:
  - sandboxes (aliases: sandbox, sbx)
  - sessions  (aliases: session, sess)
  - users     (aliases: user)`,
		Example: `  # Describe a sandbox
  buddy-sbx describe sandbox my-box

  # Describe a sandbox using shorthand
  buddy-sbx describe sbx my-box

  # Describe a supervisor session
  buddy-sbx describe session sess-9b9ad3488cc2 -s my-box

  # Describe a session in path format
  buddy-sbx describe sess my-box/sess-9b9ad3488cc2

  # Describe a user account
  buddy-sbx describe user usr_alice`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer func() {
				flagSandbox = ""
				flagOutput = ""
			}()

			resourceType := strings.ToLower(args[0])

			c, err := opt.GetClient()
			if err != nil {
				return err
			}

			switch resourceType {
			case resourceSandboxes, resourceSandbox, resourceSbx:
				if len(args) != 2 {
					return fmt.Errorf("expected exactly one sandbox name")
				}
				return runDescribeSandbox(cmd, c, args[1], flagOutput)

			case resourceSessions, resourceSession, resourceSess:
				var sbxID, sessID string
				if len(args) == 2 {
					target := args[1]
					if slashIdx := strings.IndexByte(target, '/'); slashIdx >= 0 {
						sbxID = target[:slashIdx]
						sessID = target[slashIdx+1:]
					} else {
						sessID = target
						sbxID = flagSandbox
					}
				} else if len(args) == 3 {
					sbxID = args[1]
					sessID = args[2]
				}
				if sessID == "" {
					return fmt.Errorf("session ID is required")
				}
				if sbxID == "" {
					return fmt.Errorf("session description requires --sandbox <name> or <sandbox>/<session>")
				}
				return runDescribeSession(cmd, c, sbxID, sessID, flagOutput)

			case resourceUsers, resourceUser:
				if len(args) != 2 {
					return fmt.Errorf("expected exactly one user ID or username")
				}
				return runDescribeUser(cmd, c, args[1], flagOutput)

			default:
				return fmt.Errorf("unknown resource type %q; choose from: sandboxes (sbx), sessions (sess), users", args[0])
			}
		},
	}

	cmd.Flags().StringVarP(&flagSandbox, "sandbox", "s", "", "Target sandbox for child resources (sessions)")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "", "Output format (json)")

	return cmd
}

func runDescribeSandbox(cmd *cobra.Command, c *client.Client, name, output string) error {
	sbx, err := c.GetSandbox(context.Background(), name)
	if err != nil {
		return err
	}

	if output == outputJSON {
		data, err := json.MarshalIndent(sbx, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Name:         %s\n", sbx.Name)
	if sbx.ID != "" && sbx.ID != sbx.Name {
		_, _ = fmt.Fprintf(out, "ID:           %s\n", sbx.ID)
	}
	if sbx.Namespace != "" {
		_, _ = fmt.Fprintf(out, "Namespace:    %s\n", sbx.Namespace)
	}
	_, _ = fmt.Fprintf(out, "Phase:        %s\n", sbx.Phase)
	_, _ = fmt.Fprintf(out, "Image:        %s\n", sbx.Image)
	if sbx.PodName != "" {
		_, _ = fmt.Fprintf(out, "Pod Name:     %s\n", sbx.PodName)
	}
	if sbx.PodIP != "" {
		_, _ = fmt.Fprintf(out, "Pod IP:       %s\n", sbx.PodIP)
	}
	if !sbx.CreatedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Created:      %s\n", sbx.CreatedAt.Local().Format(time.RFC1123))
	}
	if sbx.SuspensionPhase != "" {
		_, _ = fmt.Fprintf(out, "Suspension:   %s\n", sbx.SuspensionPhase)
	}
	if sbx.FailureReason != "" {
		_, _ = fmt.Fprintf(out, "FailureReason: %s\n", sbx.FailureReason)
	}
	if sbx.FailureMessage != "" {
		_, _ = fmt.Fprintf(out, "FailureMessage: %s\n", sbx.FailureMessage)
	}

	if len(sbx.Labels) > 0 {
		_, _ = fmt.Fprintln(out, "Labels:")
		for k, v := range sbx.Labels {
			_, _ = fmt.Fprintf(out, "  %s=%s\n", k, v)
		}
	}

	if sbx.Network != nil {
		_, _ = fmt.Fprintln(out, "Network:")
		_, _ = fmt.Fprintf(out, "  Internet Web: %t\n", sbx.Network.AllowInternetWeb)
		if len(sbx.Network.AllowedDomains) > 0 {
			_, _ = fmt.Fprintf(out, "  Allow Domains: %s\n", strings.Join(sbx.Network.AllowedDomains, ", "))
		}
	}

	if len(sbx.MCPServers) > 0 {
		_, _ = fmt.Fprintln(out, "MCP Servers:")
		for _, m := range sbx.MCPServers {
			_, _ = fmt.Fprintf(out, "  - Name: %s (Port: %d, Image: %s)\n", m.Name, m.Port, m.Image)
		}
	}

	return nil
}

func runDescribeSession(cmd *cobra.Command, c *client.Client, sandboxID, sessionID, output string) error {
	sess, err := c.GetSession(context.Background(), sandboxID, sessionID)
	if err != nil {
		return err
	}

	if output == outputJSON {
		data, err := json.MarshalIndent(sess, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Session ID:   %s\n", sess.ID)
	_, _ = fmt.Fprintf(out, "Sandbox:      %s\n", sess.SandboxName)
	if sess.Namespace != "" {
		_, _ = fmt.Fprintf(out, "Namespace:    %s\n", sess.Namespace)
	}
	_, _ = fmt.Fprintf(out, "State:        %s\n", sess.State)
	if sess.ExitCode != nil {
		_, _ = fmt.Fprintf(out, "Exit Code:    %d\n", *sess.ExitCode)
	}
	_, _ = fmt.Fprintf(out, "Command:      %s\n", strings.Join(sess.Command, " "))
	if sess.WorkingDir != "" {
		_, _ = fmt.Fprintf(out, "WorkingDir:   %s\n", sess.WorkingDir)
	}
	_, _ = fmt.Fprintf(out, "TTY:          %t\n", sess.TTY)
	if !sess.CreatedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Created:      %s\n", sess.CreatedAt.Local().Format(time.RFC1123))
	}
	if sess.StartedAt != nil && !sess.StartedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Started:      %s\n", sess.StartedAt.Local().Format(time.RFC1123))
	}
	if sess.CompletedAt != nil && !sess.CompletedAt.IsZero() {
		_, _ = fmt.Fprintf(out, "Completed:    %s\n", sess.CompletedAt.Local().Format(time.RFC1123))
	}

	return nil
}

func runDescribeUser(cmd *cobra.Command, c *client.Client, userID, output string) error {
	users, err := c.ListUsers(context.Background())
	if err != nil {
		return err
	}

	for _, u := range users {
		if u.ID == userID || strings.EqualFold(u.DisplayName, userID) {
			if output == outputJSON {
				data, err := json.MarshalIndent(u, "", "  ")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
				return err
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "User ID:      %s\n", u.ID)
			_, _ = fmt.Fprintf(out, "Display Name: %s\n", u.DisplayName)
			_, _ = fmt.Fprintf(out, "Namespace:    %s\n", u.Namespace)
			_, _ = fmt.Fprintf(out, "Role:         %s\n", u.Role)
			status := "active"
			if u.Disabled {
				status = "disabled"
			}
			_, _ = fmt.Fprintf(out, "Status:       %s\n", status)
			if !u.Created.IsZero() {
				_, _ = fmt.Fprintf(out, "Created:      %s\n", u.Created.Local().Format(time.RFC1123))
			}
			return nil
		}
	}

	return fmt.Errorf("user %q not found", userID)
}
