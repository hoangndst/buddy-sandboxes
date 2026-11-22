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
	cmd := &cobra.Command{
		Use: "set <openai|anthropic>", Short: "Store a provider API key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := normalizeProvider(args[0])
			if err != nil {
				return err
			}
			var value []byte
			if term.IsTerminal(int(os.Stdin.Fd())) {
				_, _ = fmt.Fprintf(os.Stderr, "Enter %s API key: ", service)
				value, err = term.ReadPassword(int(os.Stdin.Fd()))
				_, _ = fmt.Fprintln(os.Stderr)
			} else {
				var line string
				line, err = bufio.NewReader(os.Stdin).ReadString('\n')
				value = []byte(line)
				if err == io.EOF {
					err = nil
				}
			}
			if err != nil {
				return fmt.Errorf("read provider API key: %w", err)
			}
			key := strings.TrimSpace(string(value))
			for i := range value {
				value[i] = 0
			}
			if key == "" {
				return fmt.Errorf("provider API key is empty")
			}
			defer func() { key = "" }()
			c, err := getClient()
			if err != nil {
				return err
			}
			if err := c.SetCredential(cmd.Context(), service, sandbox, key); err != nil {
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
			if _, err := fmt.Fprintln(w, "SERVICE\tSCOPE\tSANDBOX\tUPDATED"); err != nil {
				return err
			}
			for _, item := range items {
				updated := item.UpdatedAt.Local().Format("2006-01-02 15:04:05 MST")
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Service, item.Scope, item.SandboxName, updated); err != nil {
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
