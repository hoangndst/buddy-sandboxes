package sbx

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newConfigCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage local connection contexts",
		Long:  "Config manages gateway endpoints and authentication tokens stored in ~/.buddy/config.yaml.",
		Example: `  # View all configured connection contexts
  buddy-sbx config get-contexts

  # Show the currently active context
  buddy-sbx config current-context

  # Switch active context
  buddy-sbx config use-context staging

  # Define or update a context endpoint
  buddy-sbx config set-context prod --endpoint https://gateway.buddy.example.com`,
	}

	cmd.AddCommand(newGetContextsCmd(opt))
	cmd.AddCommand(newCurrentContextCmd(opt))
	cmd.AddCommand(newUseContextCmd(opt))
	cmd.AddCommand(newSetContextCmd(opt))

	return cmd
}

func newGetContextsCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	return &cobra.Command{
		Use:   "get-contexts",
		Short: "List all configured contexts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := opt.Config
			if cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "CURRENT\tNAME\tENDPOINT")

			for _, ctx := range cfg.Contexts {
				currentMarker := " "
				if ctx.Name == cfg.CurrentContext {
					currentMarker = "*"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n",
					currentMarker,
					ctx.Name,
					ctx.Endpoint,
				)
			}

			return w.Flush()
		},
	}
}

func newCurrentContextCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	return &cobra.Command{
		Use:   "current-context",
		Short: "Display the active context name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := opt.Config
			if cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}
			fmt.Println(cfg.CurrentContext)
			return nil
		},
	}
}

func newUseContextCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	return &cobra.Command{
		Use:   "use-context <context-name>",
		Short: "Switch the active context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := opt.Config
			if cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}

			name := args[0]
			if ctx := cfg.GetContext(name); ctx == nil {
				return fmt.Errorf("context %q not found", name)
			}

			cfg.CurrentContext = name
			if err := saveConfig(opt.ConfigFile, cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Switched to context %q.\n", name)
			return nil
		},
	}
}

func newSetContextCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	var (
		flagCtxEndpoint string
		flagCtxToken    string
	)

	cmd := &cobra.Command{
		Use:   "set-context <context-name>",
		Short: "Add or update a context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := opt.Config
			if cfg == nil {
				return fmt.Errorf("configuration not loaded")
			}

			name := args[0]
			existing := cfg.GetContext(name)
			if existing != nil {
				if flagCtxEndpoint != "" {
					existing.Endpoint = flagCtxEndpoint
				}
				if flagCtxToken != "" {
					existing.Token = flagCtxToken
				}
			} else {
				if flagCtxEndpoint == "" {
					flagCtxEndpoint = defaultEndpoint
				}
				cfg.Contexts = append(cfg.Contexts, ContextConfig{
					Name:     name,
					Endpoint: flagCtxEndpoint,
					Token:    flagCtxToken,
				})
			}

			if cfg.CurrentContext == "" {
				cfg.CurrentContext = name
			}

			if err := saveConfig(opt.ConfigFile, cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Context %q updated.\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagCtxEndpoint, "endpoint", "", "Gateway endpoint URL")
	cmd.Flags().StringVar(&flagCtxToken, "token", "", "Authentication token")

	return cmd
}
