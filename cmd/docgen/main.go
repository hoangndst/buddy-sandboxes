package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/hoangndst/buddy-sandboxes/pkg/cli/sbx"
)

func main() {
	docType := flag.String("type", "cli", "Type of documentation to generate (cli, crd, all)")
	outDir := flag.String("out-dir", "site/content/docs/reference/cli", "Output directory for generated documentation")
	flag.StringVar(outDir, "output-dir", "site/content/docs/reference/cli", "Alias for --out-dir")
	flag.Parse()

	switch *docType {
	case "cli", "all":
		if err := GenerateCLIDocs(*outDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating CLI docs: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Successfully generated CLI reference documentation in %s\n", *outDir)
	case "crd":
		fmt.Fprintln(os.Stderr, "CRD documentation generation is managed via 'make generate-crd-docs'")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "Unknown doc type %q; supported types: cli, all, crd\n", *docType)
		os.Exit(1)
	}
}

// GenerateCLIDocs generates Hugo-compatible markdown documentation for the buddy-sbx CLI.
func GenerateCLIDocs(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	// Purge stale generated command files (buddy-sbx*.md) while leaving other content intact
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read output directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "buddy-sbx") && strings.HasSuffix(name, ".md") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("failed to remove stale doc %s: %w", name, err)
			}
		}
	}

	rootCmd := sbx.NewRootCmd()

	// Disable auto-gen timestamp tags for deterministic output
	disableAutoTags(rootCmd)

	// Index commands by their generated filename
	cmdMap := make(map[string]*cobra.Command)
	indexCommands(rootCmd, cmdMap)

	filePrepender := func(filename string) string {
		basename := filepath.Base(filename)
		cmd := cmdMap[basename]
		title := "buddy-sbx"
		linkTitle := "buddy-sbx"
		desc := ""
		slug := strings.TrimSuffix(basename, ".md")

		if cmd != nil {
			title = cmd.CommandPath()
			linkTitle = cmd.Name()
			desc = cmd.Short
		}

		return fmt.Sprintf(`---
title: "%s"
linkTitle: "%s"
description: "%s"
slug: "%s"
---

`, escapeQuotes(title), escapeQuotes(linkTitle), escapeQuotes(desc), slug)
	}

	linkHandler := func(link string) string {
		if link == "buddy-sbx.md" {
			return "../"
		}
		target := strings.TrimSuffix(link, ".md")
		return "../" + target + "/"
	}

	if err := doc.GenMarkdownTreeCustom(rootCmd, dir, filePrepender, linkHandler); err != nil {
		return fmt.Errorf("failed to generate markdown tree: %w", err)
	}

	// Remove redundant root command file (buddy-sbx.md) in favor of the canonical section _index.md
	rootFile := filepath.Join(dir, "buddy-sbx.md")
	if err := os.Remove(rootFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove redundant root command doc: %w", err)
	}

	// Generate root section _index.md
	if err := generateCLIIndex(dir, rootCmd); err != nil {
		return fmt.Errorf("failed to generate _index.md: %w", err)
	}

	return nil
}

func disableAutoTags(cmd *cobra.Command) {
	cmd.DisableAutoGenTag = true
	for _, c := range cmd.Commands() {
		disableAutoTags(c)
	}
}

func indexCommands(cmd *cobra.Command, m map[string]*cobra.Command) {
	if !cmd.IsAvailableCommand() && !cmd.HasSubCommands() {
		return
	}
	filename := strings.ReplaceAll(cmd.CommandPath(), " ", "_") + ".md"
	m[filename] = cmd
	for _, c := range cmd.Commands() {
		indexCommands(c, m)
	}
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

func generateCLIIndex(dir string, root *cobra.Command) error {
	var sb strings.Builder
	sb.WriteString(`---
title: "CLI Reference"
linkTitle: "CLI"
description: "Reference documentation for the buddy-sbx command-line interface"
weight: 10
---

# CLI Reference: buddy-sbx

` + root.Long + `

## Global Flags

The following flags are available across all commands:

- ` + "`--endpoint <url>`" + `: Buddy Gateway endpoint URL (overrides active context)
- ` + "`--token <string>`" + `: Authentication token (overrides active context)
- ` + "`--config <path>`" + `: Path to configuration file (default: ` + "`~/.buddy/config.yaml`" + `)

## Commands

| Command | Summary |
| :--- | :--- |
`)

	subcmds := root.Commands()
	sort.Slice(subcmds, func(i, j int) bool {
		return subcmds[i].Name() < subcmds[j].Name()
	})

	for _, c := range subcmds {
		if !c.IsAvailableCommand() || c.IsAdditionalHelpTopicCommand() {
			continue
		}
		pageName := strings.ReplaceAll(c.CommandPath(), " ", "_")
		sb.WriteString(fmt.Sprintf("| [`%s`](./%s/) | %s |\n", c.CommandPath(), pageName, c.Short))
	}

	indexPath := filepath.Join(dir, "_index.md")
	return os.WriteFile(indexPath, []byte(sb.String()), 0644)
}
