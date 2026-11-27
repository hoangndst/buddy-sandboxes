package gateway

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TaskRunnerFunc defines a pluggable runner function for task execution (e.g. for testing).
type TaskRunnerFunc func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) store.ExecutionResult

// RunnerInstance represents a running container environment executing an agent task.
type RunnerInstance interface {
	PodName() string
	SandboxName() string
	Namespace() string
	Release(ctx context.Context, attemptNumber int, maxAttempts int, success bool) error
}

type dedicatedSandboxRunner struct {
	client    client.Client
	namespace string
	name      string
	podName   string
}

func (r *dedicatedSandboxRunner) PodName() string     { return r.podName }
func (r *dedicatedSandboxRunner) SandboxName() string { return r.name }
func (r *dedicatedSandboxRunner) Namespace() string   { return r.namespace }

func (r *dedicatedSandboxRunner) Release(ctx context.Context, attemptNumber int, maxAttempts int, success bool) error {
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
	store         TaskStore
	client        client.Client
	executor      RemoteStreamExecutor
	workerID      string
	runnerFunc    TaskRunnerFunc
	pollInterval  time.Duration
	sweepInterval time.Duration
	orphanTimeout time.Duration
	stopCh        chan struct{}
	wg            sync.WaitGroup
	mu            sync.Mutex
	running       bool
	workerSem     chan struct{}
}

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
	}
}

// SetRunnerFunc sets a custom task execution function (used in unit tests).
func (d *TaskDispatcher) SetRunnerFunc(fn TaskRunnerFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runnerFunc = fn
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
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)

	go func() {
		hbTicker := time.NewTicker(10 * time.Second)
		defer hbTicker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-hbTicker.C:
				_ = d.store.UpdateExecutionHeartbeat(ctx, exec.ID)
			}
		}
	}()

	task, err := d.store.GetTask(ctx, exec.TaskID)
	if err != nil {
		_, _ = d.store.CompleteExecution(ctx, exec.ID, store.ExecutionResult{
			Status:       "failed",
			ErrorMessage: fmt.Sprintf("fetch task: %v", err),
		}, "")
		return
	}

	var profile *store.AgentProfile
	if task.AgentProfileID != nil && *task.AgentProfileID != "" {
		profile, _ = d.store.GetAgentProfile(ctx, *task.AgentProfileID)
	}

	d.mu.Lock()
	customRunner := d.runnerFunc
	d.mu.Unlock()

	var result store.ExecutionResult
	if customRunner != nil {
		result = customRunner(ctx, exec, profile)
	} else {
		result = d.executeHarness(ctx, task, exec, profile)
	}

	nextExecID := fmt.Sprintf("%s-att%d", exec.TaskID, exec.AttemptNumber+1)
	_, _ = d.store.CompleteExecution(ctx, exec.ID, result, nextExecID)
}

const (
	defaultHarness          = "default"
	defaultPoolName         = "default"
	statusFailed            = "failed"
	statusSucceeded         = "succeeded"
	defaultAgentImageBase   = "buddy-sandboxes-base:local"
	defaultAgentImageCodex  = "buddy-sandboxes-codex:local"
	defaultAgentImageClaude = "buddy-sandboxes-claude-code:local"
)

// resolveAgentImage resolves supported agent images based on the profile harness type.
func resolveAgentImage(profile *store.AgentProfile) string {
	if profile != nil {
		switch strings.ToLower(profile.HarnessType) {
		case "codex":
			if img := os.Getenv("BUDDY_AGENT_IMAGE_CODEX"); img != "" {
				return img
			}
			return defaultAgentImageCodex
		case "claude-code", "claude":
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

func (d *TaskDispatcher) acquireRunner(ctx context.Context, task *store.Task, exec *store.Execution, profile *store.AgentProfile) (RunnerInstance, error) {
	if d.client == nil {
		return nil, fmt.Errorf("kubernetes client not configured")
	}

	if task.WorkspaceName != "" {
		sbxName := fmt.Sprintf("sbx-%s", exec.ID)
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
				WorkspaceRef: &corev1.LocalObjectReference{
					Name: task.WorkspaceName,
				},
			},
		}

		if err := d.client.Create(ctx, sbx); err != nil && !k8serrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create sandbox: %w", err)
		}

		waitCtx, waitCancel := context.WithTimeout(ctx, 180*time.Second)
		defer waitCancel()

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-waitCtx.Done():
				return nil, fmt.Errorf("readiness timeout waiting for sandbox %s", sbxName)
			case <-ticker.C:
				current := &sandboxesv1alpha1.Sandbox{}
				if err := d.client.Get(waitCtx, client.ObjectKey{Namespace: task.Namespace, Name: sbxName}, current); err != nil {
					continue
				}
				if current.Status.Phase == sandboxesv1alpha1.SandboxPhaseRunning && current.Status.PodName != "" {
					return &dedicatedSandboxRunner{
						client:    d.client,
						namespace: task.Namespace,
						name:      sbxName,
						podName:   current.Status.PodName,
					}, nil
				}
				if current.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed {
					return nil, fmt.Errorf("sandbox %s failed during startup", sbxName)
				}
			}
		}
	}

	leaseName := fmt.Sprintf("lease-%s", exec.ID)
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

func (d *TaskDispatcher) executeHarness(ctx context.Context, task *store.Task, exec *store.Execution, profile *store.AgentProfile) store.ExecutionResult {
	if d.executor == nil || d.client == nil {
		exitCode := 1
		harnessType := defaultHarness
		if profile != nil && profile.HarnessType != "" {
			harnessType = profile.HarnessType
		}
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("no execution runner configured for harness %q (execution %s)", harnessType, exec.ID),
		}
	}

	runner, err := d.acquireRunner(ctx, task, exec, profile)
	if err != nil {
		exitCode := 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: fmt.Sprintf("acquire runner: %v", err),
		}
	}

	var finalSuccess bool
	defer func() {
		_ = runner.Release(context.Background(), exec.AttemptNumber, task.MaxAttempts, finalSuccess)
	}()

	// 1. Build harness command
	harnessCmd := []string{
		"/opt/buddy/bin/buddy-runner", "start", fmt.Sprintf("harness-%s", exec.ID),
		"--dir", "/workspace", "--tty=false", "--",
	}

	if profile != nil && profile.HarnessType == "codex" {
		harnessCmd = append(harnessCmd, "codex", "exec", task.Prompt)
	} else if profile != nil && profile.HarnessType == "claude-code" {
		harnessCmd = append(harnessCmd, "claude", "--print", "--dangerously-skip-permissions", "--prompt", task.Prompt)
	} else {
		harnessCmd = append(harnessCmd, "/bin/sh", "-c", task.Prompt)
	}

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
	stopCmd := []string{"/opt/buddy/bin/buddy-runner", "stop", fmt.Sprintf("harness-%s", exec.ID)}
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
		}
	}

	result := store.ExecutionResult{
		ExitCode:  &exitCode,
		LogBuffer: fullLog,
	}

	if exitCode != 0 {
		result.Status = statusFailed
		result.ErrorMessage = fmt.Sprintf("agent harness exited with code %d", exitCode)
		return result
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
			"/opt/buddy/bin/buddy-runner", "start", fmt.Sprintf("verify-%s", exec.ID),
			"--dir", "/workspace", "--tty=false", "--",
			"/bin/sh", "-c", profile.VerificationCommand,
		}

		var verifyBuf bytes.Buffer
		vExit, vErr := d.executor.StreamExec(
			verifyCtx,
			runner.Namespace(),
			runner.PodName(),
			verifyCmd,
			false,
			nil,
			&verifyBuf,
			&verifyBuf,
			nil,
		)

		stopVerifyCmd := []string{"/opt/buddy/bin/buddy-runner", "stop", fmt.Sprintf("verify-%s", exec.ID)}
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
			return result
		}
	}

	// 3. Extract Git Diff
	diffStat, diffPatch := d.extractGitDiff(ctx, runner.Namespace(), runner.PodName())
	result.DiffStat = diffStat
	result.Diff = diffPatch

	result.Status = statusSucceeded
	finalSuccess = true
	return result
}

type liveLogWriter struct {
	ctx     context.Context
	store   TaskStore
	execID  string
	mu      sync.Mutex
	buf     bytes.Buffer
	pending bytes.Buffer
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func newLiveLogWriter(ctx context.Context, store TaskStore, execID string) *liveLogWriter {
	w := &liveLogWriter{
		ctx:    ctx,
		store:  store,
		execID: execID,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go w.flusher()
	return w
}

func (w *liveLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.pending.Write(p)
	shouldFlush := w.pending.Len() >= 4096
	w.mu.Unlock()

	if shouldFlush {
		w.flush()
	}
	return len(p), nil
}

func (w *liveLogWriter) flush() {
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
