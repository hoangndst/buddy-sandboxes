package sbx

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func newCreateCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	cmd := &cobra.Command{
		Use:   "create <resource> [args]",
		Short: "Create a resource from flags or standard input",
		Long: `Create provisions new platform resources.

Supported resource types:
  - task    (create and dispatch an autonomous agent task)
  - secret  (store a provider credential or Git token)
  - user    (create a platform user with a dedicated namespace)
  - token   (issue a login token for a user)`,
		Example: `  # Create an autonomous coding task
  buddy-sbx create task --workspace repo-auth --prompt "Fix race condition in session manager"

  # Store an LLM provider credential secret
  buddy-sbx create secret openai

  # Store an LLM provider credential with custom endpoint and model
  buddy-sbx create secret openai --url https://example.com/v1 --model o3-mini

  # Store a Git HTTPS credential secret bound to a repository
  buddy-sbx create secret github --repo https://github.com/org/repo.git

  # Create a platform user with a dedicated namespace
  buddy-sbx create user "Alice Smith" --namespace ns-alice

  # Issue an authentication token for a user
  buddy-sbx create token usr_alice`,
	}

	cmd.AddCommand(newCreateTaskCmd(opt))
	cmd.AddCommand(newCreateSecretCmd(opt))
	cmd.AddCommand(newCreateUserCmd(opt))
	cmd.AddCommand(newCreateTokenCmd(opt))

	return cmd
}

func newCreateTaskCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	var (
		flagWorkspace   string
		flagPrompt      string
		flagProfile     string
		flagBranch      string
		flagMaxAttempts int
		flagNamespace   string
	)

	cmd := &cobra.Command{
		Use:   "task [flags]",
		Short: "Create and dispatch an autonomous agent task",
		Long:  `Create task submits an asynchronous agent task targeting a workspace.`,
		Example: `  # Create a task targeting a workspace with a prompt
  buddy-sbx create task --workspace repo-auth --prompt "Fix race condition in session manager"

  # Create a task with an agent profile and retry budget
  buddy-sbx create task -w repo-auth -p "Fix bug" --profile golang-default --max-attempts 2`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(flagWorkspace) == "" {
				return fmt.Errorf("--workspace is required")
			}
			if strings.TrimSpace(flagPrompt) == "" {
				return fmt.Errorf("--prompt is required")
			}
			c, err := opt.GetClient()
			if err != nil {
				return err
			}
			idempKey := uuid.New().String()
			req := &apiv1.CreateTaskRequest{
				WorkspaceName: flagWorkspace,
				Prompt:        flagPrompt,
				MaxAttempts:   &flagMaxAttempts,
			}
			if flagProfile != "" {
				req.AgentProfileID = &flagProfile
			}
			if flagBranch != "" {
				req.TargetBranch = &flagBranch
			}
			if flagNamespace != "" {
				req.Namespace = flagNamespace
			}
			task, err := c.CreateTask(cmd.Context(), idempKey, req)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created task %s (Status: %s, Workspace: %s)\n",
				task.ID, task.Status, task.WorkspaceName)
			return err
		},
	}

	cmd.Flags().StringVarP(&flagWorkspace, "workspace", "w", "", "Target workspace name (required)")
	cmd.Flags().StringVarP(&flagPrompt, "prompt", "p", "", "Task instructions for the agent (required)")
	cmd.Flags().StringVar(&flagProfile, "profile", "", "Agent profile ID")
	cmd.Flags().StringVarP(&flagBranch, "branch", "b", "", "Target git branch")
	cmd.Flags().IntVar(&flagMaxAttempts, "max-attempts", 1, "Maximum execution attempts")
	cmd.Flags().StringVarP(&flagNamespace, "namespace", "n", "", "Kubernetes namespace (defaults to user namespace)")

	return cmd
}

func newCreateSecretCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	options := &secretSetOptions{}

	cmd := &cobra.Command{
		Use:   "secret <service>",
		Short: "Store a credential for a supported service",
		Long: `Create secret stores an encrypted provider API key or Git token.

Supported services:
  openai, anthropic, github`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretSet(cmd, args[0], options, opt)
		},
	}

	cmd.Flags().StringVar(&options.sandbox, "sandbox", "", "Limit the key to one of your sandboxes")
	cmd.Flags().StringVar(&options.repository, "repo", "", "GitHub HTTPS repository URL for GitHub credentials")
	cmd.Flags().StringVar(&options.baseURL, "url", "", "Custom provider base URL")
	cmd.Flags().StringVar(&options.baseURL, "base-url", "", "Alias for --url")
	cmd.Flags().StringVar(&options.model, "model", "", "Default model identifier (e.g. gpt-4o, o3-mini)")
	cmd.Flags().StringVar(&options.authType, "auth-type", "", "Authentication type ('bearer' or 'api-key')")
	cmd.Flags().IntVar(&options.rpm, "rpm", 0, "Maximum requests per minute for this provider key")
	cmd.Flags().IntVar(&options.maxConcurrent, "max-concurrent", 0,
		"Maximum concurrent in-flight requests for this provider key")
	cmd.Flags().Int64Var(&options.tpm, "tpm", 0, "Maximum tokens per session/minute for this provider key")

	return cmd
}

func newCreateUserCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	var namespace string

	cmd := &cobra.Command{
		Use:   "user <display-name>",
		Short: "Create a new user with an assigned namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(namespace) == "" {
				return fmt.Errorf("--namespace is required")
			}
			c, err := opt.GetClient()
			if err != nil {
				return err
			}
			user, err := c.CreateUser(context.Background(), args[0], namespace)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created user %s (ID: %s, Namespace: %s, Role: %s)\n",
				user.DisplayName, user.ID, user.Namespace, user.Role)
			return err
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", "", "Target Kubernetes namespace for user sandboxes")
	_ = cmd.MarkFlagRequired("namespace")

	return cmd
}

func newCreateTokenCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	return &cobra.Command{
		Use:   "token <user-id>",
		Short: "Issue a new login token for a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opt.GetClient()
			if err != nil {
				return err
			}
			token, err := c.IssueUserToken(context.Background(), args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Issued token for user %s.\nSave the token now; it is shown only once:\n\n%s\n\nToken ID: %s\nExpires:  %s\n",
				args[0], token.Token, token.ID, token.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST"))
			return nil
		},
	}
}
