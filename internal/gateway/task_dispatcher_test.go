package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/store"
)

func TestTaskDispatcherDeterministicVerificationGate(t *testing.T) {
	memStore := newMemTaskStore()
	dispatcher := NewTaskDispatcher(memStore, nil, "worker-1")
	dispatcher.pollInterval = 10 * time.Millisecond
	dispatcher.sweepInterval = 10 * time.Millisecond
	dispatcher.orphanTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register profile with verification command
	_ = memStore.CreateAgentProfile(ctx, &store.AgentProfile{
		ID:                  "prof-test",
		Name:                "Test Profile",
		HarnessType:         "codex",
		ModelName:           "gpt-4o",
		VerificationCommand: "go test ./...",
	})

	// Scenario 1: Agent command exits 0, but verification fails
	profID := "prof-test"
	task1 := &store.Task{
		ID:             "task-vfail",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-repo",
		Prompt:         "Fix bug",
		IdempotencyKey: "key-1",
		AgentProfileID: &profID,
		MaxAttempts:    1,
	}
	_, _, _ = memStore.CreateTask(ctx, task1, "exec-vfail-1")

	// Set custom runner that simulates exit 0 but failed verification
	dispatcher.SetRunnerFunc(func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) store.ExecutionResult {
		exitZero := 0
		verifyFailed := false
		return store.ExecutionResult{
			Status:             testStatusFailed,
			ExitCode:           &exitZero,
			VerificationPassed: &verifyFailed,
			VerificationOutput: "FAIL: TestAuthRace",
			ErrorMessage:       "verification command failed",
		}
	})

	// Run single dispatch step
	dispatcher.pollAndDispatch(ctx)

	// Wait briefly for execution completion
	time.Sleep(30 * time.Millisecond)

	exec1, err := memStore.GetExecution(ctx, "exec-vfail-1")
	if err != nil {
		t.Fatal(err)
	}
	if exec1.Status != testStatusFailed {
		t.Fatalf("expected execution status failed, got %s", exec1.Status)
	}
	if exec1.VerificationPassed == nil || *exec1.VerificationPassed != false {
		t.Fatalf("expected verificationPassed = false, got %v", exec1.VerificationPassed)
	}
	if exec1.VerificationOutput == nil || *exec1.VerificationOutput != "FAIL: TestAuthRace" {
		t.Fatalf("unexpected verification output: %v", exec1.VerificationOutput)
	}
	t1, _ := memStore.GetTask(ctx, "task-vfail")
	if t1.Status != testStatusFailed {
		t.Fatalf("expected task status failed, got %s", t1.Status)
	}

	// Scenario 2: Agent command exits 0 and verification passes
	task2 := &store.Task{
		ID:             "task-vpass",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-repo-2",
		Prompt:         "Fix bug 2",
		IdempotencyKey: "key-2",
		AgentProfileID: &profID,
		MaxAttempts:    1,
	}
	_, _, _ = memStore.CreateTask(ctx, task2, "exec-vpass-1")

	dispatcher.SetRunnerFunc(func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) store.ExecutionResult {
		exitZero := 0
		verifyPassed := true
		return store.ExecutionResult{
			Status:             testStatusSucceeded,
			ExitCode:           &exitZero,
			VerificationPassed: &verifyPassed,
			VerificationOutput: "PASS\nok  pkg/auth",
			DiffStat:           "2 files changed, 10 insertions(+)",
		}
	})

	dispatcher.pollAndDispatch(ctx)
	time.Sleep(30 * time.Millisecond)

	exec2, _ := memStore.GetExecution(ctx, "exec-vpass-1")
	if exec2.Status != testStatusSucceeded {
		t.Fatalf("expected execution status succeeded, got %s", exec2.Status)
	}
	if exec2.VerificationPassed == nil || *exec2.VerificationPassed != true {
		t.Fatalf("expected verificationPassed = true, got %v", exec2.VerificationPassed)
	}
	t2, _ := memStore.GetTask(ctx, "task-vpass")
	if t2.Status != testStatusSucceeded {
		t.Fatalf("expected task status succeeded, got %s", t2.Status)
	}
}

func TestTaskDispatcherWorkspaceConcurrencyMutualExclusion(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	// Task 1 and Task 2 both target the exact same workspace: ws-shared
	task1 := &store.Task{
		ID:             "task-ws-1",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-shared",
		Prompt:         "Task 1",
		IdempotencyKey: "key-ws-1",
		MaxAttempts:    1,
	}
	task2 := &store.Task{
		ID:             "task-ws-2",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-shared",
		Prompt:         "Task 2",
		IdempotencyKey: "key-ws-2",
		MaxAttempts:    1,
	}
	_, _, _ = memStore.CreateTask(ctx, task1, "exec-ws-1")
	_, _, _ = memStore.CreateTask(ctx, task2, "exec-ws-2")

	// Worker claims first execution
	exec1, err := memStore.ClaimNextExecution(ctx, "worker-1")
	if err != nil || exec1 == nil || exec1.ID != "exec-ws-1" {
		t.Fatalf("expected exec-ws-1 claimed, got %v, err: %v", exec1, err)
	}

	// Worker attempts to claim second execution while exec-ws-1 is running
	exec2, err := memStore.ClaimNextExecution(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if exec2 != nil {
		t.Fatalf("expected no execution claimed due to workspace mutex, got %v", exec2.ID)
	}

	// Complete exec-ws-1
	exitZero := 0
	_, _ = memStore.CompleteExecution(ctx, "exec-ws-1", store.ExecutionResult{
		Status:   "succeeded",
		ExitCode: &exitZero,
	}, "")

	// Now exec-ws-2 can be claimed
	exec2After, err := memStore.ClaimNextExecution(ctx, "worker-1")
	if err != nil || exec2After == nil || exec2After.ID != "exec-ws-2" {
		t.Fatalf("expected exec-ws-2 claimed after first finished, got %v, err: %v", exec2After, err)
	}
}

func TestTaskDispatcherOrphanSweeper(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:             "task-orphan",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-orphan",
		Prompt:         "Task orphan",
		IdempotencyKey: "key-orphan",
		MaxAttempts:    1,
	}
	_, _, _ = memStore.CreateTask(ctx, task, "exec-orphan-1")
	exec, _ := memStore.ClaimNextExecution(ctx, "worker-dead")

	// Artificially age the heartbeat
	old := time.Now().UTC().Add(-60 * time.Second)
	exec.LastHeartbeatAt = &old

	// Run sweeper with 30s timeout
	reaped, err := memStore.ReapOrphanExecutions(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].ID != "exec-orphan-1" {
		t.Fatalf("expected exec-orphan-1 reaped, got %v", reaped)
	}

	e, _ := memStore.GetExecution(ctx, "exec-orphan-1")
	if e.Status != testStatusFailed {
		t.Fatalf("expected orphan execution status failed, got %s", e.Status)
	}
}

func TestTaskDispatcherLifecycleWithServer(t *testing.T) {
	memStore := newMemTaskStore()
	server := NewServer(Config{
		ListenAddr:       "127.0.0.1:0",
		DrainTimeout:     50 * time.Millisecond,
		PropagationDelay: 10 * time.Millisecond,
		TaskStore:        memStore,
	})

	if server.taskDispatcher == nil {
		t.Fatal("expected taskDispatcher to be initialized")
	}

	server.taskDispatcher.pollInterval = 10 * time.Millisecond
	server.taskDispatcher.sweepInterval = 10 * time.Millisecond

	go func() {
		_ = server.Start()
	}()

	// Wait for dispatcher to start
	assertRunning := func(expected bool) {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			server.taskDispatcher.mu.Lock()
			running := server.taskDispatcher.running
			server.taskDispatcher.mu.Unlock()
			if running == expected {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("expected dispatcher running = %v", expected)
	}

	assertRunning(true)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("unexpected shutdown error: %v", err)
	}

	assertRunning(false)
}

func TestTaskDispatcherExecuteHarnessFailsExplicitly(t *testing.T) {
	memStore := newMemTaskStore()
	dispatcher := NewTaskDispatcher(memStore, nil, "worker-test")
	dispatcher.pollInterval = 10 * time.Millisecond
	dispatcher.sweepInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task := &store.Task{
		ID:             "task-no-runner",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-no-runner",
		Prompt:         "Fix bug without runner",
		IdempotencyKey: "key-no-runner",
		MaxAttempts:    1,
	}
	_, _, err := memStore.CreateTask(ctx, task, "exec-no-runner-1")
	if err != nil {
		t.Fatal(err)
	}

	// Dispatch without setting a custom runner (runs executeHarness)
	dispatcher.pollAndDispatch(ctx)

	time.Sleep(30 * time.Millisecond)

	exec, err := memStore.GetExecution(ctx, "exec-no-runner-1")
	if err != nil {
		t.Fatal(err)
	}
	if exec.Status != testStatusFailed {
		t.Fatalf("expected execution status failed, got %s", exec.Status)
	}
	if exec.ExitCode == nil || *exec.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %v", exec.ExitCode)
	}
	if exec.ErrorMessage == nil || *exec.ErrorMessage == "" {
		t.Fatal("expected explicit error message")
	}
	if exec.DiffStat != nil && *exec.DiffStat != "" {
		t.Fatalf("expected empty diff stat, got %q", *exec.DiffStat)
	}
	if exec.VerificationPassed != nil {
		t.Fatalf("expected nil VerificationPassed, got %v", *exec.VerificationPassed)
	}
}

func TestTaskCancellationPreservedOnAttemptFailure(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:             "task-cancel-preserve",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-cancel",
		Prompt:         "Task to cancel",
		IdempotencyKey: "key-cancel-preserve",
		MaxAttempts:    1,
	}
	_, _, err := memStore.CreateTask(ctx, task, "exec-cancel-1")
	if err != nil {
		t.Fatal(err)
	}

	exec, err := memStore.ClaimNextExecution(ctx, "worker-1")
	if err != nil || exec == nil {
		t.Fatalf("claim failed: %v", err)
	}

	if err := memStore.CancelTask(ctx, task.ID); err != nil {
		t.Fatalf("cancel task failed: %v", err)
	}

	taskAfterCancel, _ := memStore.GetTask(ctx, task.ID)
	if taskAfterCancel.Status != testStatusCancelled {
		t.Fatalf("expected task status cancelled, got %s", taskAfterCancel.Status)
	}

	exitCode := 1
	retried, err := memStore.CompleteExecution(ctx, exec.ID, store.ExecutionResult{
		Status:       "failed",
		ExitCode:     &exitCode,
		ErrorMessage: "process terminated",
	}, "")
	if err != nil {
		t.Fatalf("complete execution error: %v", err)
	}
	if retried {
		t.Fatal("expected retried = false for cancelled task")
	}

	taskFinal, _ := memStore.GetTask(ctx, task.ID)
	if taskFinal.Status != testStatusCancelled {
		t.Fatalf("expected task status to remain cancelled, but got %s", taskFinal.Status)
	}
}

func TestRejectLateCompletionOfTerminalExecution(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:             "task-late-complete",
		UserID:         "usr-1",
		Namespace:      "default",
		WorkspaceName:  "ws-late",
		Prompt:         "Task for late worker",
		IdempotencyKey: "key-late-complete",
		MaxAttempts:    2,
	}
	_, _, err := memStore.CreateTask(ctx, task, "exec-late-1")
	if err != nil {
		t.Fatal(err)
	}

	exec, err := memStore.ClaimNextExecution(ctx, "worker-stale")
	if err != nil || exec == nil {
		t.Fatalf("claim failed: %v", err)
	}

	old := time.Now().UTC().Add(-60 * time.Second)
	exec.LastHeartbeatAt = &old
	reaped, err := memStore.ReapOrphanExecutions(ctx, 30*time.Second)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("expected 1 execution reaped, got %v, err: %v", reaped, err)
	}

	execAfterReap, _ := memStore.GetExecution(ctx, "exec-late-1")
	if execAfterReap.Status != testStatusFailed {
		t.Fatalf("expected exec status failed after reap, got %s", execAfterReap.Status)
	}

	exitZero := 0
	retried, err := memStore.CompleteExecution(ctx, "exec-late-1", store.ExecutionResult{
		Status:   "succeeded",
		ExitCode: &exitZero,
		DiffStat: "1 file changed",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error on late completion: %v", err)
	}
	if retried {
		t.Fatal("expected retried = false")
	}

	execStillFailed, _ := memStore.GetExecution(ctx, "exec-late-1")
	if execStillFailed.Status != testStatusFailed {
		t.Fatalf("late completion overwrote reaped execution status to %s", execStillFailed.Status)
	}

	taskCurrent, _ := memStore.GetTask(ctx, task.ID)
	if taskCurrent.Status == testStatusSucceeded {
		t.Fatal("late completion overwrote task status to succeeded")
	}
}
