package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage provider API keys stored by Buddy",
		Long:  "Provider keys are encrypted by Buddy and are never copied into sandbox environment values.",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretRemoveCmd())
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var sandbox string
	var flagURL string
	var flagModel string
	var flagAuthType string

	cmd := &cobra.Command{
		Use: "set <openai|anthropic>", Short: "Store a provider API key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := normalizeProvider(args[0])
			if err != nil {
				return err
			}
			var envKey string
			if service == providerOpenAI {
				envKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
			} else if service == providerAnthropic {
				envKey = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
			}
			var key string
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				var line string
				line, err = bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && err != io.EOF {
					return fmt.Errorf("read provider API key: %w", err)
				}
				key = strings.TrimSpace(line)
			}
			if key == "" && envKey != "" {
				key = envKey
			}
			if key == "" && term.IsTerminal(int(os.Stdin.Fd())) {
				_, _ = fmt.Fprintf(os.Stderr, "Enter %s API key: ", service)
				var value []byte
				value, err = term.ReadPassword(int(os.Stdin.Fd()))
				_, _ = fmt.Fprintln(os.Stderr)
				if err != nil {
					return fmt.Errorf("read provider API key: %w", err)
				}
				key = strings.TrimSpace(string(value))
				for i := range value {
					value[i] = 0
				}
			}
			if key == "" {
				return fmt.Errorf("provider API key is empty; enter interactively or set in environment")
			}
			defer func() { key = "" }()
			if flagURL == "" && service == providerOpenAI {
				flagURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
			}
			if flagModel == "" && service == providerOpenAI {
				flagModel = strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			opts := client.CredentialOptions{
				BaseURL:      flagURL,
				DefaultModel: flagModel,
				AuthType:     flagAuthType,
			}
			if err := c.SetCredential(cmd.Context(), service, sandbox, key, opts); err != nil {
				return err
			}
			if sandbox == "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stored %s API key for your account.\n", service)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Stored %s API key for sandbox %s.\n", service, sandbox)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "Limit the key to one of your sandboxes")
	cmd.Flags().StringVar(&flagURL, "url", "",
		"Custom provider base URL (e.g. https://<account>.openai.azure.com/openai/v1)")
	cmd.Flags().StringVar(&flagURL, "base-url", "", "Alias for --url")
	cmd.Flags().StringVar(&flagModel, "model", "", "Default model identifier (e.g. gpt-6-luna, o3-mini)")
	cmd.Flags().StringVar(&flagAuthType, "auth-type", "", "Authentication type ('bearer' or 'api-key')")
	return cmd
}

func newSecretListCmd() *cobra.Command {
	return &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List stored provider API keys", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			items, err := c.ListCredentials(context.Background())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "SERVICE\tSCOPE\tSANDBOX\tBASE_URL\tMODEL\tUPDATED"); err != nil {
				return err
			}
			for _, item := range items {
				updated := item.UpdatedAt.Local().Format("2006-01-02 15:04:05 MST")
				bURL := item.BaseURL
				if bURL == "" {
					bURL = "-"
				}
				model := item.DefaultModel
				if model == "" {
					model = "-"
				}
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					item.Service, item.Scope, item.SandboxName, bURL, model, updated); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
}

func newSecretRemoveCmd() *cobra.Command {
	var sandbox string
	cmd := &cobra.Command{
		Use: "rm <openai|anthropic>", Aliases: []string{"remove", "delete"},
		Short: "Remove a stored provider API key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := normalizeProvider(args[0])
			if err != nil {
				return err
			}
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.DeleteCredential(cmd.Context(), service, sandbox); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s API key.\n", service)
			return err
		},
	}
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "Remove the key scoped to one sandbox")
	return cmd
}

func normalizeProvider(service string) (string, error) {
	service = strings.ToLower(strings.TrimSpace(service))
	if service != providerOpenAI && service != providerAnthropic {
		return "", fmt.Errorf("unsupported provider %q; choose openai or anthropic", service)
	}
	return service, nil
}
