# Process Supervisor & Reconnectable Sessions

**Status**: APPROVED DESIGN SPECIFICATION  
**Scope**: In-guest Process Supervisor (`buddy-runner`), Gateway `SessionHub` Multiplexer, Reconnectable Terminal Sessions, and CLI Detach/Attach  
**Target Package**: `cmd/buddy-runner`, `internal/gateway`, `internal/store`, `cmd/buddy-sbx`  

---

## 1. Problem Statement

In autonomous agent systems and interactive developer sandboxes, long-running processes (such as package installations, multi-stage compilation, test suites, or autonomous coding loops) outlast transient client connections.

Traditional container execution approaches (`kubectl exec`, SPDY streams, raw WebSocket pipes) introduce three systemic points of failure:
1. **Network Disconnection Aborts Execution**: Dropping the client TCP connection (due to Wi-Fi handoffs, network timeouts, or laptop lid closes) causes the container runtime to send SIGHUP or terminate the exec channel, killing running commands mid-execution.
2. **Output Stream Loss**: Standard exec channels do not buffer historical output. A disconnected client loses execution logs and cannot inspect current progress upon reconnecting.
3. **No Human-in-the-Loop Multiplexing**: Only a single client can attach to an exec process. A human developer cannot inspect or take over an autonomous agent's terminal session without disrupting execution.

Buddy Sandboxes addresses this with an in-guest process supervisor (`buddy-runner`) and a gateway session multiplexer (`SessionHub`). Workloads execute inside dedicated pseudo-terminals (PTYs) with in-memory ring buffering, decoupled from incoming network connections, and accessible via detached interactive sessions.

---

## 2. System Architecture & Topology

```
+-----------------------------------------------------------------------------------+
| Developer CLI / Web UI                                                            |
|  - buddy-sbx exec <sandbox> --detach -- <cmd>                                     |
|  - buddy-sbx run <preset|image> [path] --detach -- <cmd>                          |
|  - buddy-sbx session list <sandbox>                                               |
|  - buddy-sbx session inspect <sandbox> <session-id>                               |
|  - buddy-sbx attach <sandbox> <session-id> [--take-lease]                         |
+-----------------------------------------------------------------------------------+
                                   |
                HTTP REST & WebSocket (apiv1 Channels 0..4)
                                   v
+-----------------------------------------------------------------------------------+
| BUDDY GATEWAY (buddy-api)                                                         |
|                                                                                   |
|  Client 1 (Active Writer: Leaseholder) <---+                                      |
|                                            +---> [ SessionHub Multiplexer ]       |
|  Client 2 (Observer: Read-Only)        <---+            |                         |
|                                                         |                         |
|  - PostgreSQL Metadata: sandbox_sessions table          | 1x StreamExec           |
|  - Stdin Lease & SIGWINCH Arbitration                   | (Kubelet CRI Stream)    |
|  - Background Zombie & Pod State Sweeper                |                         |
+---------------------------------------------------------|-------------------------+
                                                          v
+-----------------------------------------------------------------------------------+
| SANDBOX POD (Default-Deny Ingress NetworkPolicy)                                 |
|                                                                                   |
|  [ buddy-runner attach <session-id> ] (Invoked via StreamExec)                   |
|         |                                                                         |
|         | Local UNIX Domain Socket (/tmp/buddy/<id>.sock, mode 0600)              |
|         v                                                                         |
|  [ buddy-runner start <session-id> -- <cmd> ] (Supervisor Process)                |
|    - Pure-Go PTY Allocation (github.com/creack/pty, setsid)                       |
|    - 1MB Circular Output Ring Buffer                                              |
|    - Child Process (e.g. npm test, agent loop)                                    |
|    - Exit Tombstone Flusher (/tmp/buddy/<id>.exit, mode 0600)                     |
|                                                                                   |
|  Mounted Tooling:                                                                 |
|  - /opt/buddy/bin/buddy-runner (ReadOnly emptyDir from buddy-tools-init)          |
|  - /tmp/buddy (Shared emptyDir scratch storage, mode 0700, UID 1000)             |
+-----------------------------------------------------------------------------------+
```

### Component Roles

1. **`buddy-runner` (Guest Supervisor)**:
   A lightweight, statically compiled (`CGO_ENABLED=0`) pure-Go binary delivered into the sandbox pod. It provides two operational subcommands:
   - `start <session-id> -- <cmd...>`: Allocates a Unix pseudo-terminal (`github.com/creack/pty`), forks the target command as a child session leader (`setsid`), binds a local UNIX domain socket at `/tmp/buddy/<session-id>.sock`, and runs an internal ingestion loop copying PTY output into a 1MB circular ring buffer. Upon process termination, writes exit metadata to `/tmp/buddy/<session-id>.exit`.
   - `attach <session-id>`: Connects to the local UNIX domain socket, receives historical output replay, and pipes bidirectional stdin/stdout to the caller using standard channel headers.

2. **`SessionHub` (Gateway Multiplexer)**:
   An in-memory multiplexer within `buddy-api`. For any active session, the Gateway establishes at most **one** upstream `StreamExec` connection to the pod running `buddy-runner attach <session-id>`. When multiple WebSocket clients connect, the Gateway broadcasts output to all clients, arbitrates input through an exclusive write lease, and tracks session state in PostgreSQL.

3. **`buddy-tools-init` (Init Container Binary Injector)**:
   An ephemeral init container that copies the static `buddy-runner` binary from the controller image into a shared `emptyDir` volume mounted read-only at `/opt/buddy/bin` in the application container. This provides full process supervision across arbitrary developer images (Alpine musl, Debian glibc, Distroless) with zero base image modification.

---

## 3. REST API Surface

The Gateway exposes standard REST endpoints for managing detached sessions:

### 3.1 Endpoints

- **`POST /api/v1/sandboxes/:name/sessions`**: Launch a new detached session.
  - **Request Body**:
    ```json
    {
      "command": ["npm", "test"],
      "working_dir": "/workspace",
      "tty": true,
      "metadata": {"task": "ci-suite"}
    }
    ```
  - **Behavior**: Gateway generates a unique session ID (`sess_<uuid>`), inserts a record with state `pending` in `sandbox_sessions`, launches `buddy-runner start <session-id> -- <cmd>` in the background via non-blocking container execution, and returns HTTP 201 Created.
  - **Response (201 Created)**:
    ```json
    {
      "id": "sess_01j7abc...",
      "namespace": "sbx-hoang",
      "sandbox_name": "agent-workspace",
      "command": ["npm", "test"],
      "working_dir": "/workspace",
      "tty": true,
      "state": "running",
      "created_at": "2026-11-24T12:00:00Z"
    }
    ```

- **`GET /api/v1/sandboxes/:name/sessions`**: List all sessions for a sandbox.
  - **Response (200 OK)**:
    ```json
    {
      "sessions": [
        {
          "id": "sess_01j7abc...",
          "command": ["npm", "test"],
          "state": "running",
          "created_at": "2026-11-24T12:00:00Z"
        }
      ]
    }
    ```

- **`GET /api/v1/sandboxes/:name/sessions/:id`**: Inspect session details.
  - Returns detailed status, exit code, start/completion timestamps, and metadata.
  - If the session was running detached and unattended, the Gateway reads the `/tmp/buddy/<id>.exit` tombstone to update PostgreSQL to `completed` or `failed`.

- **`DELETE /api/v1/sandboxes/:name/sessions/:id`**: Terminate a session.
  - Gateway issues SIGTERM (followed by SIGKILL after grace period) to the child process group via `buddy-runner`, updates state to `terminated` in PostgreSQL, and returns 200 OK.

- **`GET /api/v1/sandboxes/:name/sessions/:id/stream`**: Interactive WebSocket stream.
  - Upgrades connection to WebSocket. Supports query parameters `?mode=rw` (default), `?mode=ro`, and `?take_lease=true`.
  - Replays historical scrollback from ring buffer, then streams live interactive terminal I/O.

---

## 4. Wire Protocol & Streaming Specifications

Buddy Sandboxes retains the established 1-byte multiplexing channel header from `pkg/api/v1/types.go` for all WebSocket and UNIX domain socket streams:

```
+---------------+-----------------------------------------------+
| Channel (1B)  | Payload Data (N Bytes)                        |
| 0: Stdin      | Raw terminal input keystrokes                 |
| 1: Stdout     | Terminal output stream / historical replay    |
| 2: Stderr     | Process stderr stream / system warnings       |
| 3: Exit       | JSON payload: {"exitCode": int, "error": str} |
| 4: Resize     | JSON payload: {"cols": int, "rows": int}      |
+---------------+-----------------------------------------------+
```

### 4.1 Scrollback Replay Protocol
When a client connects to `GET /api/v1/sandboxes/:name/sessions/:id/stream`:
1. **Initial Burst**: The supervisor transmits up to 1MB of buffered terminal output sequentially over Channel 1 (`Stdout`).
2. **Live Streaming Transition**: As soon as the historical buffer is flushed, the client automatically receives real-time live terminal output.
3. **Buffer Wrap Handling**: If a client requests resumption from an offset that has already been overwritten by the circular buffer, the server transmits a notification over Channel 2 (`Stderr`):
   ```
   [WARN: output buffer wrapped, replaying latest 1MB of terminal scrollback]
   ```
4. **Clean Exit**: When the child process terminates, the server emits a final Channel 3 (`Exit`) frame containing the child's exit code, and closes the connection.

### 4.2 Multi-Client Input & Resize Arbitration
To avoid keystroke interleaving and visual curses screen corruption when multiple viewers attach:
- **Exclusive Write Lease**: The first client attaching with write permissions holds the write lease. Subsequent clients attach in read-only observer mode (`?mode=ro`).
- **Lease Takeover**: A client may pass `?take_lease=true` (or `buddy-sbx attach --take-lease`) to take over the active write lease. The existing leaseholder is demoted to read-only observer with a notification frame on Channel 2.
- **Input Filtering**: Channel 0 (`Stdin`) frames from read-only observers are dropped by the Gateway with an advisory notice.
- **Resize Authorization**: Only the active write leaseholder is permitted to transmit Channel 4 (`Resize`) frames. Window resize signals from observers are ignored by the Gateway, preventing `SIGWINCH` resize storms.

### 4.3 Startup Rendezvous & Socket Probe Retry
When a session is created, `buddy-runner start` takes a brief moment to initialize the PTY, create the socket directory, and bind `/tmp/buddy/<id>.sock`.
- When `buddy-runner attach` or the Gateway probes the socket, it employs an exponential backoff retry loop (up to 3 seconds with 100ms initial interval).
- Only if the socket remains absent after 3 seconds does the Gateway declare the session failed and return HTTP 410 Gone.

---

## 5. Database Schema & State Reconciliation

Session state is persisted in PostgreSQL via migration `004_sessions.sql`.

### 5.1 Table Schema (`sandbox_sessions`)
```sql
CREATE TABLE IF NOT EXISTS sandbox_sessions (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    namespace text NOT NULL,
    sandbox_name text NOT NULL,
    command jsonb NOT NULL,
    working_dir text NOT NULL DEFAULT '/workspace',
    tty boolean NOT NULL DEFAULT true,
    state text NOT NULL CHECK (state IN ('pending', 'running', 'completed', 'failed', 'terminated')),
    exit_code integer,
    pid integer,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS sandbox_sessions_sandbox_idx
    ON sandbox_sessions(namespace, sandbox_name, state);

CREATE INDEX IF NOT EXISTS sandbox_sessions_user_idx
    ON sandbox_sessions(user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS sandbox_sessions_active_idx
    ON sandbox_sessions(state) WHERE state IN ('pending', 'running');
```

### 5.2 Strict Architectural Decoupling: Controller vs Gateway
- **Controller Decoupling**: The Kubernetes controller manager (`internal/controller`) has **zero** database connection, zero pgx dependencies, and no awareness of the PostgreSQL store. Controller manager pods operate strictly against the Kubernetes API.
- **Gateway Reconciliation**: All database state reconciliation is handled within `buddy-api`:
  1. **Attach Health Probe**: When a client attaches to `/stream`, Gateway probes the in-guest socket `/tmp/buddy/<id>.sock`. If missing and a tombstone `/tmp/buddy/<id>.exit` exists, state is reconciled to `completed` or `failed`.
  2. **Background Zombie Sweeper**: A periodic background goroutine inside `buddy-api` sweeps sessions marked `running` or `pending` whose target Kubernetes Pod is deleted or in a terminal phase (`Succeeded`, `Failed`), updating their database state to `terminated`.

---

## 6. Pure-Go PTY & In-Guest Architecture

### 6.1 `github.com/creack/pty`
To guarantee static binary portability across all Linux distributions without C library dependencies:
- PTY allocation, slave pairing, and terminal window sizing are implemented using `github.com/creack/pty`.
- The binary is built with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`.
- Process supervision uses `syscall.SysProcAttr{Setsid: true, Setpgid: true}` to ensure the child process is an independent session leader that never receives SIGHUP when parent connections sever.

### 6.2 Ring Buffer & Kernel PTY Decoupling
Linux kernel PTY buffers are constrained to 4096 bytes. If a client reading from the PTY hangs or reads slowly, the child process blocks on `write()`:
- `buddy-runner start` runs a dedicated high-priority goroutine that reads from the PTY master file descriptor into a fixed 1MB circular in-memory ring buffer.
- Attached UDS clients are dispatched output through bounded Go channels (e.g. 64KB queue).
- If a client channel overflows, the client is dropped with a buffer overrun notice, ensuring the primary child process and PTY master loop never stall.

### 6.3 Exit Tombstone Flusher
When the supervised child process terminates:
1. `buddy-runner` waits for child exit status via `syscall.Wait4`.
2. Writes an exit tombstone file `/tmp/buddy/<session-id>.exit` containing:
   ```json
   {
     "exitCode": 0,
     "startedAt": "2026-11-24T12:00:00Z",
     "completedAt": "2026-11-24T12:05:00Z"
   }
   ```
3. Closes client socket connections with Channel 3 (`Exit`) frame.
4. Leaves the `.exit` file and `.sock` in place for a 1-hour grace window so subsequent `session inspect` or `attach` calls can retrieve final status and logs.

---

## 7. Developer CLI Interface (`buddy-sbx`)

The developer CLI provides seamless commands for managing detached execution and interactive sessions:

```bash
# Execute command detached in an existing sandbox
buddy-sbx exec <sandbox> --detach -- npm test

# Launch a new sandbox and run a detached session immediately
buddy-sbx run node:20 /workspace --detach -- npm run build

# List all sessions in a sandbox
buddy-sbx session list <sandbox>

# Inspect a specific session status and exit code
buddy-sbx session inspect <sandbox> <session-id>

# Re-attach to an active session
buddy-sbx attach <sandbox> <session-id>

# Re-attach and claim the active write lease
buddy-sbx attach <sandbox> <session-id> --take-lease

# Interactive Detach Escape Sequence
# While attached to an interactive session, pressing `Ctrl+]` detaches the terminal cleanly
# without terminating the in-guest process.
```

---

## 8. Security Boundaries & Isolation Invariants

1. **Default-Deny Ingress Preservation**: All communication between `buddy-api` and `buddy-runner` uses UNIX domain sockets in `/tmp/buddy/` accessed via standard container execution (`StreamExec`). No TCP ports are opened in the pod, preserving Kubernetes NetworkPolicy default-deny ingress.
2. **Filesystem Isolation**: The UNIX socket directory `/tmp/buddy` is created with mode `0700` owned by UID 1000. Sockets and tombstones are created with mode `0600`, preventing unauthorized processes from interacting with sessions.
3. **Immutable Tooling Mount**: The `buddy-tools` volume is mounted read-only (`readOnly: true`) in the runner container. Untrusted workloads cannot modify, overwrite, or delete `buddy-runner`.
4. **Non-Root Execution**: `buddy-runner` executes strictly under UID 1000 with dropped capabilities (`ALL`) and `readOnlyRootFilesystem: true`.

---

## 9. Acceptance Criteria

1. **Detached Execution**:
   - `buddy-sbx exec <sandbox> --detach -- sh -c 'sleep 2; echo done'` exits immediately with a session ID.
   - Querying session status after 3 seconds reports `state: "completed"` and `exit_code: 0`.
2. **Reconnection & Replay**:
   - Disconnecting an active terminal session during output emission and re-attaching with `buddy-sbx attach <sandbox> <session-id>` delivers complete scrollback history before resuming live interactive I/O.
3. **Multi-Client Observation**:
   - Multiple clients attaching to the same session receive identical broadcast stdout.
   - Secondary observer keystrokes do not interfere with the active leaseholder.
4. **Quality Gates & Invariants**:
   - Statically compiled Go binary (`CGO_ENABLED=0`) verified on Alpine musl and Debian glibc sandboxes.
   - `make fmt vet && make test && make lint` passes with zero regressions.
   - KinD cluster live verification on `dell-cluster` validates detached execution survival across client disconnects.
