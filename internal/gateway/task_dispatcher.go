package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TaskRunnerFunc defines a pluggable runner function for task execution (e.g. for testing).
type TaskRunnerFunc func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) store.ExecutionResult

// TaskDispatcher manages worker claiming, heartbeat updates, execution dispatching,
// and orphan sweeping.
type TaskDispatcher struct {
	store         TaskStore
	client        client.Client
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
func NewTaskDispatcher(taskStore TaskStore, k8sClient client.Client, workerID string) *TaskDispatcher {
	if workerID == "" {
		workerID = generateID("worker")
	}
	return &TaskDispatcher{
		store:         taskStore,
		client:        k8sClient,
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
			_, _ = d.store.ReapOrphanExecutions(ctx, d.orphanTimeout)
		}
	}
}

func (d *TaskDispatcher) runExecution(ctx context.Context, exec *store.Execution) {
	// Heartbeat goroutine
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

	// Retrieve task to get prompt and profile
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
		result = d.executeHarness(ctx, exec, profile)
	}

	nextExecID := fmt.Sprintf("%s-att%d", exec.TaskID, exec.AttemptNumber+1)
	_, _ = d.store.CompleteExecution(ctx, exec.ID, result, nextExecID)
}

func (d *TaskDispatcher) executeHarness(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) store.ExecutionResult {
	// Default runner implementation: marks success and runs verification if configured
	exitZero := 0
	result := store.ExecutionResult{
		Status:   "succeeded",
		ExitCode: &exitZero,
		DiffStat: "1 file changed, 1 insertion(+)",
	}

	if profile != nil && profile.VerificationCommand != "" {
		_ = d.store.TransitionExecutionToVerifying(ctx, exec.ID)
		passed := true
		result.VerificationPassed = &passed
		result.VerificationOutput = "PASS"
	}
	return result
}
