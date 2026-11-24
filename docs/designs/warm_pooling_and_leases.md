# Warm Pooling and Ephemeral Claims Architecture

## Problem Statement

Starting container workloads from a cold state in Kubernetes takes several seconds due to API registration, scheduling, container runtime initialization, and network perimeter configuration. For AI agents executing repetitive verification tasks, interactive commands, or multi-step tool calls, this startup delay impairs execution velocity.

To provide sub-second dispatch while maintaining strict multi-tenant security and zero state contamination between tasks, Buddy Sandboxes introduces two custom resources:
1. `SandboxPool`: Maintains a pre-warmed inventory of standby sandbox runners ready to accept tasks immediately.
2. `SandboxLease`: Atomically allocates a standby runner from a pool to an execution request with a defined time-to-live and teardown finalizer.

---

## System Architecture

```
   Client / Gateway / Agent Task
                │
                ▼
      ┌──────────────────┐
      │   SandboxLease   │ ──── (Atomic CAS Claim) ────┐
      └──────────────────┘                              │
                │                                       ▼
                │                            ┌─────────────────────┐
                │                            │     SandboxPool     │
                │                            │ (minWarm: 3, max: 8)│
                ▼                            └─────────────────────┘
      ┌──────────────────┐                              │
      │  Claimed Sandbox │ ◄── (Standby Sandbox CR) ────┘
      │ (Running Runner) │                              │
      └──────────────────┘                              ▼
                │                           (Immediate Replenishment)
                ▼                                       │
      (In-Pod Task Exec)                                ▼
      (POSIX Exit Code)                     ┌─────────────────────┐
                │                           │ New Standby Sandbox │
                ▼                           └─────────────────────┘
      (Lease Complete / TTL)
                │
                ▼
      (Sandbox Terminated)
      (Zero State Leakage)
```

### Core Architectural Principles

1. **Strict Ephemeral Invariance**:
   In Kubernetes, container volume mounts and storage specifications cannot be mutated after a pod is created. Standby runners in a `SandboxPool` run with local scratch storage (`emptyDir`). Post-claim code loading occurs through streaming transport (shallow Git clone, streaming tar archives over Pod exec, or agent synchronization) directly into `/workspace`. Workloads requiring pre-existing persistent block storage bypass the warm pool and instantiate dedicated `Sandbox` resources directly.

2. **Single-Use Disposable Runners**:
   To prevent residual state leaks, background process retention, or cross-tenant credential contamination, claimed runners are disposable. When a lease completes, is marked released, or reaches its expiration deadline, the claimed `Sandbox` is deleted and its pod terminated.

3. **Immediate Replenishment**:
   When a standby runner transitions from `standby` to `claimed`, the pool controller detects that ready standby capacity has dropped below `minWarm` and immediately initiates provisioning a replacement runner, preserving ready capacity for incoming requests.

4. **Atomic Concurrency and Fencing**:
   To prevent race conditions without thundering-herd contention, `SandboxLeaseController` queries available standby runners and applies an atomic Compare-And-Swap (CAS) update on the target `Sandbox`:
   * Mutates label `sandboxes.buddy.io/pool-status` from `standby` to `claimed`.
   * Sets label `sandboxes.buddy.io/lease: <lease-name>`.
   * Sets annotation `sandboxes.buddy.io/claimed-at: <timestamp>`.
   * Sets annotation `sandboxes.buddy.io/claim-deadline: <timestamp>` (15-second reservation window).
   If the CAS succeeds, the lease records `status.boundSandboxRef` and transitions to `Bound`. If the controller restarts before binding, the pool controller scavenger revokes unconfirmed claims after the reservation deadline.

5. **Standby Process Keepalive**:
   Standby runners must remain active in `Running` phase ready for task dispatch. The pool controller automatically injects an idle keepalive command (`["sleep", "infinity"]`) into standby runners if no custom command is defined in the template.

---

## API Contract & Go Types

### Group: `sandboxes.buddy.io/v1alpha1`

### 1. `SandboxPool`

```go
// SandboxTemplateSpec defines the container blueprint for warm sandbox runners.
type SandboxTemplateSpec struct {
	// Image is the container image to run.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Command is the entrypoint array. Defaults to an idle keepalive if omitted.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args is the arguments array for the entrypoint.
	// +optional
	Args []string `json:"args,omitempty"`

	// WorkingDir is the container working directory. Defaults to /workspace.
	// +kubebuilder:default="/workspace"
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// RuntimeClassName specifies the container runtime class (e.g. gvisor, kata, runc).
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// Resources describes compute resource requirements for the sandbox container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env is the list of environment variables to set in the sandbox container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// NetworkProfileRef references an independent NetworkProfile custom resource.
	// +optional
	NetworkProfileRef *corev1.LocalObjectReference `json:"networkProfileRef,omitempty"`
}

// SandboxPoolSpec defines the desired state of SandboxPool.
// +kubebuilder:validation:XValidation:rule="self.maxWarm >= self.minWarm",message="maxWarm must be greater than or equal to minWarm"
type SandboxPoolSpec struct {
	// MinWarm is the minimum number of ready standby runners maintained in the pool.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	MinWarm int32 `json:"minWarm"`

	// MaxWarm is the maximum total runners (standby + claimed) allowed across the pool.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MaxWarm int32 `json:"maxWarm"`

	// IdleTimeoutSeconds is the maximum idle age before a surplus standby runner is recycled.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=10
	// +optional
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`

	// Template defines the container blueprint for runners spawned by this pool.
	// +kubebuilder:validation:Required
	Template SandboxTemplateSpec `json:"template"`
}

// SandboxPoolStatus defines the observed state of SandboxPool.
type SandboxPoolStatus struct {
	// ReadyReplicas is the number of available standby runners ready for immediate lease.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// AllocatedReplicas is the number of runners currently claimed by active leases.
	// +optional
	AllocatedReplicas int32 `json:"allocatedReplicas"`

	// TotalReplicas is the total number of child runners managed by the pool.
	// +optional
	TotalReplicas int32 `json:"totalReplicas"`

	// Conditions represent observations of current pool operational state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sp;pool
// +kubebuilder:printcolumn:name="MinWarm",type="integer",JSONPath=".spec.minWarm"
// +kubebuilder:printcolumn:name="MaxWarm",type="integer",JSONPath=".spec.maxWarm"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Allocated",type="integer",JSONPath=".status.allocatedReplicas"
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.totalReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SandboxPool is the Schema for the sandboxpools API.
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec,omitempty"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}
```

---

### 2. `SandboxLease`

```go
// SandboxLeasePhase represents the current phase of a sandbox lease.
// +kubebuilder:validation:Enum=Pending;Bound;Released;Expired;Failed
type SandboxLeasePhase string

const (
	SandboxLeasePhasePending  SandboxLeasePhase = "Pending"
	SandboxLeasePhaseBound    SandboxLeasePhase = "Bound"
	SandboxLeasePhaseReleased SandboxLeasePhase = "Released"
	SandboxLeasePhaseExpired  SandboxLeasePhase = "Expired"
	SandboxLeasePhaseFailed   SandboxLeasePhase = "Failed"
)

// SandboxLeaseSpec defines the desired claim configuration.
// +kubebuilder:validation:XValidation:rule="self.poolRef == oldSelf.poolRef",message="poolRef is immutable after creation"
// +kubebuilder:validation:XValidation:rule="self.executionId == oldSelf.executionId",message="executionId is immutable after creation"
// +kubebuilder:validation:XValidation:rule="self.timeoutSeconds == oldSelf.timeoutSeconds",message="timeoutSeconds is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.released) || oldSelf.released == false || (has(self.released) && self.released == true)",message="released cannot be set back to false once true"
type SandboxLeaseSpec struct {
	// PoolRef references the target SandboxPool to claim a runner from.
	// +kubebuilder:validation:Required
	PoolRef corev1.LocalObjectReference `json:"poolRef"`

	// ExecutionId is a unique task or client execution identifier. Max 63 characters.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ExecutionId string `json:"executionId"`

	// TimeoutSeconds is the maximum lease duration in seconds before automatic expiration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=300
	TimeoutSeconds int32 `json:"timeoutSeconds"`

	// QueueTimeoutSeconds is the maximum duration in seconds a lease can wait in Pending state.
	// Defaults to 60.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=3600
	// +optional
	QueueTimeoutSeconds int32 `json:"queueTimeoutSeconds,omitempty"`

	// Released indicates that the client has completed execution and released the runner early.
	// +kubebuilder:default=false
	// +optional
	Released bool `json:"released,omitempty"`
}

// SandboxLeaseStatus defines the observed state of SandboxLease.
type SandboxLeaseStatus struct {
	// Phase is the current lifecycle state of the lease.
	// +optional
	Phase SandboxLeasePhase `json:"phase,omitempty"`

	// BoundSandboxRef is a reference to the claimed child Sandbox.
	// +optional
	BoundSandboxRef *corev1.LocalObjectReference `json:"boundSandboxRef,omitempty"`

	// BoundPodName is the name of the active container pod executing the sandbox.
	// +optional
	BoundPodName string `json:"boundPodName,omitempty"`

	// LeasedAt is the timestamp when the standby runner was bound.
	// +optional
	LeasedAt *metav1.Time `json:"leasedAt,omitempty"`

	// ExpiresAt is the calculated expiration deadline.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Conditions represent observations of current lease state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbl;lease
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pool",type="string",JSONPath=".spec.poolRef.name"
// +kubebuilder:printcolumn:name="Sandbox",type="string",JSONPath=".status.boundSandboxRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SandboxLease is the Schema for the sandboxleases API.
type SandboxLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxLeaseSpec   `json:"spec,omitempty"`
	Status SandboxLeaseStatus `json:"status,omitempty"`
}
```

---

## Constants and Labels

```go
const (
	LabelPoolName   = "sandboxes.buddy.io/pool"
	LabelPoolStatus = "sandboxes.buddy.io/pool-status"
	LabelLeaseName  = "sandboxes.buddy.io/lease"

	PoolStatusStandby = "standby"
	PoolStatusClaimed = "claimed"
	PoolStatusBound   = "bound"

	AnnotationClaimedAt     = "sandboxes.buddy.io/claimed-at"
	AnnotationClaimDeadline = "sandboxes.buddy.io/claim-deadline"

	LeaseProtectionFinalizer = "sandboxes.buddy.io/lease-protection"
)
```

---

## Controller Algorithms

### 1. `SandboxPoolController` Algorithm

1. **Fetch & Reconcile Standby Children**:
   List all `Sandbox` objects matching label `sandboxes.buddy.io/pool: <pool-name>`.
2. **Classify Runners**:
   * `ready`: `sandbox.Status.Phase == Running` AND label `pool-status == standby`.
   * `allocated`: label `pool-status == claimed`.
   * `pending_init`: phase is `Pending` or not yet ready.
   * `failed_or_terminal`: phase is `Failed` or `Succeeded`. Terminate and remove from tracking.
3. **Scavenge Expired Claims**:
   If a runner has label `claimed`, but its `AnnotationClaimDeadline` has passed and no active `SandboxLease` in status `Bound` owns it, delete the orphaned runner to prevent resource leaks.
4. **Immediate Replenishment**:
   * Total managed runners = `len(ready) + len(allocated) + len(pending_init)`.
   * Standby deficit = `pool.Spec.MinWarm - len(ready) - len(pending_init)`.
   * Allowed new runners = `min(standby deficit, pool.Spec.MaxWarm - total)`.
   * If `allowed > 0`, create new `Sandbox` CRs with:
     * Name: `fmt.Sprintf("%s-%s", pool.Name, randString(5))`
     * Labels: `sandboxes.buddy.io/pool: pool.Name`, `sandboxes.buddy.io/pool-status: standby`
     * Workspace: `Ephemeral: true`, mountPath `/workspace`
     * Entrypoint: `["sleep", "infinity"]` if `template.Command` is empty
     * OwnerReference: pointing to `SandboxPool`
5. **Recycle Stale Standbys**:
   If `pool.Spec.IdleTimeoutSeconds > 0` and `len(ready) > pool.Spec.MinWarm`, delete surplus standby runners whose creation age exceeds `IdleTimeoutSeconds`.
6. **Update Status**:
   Update `status.readyReplicas`, `status.allocatedReplicas`, `status.totalReplicas`, and set condition `Ready=True`.

---

### 2. `SandboxLeaseController` Algorithm

1. **Ensure Finalizer**:
   Add `sandboxes.buddy.io/lease-protection` if not present.
2. **Handle Deletion or Explicit Release**:
   If `lease.DeletionTimestamp != nil` OR `lease.Spec.Released == true`:
   * If a runner is bound (`status.boundSandboxRef != nil`), delete the bound `Sandbox` resource to terminate the runner cleanly.
   * If `Spec.Released == true` and not deleting, set `status.phase = Released`.
   * Remove finalizer and exit.
3. **Handle Pending Leases**:
   If `status.phase == "" || status.phase == Pending`:
   * Check queue timeout: if `time.Since(creationTimestamp) > queueTimeoutSeconds`, transition to `Failed` with reason `QueueTimeoutExceeded` and exit.
   * List standby sandboxes with labels `sandboxes.buddy.io/pool: poolRef.name` and `sandboxes.buddy.io/pool-status: standby`.
   * Filter candidates where `status.phase == Running`.
   * If candidates exist:
     * Pick oldest ready candidate (FIFO) or first available.
     * Execute atomic CAS patch on candidate `Sandbox`:
       - `sandboxes.buddy.io/pool-status: claimed`
       - `sandboxes.buddy.io/lease: lease.Name`
       - `sandboxes.buddy.io/claim-deadline: now + 15s`
     * If CAS conflict error (`errors.IsConflict`), retry immediately with next candidate.
     * On CAS success:
       - Update lease status:
         - `phase: Bound`
         - `boundSandboxRef: {Name: candidate.Name}`
         - `boundPodName: candidate.Status.PodName`
         - `leasedAt: now`
         - `expiresAt: now + timeoutSeconds`
       - Requeue at `time.Until(expiresAt)`.
   * If no candidates available:
     - Set `status.phase = Pending`.
     - Requeue after 2 seconds (or wake on watcher event).
4. **Handle Active Bound Leases**:
   If `status.phase == Bound`:
   * Check TTL expiration: if `time.Now().After(expiresAt.Time)`:
     - Transition `status.phase = Expired`.
     - Delete the bound `Sandbox` resource.
     - Return success.
   * Check child health: if bound `Sandbox` phase is `Failed` or pod disappeared prematurely:
     - Transition `status.phase = Failed` with reason `RunnerTerminatedPrematurely`.
     - Return success.
   * Schedule next reconciliation wakeup at `time.Until(expiresAt.Time)`.

---

## Watch Event Mappers & Field Indexers

1. **`SandboxPoolController` Watches**:
   * `For(&sandboxesv1alpha1.SandboxPool{})`
   * `Owns(&sandboxesv1alpha1.Sandbox{})`
   * `Watches(&sandboxesv1alpha1.SandboxLease{}, handler.EnqueueRequestsFromMapFunc(mapLeaseToPool))` to replenish immediately when a lease is bound.

2. **`SandboxLeaseController` Watches**:
   * `For(&sandboxesv1alpha1.SandboxLease{})`
   * `Watches(&sandboxesv1alpha1.Sandbox{}, handler.EnqueueRequestsFromMapFunc(mapSandboxToPendingLeases))` to wake up `Pending` leases immediately when a standby runner transitions to `Running`.
   * `Watches(&sandboxesv1alpha1.Sandbox{}, handler.EnqueueRequestsFromMapFunc(mapClaimedSandboxToLease))` to react if a bound runner exits prematurely.

3. **Field Indexers**:
   * `Sandbox` indexed by `sandboxes.buddy.io/pool` and `sandboxes.buddy.io/pool-status`.

---

## Security Boundaries & Multi-Tenancy

1. **Namespace Scoping**:
   `SandboxPool`, `SandboxLease`, and child `Sandbox` resources exist within the same namespace. `spec.poolRef` uses `LocalObjectReference`, adhering to standard Kubernetes RBAC. Multi-tenant clusters isolate tenants into dedicated namespaces with their own pool configurations.

2. **Process and State Sanitization**:
   Standby runners operate with non-root security contexts, dropped Linux capabilities, and ephemeral scratch storage. Because each runner is destroyed after its lease concludes, subsequent tasks run in fresh namespaces with zero lingering processes, temporary files, or cached data.

3. **Network Egress Isolation**:
   Standby runners created by `SandboxPool` inherit the `NetworkProfile` declared in the pool template. L4 network policies, metadata service blocking, and domain allowlists remain active throughout standby and execution phases.

4. **Quota Protection**:
   `spec.maxWarm` enforces a hard ceiling on total compute footprint across standby and allocated pods, preventing unconstrained resource consumption during load spikes.

---

## Acceptance Criteria & Test Plan

1. **CRD Schema & CEL Validation**:
   * `make manifests generate` succeeds with no warnings.
   * CEL validation rules reject `maxWarm < minWarm`.
   * CEL rules enforce immutability on `poolRef`, `executionId`, `timeoutSeconds`, and monotonic `released`.
2. **Standby Lifecycle**:
   * `SandboxPoolController` provisions exactly `minWarm` standby runners in `Running` phase with `sandboxes.buddy.io/pool-status: standby`.
   * Runners remain alive in `Running` phase using the keepalive entrypoint.
3. **Atomic Claiming & Concurrency**:
   * 10 concurrent lease claims against a pool with `minWarm: 3` and `maxWarm: 5` bind distinct runners without duplicate allocation.
   * Leases beyond available capacity wait in `Pending`.
4. **Immediate Replenishment**:
   * Binding a standby runner immediately triggers the pool controller to spawn a replacement standby runner up to `maxWarm`.
5. **Teardown on Release and Expiration**:
   * Setting `spec.released: true` deletes the claimed child runner and sets `status.phase = Released`.
   * Exceeding `timeoutSeconds` deletes the child runner and sets `status.phase = Expired`.
   * Deleting the `SandboxLease` object cleans up the child runner before removing the finalizer.
6. **Local KinD Cluster Verification**:
   * Build operator, load image into local KinD cluster, deploy, and verify live creation of `SandboxPool` and `SandboxLease`.
