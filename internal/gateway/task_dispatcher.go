package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TaskRunnerFunc defines a pluggable runner function for task execution (e.g. for testing).
type TaskRunnerFunc func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) (store.ExecutionResult, error)

// NonRetryableExecutionError indicates an unrecoverable configuration error that prevents
// task execution and must not be retried.
type NonRetryableExecutionError struct {
	Err error
}

func (e *NonRetryableExecutionError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "non-retryable execution error"
}

func (e *NonRetryableExecutionError) Unwrap() error {
	return e.Err
}

func cleanResourceID(id string) string {
	return strings.ReplaceAll(id, "_", "-")
}

// RunnerInstance represents a running container environment executing an agent task.
type RunnerInstance interface {
	PodName() string
	SandboxName() string
	Namespace() string
	Release(ctx context.Context, attemptNumber int, maxAttempts int, success bool) error
}

type dedicatedSandboxRunner struct {
	client             client.Client
	namespace          string
	name               string
	podName            string
	brokerCapabilities credentials.ProviderCapabilityLifecycle
	capabilityHash     []byte
}

func (r *dedicatedSandboxRunner) PodName() string     { return r.podName }
func (r *dedicatedSandboxRunner) SandboxName() string { return r.name }
func (r *dedicatedSandboxRunner) Namespace() string   { return r.namespace }

func (r *dedicatedSandboxRunner) Release(ctx context.Context, attemptNumber int, maxAttempts int, success bool) error {
	if r.brokerCapabilities != nil && len(r.capabilityHash) > 0 {
		_ = r.brokerCapabilities.Revoke(ctx, r.capabilityHash)
	}
	if r.client == nil {
		return nil
	}
	sbx := &sandboxesv1alpha1.Sandbox{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.name}, sbx); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if success || attemptNumber < maxAttempts {
		if err := r.client.Delete(ctx, sbx); err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
		if r.podName != "" {
			waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer waitCancel()
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			pod := &corev1.Pod{}
			for {
				select {
				case <-waitCtx.Done():
					return nil
				case <-ticker.C:
					if err := r.client.Get(waitCtx, client.ObjectKey{Namespace: r.namespace, Name: r.podName}, pod); k8serrors.IsNotFound(err) {
						return nil
					}
				}
			}
		}
		return nil
	}

	patch := client.MergeFrom(sbx.DeepCopy())
	sbx.Spec.Suspend = true
	if sbx.Annotations == nil {
		sbx.Annotations = make(map[string]string)
	}
	sbx.Annotations["sandboxes.buddy.io/retained-for-debug"] = "true"
	sbx.Annotations["sandboxes.buddy.io/retained-until"] = time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	return r.client.Patch(ctx, sbx, patch)
}

type leasedSandboxRunner struct {
	client    client.Client
	namespace string
	leaseName string
	sbxName   string
	podName   string
}

func (r *leasedSandboxRunner) PodName() string     { return r.podName }
func (r *leasedSandboxRunner) SandboxName() string { return r.sbxName }
func (r *leasedSandboxRunner) Namespace() string   { return r.namespace }

func (r *leasedSandboxRunner) Release(ctx context.Context, _ int, _ int, _ bool) error {
	if r.client == nil {
		return nil
	}
	lease := &sandboxesv1alpha1.SandboxLease{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: r.leaseName}, lease); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	patch := client.MergeFrom(lease.DeepCopy())
	lease.Spec.Released = true
	return r.client.Patch(ctx, lease, patch)
}

// TaskDispatcher manages worker claiming, heartbeat updates, execution dispatching,
// and orphan sweeping.
type TaskDispatcher struct {
	store               TaskStore
	client              client.Client
	executor            RemoteStreamExecutor
	workerID            string
	runnerFunc          TaskRunnerFunc
	providerCredentials credentials.ProviderCredentialPolicy
	brokerCapabilities  credentials.ProviderCapabilityLifecycle
	pollInterval        time.Duration
	sweepInterval       time.Duration
	orphanTimeout       time.Duration
	stopCh              chan struct{}
	wg                  sync.WaitGroup
	mu                  sync.Mutex
	running             bool
	workerSem           chan struct{}
	activeCancels       map[string]context.CancelCauseFunc
}

var errTaskCancelled = errors.New("task was cancelled")

// NewTaskDispatcher creates a new TaskDispatcher instance.
func NewTaskDispatcher(taskStore TaskStore, k8sClient client.Client, executor RemoteStreamExecutor, workerID string) *TaskDispatcher {
	if workerID == "" {
		workerID = generateID("worker")
	}
	return &TaskDispatcher{
		store:         taskStore,
		client:        k8sClient,
		executor:      executor,
		workerID:      workerID,
		pollInterval:  1 * time.Second,
		sweepInterval: 15 * time.Second,
		orphanTimeout: 30 * time.Second,
		stopCh:        make(chan struct{}),
		workerSem:     make(chan struct{}, 10),
		activeCancels: make(map[string]context.CancelCauseFunc),
	}
}

// CancelTaskExecution cancels any active execution context for the specified task.
func (d *TaskDispatcher) CancelTaskExecution(taskID string) {
	d.mu.Lock()
	cancel, ok := d.activeCancels[taskID]
	d.mu.Unlock()
	if ok && cancel != nil {
		cancel(errTaskCancelled)
	}
}

// SetRunnerFunc sets a custom task execution function (used in unit tests).
func (d *TaskDispatcher) SetRunnerFunc(fn TaskRunnerFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runnerFunc = fn
}

// SetCredentials sets the credential policy and capability lifecycle services.
func (d *TaskDispatcher) SetCredentials(creds credentials.ProviderCredentialPolicy, caps credentials.ProviderCapabilityLifecycle) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.providerCredentials = creds
	d.brokerCapabilities = caps
}

// Start begins the dispatcher worker and sweeper loops.
func (d *TaskDispatcher) Start(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()

	d.wg.Add(2)
	go d.claimLoop(ctx)
	go d.sweeperLoop(ctx)
}

// Stop gracefully terminates the dispatcher loops.
func (d *TaskDispatcher) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.running {
		d.mu.Unlock()
		return nil
	}
	d.running = false
	close(d.stopCh)
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *TaskDispatcher) claimLoop(ctx context.Context) {
	defer d.wg.Done()
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.pollAndDispatch(ctx)
		}
	}
}

func (d *TaskDispatcher) pollAndDispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case d.workerSem <- struct{}{}:
		}

		exec, err := d.store.ClaimNextExecution(ctx, d.workerID)
		if err != nil || exec == nil {
			<-d.workerSem
			return
		}

		d.wg.Add(1)
		go func(e *store.Execution) {
			defer func() {
				<-d.workerSem
				d.wg.Done()
			}()
			d.runExecution(ctx, e)
		}(exec)
	}
}

func (d *TaskDispatcher) sweeperLoop(ctx context.Context) {
	defer d.wg.Done()
	ticker := time.NewTicker(d.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			orphans, _ := d.store.ReapOrphanExecutions(ctx, d.orphanTimeout)
			if d.client != nil {
				for _, orphan := range orphans {
					if orphan.SandboxName != nil && *orphan.SandboxName != "" {
						_ = d.client.Delete(ctx, &sandboxesv1alpha1.Sandbox{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: orphan.Namespace,
								Name:      *orphan.SandboxName,
							},
						})
						if d.brokerCapabilities != nil {
							_ = d.brokerCapabilities.RevokeSandbox(ctx, orphan.Namespace, *orphan.SandboxName, cleanResourceID(orphan.ID))
						}
					}
					if orphan.LeaseName != nil && *orphan.LeaseName != "" {
						lease := &sandboxesv1alpha1.SandboxLease{}
						if err := d.client.Get(ctx, client.ObjectKey{Namespace: orphan.Namespace, Name: *orphan.LeaseName}, lease); err == nil {
							patch := client.MergeFrom(lease.DeepCopy())
							lease.Spec.Released = true
							_ = d.client.Patch(ctx, lease, patch)
						}
					}
				}
			}
		}
	}
}

func (d *TaskDispatcher) runExecution(ctx context.Context, exec *store.Execution) {
	execCtx, cancelExec := context.WithCancelCause(ctx)
	defer cancelExec(nil)

	d.mu.Lock()
	d.activeCancels[exec.TaskID] = cancelExec
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.activeCancels, exec.TaskID)
		d.mu.Unlock()
	}()

	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)

	go func() {
		hbTicker := time.NewTicker(2 * time.Second)
		defer hbTicker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-hbTicker.C:
				_ = d.store.UpdateExecutionHeartbeat(ctx, exec.ID)
				t, err := d.store.GetTask(ctx, exec.TaskID)
				if err == nil && t.Status == statusCancelled {
					cancelExec(errTaskCancelled)
					return
				}
			}
		}
	}()

	task, err := d.store.GetTask(execCtx, exec.TaskID)
	if err != nil {
		nextExecID := fmt.Sprintf("%s-att%d", exec.TaskID, exec.AttemptNumber+1)
		if errors.Is(err, store.ErrNotFound) {
			nextExecID = ""
		}
		_, _ = d.store.CompleteExecution(ctx, exec.ID, store.ExecutionResult{
			Status:       statusFailed,
			ErrorMessage: fmt.Sprintf("fetch task: %v", err),
		}, nextExecID)
		return
	}

	var profile *store.AgentProfile
	if task.AgentProfileID != nil && *task.AgentProfileID != "" {
		var profErr error
		profile, profErr = d.store.GetAgentProfile(execCtx, *task.AgentProfileID)
		if profErr != nil {
			exitCode := 1
			nextExecID := fmt.Sprintf("%s-att%d", exec.TaskID, exec.AttemptNumber+1)
			if errors.Is(profErr, store.ErrNotFound) {
				nextExecID = ""
			}
			_, _ = d.store.CompleteExecution(ctx, exec.ID, store.ExecutionResult{
				Status:       statusFailed,
				ExitCode:     &exitCode,
				ErrorMessage: fmt.Sprintf("fetch agent profile %q: %v", *task.AgentProfileID, profErr),
			}, nextExecID)
			return
		}
	}

	d.mu.Lock()
	customRunner := d.runnerFunc
	d.mu.Unlock()

	var result store.ExecutionResult
	var runErr error
	if customRunner != nil {
		result, runErr = customRunner(execCtx, exec, profile)
	} else {
		result, runErr = d.executeHarness(execCtx, task, exec, profile)
	}

	if errors.Is(context.Cause(execCtx), errTaskCancelled) {
		result.Status = statusCancelled
		saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer saveCancel()
		_, _ = d.store.CompleteExecution(saveCtx, exec.ID, result, "")
		return
	}
	if ctx.Err() != nil {
		if result.Status == statusSucceeded {
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer saveCancel()
			_, _ = d.store.CompleteExecution(saveCtx, exec.ID, result, "")
			return
		}
		if result.Status == statusFailed && !errors.Is(runErr, context.Canceled) && !errors.Is(context.Cause(execCtx), context.Canceled) {
			nextExecID := fmt.Sprintf("%s-att%d", exec.TaskID, exec.AttemptNumber+1)
			var nonRetryable *NonRetryableExecutionError
			if errors.As(runErr, &nonRetryable) {
				nextExecID = ""
			}
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer saveCancel()
			_, _ = d.store.CompleteExecution(saveCtx, exec.ID, result, nextExecID)
			return
		}
		// Context-caused failure or uncompleted execution interrupted by shutdown:
		// Leave the execution row in 'running' so the orphan sweeper reaps it and schedules attempt N+1.
		return
	}

	nextExecID := fmt.Sprintf("%s-att%d", exec.TaskID, exec.AttemptNumber+1)
	var nonRetryable *NonRetryableExecutionError
	if result.Status == statusFailed && errors.As(runErr, &nonRetryable) {
		nextExecID = ""
	}
	_, _ = d.store.CompleteExecution(ctx, exec.ID, result, nextExecID)
}

const (
	defaultHarness          = "default"
	defaultPoolName         = "default"
	statusFailed            = "failed"
	statusSucceeded         = "succeeded"
	statusCancelled         = "cancelled"
	defaultAgentImageBase   = "buddy-sandboxes-base:local"
	defaultAgentImageCodex  = "buddy-sandboxes-codex:local"
	defaultAgentImageClaude = "buddy-sandboxes-claude-code:local"
	harnessCodex            = "codex"
	harnessClaudeCode       = "claude-code"
	harnessClaude           = "claude"
)

// resolveAgentImage resolves supported agent images based on the profile harness type.
func resolveAgentImage(profile *store.AgentProfile) string {
	if profile != nil {
		switch strings.ToLower(profile.HarnessType) {
		case harnessCodex:
			if img := os.Getenv("BUDDY_AGENT_IMAGE_CODEX"); img != "" {
				return img
			}
			return defaultAgentImageCodex
		case harnessClaudeCode, harnessClaude:
			if img := os.Getenv("BUDDY_AGENT_IMAGE_CLAUDE"); img != "" {
				return img
			}
			return defaultAgentImageClaude
		}
	}
	if img := os.Getenv("BUDDY_AGENT_IMAGE_BASE"); img != "" {
		return img
	}
	return defaultAgentImageBase
}

// resolveProviderForHarness maps agent harness types to supported provider credential service IDs.
func resolveProviderForHarness(harnessType string) string {
	switch strings.ToLower(strings.TrimSpace(harnessType)) {
	case harnessCodex:
		return credentials.ServiceOpenAI
	case harnessClaude, harnessClaudeCode:
		return credentials.ServiceAnthropic
	default:
		return ""
	}
}

func (d *TaskDispatcher) configureProviderCredentialsForSandbox(
	ctx context.Context,
	task *store.Task,
	exec *store.Execution,
	profile *store.AgentProfile,
	sbx *sandboxesv1alpha1.Sandbox,
	sbxName string,
) ([]byte, error) {
	harness := ""
	model := ""
	var expiresAt *time.Time
	if profile != nil {
		harness = profile.HarnessType
		model = profile.ModelName
		if profile.TimeoutSeconds > 0 {
			exp := time.Now().Add(time.Duration(profile.TimeoutSeconds+1800) * time.Second)
			expiresAt = &exp
		}
	}
	provider := resolveProviderForHarness(harness)
	if provider == "" {
		return nil, nil
	}
	if d.providerCredentials == nil || d.brokerCapabilities == nil {
		return nil, &NonRetryableExecutionError{
			Err: fmt.Errorf("provider credential store is not configured on gateway"),
		}
	}

	cleanExecID := cleanResourceID(exec.ID)
	creationID := cleanExecID
	if sbx.Annotations == nil {
		sbx.Annotations = make(map[string]string)
	}
	sbx.Annotations[sandboxCreationIDAnnotation] = creationID

	hash, err := BuildProviderBinding(ctx, d.providerCredentials, d.brokerCapabilities, ProviderBindingParams{
		UserID:      task.UserID,
		Namespace:   task.Namespace,
		SandboxName: sbxName,
		CreationID:  creationID,
		Provider:    provider,
		Model:       model,
		ExpiresAt:   expiresAt,
	}, sbx)
	if err != nil {
		var pErr *ProviderBindingError
		if errors.As(err, &pErr) && (pErr.StatusCode == http.StatusConflict || pErr.StatusCode == http.StatusBadRequest) {
			return nil, &NonRetryableExecutionError{
				Err: fmt.Errorf("configure provider credentials: %w", err),
			}
		}
		return nil, fmt.Errorf("configure provider credentials: %w", err)
	}
	return hash, nil
}

func (d *TaskDispatcher) acquireDedicatedWorkspaceRunner(
	ctx context.Context,
	task *store.Task,
	exec *store.Execution,
	profile *store.AgentProfile,
) (RunnerInstance, error) {
	cleanExecID := cleanResourceID(exec.ID)
	sbxName := fmt.Sprintf("sbx-%s", cleanExecID)
	image := resolveAgentImage(profile)

	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sbxName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"sandboxes.buddy.io/task-id":      task.ID,
				"sandboxes.buddy.io/execution-id": exec.ID,
			},
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image: image,
			Args:  []string{"sleep", "infinity"},
			WorkspaceRef: &corev1.LocalObjectReference{
				Name: task.WorkspaceName,
			},
		},
	}

	capabilityHash, err := d.configureProviderCredentialsForSandbox(ctx, task, exec, profile, sbx, sbxName)
	if err != nil {
		return nil, err
	}

	if err := d.client.Create(ctx, sbx); err != nil && !k8serrors.IsAlreadyExists(err) {
		if len(capabilityHash) > 0 && d.brokerCapabilities != nil {
			_ = d.brokerCapabilities.Revoke(ctx, capabilityHash)
		}
		return nil, fmt.Errorf("create sandbox: %w", err)
	}

	if len(capabilityHash) > 0 && d.brokerCapabilities != nil {
		if err := d.brokerCapabilities.Activate(ctx, capabilityHash, string(sbx.UID)); err != nil {
			_ = d.brokerCapabilities.Revoke(ctx, capabilityHash)
			_ = d.client.Delete(ctx, sbx)
			return nil, fmt.Errorf("unable to activate provider capability: %w", err)
		}
	}

	if err := d.store.BindExecutionRunner(ctx, exec.ID, "", sbxName); err != nil {
		if len(capabilityHash) > 0 && d.brokerCapabilities != nil {
			_ = d.brokerCapabilities.Revoke(context.Background(), capabilityHash)
		}
		_ = d.client.Delete(context.Background(), sbx)
		return nil, fmt.Errorf("bind execution runner: %w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 180*time.Second)
	defer waitCancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			if len(capabilityHash) > 0 && d.brokerCapabilities != nil {
				_ = d.brokerCapabilities.Revoke(context.Background(), capabilityHash)
			}
			_ = d.client.Delete(context.Background(), sbx)
			return nil, fmt.Errorf("readiness timeout waiting for sandbox %s", sbxName)
		case <-ticker.C:
			current := &sandboxesv1alpha1.Sandbox{}
			if err := d.client.Get(waitCtx, client.ObjectKey{Namespace: task.Namespace, Name: sbxName}, current); err != nil {
				continue
			}
			if current.Status.Phase == sandboxesv1alpha1.SandboxPhaseRunning && current.Status.PodName != "" {
				return &dedicatedSandboxRunner{
					client:             d.client,
					namespace:          task.Namespace,
					name:               sbxName,
					podName:            current.Status.PodName,
					brokerCapabilities: d.brokerCapabilities,
					capabilityHash:     capabilityHash,
				}, nil
			}
			if current.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed {
				if len(capabilityHash) > 0 && d.brokerCapabilities != nil {
					_ = d.brokerCapabilities.Revoke(context.Background(), capabilityHash)
				}
				_ = d.client.Delete(context.Background(), sbx)
				return nil, fmt.Errorf("sandbox %s failed during startup", sbxName)
			}
		}
	}
}

func (d *TaskDispatcher) acquireLeasedRunner(
	ctx context.Context,
	task *store.Task,
	exec *store.Execution,
	profile *store.AgentProfile,
) (RunnerInstance, error) {
	cleanExecID := cleanResourceID(exec.ID)
	leaseName := fmt.Sprintf("lease-%s", cleanExecID)
	poolName := defaultPoolName
	timeoutSec := int32(3600)
	if profile != nil && profile.TimeoutSeconds > 0 {
		timeoutSec = int32(profile.TimeoutSeconds + profile.VerificationTimeoutSeconds + 60)
	}

	lease := &sandboxesv1alpha1.SandboxLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"sandboxes.buddy.io/task-id":      task.ID,
				"sandboxes.buddy.io/execution-id": exec.ID,
			},
		},
		Spec: sandboxesv1alpha1.SandboxLeaseSpec{
			PoolRef: corev1.LocalObjectReference{
				Name: poolName,
			},
			ExecutionId:    exec.ID,
			TimeoutSeconds: timeoutSec,
		},
	}

	if err := d.client.Create(ctx, lease); err != nil && !k8serrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create sandbox lease: %w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	defer waitCancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("readiness timeout waiting for lease %s", leaseName)
		case <-ticker.C:
			current := &sandboxesv1alpha1.SandboxLease{}
			if err := d.client.Get(waitCtx, client.ObjectKey{Namespace: task.Namespace, Name: leaseName}, current); err != nil {
				continue
			}
			if current.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseBound && current.Status.BoundPodName != "" {
				sbxRef := ""
				if current.Status.BoundSandboxRef != nil {
					sbxRef = current.Status.BoundSandboxRef.Name
				}
				if err := d.store.BindExecutionRunner(ctx, exec.ID, leaseName, sbxRef); err != nil {
					return nil, fmt.Errorf("bind execution runner: %w", err)
				}
				return &leasedSandboxRunner{
					client:    d.client,
					namespace: task.Namespace,
					leaseName: leaseName,
					sbxName:   sbxRef,
					podName:   current.Status.BoundPodName,
				}, nil
			}
			if current.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseFailed || current.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseExpired {
				return nil, fmt.Errorf("sandbox lease %s failed to bind (phase: %s)", leaseName, current.Status.Phase)
			}
		}
	}
}

func (d *TaskDispatcher) acquireRunner(ctx context.Context, task *store.Task, exec *store.Execution, profile *store.AgentProfile) (RunnerInstance, error) {
	if d.client == nil {
		return nil, fmt.Errorf("kubernetes client not configured")
	}

	harness := ""
	if profile != nil {
		harness = strings.ToLower(strings.TrimSpace(profile.HarnessType))
	}
	if task.WorkspaceName == "" && (harness == harnessCodex || harness == harnessClaude || harness == harnessClaudeCode) {
		return nil, &NonRetryableExecutionError{
			Err: fmt.Errorf("agent harness %q requires an explicit workspace", harness),
		}
	}

	if task.WorkspaceName != "" {
		return d.acquireDedicatedWorkspaceRunner(ctx, task, exec, profile)
	}

	return d.acquireLeasedRunner(ctx, task, exec, profile)
}

func (d *TaskDispatcher) executeHarness(ctx context.Context, task *store.Task, exec *store.Execution, profile *store.AgentProfile) (store.ExecutionResult, error) {
	if profile == nil {
		exitCode := 1
		err := errors.New("agent profile is required for headless task execution")
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: err.Error(),
		}, &NonRetryableExecutionError{Err: err}
	}

	if profile.ModelName != "" {
		if err := credentials.ValidateModelName(profile.ModelName); err != nil {
			exitCode := 1
			return store.ExecutionResult{
				Status:       statusFailed,
				ExitCode:     &exitCode,
				ErrorMessage: fmt.Sprintf("invalid model name %q: %v", profile.ModelName, err),
			}, &NonRetryableExecutionError{Err: err}
		}
	}

	harnessType := strings.ToLower(strings.TrimSpace(profile.HarnessType))
	var harnessArgs []string
	switch harnessType {
	case harnessCodex:
		harnessArgs = []string{"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox"}
		if profile.ModelName != "" {
			harnessArgs = append(harnessArgs, "--model", profile.ModelName)
		}
		harnessArgs = append(harnessArgs, "exec", "--skip-git-repo-check", task.Prompt)
	case harnessClaude, harnessClaudeCode:
		harnessArgs = []string{"claude", "--dangerously-skip-permissions"}
		if profile.ModelName != "" {
			harnessArgs = append(harnessArgs, "--model", profile.ModelName)
		}
		harnessArgs = append(harnessArgs, "-p", task.Prompt)
	case "custom":
		harnessArgs = []string{"/bin/sh", "-c", task.Prompt}
	default:
		exitCode := 1
		err := fmt.Errorf("unknown or unsupported harness type %q", profile.HarnessType)
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: err.Error(),
		}, &NonRetryableExecutionError{Err: err}
	}

	if d.executor == nil || d.client == nil {
		exitCode := 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("no execution runner configured for harness %q (execution %s)", harnessType, exec.ID),
		}, nil
	}

	runner, err := d.acquireRunner(ctx, task, exec, profile)
	if err != nil {
		exitCode := 1
		var nonRetryable *NonRetryableExecutionError
		var runErr error = err
		if errors.As(err, &nonRetryable) {
			runErr = nonRetryable
		}
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("acquire runner: %v", err),
		}, runErr
	}

	var finalSuccess bool
	defer func() {
		_ = runner.Release(context.Background(), exec.AttemptNumber, task.MaxAttempts, finalSuccess)
	}()

	cleanExecID := cleanResourceID(exec.ID)

	// 1. Build harness command
	harnessCmd := []string{
		"/opt/buddy/bin/buddy-runner", "start", fmt.Sprintf("harness-%s", cleanExecID),
		"--dir", "/workspace", "--tty=false", "--",
	}
	harnessCmd = append(harnessCmd, harnessArgs...)

	harnessTimeout := 1800 * time.Second
	if profile != nil && profile.TimeoutSeconds > 0 {
		harnessTimeout = time.Duration(profile.TimeoutSeconds) * time.Second
	}
	harnessCtx, harnessCancel := context.WithTimeout(ctx, harnessTimeout)
	defer harnessCancel()

	logWriter := newLiveLogWriter(ctx, d.store, exec.ID)
	defer func() {
		_ = logWriter.Close()
	}()

	exitCode, execErr := d.executor.StreamExec(
		harnessCtx,
		runner.Namespace(),
		runner.PodName(),
		harnessCmd,
		false,
		nil,
		logWriter,
		logWriter,
		nil,
	)

	// Clean up any lingering supervisor process
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	stopCmd := []string{"/opt/buddy/bin/buddy-runner", "stop", fmt.Sprintf("harness-%s", cleanExecID)}
	_, _ = d.executor.StreamExec(stopCtx, runner.Namespace(), runner.PodName(), stopCmd, false, nil, nil, nil, nil)

	_ = logWriter.Close()
	fullLog := logWriter.String()

	if execErr != nil {
		exitCode = 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("stream exec: %v", execErr),
			LogBuffer:    fullLog,
		}, execErr
	}

	result := store.ExecutionResult{
		ExitCode:  &exitCode,
		LogBuffer: fullLog,
	}

	if exitCode != 0 {
		result.Status = statusFailed
		result.ErrorMessage = fmt.Sprintf("agent harness exited with code %d", exitCode)
		return result, nil
	}

	// 2. Verification Gate
	if profile != nil && strings.TrimSpace(profile.VerificationCommand) != "" {
		_ = d.store.TransitionExecutionToVerifying(ctx, exec.ID)
		verifyTimeout := 300 * time.Second
		if profile.VerificationTimeoutSeconds > 0 {
			verifyTimeout = time.Duration(profile.VerificationTimeoutSeconds) * time.Second
		}
		verifyCtx, verifyCancel := context.WithTimeout(ctx, verifyTimeout)
		defer verifyCancel()

		verifyCmd := []string{
			"/opt/buddy/bin/buddy-runner", "start", fmt.Sprintf("verify-%s", cleanExecID),
			"--dir", "/workspace", "--tty=false", "--",
			"/bin/sh", "-c", profile.VerificationCommand,
		}

		verifyBuf := newTailBuffer(maxLogBufferSize)
		vExit, vErr := d.executor.StreamExec(
			verifyCtx,
			runner.Namespace(),
			runner.PodName(),
			verifyCmd,
			false,
			nil,
			verifyBuf,
			verifyBuf,
			nil,
		)

		stopVerifyCmd := []string{"/opt/buddy/bin/buddy-runner", "stop", fmt.Sprintf("verify-%s", cleanExecID)}
		_, _ = d.executor.StreamExec(context.Background(), runner.Namespace(), runner.PodName(), stopVerifyCmd, false, nil, nil, nil, nil)

		result.VerificationOutput = verifyBuf.String()
		passed := (vErr == nil && vExit == 0)
		result.VerificationPassed = &passed

		if !passed {
			result.Status = statusFailed
			result.ExitCode = &vExit
			if vErr != nil {
				result.ErrorMessage = fmt.Sprintf("verification failed: %v", vErr)
			} else {
				result.ErrorMessage = fmt.Sprintf("verification command exited with code %d", vExit)
			}
			return result, nil
		}
	}

	// 3. Extract Git Diff
	diffStat, diffPatch := d.extractGitDiff(ctx, runner.Namespace(), runner.PodName())
	result.DiffStat = diffStat
	result.Diff = diffPatch

	result.Status = statusSucceeded
	finalSuccess = true
	return result, nil
}

type liveLogWriter struct {
	ctx     context.Context
	store   TaskStore
	execID  string
	mu      sync.Mutex
	flushMu sync.Mutex
	buf     *tailBuffer
	pending bytes.Buffer
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func newLiveLogWriter(ctx context.Context, store TaskStore, execID string) *liveLogWriter {
	w := &liveLogWriter{
		ctx:    ctx,
		store:  store,
		execID: execID,
		buf:    newTailBuffer(maxLogBufferSize),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go w.flusher()
	return w
}

const maxLogBufferSize = 64 * 1024

func (w *liveLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.buf.Write(p)
	w.pending.Write(p)
	shouldFlush := w.pending.Len() >= 4096
	w.mu.Unlock()

	if shouldFlush {
		w.flush()
	}
	return len(p), nil
}

func (w *liveLogWriter) flush() {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()

	w.mu.Lock()
	if w.pending.Len() == 0 {
		w.mu.Unlock()
		return
	}
	chunk := w.pending.String()
	w.pending.Reset()
	w.mu.Unlock()

	if w.store != nil {
		_ = w.store.AppendExecutionLog(w.ctx, w.execID, chunk)
	}
}

func (w *liveLogWriter) flusher() {
	defer close(w.doneCh)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			w.flush()
			return
		case <-ticker.C:
			w.flush()
		}
	}
}

func (w *liveLogWriter) Close() error {
	w.mu.Lock()
	select {
	case <-w.stopCh:
		w.mu.Unlock()
		return nil
	default:
		close(w.stopCh)
	}
	w.mu.Unlock()
	<-w.doneCh
	return nil
}

func (w *liveLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		_, err := b.buf.Write(p[:remaining])
		return len(p), err
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func (d *TaskDispatcher) extractGitDiff(ctx context.Context, namespace, podName string) (string, string) {
	diffCtx, diffCancel := context.WithTimeout(ctx, 30*time.Second)
	defer diffCancel()

	// Check if git repository
	checkCmd := []string{"git", "-C", "/workspace", "rev-parse", "--is-inside-work-tree"}
	var checkOut bytes.Buffer
	code, err := d.executor.StreamExec(diffCtx, namespace, podName, checkCmd, false, nil, &checkOut, nil, nil)
	if err != nil || code != 0 {
		return "", ""
	}

	// Check if HEAD exists
	headCmd := []string{"git", "-C", "/workspace", "rev-parse", "--verify", "HEAD"}
	hCode, _ := d.executor.StreamExec(diffCtx, namespace, podName, headCmd, false, nil, nil, nil, nil)
	hasHEAD := (hCode == 0)

	// Stage intent-to-add
	addCmd := []string{"git", "-C", "/workspace", "add", "-A", "-N"}
	_, _ = d.executor.StreamExec(diffCtx, namespace, podName, addCmd, false, nil, nil, nil, nil)

	diffRef := "HEAD"
	if !hasHEAD {
		diffRef = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	}

	// Diff stat
	statCmd := []string{"git", "-C", "/workspace", "-c", "diff.external=", "-c", "core.fsmonitor=", "diff", "--stat", diffRef}
	statOut := newLimitedBuffer(64 * 1024)
	_, _ = d.executor.StreamExec(diffCtx, namespace, podName, statCmd, false, nil, statOut, nil, nil)

	// Diff patch
	patchCmd := []string{"git", "-C", "/workspace", "-c", "diff.external=", "-c", "core.fsmonitor=", "diff", diffRef}
	patchOut := newLimitedBuffer(64 * 1024)
	_, _ = d.executor.StreamExec(diffCtx, namespace, podName, patchCmd, false, nil, patchOut, nil, nil)

	return statOut.String(), patchOut.String()
}
