# Process Namespace and Sandbox Introspection

## Problem Statement

The controller starts the requested agent image directly. A shell or test process can exit while a child continues running; when that child exits, it may remain a zombie if its new parent does not reap it. This is a preventative capability from the roadmap, not a claim that the project has observed PID exhaustion. Existing node-level PID caps limit process growth but do not reap zombies. Shared process namespaces can reap children adopted by PID 1, so this milestone makes that behavior available as an opt-in for workloads that need it.

The agent also lacks stable Kubernetes-provided values for the Sandbox and Pod identity, its CPU and memory requests and limits, and the configured execution budget. These values have specific agent-facing uses: scripts can tag logs and distinguish a recreated Pod; build and test workers can choose parallelism based on CPU and memory settings; long-running work can consider the configured duration before starting more tasks.

Adding a custom supervisor as the agent entrypoint would change or wrap arbitrary images. It would also require a binary delivery path that works with read-only root filesystems, `noexec` workspace mounts, and minimal images. Kubernetes already supplies a pause process that can reap orphaned children when the Pod uses a shared process namespace.

## Goals and Non-Goals

### Goals

- Add opt-in process namespace sharing through the Sandbox API, REST API, and `buddy-sbx run`.
- Keep process namespace sharing disabled by default to preserve existing image behavior.
- Provide nonsecret Pod identity, effective CPU and memory requests and limits, and the configured maximum duration to the agent through the Downward API.
- Preserve the requested image, entrypoint, command, arguments, and existing container security context.
- Define Pod recreation, workspace data, signal, and rollback behavior.

### Non-Goals

- Add or inject a `buddy-runner` binary or custom PID 1 process.
- Restart, monitor, or provide an exec API for user processes.
- Route Kubernetes container runtime interface (CRI) exec sessions through an in-container daemon.
- Broadcast SIGTERM or SIGINT to every process or process group.
- Add new agent-visible Secret mounts or credentials APIs.
- Replace an existing Pod automatically when `spec.shareProcessNamespace` changes.

## System Architecture

### Opt-In Process Namespace

Add optional `spec.shareProcessNamespace` to `SandboxSpec` as a pointer boolean with CRD default `false`. The controller copies it to `PodSpec.shareProcessNamespace`. A missing value is treated as false in controller code, including unit tests that bypass API-server defaults. The field is immutable after Sandbox creation. A Common Expression Language (CEL) transition rule compares effective values, treating absent as false:

```cel
(!has(oldSelf.shareProcessNamespace) || oldSelf.shareProcessNamespace == false) ==
(!has(self.shareProcessNamespace) || self.shareProcessNamespace == false)
```

Attach this rule to the `SandboxSpec` object schema, where `oldSelf` and `self` refer to the old and new spec. The rule permits absent-to-absent, absent-to-false, false-to-absent, and unrelated updates, including old stored objects. It rejects absent/false-to-true and true-to-false transitions.

When enabled, the Pod's pause process is PID 1 and the agent entrypoint receives another PID. Kubernetes' pause process handles SIGCHLD and calls `waitpid(-1, ..., WNOHANG)`, reaping children adopted by PID 1. This handles orphaned descendants after their parent exits. It does not reap a child while its living parent has not waited for it, and it does not restart or supervise application processes.

The controller creates a Pod only when the managed Pod does not exist. It does not patch or replace an existing Pod when the process namespace policy changes; that Sandbox field is immutable. Deadline reconciliation is the exception and is limited to Kubernetes-supported setting or tightening of `activeDeadlineSeconds`. Changing process namespace policy requires deleting and recreating the Sandbox. Pod recreation loses `emptyDir` workspace data. An external PVC remains independent of the Pod lifecycle.

### Downward API Environment Contract

The controller adds these values to the `agent` container. They are available whether or not process namespace sharing is enabled.

| Name | Kubernetes source | Format and behavior |
| --- | --- | --- |
| `BUDDY_SANDBOX_NAME` | `metadata.labels['sandboxes.buddy.io/sandbox']` | Sandbox resource name |
| `BUDDY_POD_NAME` | `metadata.name` | Pod name |
| `BUDDY_POD_NAMESPACE` | `metadata.namespace` | Pod namespace |
| `BUDDY_POD_UID` | `metadata.uid` | Pod instance UID |
| `BUDDY_CPU_REQUEST_MILLICORES` | `resourceFieldRef` for `requests.cpu`, divisor `1m`, container `agent` | Effective admitted request in millicores; zero if no request exists unless admission supplies one |
| `BUDDY_CPU_LIMIT_MILLICORES` | `resourceFieldRef` for `limits.cpu`, divisor `1m`, container `agent` | Effective admitted limit in millicores; if no limit exists, Kubernetes can expose node allocatable |
| `BUDDY_MEMORY_REQUEST_BYTES` | `resourceFieldRef` for `requests.memory`, divisor `1`, container `agent` | Effective admitted request in bytes; zero if no request exists unless admission supplies one |
| `BUDDY_MEMORY_LIMIT_BYTES` | `resourceFieldRef` for `limits.memory`, divisor `1`, container `agent` | Effective admitted limit in bytes; if no limit exists, Kubernetes can expose node allocatable |
| `BUDDY_INITIAL_MAX_DURATION_SECONDS` | `metadata.annotations['sandboxes.buddy.io/initial-max-duration-seconds']` | Existing `spec.maxDurationSeconds` at Pod creation, copied as base-10 seconds; `0` means no initial deadline |

The controller always sets the initial-duration annotation. It copies the positive `spec.maxDurationSeconds` value when the Pod is created and writes `0` otherwise. The same positive value is used for the initial `PodSpec.activeDeadlineSeconds`. Kubernetes measures this budget from Pod start, including init-container time. It is not an absolute timestamp or remaining time. The environment variable is a creation-time snapshot; it does not change if a Sandbox deadline is later tightened.

The resource values reflect the admitted Pod. A namespace `LimitRange` can add defaults. Kubernetes may expose node allocatable for an unset CPU or memory limit; that value is informational and does not enforce a limit. These environment variables are fixed at container startup and do not update if a cluster supports in-place resource resizing.

The exact names listed in the table are reserved. Add CRD validation at `SandboxSpec` scope that rejects any `spec.env` entry whose `name` is in the table's name set, and have REST validation return HTTP 400 for the same collisions. Other names with the `BUDDY_` prefix remain available to Sandbox authors. Controller-provided `env` entries are appended after user entries, and Kubernetes gives explicit `env` entries precedence over `envFrom` entries.

`spec.maxDurationSeconds` must be between one and 2,147,483,647 seconds when set, matching the range Kubernetes accepts for `PodSpec.activeDeadlineSeconds`. It remains mutable only for monotonic tightening. Add this CEL transition rule at `SandboxSpec` scope:

```cel
!has(oldSelf.maxDurationSeconds) ||
(has(self.maxDurationSeconds) && self.maxDurationSeconds <= oldSelf.maxDurationSeconds)
```

An absent value can remain absent or be set to a positive value. A positive value can remain unchanged or be reduced to a smaller positive value. It cannot be removed or increased after it has been set. For each existing nonterminal Pod, compare the desired Sandbox deadline with `PodSpec.activeDeadlineSeconds`:

| Desired Sandbox value | Existing Pod value | Controller behavior |
| --- | --- | --- |
| absent | absent | No patch; `DeadlinePolicyReady=True`, reason `Matched` |
| positive `d` | absent | Patch Pod deadline to `d`; set policy condition true after successful patch |
| positive `d` | positive `p`, `d < p` | Patch Pod deadline to `d`; set policy condition true after successful patch |
| positive `d` | positive `p`, `d == p` | No patch; `DeadlinePolicyReady=True`, reason `Matched` |
| positive `d` | positive `p`, `d > p` | Do not patch; condition false because this would relax the deadline |
| absent | positive `p` | Do not patch; condition false because this would remove the deadline |

A stored Pod value of zero is invalid and is reported as a mismatch. A Pod patch error is returned for retry and is not treated as success. Set `DeadlinePolicyReady=False`, reason `DeadlinePolicyMismatch`, for unsupported relaxations/removal; set it true again when the Sandbox spec matches or can tighten the Pod value. On nonterminal Pods a mismatch sets Ready false with the same reason and takes precedence over the normal PodPending/PodRunning Ready update. On terminal Pods, never patch the Pod or change phase, completion time, exit code, or terminal Ready reason; retain the policy diagnostic condition if the values mismatch. Use stable condition updates so unchanged state does not churn transition times. Recreate the Sandbox to apply a relaxed or removed deadline. The initial-duration environment value remains unchanged.

Kubernetes measures `activeDeadlineSeconds` from `Pod.status.startTime`, including init-container time. Before the Pod has started, the controller does not consume the deadline against Sandbox acknowledgement or scheduling delay. Once Pod start time exists, calculate remaining time from the matched Pod's actual `spec.activeDeadlineSeconds` and `status.startTime` for both Pending and Running Pods, so init-container time counts. When no Pod start time exists, do not fabricate elapsed time or schedule deadline requeues. Keep the existing `Sandbox.status.startTime` meaning as the controller's initial acknowledgement timestamp for API compatibility; it is not the deadline clock and is not overwritten. Kubernetes enforces the deadline; controller requeues only observe and update Sandbox status.

### REST and CLI Contract

- Add optional `shareProcessNamespace` to `CreateSandboxRequest` as `*bool` and to `SandboxResponse` as a boolean. The Sandbox field is immutable, so the response remains consistent with the created Pod policy.
- Omission means false. A true value is copied to the Sandbox spec and returned by create, get, and list responses.
- Add `buddy-sbx run --share-process-namespace`. The flag defaults to false and sends true only when specified.
- Reject explicitly supplied REST `maxDurationSeconds` values outside one to 2,147,483,647 seconds with HTTP 400 before creating the Sandbox. Omission remains nil; explicit zero is invalid.
- For `buddy-sbx run --duration`, zero means omitted, negative values and values above 2,147,483,647 seconds are rejected, and positive fractional seconds round up to the next whole second so the API never receives zero for a positive duration.
- Allow an unset deadline to be set and a positive deadline to be reduced. Reject removal or increase after a positive deadline has been set.
- Setting a deadline on an already-running Pod measures from its existing Pod start time. If the Pod has already exceeded the new duration, Kubernetes may terminate it immediately; this is an accepted consequence of tightening.

The process namespace setting is create-time policy. The REST response returns the immutable Sandbox value. To use a different setting, delete and recreate the Sandbox. Recreating its Pod through direct Pod deletion does not change the immutable Sandbox value.

### Process Termination

Pod deletion continues to use Kubernetes termination. The kubelet asks the runtime to send the image's stop signal to the main process in each container; SIGTERM is the default unless the image defines a supported stop signal. The Pod's existing Kubernetes default termination grace period is 30 seconds, after which remaining processes receive SIGKILL. Container stop ordering is not guaranteed for ordinary containers. Shared PID namespace does not provide process-group signal fan-out. TTY cancellation and SIGINT behavior remain part of the existing exec path.

The agent image's entrypoint remains responsible for forwarding its stop signal to its own children. When process namespace sharing is enabled, images must not assume the entrypoint is PID 1 or use PID 1 as the agent process; `/pause` is PID 1.

## API Contract

Example opt-in Sandbox:

```yaml
apiVersion: sandboxes.buddy.io/v1alpha1
kind: Sandbox
metadata:
  name: build-task
  namespace: agents
spec:
  image: golang:1.25
  shareProcessNamespace: true
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: "1"
      memory: 2Gi
  maxDurationSeconds: 1800
```

When `shareProcessNamespace` is omitted or false, the Pod keeps separate process namespaces and the image entrypoint retains PID 1 behavior. The introspection environment remains available.

## Security Boundaries

- Process namespace sharing is opt-in because it changes process IDs and expands process visibility inside the Pod. Processes can inspect or signal other Pod processes subject to Unix permissions. `/proc/<pid>/environ` and `/proc/<pid>/root` are part of this shared process boundary.
- All containers in an opted-in Pod share this process trust boundary. Future sidecars that handle secrets or requests from a different trust domain require a separate design review or placement in a different Pod.
- The new environment values contain identity, resource, and duration data, not credentials. Do not mount new Secrets into the `agent` container as part of this milestone.
- Existing Git credentials remain in the Git init container. A Secret volume's tmpfs backing on Linux limits durable node storage exposure; it does not prevent code in a container with the mount from reading the Secret.
- The exact controller-provided names are reserved. This prevents Sandbox authors from spoofing those values through `spec.env` without reserving the full `BUDDY_` prefix. `envFrom` remains lower precedence than explicit entries.

## Failure Modes and Recovery

| Failure or behavior | Result and recovery |
| --- | --- |
| Sandbox omits `shareProcessNamespace` | Controller leaves the Pod setting false. Existing image behavior is preserved. |
| Image requires its entrypoint to have PID 1 and sharing is enabled | The image may fail or behave differently. The Sandbox field cannot be changed. Delete and recreate the Sandbox with sharing omitted or false. |
| User tries to change `spec.shareProcessNamespace` after Sandbox creation | CRD transition validation rejects the update. Create a new Sandbox with the desired setting. |
| Pod recreation uses an `emptyDir` workspace | Workspace data is lost with the Pod. Copy needed data or use an external PVC before recreation. |
| User needs to change process namespace mode while an external PVC is mounted | Delete and recreate the Sandbox with the desired mode. The PVC remains independent and can be mounted by the replacement Pod; deleting only the Pod recreates it with the Sandbox's unchanged setting. |
| `spec.env` contains an exact controller-owned name | CRD or REST validation rejects the request with a clear client error. Other `BUDDY_` names remain available. |
| CPU or memory limits are omitted | Kubernetes may expose node allocatable through the Downward API; the value is informational and is not a container limit. |
| `maxDurationSeconds` is omitted at Pod creation | `BUDDY_INITIAL_MAX_DURATION_SECONDS` is `0` and `activeDeadlineSeconds` remains unset. |
| `maxDurationSeconds` is outside one to 2,147,483,647 | CRD rejects the object; REST returns HTTP 400 before attempting creation. |
| CLI duration is positive but less than one second | CLI rounds up to one second; zero is omitted, and negative or out-of-range values are rejected. |
| A configured deadline is increased or removed after creation | CRD transition validation rejects the update. |
| A configured deadline is tightened after Pod creation | The controller patches `activeDeadlineSeconds`; the initial-duration environment value stays unchanged. |
| A deadline is set after the Pod has already run longer than the new value | The deadline remains based on `Pod.status.startTime`, so Kubernetes may immediately mark the Pod expired and terminate it. |
| Legacy Sandbox and Pod already disagree on deadline | Controller reports `DeadlinePolicyReady=False` with reason `DeadlinePolicyMismatch`; nonterminal Ready is false and no illegal Pod patch is made. Recreate the Sandbox to apply a relaxed or removed deadline. |
| Controller rolls back while the new CRD remains installed | Older controller code ignores the optional field. Existing Pods remain unchanged; newly created Pods use false by default. |
| CRD is rolled back while Sandbox objects still contain the field | Older schema may prune or reject the field. Do not downgrade the CRD until no Sandbox depends on it. |
| Pod is deleted | Kubelet sends the stop signal, waits the existing 30 second default grace period, then kills remaining processes. The application entrypoint must forward the signal to its children. |
| Sandbox deletion is in progress | The controller stops provisioning child resources so foreground garbage collection can remove the Pod and finish deleting the Sandbox. |

## Alternatives Weighed

### Inject a Custom `buddy-runner`

Rejected. A wrapper changes image entrypoint behavior, and delivering a binary into arbitrary images would need an image contract or executable volume. A volume can be mounted `noexec`. Kubernetes' pause process already reaps orphaned children when process namespace sharing is enabled.

### Enable Shared Process Namespaces by Default

Rejected. Every arbitrary image would lose its own PID 1 behavior without opting in. An opt-in field preserves compatibility while allowing a Sandbox author to accept the trade-off.

### Add a Loopback Metadata Service

Rejected. No such endpoint exists in the current repository. Any agent process could connect to a loopback listener, so it would not create a credential boundary. Downward API fields provide the required nonsecret metadata without a server.

### Mount Secrets into the Agent as Tmpfs Files

Rejected. Memory-backed storage limits durable node storage exposure but does not stop arbitrary agent code from reading a Secret mounted into its container. Git credentials remain limited to the init container.

### Broadcast Signals with a New Supervisor

Rejected. Kubernetes already sends a stop signal to each container's main process and enforces a grace period. A process-group signal service would require an additional daemon or an entrypoint wrapper and would still need to define behavior for arbitrary images.

## Implementation Plan

1. Update `api/v1alpha1/sandbox_types.go` with the pointer field, defaults, one-to-2,147,483,647 duration range, SandboxSpec-scope CEL transition rules, and exact `spec.env` collision rule; regenerate `config/crd/bases/sandboxes.buddy.io_sandboxes.yaml`.
2. Update `internal/controller/sandbox_controller.go`, `api/v1alpha1/sandbox_types.go` condition constants, and controller tests to preserve current container configuration, set process namespace mode, add the initial-duration annotation and Downward API variables, reconcile only legal Pod deadline tightening, report mismatch conditions with `meta.SetStatusCondition`, preserve terminal state, and use the matched Pod deadline and Pod start time for deadline requeue without changing existing Sandbox `status.startTime` meaning. Terminal Sandboxes still fetch the Pod only to update deadline-policy diagnostics and never patch it.
3. Update `pkg/api/v1/types.go`, `internal/gateway/handlers.go`, and gateway tests for REST create/get/list parity, reserved-name and duration validation, HTTP 400 behavior, and the response field.
4. Update `cmd/buddy-sbx/run.go` and CLI tests for the opt-in flag, positive fractional duration ceiling, zero omission, and negative/out-of-range rejection.
5. Update `docs/roadmap.md`, `docs/architecture.md`, `docs/designs/SITEMAP.md`, and `README.md`. Record the API migration and process trust boundary.
6. Add envtest transition cases for legacy fields absent, absent-to-false, false-to-absent, absent/false-to-true, true-to-false, unrelated updates, and max-duration absent/set/tighten/remove/increase/zero cases. Add controller tests for every deadline reconciliation matrix row, patch failures, condition clearing/no churn, Ready precedence, terminal Pods and diagnostics, Pod start-time origin, Pending and Running requeues, scheduling/init delay, and no child recreation during Sandbox deletion.
7. Verify live on KinD by running `make docker-build IMG=buddy-sandboxes-controller:local`, `kind load docker-image buddy-sandboxes-controller:local --name dell-cluster`, `make install`, and `make deploy IMG=buddy-sandboxes-controller:local`. Confirm the rollout with `kubectl get pods -n buddy-sandboxes-system` and require the controller Pod to be Running. Then create two Sandboxes with the same image and a command that prints PID 1 and selected `BUDDY_*` values. Leave `shareProcessNamespace` unset on the first and set it true on the second. Confirm the default agent is PID 1 and the opted-in Pod has `/pause` as PID 1. In the opted-in Pod, start a short-lived parent that backgrounds a child and exits; poll `/proc` until that child is no longer present. Confirm identity/resource/duration variables have the expected values and units. Delete a running Pod whose entrypoint traps TERM for a short delay, time deletion, and confirm the Pod exits within the configured grace period. Record commands, Pod events, and observed output in the PR verification summary.

## Acceptance Criteria

1. YAML, REST, and CLI requests omit process namespace sharing by default; explicit opt-in produces a Pod with `shareProcessNamespace: true`.
2. CRD validation tests older objects with absent fields: absent-to-absent, absent-to-false, and unrelated updates succeed; absent/false-to-true and true-to-false fail. Deadline validation accepts absent-to-absent/set, and positive equal/tightening updates, while rejecting zero, removal, and increases.
3. The requested image, command, arguments, and security context remain unchanged.
4. All `BUDDY_` variables use the documented Kubernetes sources and units. The initial-duration annotation mirrors `spec.maxDurationSeconds` at Pod creation, or contains `0` when it is absent. Supported deadline tightening patches `activeDeadlineSeconds` without changing the initial-duration environment value. Deadline countdown and requeue use `Pod.status.startTime`, including init-container time, and do not charge scheduling delay.
5. A user cannot set an exact controller-owned name using explicit `spec.env`; other `BUDDY_` names remain valid, and REST rejects collisions with HTTP 400.
6. Git Secret references remain only in the Git init container. No agent Secret mount or new credential environment variable is introduced.
7. Controller tests prove all deadline reconciliation matrix cases, stable mismatch conditions, Ready precedence, terminal state preservation, and deadline origin without changing Sandbox `status.startTime`. CLI tests prove fractional, zero, negative, and out-of-range duration behavior; REST tests prove explicit invalid values return HTTP 400 without creating an object. A newly applied deadline on an old Pod proves expiration is measured from Pod start time and may be immediate.
8. KinD confirms the operator Pod is Running in `buddy-sandboxes-system`, the expected PID 1 and orphan-reaping behavior only when sharing is enabled, default false behavior, the Downward API values, and Pod termination grace behavior.
9. `make manifests generate && make fmt vet && make test && make lint` passes. The operator image builds, loads, and deploys on `dell-cluster`, and live lifecycle verification passes.

## Primary References

- [Kubernetes shared process namespaces](https://kubernetes.io/docs/tasks/configure-pod-container/share-process-namespace/)
- [Kubernetes pause process implementation](https://github.com/kubernetes/kubernetes/blob/master/build/pause/linux/pause.c)
- [Kubernetes Downward API](https://kubernetes.io/docs/concepts/workloads/pods/downward-api/)
- [Kubernetes Pod termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-termination/)
- [Kubernetes Secrets](https://kubernetes.io/docs/concepts/configuration/secret/)
- [Kubernetes Pod API](https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/)
- [Kubernetes init containers and active deadline behavior](https://kubernetes.io/docs/concepts/workloads/pods/init-containers/)
