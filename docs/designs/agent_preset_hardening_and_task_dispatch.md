# Agent Preset Hardening, One-Shot Task Dispatch, and Safe Execution Modes

## 1. Problem Statement

Executing autonomous coding agents (such as Codex and Claude Code) in Kubernetes sandboxes introduces two operational hurdles:

1. **Unprivileged Namespace and Process Collisions**: Coding agents designed for desktop operating systems attempt to launch local daemons or construct nested user namespaces via Bubblewrap (`bwrap`). In unprivileged Kubernetes containers (where `AllowPrivilegeEscalation` is disabled and capabilities are dropped), nested namespace creation fails with `EPERM`. Similarly, minimal container images without `/usr/bin/ps` cause agent background process monitors to crash. Because the Kubernetes pod and container runtime already constitute the primary security boundary, agents must run in direct execution mode with container-adapted CLI arguments.
2. **Headless Task Execution and Workspace Visibility**: When developers dispatch batch tasks (`buddy-sbx run codex . -- "Fix race condition"`), the CLI must stream stdout and stderr cleanly without PTY artifacts (such as CRLF translation or terminal control sequences), handle stdin appropriately (supplying EOF when run from a terminal, or streaming piped data), defensively guard against downloading corrupted partial state on failure or cancellation, and provide explicit file modification reports upon successful synchronization.

## 2. System Architecture

The architecture spans three primary components:

```
+-------------------------------------------------------------------------------+
| Host Workstation (buddy-sbx)                                                  |
|                                                                               |
|  +--------------------+     +-----------------------------------------------+ |
|  | CLI Command Parser | --> | Execution Mode Splitter                       | |
|  +--------------------+     +-----------------------------------------------+ |
|                                   |                              |            |
|                   Interactive (len == 0)             Headless (len > 0)       |
|                                   |                              |            |
|                                   v                              v            |
|                            [TTY: true]                    [TTY: false]        |
|                            [setupTTY()]                   [raw skipped]       |
|                            [ResizeChan]                   [ResizeChan: nil]   |
|                            [Stdin: os.Stdin]              [Stdin: nil or pipe]|
|                                   \                              /            |
|                                    v                            v             |
|                                 +----------------------------------+          |
|                                 | WebSocket Exec Client            |          |
|                                 +----------------------------------+          |
|                                                  |                            |
+--------------------------------------------------|----------------------------+
                                                   |
                                                   v
+-------------------------------------------------------------------------------+
| Kubernetes Pod (Container Runtime)                                            |
|                                                                               |
|  +--------------------------------------------------------------------------+ |
|  | Agent Process (/workspace)                                               | |
|  |  - Codex: codex --no-daemon --dangerously-bypass-approvals-and-sandbox     | |
|  |           exec --skip-git-repo-check "<prompt>"                          | |
|  |  - Claude: claude --dangerously-skip-permissions -p "<prompt>"           | |
|  +--------------------------------------------------------------------------+ |
+-------------------------------------------------------------------------------+
```

### 2.1 Hardened Agent Presets

Presets define canonical container command structures adapted for Kubernetes execution:

* **Codex**:
  * Interactive entrypoint: `["codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox"]`
  * Headless task entrypoint: `["codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "exec", "--skip-git-repo-check", "<prompt>"]`
  * Model flag injection: If `--model <model>` is supplied, `--model <model>` is placed before the `exec` subcommand.
* **Claude Code**:
  * Interactive entrypoint: `["claude", "--dangerously-skip-permissions"]`
  * Headless task entrypoint: `["claude", "--dangerously-skip-permissions", "-p", "<prompt>"]`
  * Model flag injection: If `--model <model>` is supplied, `--model <model>` is placed before the `-p` parameter.
* **Argument Concatenation**: When task arguments contain multiple tokens (`buddy-sbx run claude . -- Fix compiler warnings`), tokens are joined with single spaces into a single prompt argument.

### 2.2 Dual-Mode Execution Dispatcher

The CLI evaluates `len(taskArgs)` to select the runtime pipeline:

1. **Interactive Session (`len(taskArgs) == 0`)**:
   * Remote PTY allocated (`TTY: true`).
   * Host terminal configured in raw mode via `setupTTY()`.
   * Dynamic window resize tracking enabled via `SIGWINCH` signal forwarding (`ResizeChan`).
   * Standard input mapped directly to `os.Stdin`.
2. **Headless Task Dispatch (`len(taskArgs) > 0`)**:
   * Raw multiplexed streaming (`TTY: false`).
   * Terminal raw mode is completely bypassed, ensuring the host terminal line discipline remains intact and newlines are transmitted as standard LF (`\n`).
   * Window resize handling is bypassed (`ResizeChan: nil`).
   * Standard input evaluation:
     * If `term.IsTerminal(int(os.Stdin.Fd()))` is true: `opts.Stdin` is set to `nil`. The remote process receives an immediate EOF on stdin, preventing child processes or agent tools from stalling.
     * If `term.IsTerminal(int(os.Stdin.Fd()))` is false: `opts.Stdin` is set to `os.Stdin`, enabling shell pipes (`cat input.txt | buddy-sbx run codex . -- "analyze"`).

### 2.3 Safe Execution and Workspace Protection

Local workspace protection is achieved through a client-side execution flag:

* **`--no-sync`**:
  * The local project snapshot is uploaded and initialized inside the container `/workspace` volume so the agent has a complete, working environment with scratch space.
  * After the agent terminates, `buddy-sbx` skips remote archive download (`c.DownloadWorkspace`) and local archive merge (`workspace.MergeArchive`).
  * The local working directory remains completely untouched.
  * Informational notice is emitted: `ℹ Workspace sync skipped (--no-sync): local project directory untouched.`
* **`--read-only` Rejection**:
  * Passing `--read-only` is rejected with an explicit error explaining that container workspaces require write access for git initialization and agent caches, directing users to `--no-sync`.

### 2.4 Pre-Sync Execution Guard and Pod Lifecycle

To protect local repositories against corruption from interrupted or aborted agent executions:

* **Pre-Sync Guard**:
  * In headless task mode (`len(taskArgs) > 0`), the CLI verifies that `ctx.Err() == nil`, `execErr == nil`, and `exitCode == 0` prior to initiating workspace download or merge. If execution is cancelled (e.g. `Ctrl+C` / `SIGINT`), if an execution error occurs, or if the agent exits with a non-zero code, workspace synchronization is aborted immediately.
  * In interactive mode (`len(taskArgs) == 0`), user edits are synchronized upon session termination unless execution was cancelled or terminated abnormally by connection failure.
  * When synchronization is aborted, the local workspace remains untouched and the sandbox pod is retained to allow forensic inspection and recovery.
* **Pod Cleanup (`--rm`) Policy**:
  * For headless task runs (`len(taskArgs) > 0`), `--rm` defaults to `true`. Successful runs automatically delete their sandbox pod upon clean synchronization.
  * If an execution fails, is cancelled, or encounters a merge conflict, pod deletion is suppressed, and the pod is retained for forensic inspection.
  * Explicit CLI flags (`--rm=false` or `--rm=true`) override the default.

### 2.5 Merge Diff Reporting

Upon clean task completion and successful merge:

* Added (`A`), modified (`M`), and deleted (`D`) files from `workspace.MergeResult` are sorted alphabetically and printed to `os.Stderr`.
* Using `os.Stderr` preserves `os.Stdout` for Unix pipelines (such as `buddy-sbx run ... | jq`).
* If no files changed, the CLI prints: `✓ Workspace synchronized (no files changed).`
* Long file lists are capped at 20 items, appending a summary line for remaining files.

## 3. CLI Interface and Flags

### 3.1 Run Command Flags

| Flag | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `--no-sync` | boolean | `false` | Execute without synchronizing workspace modifications back to the local repository. |
| `--model` | string | `""` | Override the model used by the agent preset. |
| `--rm` | boolean | contextual | Remove sandbox after execution (defaults to `true` for headless task dispatch, `false` for interactive). |

### 3.2 Error and Help Text Suppression

To ensure scriptability and clean integration into pipelines:

* `rootCmd.SilenceUsage = true`: Prevents Cobra from printing usage help when an agent exits with a non-zero code.
* `rootCmd.SilenceErrors = true`: Prevents Cobra from printing duplicate `Error:` banners when application errors are handled.
* `main.go` unwraps `ExitCodeError` via `errors.As` and exits with the exact container exit code.

## 4. Security Boundaries

1. **Infrastructure Sandbox Boundary**: Multi-tenant isolation is enforced at the Kubernetes pod level (unprivileged non-root UID 1000, dropped capabilities, Seccomp profiles, and network egress controls).
2. **Autonomous Tool Execution**: Within the isolated pod, agents run without interactive confirmation prompts (`--dangerously-bypass-approvals-and-sandbox`, `--dangerously-skip-permissions`). This enables unassisted execution without compromising host or cluster security.
3. **Local Workspace Boundary**: The host repository is protected by the pre-sync execution guard and `--no-sync` flag. Incomplete runs never overwrite local files.

## 5. Failure Modes and Recovery

| Failure Mode | Detection | Mitigation / Recovery |
| :--- | :--- | :--- |
| User cancellation (`Ctrl+C`) | `ctx.Err() != nil` | Abort download/merge immediately. Retain sandbox pod. Output warning message. |
| Agent non-zero exit | `exitCode != 0` | Abort download/merge. Retain sandbox pod. Return exact exit code. |
| Agent crash / disconnect | `execErr != nil` | Abort download/merge. Retain sandbox pod. Report connection failure. |
| Workspace merge conflict | `len(merge.Conflicts) > 0` | Retain sandbox pod. Print conflict files. Leave conflicting files unmerged. |
| Headless stdin stall | Terminal stdin detected | Set `opts.Stdin = nil` so remote container receives immediate EOF. |

## 6. Verification Criteria

1. **Preset Command Construction**: Unit tests verify command argument generation for Codex and Claude Code across interactive and headless modes, with and without `--model`.
2. **Dual-Mode Dispatching**: Headless runs bypass `setupTTY()`, set `TTY = false`, and supply `Stdin = nil` for terminal runs or `Stdin = os.Stdin` for piped runs.
3. **Pre-Sync Guard**: Simulated cancellations and non-zero exit codes verify that `DownloadWorkspace` and `MergeArchive` are skipped and the sandbox pod is retained.
4. **Merge Diff Output**: Tests verify that added, modified, and deleted files are formatted cleanly to `os.Stderr`.
5. **Local KinD Cluster Verification**: Execute live interactive and headless agent runs on the local cluster, verifying clean exit code propagation, sync diff reporting, and automatic cleanup.
