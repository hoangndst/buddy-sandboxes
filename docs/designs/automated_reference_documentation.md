# Automated Reference Documentation Architecture

## Overview

Buddy Sandboxes provides a unified Kubernetes-native developer experience comprising custom resource definitions (CRDs), an in-cluster control plane, and the `buddy-sbx` developer CLI. As the platform evolves, reference documentation for commands and API types must remain accurate, complete, and reproducible without manual intervention.

This specification describes the automated reference documentation pipeline. The Go source code serves as the single source of truth for:
1. **Developer CLI Reference**: Generated directly from the Cobra command hierarchy using `github.com/spf13/cobra/doc`.
2. **Kubernetes CRD API Reference**: Generated directly from Kubernetes API type definitions in `api/v1alpha1/` using `github.com/elastic/crd-ref-docs`.

Both reference sets are integrated into the Hugo website under `site/`, complete with structured navigation, readable layouts, deterministic output, and automated drift detection in continuous integration.

---

## 1. CLI Documentation Architecture

### 1.1 Decoupling Command Definition (`pkg/cli/sbx`)

The developer CLI implementation previously resided entirely within `cmd/buddy-sbx` under package `main`. In Go, package `main` cannot be imported by external tools or standalone generators.

To enable automated documentation generation while preserving binary behavior and backwards compatibility, command tree construction and terminal helpers are refactored into the importable package `pkg/cli/sbx`:

```text
pkg/cli/sbx/
├── root.go             # NewRootCmd() constructor, persistent flags, ExitCodeError
├── get.go              # get sandboxes, sessions, secrets, users
├── describe.go         # describe sandboxes, sessions, users
├── delete.go           # delete sandboxes, sessions, secrets, tokens
├── create.go           # create secret, user, token
├── set.go              # set role, set user
├── run.go              # run agent workloads
├── attach.go           # attach to sessions
├── logs.go             # stream and snapshot logs
├── exec.go             # interactive exec
├── portforward.go      # port-forward tunnels
├── cp.go               # file transfer
├── suspend.go          # suspend sandboxes
├── resume.go           # resume sandboxes
├── config.go           # configuration management
├── login.go            # user authentication
├── constants.go        # shared resource names and error strings
├── tty.go              # terminal resize and raw mode interfaces
├── tty_unix.go         # unix terminal resize signal handling
└── tty_windows.go      # windows terminal resize stub
```

The application entrypoint `cmd/buddy-sbx/main.go` becomes a minimal execution wrapper that preserves exact process exit code propagation:

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/hoangndst/buddy-sandboxes/pkg/cli/sbx"
)

func main() {
	cmd := sbx.NewRootCmd()
	if err := cmd.Execute(); err != nil {
		var exitErr sbx.ExitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
```

### 1.2 Documentation Generator (`cmd/docgen`)

A dedicated documentation generator utility is introduced at `cmd/docgen/main.go`. It imports `sbx.NewRootCmd()` to ensure the exact same command hierarchy, flags, aliases, and help strings are documented.

Key generator invariants:
1. **Deterministic Output**: Sets `cmd.DisableAutoGenTag = true` to remove machine- and timestamp-dependent footers.
2. **Selective Stale File Cleanup**: Purges previous `buddy-sbx*.md` files in the target directory (`site/content/docs/reference/cli/`) prior to regeneration, preventing orphaned files when commands change while preserving section metadata.
3. **Hugo Front Matter**: Uses `doc.GenMarkdownTreeCustom` with a `filePrepender` callback. To determine command metadata from the filename passed to `filePrepender`:
   - A command index is built upfront by traversing `rootCmd` recursively and mapping generated file names (e.g. `buddy-sbx_get.md`) to their corresponding `*cobra.Command`.
   - The prepender injects standard Hugo front matter:
     ```yaml
     ---
     title: "buddy-sbx get"
     linkTitle: "get"
     description: "Display one or many resources"
     slug: "buddy-sbx_get"
     ---
     ```
4. **Cross-Reference Links**: A custom `linkHandler` translates inter-command markdown references into clean relative Hugo links.
5. **Root Index Generation**: `cmd/docgen` generates `site/content/docs/reference/cli/_index.md` summarizing CLI usage, global options, and linking to all subcommands.
6. **Command Metadata Hygiene**: Audit and expand missing `Example`, `Long`, and `Aliases` fields on all commands. Hidden commands (`Hidden: true`) are omitted automatically.

---

## 2. CRD API Documentation Architecture

### 2.1 Tooling & Configuration

CRD API reference documentation is generated using `github.com/elastic/crd-ref-docs`, pinned to version `v0.3.0` and installed to `bin/` via the project's standard `go-install-tool` Makefile macro.

Configuration is defined in `hack/crd-ref-docs/config.yaml`:
```yaml
processor:
  ignoreTypes:
    - ".*List$"
  ignoreFields:
    - "TypeMeta"

render:
  kubernetesVersion: "1.32"
  externalPackages:
    - typeMatch: "^k8s\\.io/api/core/v1"
      docsURL: "https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#"
    - typeMatch: "^k8s\\.io/apimachinery/pkg/apis/meta/v1"
      docsURL: "https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#"
```

### 2.2 Template & Content Organization

`crd-ref-docs` `v0.3.0` utilizes standard template files in `hack/crd-ref-docs/templates/`:
- `gv_list.tpl`: Root entrypoint rendering Hugo front matter, reference title, package description, and table of contents.
- `gv_details.tpl`: GroupVersion section overview and kind lists.
- `type.tpl`: Individual declaration rendering type descriptions, kind, apiVersion, and field summary tables.
- `type_members.tpl`: Struct member formatting for field names, types, descriptions, default values, and Kubebuilder validation constraints.

Output is directed to `site/content/docs/reference/crd/_index.md`, establishing an in-depth reference for:
- `Sandbox` (`sandboxes.buddy.io/v1alpha1`)
- `SandboxPool` (`sandboxes.buddy.io/v1alpha1`)
- `SandboxLease` (`sandboxes.buddy.io/v1alpha1`)
- `NetworkProfile` (`sandboxes.buddy.io/v1alpha1`)
- `Workspace` (`sandboxes.buddy.io/v1alpha1`)

Field comments across `api/v1alpha1/*_types.go` are audited and improved to ensure every field has an accurate description without altering API contracts or Kubebuilder markers.

---

## 3. Hugo Website Integration

### 3.1 Information Architecture

The website content structure under `site/content/` is organized into a hierarchical documentation section:

```text
site/content/
├── _index.md                             # Landing page content
└── docs/
    ├── _index.md                         # Documentation root overview
    ├── getting-started/
    │   └── _index.md                     # Installation & quickstart
    ├── concepts/
    │   └── _index.md                     # Architecture, security & warm pooling
    ├── guides/
    │   └── _index.md                     # Workflows & agent sandboxing
    └── reference/
        ├── _index.md                     # Reference overview
        ├── cli/
        │   ├── _index.md                 # CLI overview
        │   └── buddy-sbx_*.md            # Generated subcommand pages
        └── crd/
            └── _index.md                 # Generated CRD API Reference
```

### 3.2 Layouts & Visual Presentation

New documentation layouts are added under `site/layouts/docs/`:
- `list.html` and `single.html`: Render a responsive two-column documentation interface.
  - Left column: Hierarchical navigation sidebar highlighting current active page.
  - Right column: Main article body with breadcrumbs, section headings with anchor links, parameter tables, and syntax-highlighted code blocks.
- Typography and theme follow Buddy Sandboxes branding:
  - Font families: `Geist Sans` for text, `Geist Mono` for commands and code.
  - Color palette: Theme tokens from `main.css` (`#f4efe6` light background, `#1c1816` dark background, accent purple).
- Header integration: Adds a "Docs" navigation link in `site-header` across all site pages.

---

## 4. Build Tooling & Makefile Integration

The following standard targets are added to `Makefile`:

```makefile
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
CRD_REF_DOCS_VERSION ?= v0.3.0

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS)
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: generate-cli-docs
generate-cli-docs: ## Generate Hugo CLI reference documentation from Cobra command tree.
	@mkdir -p site/content/docs/reference/cli
	@go run ./cmd/docgen --type=cli --out-dir=site/content/docs/reference/cli

.PHONY: generate-crd-docs
generate-crd-docs: crd-ref-docs ## Generate Hugo CRD API reference documentation from Go types.
	@mkdir -p site/content/docs/reference/crd
	@$(CRD_REF_DOCS) \
		--source-path=./api/v1alpha1 \
		--config=hack/crd-ref-docs/config.yaml \
		--templates-dir=hack/crd-ref-docs/templates \
		--renderer=markdown \
		--output-path=site/content/docs/reference/crd/_index.md

.PHONY: generate-docs
generate-docs: generate-cli-docs generate-crd-docs ## Generate all reference documentation.

.PHONY: verify-cli-docs
verify-cli-docs: generate-cli-docs ## Verify CLI documentation is up-to-date.
	@test -z "$$(git status --porcelain site/content/docs/reference/cli)" || { \
		echo "Error: CLI reference documentation is out of date or contains untracked changes. Run 'make generate-cli-docs'."; \
		git status --porcelain site/content/docs/reference/cli; \
		exit 1; \
	}

.PHONY: verify-crd-docs
verify-crd-docs: generate-crd-docs ## Verify CRD API reference documentation is up-to-date.
	@test -z "$$(git status --porcelain site/content/docs/reference/crd)" || { \
		echo "Error: CRD API reference documentation is out of date or contains untracked changes. Run 'make generate-crd-docs'."; \
		git status --porcelain site/content/docs/reference/crd; \
		exit 1; \
	}

.PHONY: verify-docs
verify-docs: verify-cli-docs verify-crd-docs ## Verify all reference documentation is up-to-date.
```

All generation and verification targets run completely offline once tools are cached, require zero cluster access or active gateway services, and execute zero operational workload commands.

---

## 5. Continuous Integration & Quality Gates

In `.github/workflows/ci.yml`:
1. **Drift Detection**: Run `make verify-docs`. If any Go API field comment, Cobra command flag, or description is updated without committing regenerated docs, CI fails.
2. **Website Build Verification**: Verify that the Hugo static site builds without warnings or broken links using the official Hugo setup action:
   ```yaml
   - name: Verify Documentation Generation
     run: make verify-docs

   - name: Setup Hugo
     uses: peaceiris/actions-hugo@v3
     with:
       hugo-version: '0.170.1'
       extended: true

   - name: Verify Hugo Site Build
     run: |
       cd site
       hugo --minify
   ```
3. **Workflow Triggers**: Remove `site/**` from `paths-ignore` in `ci.yml` so that documentation updates, template modifications, and API doc drift are always validated.

---

## 6. Testing Strategy

1. **Unit Tests for Doc Generator**:
   - Verify `NewRootCmd()` constructs full command tree with expected command count and aliases.
   - Test markdown generation output to verify front matter presence, link rewriting, and timestamp omission.
   - Test that stale markdown files are purged on generation.
2. **Regression Verification**:
   - Verify all existing `buddy-sbx` commands (`get`, `describe`, `delete`, `create`, `set`, `run`, `attach`, `logs`, etc.) pass existing unit tests without changes to CLI behavior.
3. **Hugo Compilation**:
   - Verify `hugo --minify` executes with exit code 0 and populates `site/public/` with valid HTML, navigation anchors, and assets.
