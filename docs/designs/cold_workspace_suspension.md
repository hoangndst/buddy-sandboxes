# Cold Workspace Suspension and Resume

## Problem

A running Sandbox consumes Pod compute even when its owner no longer needs an interactive session. The controller currently recreates a missing Pod, so deleting a Pod cannot suspend a Sandbox. Deleting a Pod also destroys its `emptyDir` workspace. The public API has no suspend or resume operation.

The project cannot infer that arbitrary workloads are idle. The gateway can observe attached exec and port-forward clients, but a process inside the Pod can continue work after those clients disconnect. CPU usage also cannot distinguish valuable low-CPU work from inactivity. Version one therefore provides explicit cold suspend and resume operations for PVC-backed workspaces. Automatic idle-timeout suspension remains out of scope until supported workloads provide a busy/idle signal.

Cold suspension removes the execution Pod and its processes. Resume creates a new Pod and starts the configured workload again. The workspace PVC survives the Pod replacement. This is not process checkpointing, and commands may repeat external side effects when restarted.

## Goals

- Add idempotent explicit suspend and resume operations through REST and `buddy-sbx`.
- Preserve writable PVC workspace data across Pod removal and replacement.
- Represent desired suspension and observed transition state without changing execution outcome phases.
- Prevent the controller from recreating a Pod while suspension is desired.
- Keep the existing maximum-duration budget across Pod generations and suspension time.
- Return clear status and errors for ineligible, transitioning, or terminal Sandboxes.

## Non-Goals

- Automatically infer idle state or add `idleTimeoutSeconds` in version one.
- Wake a suspended Sandbox implicitly on `exec`, port-forward, upload, or download.
- Copy `emptyDir` into a PVC during suspension.
- Preserve process memory, open sockets, or running command state.
- Coordinate session draining through a distributed gateway session registry.
- Promise a fixed resume time across storage classes, schedulers, and image-pull states.

## User Journeys

### Suspend a writable PVC-backed Sandbox

- **Given** a nonterminal Sandbox using a writable external claim or a managed PVC.
- **When** a user runs `buddy-sbx suspend <name>` or calls the suspend REST endpoint.
- **Then** the request is accepted, the controller marks the transition, deletes the execution Pod with a 10 second grace period, waits for Pod disappearance, clears the current Pod identity, and reports stable `Suspended`. The PVC remains attached to the Sandbox lifecycle.

### Resume a suspended Sandbox

- **Given** a Sandbox in stable `Suspended` state whose deadline has not expired.
- **When** a user runs `buddy-sbx resume <name>` or calls the resume REST endpoint.
- **Then** the desired state changes to active, the controller waits for any old Pod to disappear, creates at most one replacement with the same workspace claim and remaining deadline, and reports active after the new Pod is Running. The configured command starts again.

### Use a suspended Sandbox

- **Given** a Sandbox whose suspend request is accepted or whose status is `Suspended` or `Resuming`.
- **When** a client requests exec, port-forward, upload, or download.
- **Then** the gateway returns HTTP 409 with a message directing the caller to resume first. No route silently wakes the Sandbox.

## API and State Model

### Desired state

Add optional `spec.suspend` boolean with default `false`. `true` requests suspension; `false` requests active operation. The field is mutable. The REST API and CLI are the supported user interfaces.

The existing `Sandbox.status.phase` remains `Pending`, `Running`, `Succeeded`, or `Failed`. Suspension is a separate lifecycle dimension so that it does not misrepresent workload completion. Add `status.suspensionPhase` with values:

| Value | Meaning |
| --- | --- |
| `None` | Active desired state; normal Pod reconciliation applies |
| `Suspending` | Suspend requested; Pod is being terminated or is still visible |
| `Suspended` | Suspend requested and no execution Pod exists |
| `Resuming` | Resume requested; the previous Pod is gone and a replacement is starting |

Add `status.podUID` for the current execution Pod. Clear `status.podName` and `status.podUID` after the Pod is gone. Populate them from the replacement Pod after observing it. Add write-once `status.firstPodStartTime` from the first Pod's `status.startTime`; it remains unchanged across resume. If suspension sees a started Pod before that origin is persisted, persist it first and defer Pod deletion until a later reconcile. Keep the established meaning of `status.startTime` as the time the controller first acknowledged the Sandbox. Set `completionTime` and `exitCode` only for terminal outcomes and preserve them after they are set.

A `SuspensionReady` condition reports stable active or suspended state. During transitions it is false with a transition reason. Invalid direct CRD updates, including suspension of terminal or ineligible Sandboxes, leave the Pod intact and report a false condition. The REST handlers reject those requests before updating desired state.

### Allowed transitions

| Current execution phase | Desired suspend | Controller behavior |
| --- | --- | --- |
| `Pending` or `Running`, active | true | Set `Suspending`, delete current Pod with 10 second grace, wait for NotFound, then set `Suspended` |
| `Pending` or `Running`, suspended | false | Set `Resuming`; wait for old Pod NotFound; create one replacement; set `None` when it is Running or reaches a terminal outcome |
| `Pending` or `Running`, transition | Same as current request | Idempotently continue toward the requested desired state |
| `Pending` or `Running`, transition | Opposite request | Latest accepted `spec.suspend` value wins; reconcile from the observed Pod state without creating a second Pod |
| `Succeeded` or `Failed` | either change | REST returns 409. Direct spec updates do not change terminal status or create/delete Pods; report an invalid transition condition |

When suspension is requested before a Pod has been created, the controller enters `Suspended` without creating one. If a suspend is canceled before Pod deletion begins, the controller can return to active without replacing the existing Pod. Once deletion starts, resume waits for the old Pod to become NotFound before creating a replacement. If replacement execution reaches Succeeded or Failed before Running is observed, clear `Resuming`, set `SuspensionReady=False` with a terminal reason, and preserve the execution result. Never force-delete a Pod.

### REST and CLI operations

Add:

- `POST /api/v1/sandboxes/{id}/suspend`
- `POST /api/v1/sandboxes/{id}/resume`

Both routes use the tenant namespace assigned by the existing authentication middleware, which maps the caller's bearer token to a namespace. The handler reads the Sandbox and updates `spec.suspend` using that object's `metadata.resourceVersion`; clients do not send a version. The handler performs one update attempt and returns `409 Conflict` if the resource changed concurrently. Repeating a request already reflected in `spec.suspend` is idempotent and does not issue an update. A request to the opposite state during a transition is accepted and becomes the latest desired state. Terminal or ineligible state returns `409 Conflict` with an actionable message. Successful requests return `202 Accepted` with the Sandbox representation once the desired state is recorded. The response includes `suspend`, `suspensionPhase`, and current Pod identity.

Add `buddy-sbx suspend <id>` and `buddy-sbx resume <id>`. Each command accepts `--timeout` with a default of `3m`. After the REST request returns `202 Accepted`, it polls `GET /api/v1/sandboxes/{id}` once per second. Suspend succeeds when `suspensionPhase` is `Suspended`; resume succeeds when `suspensionPhase` is `None` and execution `phase` is `Running`. Either command returns an error immediately if execution becomes `Succeeded` or `Failed`, the API returns an error, or the timeout expires. On timeout, the command reports that the desired state remains recorded and prints the latest Sandbox status. Timeout only ends client polling; it does not reverse the request.

`buddy-sbx exec` and `run` do not resume implicitly. Users must call `resume` first. While suspension is requested or the lifecycle is `Suspending`, `Suspended`, or `Resuming`, all new gateway operations that target the Pod, including exec, port-forward, logs, upload, and download, return HTTP 409.

## Storage Eligibility and Lifetime

The controller determines eligibility from the volume it actually mounts:

- A nonempty `spec.workspace.persistentVolumeClaim.claimName` is PVC-backed, unless `readOnly` is true.
- A managed PVC is used when `spec.workspace.ephemeral` is false and `spec.workspace.size` is nonempty.
- All other configurations mount `emptyDir` and cannot be suspended.

Reject suspension for read-only PVCs and for any effective `emptyDir` workspace. Add CRD validation for the schema-level cases and duplicate validation in REST. The controller repeats the check so bypass paths report a condition without deleting the Pod. Freeze `spec.workspace` after Sandbox creation so claim identity, mount path, and Git seeding behavior cannot change across Pod generations. Existing Sandboxes with omitted workspace fields remain valid but ineligible for suspension.

A managed PVC remains owned by the Sandbox and is deleted when the Sandbox is deleted. An external PVC remains separately owned and is not deleted with the Sandbox. Suspension preserves either claim while the Sandbox exists. Writable external claims may be shared; Kubernetes access modes do not provide a general exclusive-writer guarantee. Users must prevent other writers from changing the workspace during suspension and resume.

## Controller Behavior

The reconciler processes `spec.suspend` before normal Pod creation. In active mode it keeps current behavior. Before deletion, it observes any terminal Pod or terminated agent container and records that execution outcome instead. If needed, it first persists `firstPodStartTime`; it then persists `Suspending` as the commit point before requesting Pod deletion. If either status write fails, the Pod remains intact. A resume that wins before `Suspending` is persisted leaves the Pod active; after the commit point, resume waits for that Pod to become NotFound before creating a replacement. During suspension it requeues while a Pod exists or has a deletion timestamp and sets `Suspended` only after a `Get` returns NotFound. On resume it checks the same effective workspace claim, confirms the old deterministic Pod name is absent, then creates one replacement and reconciles as usual. It must handle `AlreadyExists` by fetching and reconciling that Pod rather than creating another.

Set `DeleteOptions.GracePeriodSeconds` to 10 for suspension. This permits ordinary stop-signal handling but can interrupt workloads that need longer cleanup. Clients must treat active operations as cancelable. Deleting the Pod ends existing exec, port-forward, upload, and download work. The gateway does not coordinate session draining across replicas in version one.

Gateway handlers for exec, port-forward, upload, and download check both `spec.suspend == false` and `status.suspensionPhase == None`, and require execution `phase == Running` before accessing the Pod. Logs use the suspension guard but remain available for active Pending, Running, Succeeded, and Failed Sandboxes while Kubernetes retains the Pod logs. A gateway request that reads active state just before a suspend request may race with Pod deletion and fail mid-operation. Requests that observe a suspend or resume transition are rejected with 409. Existing log-follow streams are not drained or restarted; Pod deletion closes their Kubernetes log stream and the HTTP response ends. Shared session state is not introduced.

## Deadline Across Pod Generations

`spec.maxDurationSeconds` remains set-once and tighten-only. Its clock starts at the first execution Pod's `status.startTime` and continues while the Sandbox is suspended. The controller records that timestamp once in `status.firstPodStartTime` and never replaces it with a later Pod start time. `status.startTime` retains its existing controller-acknowledgement meaning.

The absolute Sandbox deadline is `firstPodStartTime + maxDurationSeconds`. It is authoritative across every Pod generation and continues to elapse while suspended. The first Pod uses the full configured `activeDeadlineSeconds`; a replacement Pod receives the remaining budget rounded down to whole seconds at creation. Store the expected per-generation value in a controller-owned Pod annotation, `sandboxes.buddy.io/pod-deadline-seconds`, and compare the Pod field against that annotation rather than against the full Sandbox limit. When `maxDurationSeconds` is tightened, recalculate the absolute deadline from the same first Pod timestamp, reduce the current Pod deadline if needed, and update its annotation. Never increase a Pod deadline.

Kubernetes measures each Pod's `activeDeadlineSeconds` from that Pod's own start time, so it cannot enforce the original absolute Sandbox deadline by itself after a replacement starts later. The controller schedules reconciliation for the absolute deadline while a Pod is Pending or Running and while the Sandbox is Suspended. For a suspended Sandbox with no Pod, return `RequeueAfter = max(remaining, 1ms)` only when `maxDurationSeconds` and `firstPodStartTime` are both set and the deadline is still in the future. Recompute this from persisted status on each reconcile, including after controller restart. If the deadline is at or before the current time, inspect the current Pod before processing expiry. If the agent container's terminal `finishedAt` is at or before the deadline, preserve its execution outcome and use `finishedAt` as the Sandbox completion time, even when observation is delayed. If `finishedAt` is after the deadline or unavailable, mark the Sandbox `Failed` with reason `DeadlineExceeded`; do not overwrite a Sandbox outcome already persisted as terminal, and do not delete an already terminal Pod. Set the deadline condition and prevent further resume or Pod creation. The controller check also terminates a Pending replacement whose Pod-local deadline has not started. If less than one whole second remains when resuming, round down, mark the Sandbox `Failed`, and do not create a Pod. A Sandbox suspended before its first Pod starts has no execution clock yet; the first started Pod establishes it.

## Git Seeding on Resume

The existing `git-init` init container runs for every new Pod. Make the initialization rule safe for retained workspaces:

1. If the target is empty, clone using the configured repository settings.
2. If the target is a valid Git working tree, exit successfully without changing files or remote configuration.
3. If the target is nonempty and is not a valid Git working tree, fail with an actionable message and do not delete or modify its contents.

This rule avoids reseeding or overwriting edits. A partial initial clone that leaves an invalid nonempty target requires user recovery; the controller does not erase it or retry destructively.

## Failure Modes and Recovery

| Failure | Behavior and recovery |
| --- | --- |
| User suspends an effective `emptyDir` or read-only workspace | REST returns 409; direct CRD changes leave the Pod intact and set `SuspensionReady=False` with an eligibility reason |
| Suspend or resume requested after terminal completion | REST returns 409; terminal phase, exit code, and completion time stay unchanged |
| Pod deletion is slow | Keep `Suspending` or `Resuming` and requeue; do not report `Suspended` or create a replacement until the old Pod is NotFound |
| Pod deletion or replacement creation fails | Keep the transition state, set a false condition with the Kubernetes error, emit an event, and retry reconciliation |
| Resume cannot attach or bind the claim | Keep `Resuming`; normal Pod conditions report scheduling and volume errors; CLI wait can time out without changing desired state |
| Suspend races with a gateway request | A request admitted before the gateway observes `spec.suspend=true` may be cut off by Pod deletion; later requests receive 409 |
| Existing Git target is valid | Skip cloning and leave workspace contents unchanged |
| Existing Git target is nonempty but invalid or partially cloned | Fail closed without modifying it; user must repair the workspace before resuming |
| Absolute deadline expires with a Pod active or Pending | Gracefully delete the Pod, mark the Sandbox `Failed`, preserve the claim, and reject resume |
| Absolute deadline expires while suspended | Mark the Sandbox `Failed`, preserve the workspace claim, and reject resume |
| A new logs request arrives while suspended or transitioning | Return HTTP 409; an existing `follow=true` stream ends when its Pod is deleted |
| Per-Pod deadline annotation does not match `activeDeadlineSeconds` | Report a deadline policy mismatch and do not increase the Pod deadline |
| Less than one whole second remains when resuming | Round remaining time down; mark the Sandbox `Failed` and do not create a Pod |
| Managed PVC and Sandbox are deleted | Existing owner-reference behavior deletes the managed claim with the Sandbox |
| External PVC and Sandbox are deleted | The external claim remains; the Sandbox controller does not own or delete it |
| Older controller is deployed with a suspended object | It may recreate the Pod because it does not understand `spec.suspend`; before rollback, resume all Sandboxes or migrate them to active state |
| CRD schema is rolled back while objects use the new field | The field can be rejected or pruned; restore the expanded CRD before the new controller and do not downgrade the schema while suspended objects remain |

## Security and Operational Boundaries

- The suspend and resume endpoints use existing bearer-token authentication. The middleware resolves the tenant namespace, and the gateway can update only Sandboxes in that namespace. The handler uses the fetched object's resource version for a single optimistic update attempt; conflicts return HTTP 409 for the client to retry.
- Suspending intentionally terminates the workload and any active Pod operations. It does not execute a user-supplied save hook or run privileged filesystem operations.
- No Pod force deletion is used. The bounded grace period is 10 seconds; unfinished processes can be killed when that period expires.
- PVCs are durable user data. The managed and external claim lifetime distinction is visible in the API and CLI help.
- The gateway does not claim that disconnect means idle. Automatic idle suspension is deferred until a workload-owned busy/idle producer is defined and stale state blocks automation.
- “Scale to zero” applies to the execution Pod, not the controller, gateway, PVC storage, or Sandbox object.

## Alternatives Considered

### Automatic suspension from gateway inactivity or CPU metrics

Rejected for version one. A workload can continue useful work without an attached client, and low CPU does not prove idleness. A later opt-in policy can use a workload-owned Kubernetes Lease. Missing or stale heartbeat state must mean unknown and block automatic suspension.

### Copy `emptyDir` into a managed PVC during suspend

Rejected for version one. It requires a copy process, interruption recovery, claim naming and ownership rules, capacity errors, and protection against data changes during copy. PVC-backed workspaces already provide the needed persistence without a migration path.

### Resume implicitly on `exec`

Rejected. It makes a command appear to hang while the Pod and storage return, complicates concurrent requests, and hides the cold restart. An explicit `resume` request reports the state change and lets callers choose their wait behavior.

### Distributed gateway session registry and graceful drain

Rejected for version one. Session state is currently process-local, and a correct cross-replica drain fence needs shared coordination and its own failure semantics. Pod termination already ends operations; the API documents this effect.

### Store a new deadline origin in `status.startTime`

Rejected. That field already means controller acknowledgement time. `status.firstPodStartTime` preserves compatibility and provides the execution-time origin across Pod generations.

## Rollout and Rollback

Install the expanded CRD before deploying a controller that writes suspension fields or states. Existing Sandboxes default to `spec.suspend=false` and active suspension state. The new API fields are additive. Before rolling back to an older controller, resume all suspended Sandboxes and confirm their Pods exist. Do not roll back the CRD schema while any object relies on the new fields.

## Implementation Plan

The implementation is limited to the following files, grouped by phase. Each source phase must leave its affected packages building and their focused tests passing before the next phase begins. Add a file only if compilation or generated output requires it, and document that addition before expanding the write boundary.

1. **API schema and generated output: modify** `api/v1alpha1/sandbox_types.go`; regenerate `api/v1alpha1/zz_generated.deepcopy.go` and `config/crd/bases/sandboxes.buddy.io_sandboxes.yaml`. Add `spec.suspend`, workspace immutability and eligibility validation, suspension status, Pod UID, and first Pod start time. Verify the additive default, enum schema, CEL rule, and generated output through `internal/controller/suite_test.go`.
2. **Reconciliation and workspace initialization: modify** `internal/controller/sandbox_controller.go`, `internal/controller/sandbox_controller_test.go`, and `internal/controller/process_policy_test.go`. Implement the suspend/resume transitions, graceful deletion and NotFound fence, claim reuse, duplicate-create handling, Git seeding rules, generation deadline annotation, absolute deadline enforcement, and timed requeue while suspended with no Pod. Recompute the deadline from persisted status on every reconcile so a controller restart restores the timer. Return `RequeueAfter = max(remaining, 1ms)` only when `maxDurationSeconds` and `firstPodStartTime` are set and the deadline is still in the future; process expiry immediately at or before now, and schedule no deadline requeue if either input is absent. At expiry, fail with reason `DeadlineExceeded`, set completion time, gracefully delete any current nonterminal Pod, and prevent resume or replacement creation. Cover all listed controller cases in these tests.
3. **REST and gateway: modify** `pkg/api/v1/types.go`, `internal/gateway/server.go`, `internal/gateway/handlers.go`, `internal/gateway/ws.go`, `internal/gateway/handlers_test.go`, and `internal/gateway/drain_test.go`. Add authenticated suspend/resume operations and response fields. Gate exec, port-forward, logs, upload, and download on desired and observed suspension state. Return 409 for new log and `follow=true` requests during suspension or transition. Do not drain or restart an existing log-follow stream; Pod deletion closes its Kubernetes log stream and ends the response.
4. **Client and CLI: create** `pkg/client/suspend.go`, `cmd/buddy-sbx/suspend.go`, and `cmd/buddy-sbx/suspend_test.go`; **modify** `pkg/client/client_test.go` and `cmd/buddy-sbx/root.go`. Add client suspend/resume methods and register `suspend` and `resume` commands. Each command accepts Cobra duration flag `--timeout` defaulting to `3m`, polls once per second only after the REST operation returns 202, and succeeds for suspend at `Suspended` or resume at `None` plus execution `Running`. Return immediately on terminal phase or API error. Timeout prints the latest status and leaves desired state unchanged; do not add an environment or config-file timeout.
5. **Project documentation: modify** `README.md`, `docs/architecture.md`, `docs/roadmap.md`, `docs/designs/SITEMAP.md`, and this design. Update the deadline architecture/ADR, suspension behavior, rollout and rollback procedure, and roadmap status without adding per-directory README files.
6. **Verification: modify no additional files unless a test failure requires a scoped regression fix.** Add assertions in the listed tests for CRD default/status/immutability; REST eligibility, terminal-state, idempotency, and resource-version conflict behavior; Pod deletion before stable suspension and before replacement; opposite-request convergence and duplicate avoidance; Pending/Running/suspended deadline expiry, deadline requeue for remaining times below and above 1ms, no requeue with missing deadline inputs, immediate expiry at or before now, restart requeue reconstruction, and deadline tightening; every gateway route including logs and follow-log admission; CLI stable-state, terminal/API-error, and timeout behavior; Git clone, valid-worktree skip, and invalid-nonempty fail-closed behavior. Test generated manifests. Keep rollback to an older controller as a documented operational procedure; do not simulate a downgrade in automated tests.
7. Run `make manifests generate && make fmt vet && make test && make lint`. Then build and load the operator, install CRDs, deploy to KinD `dell-cluster`, and verify suspension, claim retention, Pod replacement identity, Git workspace preservation, deadline expiry and restart recovery, gateway conflicts, terminal rejection, and the running operator pod.

## Acceptance Criteria

- A user can suspend and resume a nonterminal writable PVC-backed Sandbox using REST and `buddy-sbx`.
- An effective `emptyDir`, read-only claim, or terminal Sandbox cannot be suspended through REST; direct invalid updates never delete a Pod.
- Repeated requests are idempotent. Concurrent opposite requests converge to the latest desired state without duplicate Pods.
- `Suspended` is reported only after the old Pod is NotFound. Resume waits for old Pod disappearance and creates exactly one replacement against the same PVC with a new Pod UID.
- A managed PVC survives suspension and remains owned by the Sandbox. An external PVC survives Sandbox deletion and never receives an owner reference from this controller.
- Git resume skips a valid existing repository and fails without mutation on an invalid nonempty target.
- All gateway routes targeting a Pod, including exec, port-forward, logs/follow, upload, and download, reject suspended or transitioning Sandboxes. Existing operations may end when the Pod terminates.
- The original execution deadline continues across Pod generations and suspension. Each Pod generation records its own expected `activeDeadlineSeconds`; policy matching uses that expectation. The controller enforces the absolute deadline for Pending and Running Pods and for suspended Sandboxes. An expired deadline prevents resume and produces a terminal failure.
- A suspended Sandbox with no Pod and a recorded `firstPodStartTime` is requeued for the remaining absolute deadline. The deadline requeue is reconstructed from persisted status after controller restart.
- Existing `status.startTime`, terminal `completionTime`, and terminal `exitCode` retain their documented meanings.
- CRD/API tests verify the additive default, status schema, workspace immutability, and generated manifest output. Rollback to an older controller is documented as an operational procedure and is not simulated in automated tests.
- `make manifests generate && make fmt vet && make test && make lint` passes, and the controller is verified live on KinD `dell-cluster`.

## Primary References

- [Kubernetes Pod API](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-v1/)
- [Kubernetes Pod lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)
- [Kubernetes Pod termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-termination/)
- [Kubernetes persistent volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
- [Kubernetes emptyDir](https://kubernetes.io/docs/concepts/storage/volumes/#emptydir)
- [Kubernetes Leases](https://kubernetes.io/docs/concepts/architecture/leases/)
- [controller-runtime reconcile package](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile)
