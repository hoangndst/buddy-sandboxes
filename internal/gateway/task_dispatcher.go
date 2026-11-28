package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/agents"
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
	defaultHarness  = "default"
	defaultPoolName = "default"
	statusFailed    = "failed"
	statusSucceeded = "succeeded"
	statusCancelled = "cancelled"
	exitCodeConfig  = 78
)

func (d *TaskDispatcher) configureProviderCredentialsForSandbox(
	ctx context.Context,
	task *store.Task,
	exec *store.Execution,
	profile *store.AgentProfile,
	plan agents.RunPlan,
	sbx *sandboxesv1alpha1.Sandbox,
	sbxName string,
) ([]byte, error) {
	provider := plan.Provider
	if provider == "" {
		return nil, nil
	}
	model := ""
	if profile != nil {
		model = profile.ModelName
	}
	timeoutSec := plan.TimeoutSeconds
	verifySec := plan.VerificationTimeoutSeconds
	exp := time.Now().Add(time.Duration(timeoutSec+verifySec+1800) * time.Second)
	expiresAt := &exp

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
	plan agents.RunPlan,
) (RunnerInstance, error) {
	cleanExecID := cleanResourceID(exec.ID)
	sbxName := fmt.Sprintf("sbx-%s", cleanExecID)
	image := plan.Image

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

	capabilityHash, err := d.configureProviderCredentialsForSandbox(ctx, task, exec, profile, plan, sbx, sbxName)
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

func (d *TaskDispatcher) acquireRunner(ctx context.Context, task *store.Task, exec *store.Execution, profile *store.AgentProfile, plan agents.RunPlan) (RunnerInstance, error) {
	if d.client == nil {
		return nil, fmt.Errorf("kubernetes client not configured")
	}

	if task.WorkspaceName == "" && plan.RequiresWorkspace {
		return nil, &NonRetryableExecutionError{
			Err: fmt.Errorf("agent harness %q requires an explicit workspace", profile.HarnessType),
		}
	}

	return d.acquireDedicatedWorkspaceRunner(ctx, task, exec, profile, plan)
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

	agent := agents.AgentFromProfile(profile)
	harness := agents.HarnessFromEnv(profile.HarnessType)
	plan, err := agents.PlanWithExecution(task, exec, agent, harness)
	if err != nil {
		exitCode := 1
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
			ErrorMessage: fmt.Sprintf("no execution runner configured for harness %q (execution %s)", profile.HarnessType, exec.ID),
		}, nil
	}

	runner, err := d.acquireRunner(ctx, task, exec, profile, plan)
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

	// 1. Deliver task specification via buddy-runner write-file /run/buddy/task.json
	writeTaskCmd := []string{"/opt/buddy/bin/buddy-runner", "write-file", agents.DefaultTaskFile}
	writeCtx, writeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer writeCancel()
	_, writeErr := d.executor.StreamExec(
		writeCtx,
		runner.Namespace(),
		runner.PodName(),
		writeTaskCmd,
		false,
		bytes.NewReader(plan.TaskJSON),
		nil,
		nil,
		nil,
	)
	if writeErr != nil {
		exitCode := 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("write task specification: %v", writeErr),
		}, writeErr
	}

	// 2. Deliver prompt file via buddy-runner write-file /run/buddy/prompt.txt
	writePromptCmd := []string{"/opt/buddy/bin/buddy-runner", "write-file", agents.DefaultPromptFile}
	_, writePromptErr := d.executor.StreamExec(
		writeCtx,
		runner.Namespace(),
		runner.PodName(),
		writePromptCmd,
		false,
		bytes.NewReader(plan.PromptText),
		nil,
		nil,
		nil,
	)
	if writePromptErr != nil {
		exitCode := 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("write prompt file: %v", writePromptErr),
		}, writePromptErr
	}

	// 3. Deliver system-prompt file if configured
	if len(plan.SystemPromptText) > 0 {
		writeSysPromptCmd := []string{"/opt/buddy/bin/buddy-runner", "write-file", agents.DefaultSystemPromptFile}
		_, writeSysErr := d.executor.StreamExec(
			writeCtx,
			runner.Namespace(),
			runner.PodName(),
			writeSysPromptCmd,
			false,
			bytes.NewReader(plan.SystemPromptText),
			nil,
			nil,
			nil,
		)
		if writeSysErr != nil {
			exitCode := 1
			return store.ExecutionResult{
				Status:       statusFailed,
				ExitCode:     &exitCode,
				ErrorMessage: fmt.Sprintf("write system prompt file: %v", writeSysErr),
			}, writeSysErr
		}
	}

	// 4. Launch supervised agent run under PTY session
	harnessCmd := []string{
		"/opt/buddy/bin/buddy-runner", "start", fmt.Sprintf("harness-%s", cleanExecID),
		"--dir", "/workspace", "--tty=false", "--",
	}
	harnessCmd = append(harnessCmd, plan.Command...)

	totalDeadline := time.Duration(plan.TimeoutSeconds+plan.VerificationTimeoutSeconds)*time.Second + 30*time.Second
	harnessCtx, harnessCancel := context.WithTimeout(ctx, totalDeadline)
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
		if errors.Is(execErr, context.DeadlineExceeded) || errors.Is(harnessCtx.Err(), context.DeadlineExceeded) {
			exitCode = 124
			return store.ExecutionResult{
				Status:       statusFailed,
				ExitCode:     &exitCode,
				ErrorMessage: "execution total deadline exceeded",
				LogBuffer:    fullLog,
			}, execErr
		}
		exitCode = 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("stream exec: %v", execErr),
			LogBuffer:    fullLog,
		}, execErr
	}

	result, ok, err := d.evaluateInGuestResult(ctx, exec.ID, runner, exitCode, fullLog)
	if !ok || err != nil {
		return result, err
	}

	// 6. Extract Git Diff
	diffStat, diffPatch := d.extractGitDiff(ctx, runner.Namespace(), runner.PodName())
	result.DiffStat = diffStat
	result.Diff = diffPatch

	result.Status = statusSucceeded
	finalSuccess = true
	return result, nil
}

func (d *TaskDispatcher) evaluateInGuestResult(
	ctx context.Context,
	execID string,
	runner RunnerInstance,
	exitCode int,
	fullLog string,
) (store.ExecutionResult, bool, error) {
	result := store.ExecutionResult{
		ExitCode:  &exitCode,
		LogBuffer: fullLog,
	}

	// Inspect in-guest result record from /run/buddy/result.json
	readResultCtx, readCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer readCancel()
	var resultBuf bytes.Buffer
	_, _ = d.executor.StreamExec(readResultCtx, runner.Namespace(), runner.PodName(), []string{"cat", agents.DefaultResultFile}, false, nil, &resultBuf, nil, nil)

	if resultBuf.Len() == 0 {
		failCode := exitCode
		if failCode == 0 {
			failCode = 1
		}
		result.Status = statusFailed
		result.ExitCode = &failCode
		result.ErrorMessage = fmt.Sprintf("missing result record %s", agents.DefaultResultFile)
		return result, false, nil
	}

	var resSpec agents.ResultSpec
	if err := json.Unmarshal(resultBuf.Bytes(), &resSpec); err != nil {
		failCode := exitCode
		if failCode == 0 {
			failCode = 1
		}
		result.Status = statusFailed
		result.ExitCode = &failCode
		result.ErrorMessage = fmt.Sprintf("unmarshal result record: %v", err)
		return result, false, nil
	}

	if resSpec.Verification != nil {
		result.VerificationPassed = &resSpec.Verification.Passed
		result.VerificationOutput = resSpec.Verification.Output
		if d.store != nil {
			_ = d.store.TransitionExecutionToVerifying(ctx, execID)
		}
	}

	if resSpec.Status == statusFailed || resSpec.ExitCode != 0 || (resSpec.Verification != nil && !resSpec.Verification.Passed) {
		result.Status = statusFailed
		exitC := resSpec.ExitCode
		if exitC == 0 && exitCode != 0 {
			exitC = exitCode
		} else if exitC == 0 {
			exitC = 1
		}
		result.ExitCode = &exitC
		if resSpec.Verification != nil && !resSpec.Verification.Passed {
			result.ErrorMessage = "verification gate failed"
		} else {
			result.ErrorMessage = fmt.Sprintf("agent harness exited with code %d", exitC)
		}
		if exitC == exitCodeConfig {
			return result, false, &NonRetryableExecutionError{Err: errors.New(result.ErrorMessage)}
		}
		return result, false, nil
	}

	if exitCode != 0 {
		result.Status = statusFailed
		result.ErrorMessage = fmt.Sprintf("agent harness exited with code %d", exitCode)
		if exitCode == exitCodeConfig {
			return result, false, &NonRetryableExecutionError{Err: errors.New(result.ErrorMessage)}
		}
		return result, false, nil
	}

	return result, true, nil
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
		flushCtx := w.ctx
		if flushCtx.Err() != nil {
			var cancel context.CancelFunc
			flushCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
		}
		_ = w.store.AppendExecutionLog(flushCtx, w.execID, chunk)
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
