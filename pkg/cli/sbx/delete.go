package sbx

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newDeleteCmd() *cobra.Command {
	var (
		flagSandbox string
		flagUser    string
		flagRepo    string
	)

	cmd := &cobra.Command{
		Use:   "delete <resource> <name...>",
		Short: "Delete resources by resource and name",
		Long: `Delete removes resources from the Kubernetes cluster.

Supported resource types:
  - sandboxes (aliases: sandbox, sbx)
  - sessions  (aliases: session, sess)
  - secrets   (aliases: secret)
  - tokens    (aliases: token)`,
		Example: `  # Delete a single sandbox
  buddy-sbx delete sandbox my-box

  # Delete multiple sandboxes
  buddy-sbx delete sbx my-box-1 my-box-2

  # Delete a sandbox belonging to another user (admin only)
  buddy-sbx delete sbx my-box --user usr_alice

  # Delete a detached supervisor session
  buddy-sbx delete session sess-9b9ad3488cc2 -s my-box

  # Delete a session in path format
  buddy-sbx delete sess my-box/sess-9b9ad3488cc2

  # Delete a stored credential secret
  buddy-sbx delete secret openai

  # Delete a user token
  buddy-sbx delete token tok_abc --user usr_alice`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer func() {
				flagSandbox = ""
				flagUser = ""
				flagRepo = ""
			}()

			resourceType := strings.ToLower(args[0])
			names := args[1:]

			c, err := getClient()
			if err != nil {
				return err
			}

			switch resourceType {
			case resourceSandboxes, resourceSandbox, resourceSbx:
				return runDeleteSandboxes(cmd, c, names, flagUser)

			case resourceSessions, resourceSession, resourceSess:
				return runDeleteSessions(cmd, c, names, flagSandbox)

			case resourceSecrets, resourceSecret:
				return runDeleteSecrets(cmd, c, names, flagSandbox, flagRepo)

			case resourceTokens, resourceToken:
				if flagUser == "" {
					return fmt.Errorf("deleting a token requires --user <user-id>")
				}
				return runDeleteTokens(cmd, c, names, flagUser)

			default:
				return fmt.Errorf("unknown resource type %q; "+
					"choose from: sandboxes (sbx), sessions (sess), secrets, tokens", args[0])
			}
		},
	}

	cmd.Flags().StringVarP(&flagSandbox, "sandbox", "s", "", "Target sandbox for child resources (sessions, secrets)")
	cmd.Flags().StringVarP(&flagUser, "user", "u", "", "Target user ID (for token revocation or admin sandbox deletion)")
	cmd.Flags().StringVar(&flagRepo, "repo", "", "Repository URL for Git credentials")

	return cmd
}

func runDeleteSandboxes(cmd *cobra.Command, c *client.Client, names []string, userID string) error {
	ctx := cmd.Context()
	var hasErrors bool

	for _, name := range names {
		var err error
		if userID != "" {
			err = c.DeleteUserSandbox(ctx, userID, name)
		} else {
			err = c.DeleteSandbox(ctx, name)
		}

		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error deleting sandbox %q: %v\n", name, err)
			hasErrors = true
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "sandbox %q deleted\n", name)
		}
	}

	if hasErrors {
		return fmt.Errorf("one or more sandbox deletions failed")
	}
	return nil
}

func runDeleteSessions(cmd *cobra.Command, c *client.Client, names []string, sandboxFlag string) error {
	ctx := cmd.Context()
	var hasErrors bool

	for _, target := range names {
		var sbxID, sessID string
		if slashIdx := strings.IndexByte(target, '/'); slashIdx >= 0 {
			sbxID = target[:slashIdx]
			sessID = target[slashIdx+1:]
		} else {
			sessID = target
			sbxID = sandboxFlag
		}

		if sbxID == "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"Error deleting session %q: missing sandbox scope (specify --sandbox or <sandbox>/<session>)\n", sessID)
			hasErrors = true
			continue
		}

		if err := c.DeleteSession(ctx, sbxID, sessID); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error deleting session %q in sandbox %q: %v\n", sessID, sbxID, err)
			hasErrors = true
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "session %q deleted\n", sessID)
		}
	}

	if hasErrors {
		return fmt.Errorf("one or more session deletions failed")
	}
	return nil
}

func runDeleteSecrets(cmd *cobra.Command, c *client.Client, names []string, sandbox, repo string) error {
	ctx := cmd.Context()
	var hasErrors bool

	for _, name := range names {
		service, err := normalizeProvider(name)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", err)
			hasErrors = true
			continue
		}

		descriptor, _ := credentials.LookupService(service)
		isGitRepository := descriptor.Kind == credentials.ServiceKindGitRepository

		if descriptor.RequiresRepository && repo == "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %s credentials require --repo\n", descriptor.DisplayName)
			hasErrors = true
			continue
		}
		if !descriptor.RequiresRepository && repo != "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: --repo is not valid for %s credentials\n", descriptor.DisplayName)
			hasErrors = true
			continue
		}
		if sandbox != "" && !descriptor.SupportsSandboxScope {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %s credentials do not support sandbox scope\n", descriptor.DisplayName)
			hasErrors = true
			continue
		}

		if isGitRepository {
			err = c.DeleteGitCredential(ctx, repo, sandbox)
		} else {
			err = c.DeleteCredential(ctx, service, sandbox)
		}

		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error deleting secret %q: %v\n", name, err)
			hasErrors = true
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "secret %q deleted\n", name)
		}
	}

	if hasErrors {
		return fmt.Errorf("one or more secret deletions failed")
	}
	return nil
}

func runDeleteTokens(cmd *cobra.Command, c *client.Client, tokenIDs []string, userID string) error {
	ctx := cmd.Context()
	var hasErrors bool

	for _, tokenID := range tokenIDs {
		if err := c.RevokeUserToken(ctx, userID, tokenID); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error revoking token %q for user %q: %v\n", tokenID, userID, err)
			hasErrors = true
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "token %q revoked for user %q\n", tokenID, userID)
		}
	}

	if hasErrors {
		return fmt.Errorf("one or more token revocations failed")
	}
	return nil
}
