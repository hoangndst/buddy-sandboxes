package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage local connection contexts",
		Long:  "Config manages endpoints, authentication tokens, and tenant namespaces stored in ~/.buddy/config.yaml.",
	}

	cmd.AddCommand(newGetContextsCmd())
	cmd.AddCommand(newCurrentContextCmd())
	cmd.AddCommand(newUseContextCmd())
	cmd.AddCommand(newSetContextCmd())

	return cmd
}

func newGetContextsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-contexts",
		Short: "List all configured contexts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cliConfig == nil {
				return fmt.Errorf("configuration not loaded")
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "CURRENT\tNAME\tENDPOINT\tNAMESPACE")

			for _, ctx := range cliConfig.Contexts {
				currentMarker := " "
				if ctx.Name == cliConfig.CurrentContext {
					currentMarker = "*"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					currentMarker,
					ctx.Name,
					ctx.Endpoint,
					ctx.DefaultNamespace,
				)
			}

			return w.Flush()
		},
	}
}

func newCurrentContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current-context",
		Short: "Display the active context name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cliConfig == nil {
				return fmt.Errorf("configuration not loaded")
			}
			fmt.Println(cliConfig.CurrentContext)
			return nil
		},
	}
}

func newUseContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-context <context-name>",
		Short: "Switch the active context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cliConfig == nil {
				return fmt.Errorf("configuration not loaded")
			}

			name := args[0]
			if ctx := cliConfig.GetContext(name); ctx == nil {
				return fmt.Errorf("context %q not found", name)
			}

			cliConfig.CurrentContext = name
			if err := saveConfig(flagConfig, cliConfig); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Switched to context %q.\n", name)
			return nil
		},
	}
}

func newSetContextCmd() *cobra.Command {
	var (
		flagCtxEndpoint  string
		flagCtxToken     string
		flagCtxNamespace string
	)

	cmd := &cobra.Command{
		Use:   "set-context <context-name>",
		Short: "Add or update a context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cliConfig == nil {
				return fmt.Errorf("configuration not loaded")
			}

			name := args[0]
			existing := cliConfig.GetContext(name)
			if existing != nil {
				if flagCtxEndpoint != "" {
					existing.Endpoint = flagCtxEndpoint
				}
				if flagCtxToken != "" {
					existing.Token = flagCtxToken
				}
				if flagCtxNamespace != "" {
					existing.DefaultNamespace = flagCtxNamespace
				}
			} else {
				if flagCtxEndpoint == "" {
					flagCtxEndpoint = defaultEndpoint
				}
				if flagCtxNamespace == "" {
					flagCtxNamespace = defaultNamespaceName
				}
				cliConfig.Contexts = append(cliConfig.Contexts, ContextConfig{
					Name:             name,
					Endpoint:         flagCtxEndpoint,
					Token:            flagCtxToken,
					DefaultNamespace: flagCtxNamespace,
				})
			}

			if cliConfig.CurrentContext == "" {
				cliConfig.CurrentContext = name
			}

			if err := saveConfig(flagConfig, cliConfig); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Context %q updated.\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&flagCtxEndpoint, "endpoint", "", "Gateway endpoint URL")
	cmd.Flags().StringVar(&flagCtxToken, "token", "", "Authentication token")
	cmd.Flags().StringVar(&flagCtxNamespace, "namespace", "", "Default Kubernetes namespace")

	return cmd
}
