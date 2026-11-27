# Agent Adapter Contract: buddy.agent/v1

## Overview

The `buddy.agent/v1` contract defines the execution and communication protocol between the Buddy Sandboxes gateway dispatcher, the in-guest supervisor (`buddy-runner`), and agent harnesses running inside sandboxes.

By standardizing on a file-based JSON contract, the platform eliminates agent-specific switches and hardcoded command-line builders from the central gateway dispatcher. Any container image adhering to `buddy.agent/v1` can run headless coding agents, deterministic verification gates, and interactive sessions.

## Filesystem Architecture

Every sandbox provisioned for agent tasks mounts an in-memory scratch volume at `/run/buddy` (backed by a 64Mi memory-backed `emptyDir` volume) alongside the workspace repository at `/workspace`:

```
/workspace                   Persistent repository checkout (PVC or git-init)
/run/buddy                   Memory-backed ephemeral volume (tmpfs, 64Mi size limit)
  ├── task.json              Execution metadata and parameters written by dispatcher (mode 0600)
  ├── prompt.txt             Full user prompt text passed via filesystem (mode 0600)
  ├── system-prompt.txt      Optional system persona and engineering constraints (mode 0600)
  ├── result.json            Output status and verification record written by supervisor (mode 0600)
  └── memory.md              Read-only contextual memory slice (reserved for post-D)
/opt/buddy/bin               Supervisor binaries injected via tools volume
  └── buddy-runner           Supervisor binary providing agent-run and write-file
/etc/buddy/agent             Harness configuration and adapter
  └── run.sh                 Universal agent adapter entrypoint
```

## Contract Schemas

### 1. Task Specification (`/run/buddy/task.json`)

Written by the dispatcher into `/run/buddy/task.json` via `buddy-runner write-file` before initiating execution:

```json
{
  "spec_version": "buddy.agent/v1",
  "task_id": "task-01j8abc123",
  "executionId": "exec-01j8abc456-att1",
  "attempt": 1,
  "targetBranch": "feature/auth-race-fix",
  "prompt": "Fix concurrency race condition in auth handler",
  "prompt_file": "/run/buddy/prompt.txt",
  "system_prompt": "You are a systems engineer adhering strictly to zero-downtime guidelines.",
  "system_prompt_file": "/run/buddy/system-prompt.txt",
  "max_steps": 30,
  "model": "claude-3-5-sonnet-20241022",
  "provider": "anthropic",
  "verify": {
    "command": "go test -race ./...",
    "timeout": 300
  },
  "env": {
    "CGO_ENABLED": "1"
  },
  "context": {
    "workspace": "backend-api"
  }
}
```

#### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `spec_version` | string | Yes | Protocol version identifier. Must be `"buddy.agent/v1"`. |
| `task_id` | string | Yes | Unique identifier of the task being executed. |
| `executionId` | string | No | Identifier of the active execution attempt. |
| `attempt` | integer | No | Attempt number (1-based index). |
| `targetBranch` | string | No | Target git branch for the change set. |
| `prompt` | string | Yes | Primary user instruction or task objective. |
| `prompt_file` | string | No | Path to file containing the prompt text (`/run/buddy/prompt.txt`). |
| `system_prompt` | string | No | Optional persona, instructions, and engineering constraints. |
| `system_prompt_file`| string | No | Path to file containing system prompt (`/run/buddy/system-prompt.txt`). |
| `max_steps` | integer | No | Maximum reasoning turns before terminating. Defaults to 30. |
| `model` | string | No | Target model name forwarded to the provider proxy. |
| `provider` | string | No | Model provider identifier (e.g. `anthropic`, `openai`). |
| `verify` | object | No | Optional post-run verification gate definition. |
| `verify.command` | string | Yes (if `verify` set) | Shell command to execute inside `/workspace` after the harness completes. |
| `verify.timeout` | integer | No | Maximum execution time in seconds for verification. Defaults to 300. |
| `env` | map[string]string | No | Key-value environment variables injected into the agent run. |
| `context` | map[string]string | No | Extra metadata key-value pairs forwarded to the agent. |

---

### 2. Result Specification (`/run/buddy/result.json`)

Written by the in-guest supervisor (`buddy-runner agent-run`) upon completion of both the harness and any configured verification gate:

```json
{
  "spec_version": "buddy.agent/v1",
  "status": "succeeded",
  "exit_code": 0,
  "summary": "Resolved auth mutex deadlock and verified race detector passing",
  "proposed_memories": [
    {
      "key": "auth_lock_ordering",
      "content": "Acquire user session lock before tenant registry lock to prevent deadlock"
    }
  ],
  "verification": {
    "passed": true,
    "exit_code": 0,
    "output": "PASS: TestAuthRace (0.42s)\nok  backend/auth  0.45s\n"
  }
}
```

#### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `spec_version` | string | Yes | Protocol version identifier. Must match `"buddy.agent/v1"`. |
| `status` | string | Yes | Overall execution status: `"succeeded"` or `"failed"`. |
| `exit_code` | integer | Yes | Primary exit code of the agent process or failure indicator. |
| `summary` | string | No | High-level summary of actions performed. |
| `proposed_memories`| array | No | Structured memory proposals for evidence-backed persistence. |
| `verification` | object | No | Results from the deterministic verification gate. |
| `verification.passed` | boolean | Yes (if gate ran) | True if verification command exited with code 0. |
| `verification.exit_code` | integer | Yes (if gate ran) | Process exit code of the verification command. |
| `verification.output` | string | No | Truncated stdout/stderr output from verification (up to 64 KiB). |

---

## Execution Lifecycle and Error Codes

The execution follows a strict lifecycle managed by `buddy-runner agent-run`:

```
                  ┌──────────────────────────────────────────────┐
                  │ 1. Gateway writes /run/buddy/task.json,      │
                  │    prompt.txt, and system-prompt.txt         │
                  └──────────────────────┬───────────────────────┘
                                         │
                                         ▼
                  ┌──────────────────────────────────────────────┐
                  │ 2. Supervisor validates contract version     │
                  │    and runs /etc/buddy/agent/run.sh          │
                  └──────────────────────┬───────────────────────┘
                                         │
                            Harness exits (code H)
                                         │
                                         ▼
                  ┌──────────────────────────────────────────────┐
                  │ 3. Supervisor Verification Gate              │
                  │    - Runs verify.command in isolated group   │
                  │    - Enforces verify.timeout (exit code 124) │
                  └──────────────────────┬───────────────────────┘
                                         │
                            Gate exits (code V)
                                         │
                                         ▼
                  ┌──────────────────────────────────────────────┐
                  │ 4. Write /run/buddy/result.json              │
                  │    - status = succeeded ONLY if              │
                  │      H == 0 AND (V == 0 or no verify)        │
                  └──────────────────────────────────────────────┘
```

### Exit Codes and Retry Semantics

| Exit Code | Cause | Dispatcher Classification |
|---|---|---|
| `0` | Harness and verification succeeded | Success |
| `78` (`EX_CONFIG`) | Unsupported `spec_version` or missing agent entrypoint | Non-retryable failure (stops immediate burn of task retries) |
| `124` | Verification command exceeded configured timeout | Failed verification gate |
| Other `!= 0` | Harness or verification execution error | Retryable task failure (up to `max_attempts`) |

### Verification Gate Invariants

1. Verification is strictly supervisor-enforced. Verification commands are never fed as prompt instructions to the model.
2. If the agent harness exits 0, but the verification command fails (exit code != 0 or timeout exceeded), `status` is marked `"failed"` and task execution fails with `verification gate failed`.
3. If the agent harness exits non-zero, the verification command is skipped, and `status` is marked `"failed"` with the harness exit code.
4. If no verification command is configured in `task.json`, the execution status directly reflects harness exit code 0.
