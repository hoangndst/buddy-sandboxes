# Headless Task Dispatch and Automated Verification Gates

## Overview

This design establishes autonomous, non-interactive task execution and verification in Buddy Sandboxes. Developers and external automation systems can dispatch coding tasks targeting persistent workspaces or warm standby runners, stream execution logs in real time, enforce independent verification gates to prevent premature completion claims by AI agents, and inspect unified git diffs directly via the CLI.

---

## 1. Problem Statement and Motivation

1. **Volume Immutability Barrier**: In Kubernetes, Pod volume mounts are immutable after container start. A running standby runner cannot mount a persistent volume claim (PVC) associated with an existing `Workspace`. The control plane must differentiate between tasks operating on persistent PVC-backed workspaces and tasks operating on ephemeral environments.
2. **Untrusted Agent Self-Reporting**: LLM coding agents frequently self-report completion (exit code 0) despite failing tests, compilation errors, or unresolved requirements. The platform must execute an automated, out-of-band verification command (`verification_command`) inside `/workspace` before marking any task attempt as successful.
3. **Execution Resilience & Detached Supervision**: Direct ephemeral exec connections between the gateway and runner pods are vulnerable to network blips and gateway restarts. The execution harness must run under the in-guest process supervisor (`buddy-runner`) in detached background mode so supervisor sessions survive gateway reconnections and buffer terminal output reliably.
4. **Deterministic Patch Capture & Memory Bounds**: Developers need immediate access to unified git diffs produced by agent attempts to evaluate code changes before merging. These diffs must be captured deterministically without shell injection vulnerabilities, bounded by stream memory limits, and exposed via standard CLI commands.
5. **Safe Post-Mortem Inspection**: Retaining active sandbox pods on failed attempts can lock single-occupancy PVC volumes and deadlock subsequent retry attempts. Failed sandboxes must be suspended or bounded by explicit TTLs, and never retained on intermediate retries.

---

## 2. Architecture and System Overview

```
+-------------------------------------------------------------------------------+
| DEVELOPER & AUTOMATION INTERFACE                                              |
| buddy-sbx create task / buddy-sbx get task <id> --diff / buddy-sbx logs task  |
+-------------------------------------------------------------------------------+
                                        |
                                        v
+-------------------------------------------------------------------------------+
| IN-CLUSTER GATEWAY (buddy-api)                                                |
| - REST API /api/v1/tasks, /api/v1/executions (with Diff in response schemas)  |
| - PostgreSQL Store: tasks, executions (diff, diff_stat), agent_profiles       |
| - TaskDispatcher: worker loop with RemoteStreamExecutor & Sweeper K8s cleanup |
+-------------------------------------------------------------------------------+
                                        |
                 +----------------------+----------------------+
                 |                                             |
   (Persistent Workspace: PVC)                    (Warm Pool: Ephemeral)
                 v                                             v
+----------------------------------+        +-----------------------------------+
| Dedicated Sandbox Custom Resource|        | SandboxLease -> SandboxPool       |
| - spec.workspaceRef: <name>      |        | - spec.poolRef: <pool-name>       |
| - Mounted PVC at /workspace      |        | - Pre-warmed standby runner       |
+----------------------------------+        +-----------------------------------+
                 \                                             /
                  +---------------------+---------------------+
                                        |
                                        v
+-------------------------------------------------------------------------------+
| IN-GUEST EXECUTION & SUPERVISOR (Sandbox Pod)                                 |
| 1. Stage /tmp/buddy/harness-<execID>.sh with prompt & environment variables   |
| 2. buddy-runner start --daemon harness-<execID> --dir /workspace              |
| 3. buddy-runner attach harness-<execID> --raw (stream logs & heartbeats)      |
| 4. buddy-runner stop harness-<execID> (terminate rogue background processes)  |
| 5. If exit code 0 -> stage & run /tmp/buddy/verify-<execID>.sh                |
| 6. Extract git diff via sanitized flags and io.LimitReader into DB diff col   |
| 7. Intermediate retry: delete sandbox immediately to unblock volume lock      |
|    Final failure: mark spec.suspend = true or set hard TTL annotation         |
+-------------------------------------------------------------------------------+
```

---

## 3. Database Schema and Persistence Layer

### 3.1 Migration `007_execution_diff.sql`

To capture the complete unified patch alongside existing diff statistics:

```sql
ALTER TABLE executions ADD COLUMN IF NOT EXISTS diff text;
```

`internal/store/postgres.go#RequireCurrentSchema` is updated to verify migration `007_execution_diff`.

### 3.2 Model and Store Updates (`internal/store/tasks.go`)

`ExecutionResult` and `Execution` structures include `Diff`:

```go
type ExecutionResult struct {
    Status             string
    ExitCode           *int
    VerificationPassed *bool
    VerificationOutput string
    DiffStat           string
    Diff               string
    ErrorMessage       string
    LogBuffer          string
}

type Execution struct {
    ID                 string
    TaskID             string
    Namespace          string
    WorkspaceName      string
    AttemptNumber      int
    WorkerID           *string
    LeaseName          *string
    SandboxName        *string
    Status             string
    ExitCode           *int
    VerificationPassed *bool
    VerificationOutput *string
    DiffStat           *string
    Diff               *string
    ErrorMessage       *string
    LogBuffer          *string
    StartedAt          *time.Time
    LastHeartbeatAt    *time.Time
    CompletedAt        *time.Time
    CreatedAt          time.Time
}
```

SQL statements in `CompleteExecution`, `GetExecution`, and `ListExecutionsByTask` are updated to project and scan `diff`:

```sql
-- CompleteExecution
UPDATE executions
SET status = CASE WHEN status = 'cancelled' THEN status ELSE $2 END,
    exit_code = $3,
    verification_passed = $4,
    verification_output = $5,
    diff_stat = $6,
    diff = $7,
    error_message = $8,
    log_buffer = $9,
    completed_at = NOW()
WHERE id = $1;
```

Both `diff_stat` and `diff` are capped at 64 KB using UTF-8-safe truncation (`truncateOutput`) to prevent unbounded database memory growth.

### 3.3 Public API Schema Updates (`pkg/api/v1/types.go`)

`apiv1.ExecutionResponse` includes the unified diff:

```go
type ExecutionResponse struct {
    ID                 string     `json:"id"`
    TaskID             string     `json:"taskId"`
    Namespace          string     `json:"namespace"`
    WorkspaceName      string     `json:"workspaceName"`
    AttemptNumber      int        `json:"attemptNumber"`
    Status             string     `json:"status"`
    ExitCode           *int       `json:"exitCode,omitempty"`
    VerificationPassed *bool      `json:"verificationPassed,omitempty"`
    VerificationOutput *string    `json:"verificationOutput,omitempty"`
    DiffStat           *string    `json:"diffStat,omitempty"`
    Diff               *string    `json:"diff,omitempty"`
    ErrorMessage       *string    `json:"errorMessage,omitempty"`
    StartedAt          *time.Time `json:"startedAt,omitempty"`
    CompletedAt        *time.Time `json:"completedAt,omitempty"`
    CreatedAt          time.Time  `json:"createdAt"`
}
```

`executionToResponse` in `internal/gateway/task_handlers.go` maps `Diff: e.Diff`.

---

## 4. Runner Provisioning and Lifecycle Management

The dispatcher uses a unified `RunnerProvisioner` abstraction:

```go
type RunnerInstance interface {
    PodName() string
    SandboxName() string
    Namespace() string
    Release(ctx context.Context, attemptNumber int, maxAttempts int, success bool) error
}

type RunnerProvisioner interface {
    AcquireRunner(ctx context.Context, task *store.Task, exec *store.Execution, profile *store.AgentProfile) (RunnerInstance, error)
}
```

### 4.1 Dedicated Sandbox Provisioning (Persistent Workspace)
When `task.WorkspaceName` refers to an existing `Workspace` CR:
1. Dispatcher constructs a `Sandbox` custom resource:
   - Name: `sbx-task-<execID>`
   - Namespace: `task.Namespace`
   - `spec.workspaceRef = &corev1.LocalObjectReference{Name: task.WorkspaceName}`
   - `spec.image`: resolved from profile harness type (`buddy-sandboxes-codex:local`, `buddy-sandboxes-claude-code:local`, or `buddy-sandboxes-base:local`).
   - `spec.network.allowAPIBroker`: enabled when the profile harness corresponds to a model provider (e.g. OpenAI for Codex, Anthropic for Claude), issuing a broker capability token and activating it on creation.
2. Dispatcher creates the resource via Kubernetes API client.
3. If provider credentials are required, activates the pending broker capability against the newly created Sandbox UID.
4. Dispatcher polls for `Sandbox` phase `Running` with a strict 180-second readiness timeout. On timeout, the execution fails with `readiness timeout waiting for sandbox`.
5. **Lifecycle and Retention Policy on Completion**:
   - `Release()` revokes any issued broker capabilities to ensure credentials cannot be reused.
   - If execution status is `succeeded`: `Release()` deletes the `Sandbox` resource immediately.
   - If execution status is `failed` and `attemptNumber < maxAttempts`: `Release()` deletes the `Sandbox` immediately and waits for Pod termination to clear `ws.Status.BoundSandboxRef`. This prevents retry deadlocks on `ReadWriteOnce` persistent volumes.
   - If execution status is `failed` and `attemptNumber == maxAttempts` (final failure): `Release()` patches `spec.suspend = true` on the Sandbox and sets annotations `sandboxes.buddy.io/retained-for-debug=true` and `sandboxes.buddy.io/retained-until=<now+1h>`. Suspending releases the workspace volume lock while preserving storage for post-mortem debugging.

### 4.2 Leased Runner Provisioning (Warm Pool)
When a task references a `SandboxPool` (or operates on ephemeral scratch space):
1. Dispatcher creates a `SandboxLease` custom resource:
   - Name: `lease-<execID>`
   - `spec.poolRef = {Name: poolName}`
   - `spec.executionId = exec.ID`
   - `spec.timeoutSeconds`: calculated from profile timeouts.
2. Dispatcher waits for lease phase `Bound` (timeout 60 seconds).
3. Pod name is retrieved from `lease.Status.BoundPodName`.
4. On completion, `Release()` patches `spec.released = true`.

---

## 5. In-Guest Execution and Supervisor Protocol

Execution proceeds through the in-guest supervisor `/opt/buddy/bin/buddy-runner` using detached background sessions:

### 5.1 Constructor Wiring (`TaskDispatcher`)
`TaskDispatcher` receives `RemoteStreamExecutor`:
```go
func NewTaskDispatcher(
    taskStore TaskStore,
    k8sClient client.Client,
    executor RemoteStreamExecutor,
    workerID string,
) *TaskDispatcher
```
In `internal/gateway/server.go`, `cfg.RemoteExecutor` is passed directly to `NewTaskDispatcher`.

### 5.2 Detached Harness Startup
To avoid SPDY exec connection blocking or process termination upon gateway network interruption:
1. The dispatcher formats the harness wrapper script `/tmp/buddy/harness-<execID>.sh` containing safe environment variables (`BUDDY_TASK_PROMPT`, `BUDDY_MODEL_NAME`, `BUDDY_MAX_STEPS`) and executes it.
2. The supervisor is launched in detached mode:
   ```bash
   /opt/buddy/bin/buddy-runner start --daemon harness-<execID> --dir /workspace -- /bin/sh /tmp/buddy/harness-<execID>.sh
   ```
3. The dispatcher confirms exit code 0 from the start command (supervisor daemon initialized).
4. The dispatcher attaches to the session socket:
   ```bash
   /opt/buddy/bin/buddy-runner attach harness-<execID> --raw
   ```
   Streaming stdout and stderr are ingested into the execution log buffer. A parallel ticker calls `store.UpdateExecutionHeartbeat` every 10 seconds.
5. Upon agent harness exit, the dispatcher issues:
   ```bash
   /opt/buddy/bin/buddy-runner stop harness-<execID>
   ```
   This terminates lingering background child processes (such as rogue daemons or file watchers) to prevent port and file lock contention during verification.

---

## 6. Independent Verification Gate and Diff Extraction

### 6.1 Independent Verification Gate
If `profile.VerificationCommand` is configured:
1. The dispatcher transitions execution status in PostgreSQL to `verifying`.
2. The verification script is written to `/tmp/buddy/verify-<execID>.sh` with mode `0700` to prevent shell quote interpolation errors.
3. The dispatcher executes:
   ```bash
   /opt/buddy/bin/buddy-runner start --daemon verify-<execID> --dir /workspace -- /bin/sh /tmp/buddy/verify-<execID>.sh
   ```
4. The dispatcher attaches to `verify-<execID>` with a strict timeout context (`profile.VerificationTimeoutSeconds`, default 300s).
5. If the verification timeout expires:
   - The dispatcher issues `/opt/buddy/bin/buddy-runner stop verify-<execID> --signal=SIGKILL`.
   - Execution is marked `failed` with error `verification command timed out`.
6. If the command exits within deadline:
   - Exit code 0: `verification_passed = true`.
   - Exit code != 0: `verification_passed = false`, execution status is set to `failed`.

### 6.2 Sanitized Git Diff Extraction
Before tearing down or releasing the runner, the dispatcher runs diff extraction inside `/workspace`:
1. Check if `/workspace` is a git repository:
   ```bash
   git -C /workspace rev-parse --is-inside-work-tree
   ```
   If this command exits non-zero, diff capture logs a descriptive warning (`workspace is not a git repository`) and proceeds without failing the execution.
2. Check if HEAD exists:
   ```bash
   git -C /workspace rev-parse --verify HEAD
   ```
   If HEAD does not exist (empty repository with zero commits), diff against the empty tree object `4b825dc642cb6eb9a060e54bf8d69288fbee4904`.
3. Stage untracked files intent-to-add:
   ```bash
   git -C /workspace add -A -N
   ```
4. Capture stat and unified diff with external drivers disabled to prevent malicious `.git/config` execution:
   ```bash
   git -C /workspace -c diff.external= -c core.fsmonitor= diff --stat HEAD
   git -C /workspace -c diff.external= -c core.fsmonitor= diff HEAD
   ```
5. Stdout streams are consumed through `io.LimitReader(stdout, 65536)` to protect gateway memory, and stored in `executions.diff_stat` and `executions.diff`.

---

## 7. Developer CLI Command Hierarchy

The CLI adheres to Kubernetes verb-first standards:

### 7.1 `buddy-sbx create task`
Implemented in `pkg/cli/sbx/create.go`:
```bash
buddy-sbx create task --workspace repo-auth --prompt "Fix race condition in session manager" [--profile golang-default] [--branch main] [--max-attempts 2]
```
- Flags:
  - `--workspace`, `-w`: Target workspace name (required).
  - `--prompt`, `-p`: Natural language developer instruction (required).
  - `--profile`: Agent profile ID or name (optional).
  - `--branch`, `-b`: Target git branch (optional).
  - `--max-attempts`: Maximum retry attempts (optional, default 1).
  - `--namespace`, `-n`: Target namespace (optional).
- Generates a client-side UUID idempotency key and invokes `client.CreateTask`.

### 7.2 `buddy-sbx get task <id> [--diff]`
Implemented in `pkg/cli/sbx/get.go`:
```bash
# Table or wide details
buddy-sbx get tasks
buddy-sbx get task <task-id>

# Print unified diff patch
buddy-sbx get task <task-id> --diff
```
- When `--diff` is set on `get task <id>`:
  - Inspects `task.ActiveExecutionID`. If null, prints `No active execution for task <id>`.
  - Calls `client.GetExecution(ctx, *task.ActiveExecutionID)`.
  - If `exec.Diff` is non-nil and non-empty, outputs the raw patch directly to stdout.
  - If empty, prints `No diff available for execution <id> (status: <status>)`.

### 7.3 `buddy-sbx logs task <id> [-f]`
Implemented in `pkg/cli/sbx/logs.go`:
```bash
buddy-sbx logs task <task-id>
buddy-sbx logs task <task-id> -f
buddy-sbx logs execution <execution-id>
```
- Updates Cobra argument validation to `cobra.RangeArgs(1, 2)`.
- If `args[0] == "task"`, resolves `task.ActiveExecutionID` and retrieves execution logs.
- For `-f` follow mode: polls execution logs every 1 second until the execution enters a terminal state (`succeeded`, `failed`, `cancelled`), printing new lines incrementally.

---

## 8. Security Boundaries and Failure Modes

1. **Workspace Mutual Exclusion**:
   The unique index `executions_active_workspace_idx` on `(namespace, workspace_name)` guarantees that only one task attempt can execute against a persistent workspace at any given time. Conflicting submissions remain queued in `scheduled` status.
2. **K8s Sweeper Cleanup Protocol**:
   When `TaskDispatcher.sweeperLoop` detects an orphaned execution via `ReapOrphanExecutions`, it performs a Kubernetes cleanup pass:
   - For dedicated sandboxes: issues `client.Delete` on `orphan.SandboxName` and waits for pod termination to unmount the PVC.
   - For leased sandboxes: patches `SandboxLease.spec.released = true`.
   Only after Kubernetes storage release is verified does the next retry attempt proceed to claim the workspace.
3. **Container Isolation**:
   All execution processes, including verification tests and git diff tools, run strictly inside the sandbox container under gVisor and PSA restricted security constraints.
4. **Retention TTL Sweeper**:
   Sandboxes suspended for post-mortem debugging with `sandboxes.buddy.io/retained-for-debug=true` carry `sandboxes.buddy.io/retained-until`. A background sweeper running every 5 minutes deletes expired debug sandboxes.

---

## 9. Acceptance Criteria and KinD Verification Plan

1. **Unit and Integration Tests**:
   - `internal/store/tasks_test.go`: Verify migration `006_execution_diff.sql`, diff column scan and update, truncation to 64 KB, and `RequireCurrentSchema` version bump.
   - `internal/gateway/task_dispatcher_test.go`: Verify runner acquisition, daemonized supervisor execution, verification gate pass/fail paths, sanitized git diff extraction, and sweeper K8s cleanup.
   - `pkg/cli/sbx/create_test.go`, `get_test.go`, `logs_test.go`: Verify `create task`, `get task --diff`, and `logs task` Cobra execution.
2. **Local KinD Cluster Verification (`dell-cluster`)**:
   - Build operator container: `make docker-build IMG=buddy-sandboxes-controller:local`.
   - Load image into KinD: `kind load docker-image buddy-sandboxes-controller:local --name dell-cluster`.
   - Apply CRDs and deploy controller: `make install && make deploy IMG=buddy-sandboxes-controller:local`.
   - Deploy `buddy-api` gateway with task dispatcher enabled.
   - **Verification Scenario A (Success & Diff)**:
     - Apply a test `Workspace` CR with seeded git repository.
     - Execute `buddy-sbx create task --workspace test-ws --prompt "Add hello.txt" --verify "test -f hello.txt"`.
     - Verify task transitions `scheduled` -> `running` -> `verifying` -> `succeeded`.
     - Run `buddy-sbx get task <id> --diff` and verify the patch contains `+hello.txt`.
     - Verify Sandbox CR was deleted upon success.
   - **Verification Scenario B (Verification Failure & Debug Retention)**:
     - Execute task with failing verification: `--verify "exit 1"`.
     - Verify task transitions to `failed`, `verification_passed = false`.
     - Verify Sandbox is suspended (`spec.suspend = true`) and labeled `sandboxes.buddy.io/retained-for-debug=true`.
