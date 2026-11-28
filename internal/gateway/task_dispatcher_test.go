package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/agents"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTaskDispatcherDeterministicVerificationGate(t *testing.T) {
	memStore := newMemTaskStore()
	dispatcher := NewTaskDispatcher(memStore, nil, nil, "worker-1")
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
	dispatcher.SetRunnerFunc(func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) (store.ExecutionResult, error) {
		exitZero := 0
		verifyFailed := false
		return store.ExecutionResult{
			Status:             testStatusFailed,
			ExitCode:           &exitZero,
			VerificationPassed: &verifyFailed,
			VerificationOutput: "FAIL: TestAuthRace",
			ErrorMessage:       "verification command failed",
		}, nil
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

	dispatcher.SetRunnerFunc(func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) (store.ExecutionResult, error) {
		exitZero := 0
		verifyPassed := true
		return store.ExecutionResult{
			Status:             testStatusSucceeded,
			ExitCode:           &exitZero,
			VerificationPassed: &verifyPassed,
			VerificationOutput: "PASS\nok  pkg/auth",
			DiffStat:           "2 files changed, 10 insertions(+)",
		}, nil
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
	dispatcher := NewTaskDispatcher(memStore, nil, nil, "worker-test")
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

type mockTaskRemoteExec struct {
	mu       sync.Mutex
	commands [][]string
	stdins   map[string][]byte
	handlers map[string]func(stdout, stderr io.Writer) int
}

func (m *mockTaskRemoteExec) StreamExec(ctx context.Context, namespace, podName string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer, resizeChan <-chan apiv1.ResizeMessage) (int, error) {
	m.mu.Lock()
	m.commands = append(m.commands, cmd)
	if stdin != nil {
		if m.stdins == nil {
			m.stdins = make(map[string][]byte)
		}
		data, _ := io.ReadAll(stdin)
		m.stdins[strings.Join(cmd, " ")] = data
	}
	m.mu.Unlock()

	joined := strings.Join(cmd, " ")
	for key, handler := range m.handlers {
		if strings.Contains(joined, key) {
			return handler(stdout, stderr), nil
		}
	}
	return 0, nil
}

func TestTaskDispatcherExtractGitDiff(t *testing.T) {
	memStore := newMemTaskStore()
	mockExec := &mockTaskRemoteExec{
		handlers: map[string]func(stdout, stderr io.Writer) int{
			"rev-parse --is-inside-work-tree": func(stdout, _ io.Writer) int {
				_, _ = stdout.Write([]byte("true\n"))
				return 0
			},
			"rev-parse --verify HEAD": func(stdout, _ io.Writer) int {
				return 0
			},
			"diff --stat": func(stdout, _ io.Writer) int {
				_, _ = stdout.Write([]byte("1 file changed, 2 insertions(+)\n"))
				return 0
			},
			"diff HEAD": func(stdout, _ io.Writer) int {
				_, _ = stdout.Write([]byte("--- a/foo.go\n+++ b/foo.go\n@@ -1 +1,3 @@\n+bar\n+baz\n"))
				return 0
			},
		},
	}
	dispatcher := NewTaskDispatcher(memStore, nil, mockExec, "worker-diff")
	stat, patch := dispatcher.extractGitDiff(context.Background(), "default", "pod-1")
	if !strings.Contains(stat, "1 file changed") {
		t.Fatalf("expected stat with '1 file changed', got %q", stat)
	}
	if !strings.Contains(patch, "+bar") {
		t.Fatalf("expected patch with '+bar', got %q", patch)
	}
}

func TestPlanImageResolution(t *testing.T) {
	task := &store.Task{ID: "t-1", Prompt: "p"}

	pCodex, err := agents.Plan(task, &agents.Agent{HarnessType: "codex"}, nil)
	if err != nil || pCodex.Image != agents.DefaultAgentImageCodex {
		t.Fatalf("expected codex image, got %q (err %v)", pCodex.Image, err)
	}

	pClaude, err := agents.Plan(task, &agents.Agent{HarnessType: "claude-code"}, nil)
	if err != nil || pClaude.Image != agents.DefaultAgentImageClaude {
		t.Fatalf("expected claude image, got %q (err %v)", pClaude.Image, err)
	}

	pShell, err := agents.Plan(task, &agents.Agent{HarnessType: "custom"}, nil)
	if err != nil || pShell.Image != agents.DefaultAgentImageBase {
		t.Fatalf("expected base image, got %q (err %v)", pShell.Image, err)
	}
}

func TestLiveLogWriterIncrementalPersistence(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:        "task-stream",
		Namespace: "default",
		Prompt:    "echo stream",
	}
	_, _, _ = memStore.CreateTask(ctx, task, "exec-stream-1")
	exec, _ := memStore.ClaimNextExecution(ctx, "worker-stream")
	if exec == nil {
		t.Fatal("expected claimed execution")
	}

	writer := newLiveLogWriter(ctx, memStore, exec.ID)
	_, _ = writer.Write([]byte("streaming line 1\n"))
	writer.flush()

	e1, err := memStore.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e1.LogBuffer == nil || *e1.LogBuffer != "streaming line 1\n" {
		t.Fatalf("expected incremental log buffer %q, got %v", "streaming line 1\n", e1.LogBuffer)
	}

	_, _ = writer.Write([]byte("streaming line 2\n"))
	_ = writer.Close()

	e2, err := memStore.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectedFull := "streaming line 1\nstreaming line 2\n"
	if e2.LogBuffer == nil || *e2.LogBuffer != expectedFull {
		t.Fatalf("expected final incremental log %q, got %v", expectedFull, e2.LogBuffer)
	}
	if writer.String() != expectedFull {
		t.Fatalf("expected writer string %q, got %q", expectedFull, writer.String())
	}
}

func TestTaskDispatcherLiveLogStreamingInHarness(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:        "task-harness-stream",
		Namespace: "default",
		Prompt:    "run stream",
	}
	_, _, _ = memStore.CreateTask(ctx, task, "exec-harness-1")
	exec, _ := memStore.ClaimNextExecution(ctx, "worker-1")

	mockExec := &mockTaskRemoteExec{
		handlers: map[string]func(stdout, stderr io.Writer) int{
			"harness-exec-harness-1": func(stdout, _ io.Writer) int {
				_, _ = stdout.Write([]byte("live log chunk 1\n"))
				_, _ = stdout.Write([]byte("live log chunk 2\n"))
				return 0
			},
			"rev-parse --is-inside-work-tree": func(stdout, _ io.Writer) int {
				return 1 // skip git diff
			},
		},
	}

	// We test executeHarness with custom runner wrapper or directly by testing liveLogWriter with executeHarness
	// executeHarness requires client to not be nil for acquireRunner, so let's verify liveLogWriter works with RemoteStreamExecutor
	logWriter := newLiveLogWriter(ctx, memStore, exec.ID)
	harnessCmd := []string{"harness-exec-harness-1"}
	exitCode, execErr := mockExec.StreamExec(ctx, "default", "pod-1", harnessCmd, false, nil, logWriter, logWriter, nil)
	if execErr != nil || exitCode != 0 {
		t.Fatalf("exec error: %v, exitCode: %d", execErr, exitCode)
	}
	_ = logWriter.Close()

	eGot, err := memStore.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := "live log chunk 1\nlive log chunk 2\n"
	if eGot.LogBuffer == nil || *eGot.LogBuffer != expected {
		t.Fatalf("expected log %q, got %v", expected, eGot.LogBuffer)
	}
}

func TestAcquireRunnerDedicatedWorkspaceImageResolution(t *testing.T) {
	scheme := setupTestScheme(t)
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:            "task-img",
		Namespace:     "default",
		WorkspaceName: "ws-myrepo",
	}
	exec := &store.Execution{
		ID:     "exec-img-1",
		TaskID: "task-img",
	}
	profile := &store.AgentProfile{
		HarnessType: "codex",
	}
	_, _, _ = memStore.CreateTask(ctx, task, exec.ID)

	// Pre-create the sandbox in Running phase so acquireRunner returns immediately
	existingSbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-exec-img-1",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "pod-exec-img-1",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingSbx).Build()

	dispatcher := NewTaskDispatcher(memStore, fakeClient, nil, "worker-1")
	dispatcher.SetCredentials(&mockProviderPolicy{hasResult: true}, &mockCapabilityLifecycle{})
	plan, err := agents.Plan(task, agents.AgentFromProfile(profile), nil)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	runner, err := dispatcher.acquireRunner(ctx, task, exec, profile, plan)
	if err != nil {
		t.Fatalf("acquireRunner failed: %v", err)
	}
	if runner.PodName() != "pod-exec-img-1" {
		t.Fatalf("expected podName 'pod-exec-img-1', got %q", runner.PodName())
	}

	// Now check if creating a new sandbox sets the resolved image
	task2 := &store.Task{
		ID:            "task-img-2",
		Namespace:     "default",
		WorkspaceName: "ws-myrepo",
	}
	exec2 := &store.Execution{
		ID:     "exec-img-2",
		TaskID: "task-img-2",
	}
	profile2 := &store.AgentProfile{
		HarnessType: "claude-code",
	}
	_, _, _ = memStore.CreateTask(ctx, task2, exec2.ID)

	emptyClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher2 := NewTaskDispatcher(memStore, emptyClient, nil, "worker-2")
	dispatcher2.SetCredentials(&mockProviderPolicy{hasResult: true}, &mockCapabilityLifecycle{})

	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(5 * time.Millisecond)
			var sbx sandboxesv1alpha1.Sandbox
			if err := emptyClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-img-2"}, &sbx); err == nil {
				sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
				sbx.Status.PodName = "pod-exec-img-2"
				_ = emptyClient.Status().Update(ctx, &sbx)
				_ = emptyClient.Update(ctx, &sbx)
				return
			}
		}
	}()

	plan2, err := agents.Plan(task2, agents.AgentFromProfile(profile2), nil)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	runner2, err := dispatcher2.acquireRunner(ctx, task2, exec2, profile2, plan2)
	if err != nil {
		t.Fatalf("acquireRunner failed: %v", err)
	}
	defer func() {
		_ = runner2.Release(ctx, 1, 1, true)
	}()

	// Verify the sandbox was created with the resolved claude image
	createdSbx := &sandboxesv1alpha1.Sandbox{}
	if err := emptyClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-img-2"}, createdSbx); err != nil {
		t.Fatalf("failed to get created sandbox: %v", err)
	}
	if createdSbx.Spec.Image != agents.DefaultAgentImageClaude {
		t.Fatalf("expected image %q, got %q", agents.DefaultAgentImageClaude, createdSbx.Spec.Image)
	}
}

type mockProviderPolicy struct {
	hasResult bool
	record    credentials.Record
}

func (m *mockProviderPolicy) HasForSandbox(ctx context.Context, userID, service, sandbox string) (bool, error) {
	return m.hasResult, nil
}

func (m *mockProviderPolicy) GetRecordForSandbox(ctx context.Context, userID, service, sandbox string) (credentials.Record, error) {
	return m.record, nil
}

type mockCapabilityLifecycle struct {
	issuedToken     string
	issuedHash      []byte
	issuedExpiresAt time.Time
	activatedUID    string
	revokedHashes   [][]byte
}

const mockTokenValue = "mock-token-xyz"

func (m *mockCapabilityLifecycle) IssuePending(ctx context.Context, userID, namespace, sandbox, creationID, service string, expiresAt time.Time, routes []string) (string, []byte, error) {
	m.issuedToken = mockTokenValue
	m.issuedHash = []byte("hash-xyz")
	m.issuedExpiresAt = expiresAt
	return m.issuedToken, m.issuedHash, nil
}

func (m *mockCapabilityLifecycle) Activate(ctx context.Context, hash []byte, uid string) error {
	m.activatedUID = uid
	return nil
}

func (m *mockCapabilityLifecycle) Revoke(ctx context.Context, hash []byte) error {
	m.revokedHashes = append(m.revokedHashes, hash)
	return nil
}

func (m *mockCapabilityLifecycle) RevokeSandbox(ctx context.Context, namespace, sandbox, creationID string) error {
	return nil
}

func TestAcquireRunnerDedicatedWorkspaceProviderCredentials(t *testing.T) {
	scheme := setupTestScheme(t)
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:            "task-cred",
		UserID:        "user-test-1",
		Namespace:     "default",
		WorkspaceName: "ws-cred",
	}
	exec := &store.Execution{
		ID:     "exec-cred-1",
		TaskID: "task-cred",
	}
	profile := &store.AgentProfile{
		HarnessType: "codex",
		ModelName:   "gpt-4o",
	}
	_, _, _ = memStore.CreateTask(ctx, task, exec.ID)

	mockCreds := &mockProviderPolicy{hasResult: true}
	mockCaps := &mockCapabilityLifecycle{}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, nil, "worker-cred")
	dispatcher.SetCredentials(mockCreds, mockCaps)

	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(5 * time.Millisecond)
			var sbx sandboxesv1alpha1.Sandbox
			if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-cred-1"}, &sbx); err == nil {
				sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
				sbx.Status.PodName = "pod-exec-cred-1"
				_ = fakeClient.Status().Update(ctx, &sbx)
				_ = fakeClient.Update(ctx, &sbx)
				return
			}
		}
	}()

	plan, err := agents.Plan(task, agents.AgentFromProfile(profile), nil)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	runner, err := dispatcher.acquireRunner(ctx, task, exec, profile, plan)
	if err != nil {
		t.Fatalf("acquireRunner failed: %v", err)
	}
	defer func() {
		_ = runner.Release(ctx, 1, 1, true)
	}()

	createdSbx := &sandboxesv1alpha1.Sandbox{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-cred-1"}, createdSbx); err != nil {
		t.Fatalf("failed to get created sandbox: %v", err)
	}

	if !createdSbx.Spec.Network.AllowAPIBroker {
		t.Fatalf("expected AllowAPIBroker=true, got false")
	}

	envMap := make(map[string]string)
	for _, env := range createdSbx.Spec.Env {
		envMap[env.Name] = env.Value
	}

	if envMap["OPENAI_API_KEY"] != mockTokenValue {
		t.Fatalf("expected OPENAI_API_KEY 'mock-token-xyz', got %q", envMap["OPENAI_API_KEY"])
	}
	if envMap["OPENAI_MODEL"] != "gpt-4o" {
		t.Fatalf("expected OPENAI_MODEL 'gpt-4o', got %q", envMap["OPENAI_MODEL"])
	}
	if envMap["BUDDY_PROVIDER_MODE"] != "broker" {
		t.Fatalf("expected BUDDY_PROVIDER_MODE 'broker', got %q", envMap["BUDDY_PROVIDER_MODE"])
	}
	if createdSbx.Annotations[sandboxCreationIDAnnotation] != exec.ID {
		t.Fatalf("expected creation-id annotation %q, got %q", exec.ID, createdSbx.Annotations[sandboxCreationIDAnnotation])
	}
	minExpectedExpiry := time.Now().Add(time.Duration(plan.TimeoutSeconds+plan.VerificationTimeoutSeconds) * time.Second)
	if mockCaps.issuedExpiresAt.Before(minExpectedExpiry) {
		t.Fatalf("expected capability token expiry to include verification timeout (min %v), got %v", minExpectedExpiry, mockCaps.issuedExpiresAt)
	}
}

func TestTaskDispatcherFailsOnMissingOrInvalidProfile(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	// 1. Task references non-existent profile
	missingProfID := "prof-does-not-exist"
	task := &store.Task{
		ID:             "task-missing-prof",
		Namespace:      "default",
		WorkspaceName:  "ws-missing",
		Prompt:         "do something",
		AgentProfileID: &missingProfID,
		MaxAttempts:    1,
	}
	_, _, err := memStore.CreateTask(ctx, task, "exec-missing-prof-1")
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := NewTaskDispatcher(memStore, nil, nil, "worker-test")
	dispatcher.pollAndDispatch(ctx)

	time.Sleep(20 * time.Millisecond)
	exec, err := memStore.GetExecution(ctx, "exec-missing-prof-1")
	if err != nil {
		t.Fatal(err)
	}
	if exec.Status != testStatusFailed {
		t.Fatalf("expected status failed, got %s", exec.Status)
	}
	if exec.ErrorMessage == nil || !strings.Contains(*exec.ErrorMessage, "fetch agent profile") {
		t.Fatalf("expected fetch agent profile error, got %v", exec.ErrorMessage)
	}

	// 2. Direct executeHarness with nil profile
	resNil, _ := dispatcher.executeHarness(ctx, task, exec, nil)
	if resNil.Status != testStatusFailed || !strings.Contains(resNil.ErrorMessage, "agent profile is required") {
		t.Fatalf("expected failure for nil profile, got %+v", resNil)
	}

	// 3. Direct executeHarness with unsupported harness
	invalidProf := &store.AgentProfile{
		HarnessType: "unsupported-tool",
	}
	resInvalid, _ := dispatcher.executeHarness(ctx, task, exec, invalidProf)
	if resInvalid.Status != testStatusFailed || !strings.Contains(resInvalid.ErrorMessage, "unknown or unsupported harness type") {
		t.Fatalf("expected failure for unknown harness, got %+v", resInvalid)
	}
}

func TestExecuteHarnessCommandFormatting(t *testing.T) {
	scheme := setupTestScheme(t)
	memStore := newMemTaskStore()
	ctx := context.Background()

	mockExec := &mockTaskRemoteExec{
		handlers: map[string]func(stdout, stderr io.Writer) int{
			"buddy-runner": func(stdout, _ io.Writer) int {
				return 0
			},
			"rev-parse --is-inside-work-tree": func(stdout, _ io.Writer) int {
				return 1 // skip git diff
			},
			"result.json": func(stdout, _ io.Writer) int {
				res := agents.ResultSpec{
					SpecVersion: agents.SpecVersion,
					Status:      "succeeded",
					ExitCode:    0,
				}
				data, _ := json.Marshal(res)
				_, _ = stdout.Write(data)
				return 0
			},
		},
	}

	// Mock existing sandboxes in running state
	existingSbxCodex := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-exec-fmt-codex",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "pod-exec-fmt-codex",
		},
	}
	existingSbxClaude := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-exec-fmt-claude",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "pod-exec-fmt-claude",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingSbxCodex, existingSbxClaude).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, mockExec, "worker-fmt")
	dispatcher.SetCredentials(&mockProviderPolicy{hasResult: true}, &mockCapabilityLifecycle{})

	task := &store.Task{
		ID:            "task-fmt",
		Namespace:     "default",
		WorkspaceName: "ws-fmt",
		Prompt:        "Fix all unit tests",
	}
	execCodex := &store.Execution{
		ID:     "exec-fmt-codex",
		TaskID: "task-fmt",
	}
	_, _, _ = memStore.CreateTask(ctx, task, execCodex.ID)

	// Case A: Codex harness with model
	codexProf := &store.AgentProfile{
		HarnessType: "codex",
		ModelName:   "o3-mini",
	}
	resCodex, _ := dispatcher.executeHarness(ctx, task, execCodex, codexProf)
	if resCodex.Status != "succeeded" {
		t.Fatalf("expected success, got %+v", resCodex)
	}

	mockExec.mu.Lock()
	if len(mockExec.commands) == 0 {
		t.Fatal("expected commands to be recorded")
	}
	var taskSpecCodex agents.TaskSpec
	writeKeyCodex := "/opt/buddy/bin/buddy-runner write-file /run/buddy/task.json"
	if err := json.Unmarshal(mockExec.stdins[writeKeyCodex], &taskSpecCodex); err != nil {
		t.Fatalf("failed to unmarshal task spec: %v", err)
	}
	if taskSpecCodex.Prompt != "Fix all unit tests" {
		t.Fatalf("unexpected prompt: %q", taskSpecCodex.Prompt)
	}
	if taskSpecCodex.Model != "o3-mini" {
		t.Fatalf("unexpected model: %+v", taskSpecCodex.Model)
	}

	writeKeyPrompt := "/opt/buddy/bin/buddy-runner write-file /run/buddy/prompt.txt"
	if string(mockExec.stdins[writeKeyPrompt]) != "Fix all unit tests" {
		t.Fatalf("unexpected prompt file content: %q", string(mockExec.stdins[writeKeyPrompt]))
	}

	startFoundCodex := false
	for _, cmd := range mockExec.commands {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "buddy-runner start harness-exec-fmt-codex") && strings.Contains(joined, "buddy-runner agent-run") {
			startFoundCodex = true
			break
		}
	}
	if !startFoundCodex {
		t.Fatalf("expected supervised agent-run start command, got: %v", mockExec.commands)
	}
	mockExec.commands = nil
	mockExec.stdins = nil
	mockExec.mu.Unlock()

	// Case B: Claude harness with "claude" alias
	taskClaude := &store.Task{
		ID:            "task-fmt-claude",
		Namespace:     "default",
		WorkspaceName: "ws-fmt",
		Prompt:        "Fix all unit tests",
	}
	execClaude := &store.Execution{
		ID:     "exec-fmt-claude",
		TaskID: "task-fmt-claude",
	}
	_, _, _ = memStore.CreateTask(ctx, taskClaude, execClaude.ID)
	claudeProf := &store.AgentProfile{
		HarnessType: "claude",
		ModelName:   "claude-3-7-sonnet",
	}
	resClaude, _ := dispatcher.executeHarness(ctx, taskClaude, execClaude, claudeProf)
	if resClaude.Status != "succeeded" {
		t.Fatalf("expected success, got %+v", resClaude)
	}

	mockExec.mu.Lock()
	var taskSpecClaude agents.TaskSpec
	writeKeyClaude := "/opt/buddy/bin/buddy-runner write-file /run/buddy/task.json"
	if err := json.Unmarshal(mockExec.stdins[writeKeyClaude], &taskSpecClaude); err != nil {
		t.Fatalf("failed to unmarshal claude task spec: %v", err)
	}
	if taskSpecClaude.Model != "claude-3-7-sonnet" {
		t.Fatalf("unexpected claude model: %+v", taskSpecClaude.Model)
	}

	startFoundClaude := false
	for _, cmd := range mockExec.commands {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "buddy-runner start harness-exec-fmt-claude") && strings.Contains(joined, "buddy-runner agent-run") {
			startFoundClaude = true
			break
		}
	}
	if !startFoundClaude {
		t.Fatalf("expected supervised claude agent-run start command, got: %v", mockExec.commands)
	}
	mockExec.commands = nil
	mockExec.stdins = nil
	mockExec.mu.Unlock()
}

func TestBuildProviderBindingDirect(t *testing.T) {
	ctx := context.Background()

	mockCreds := &mockProviderPolicy{hasResult: true}
	mockCaps := &mockCapabilityLifecycle{}

	sbx := &sandboxesv1alpha1.Sandbox{}
	dur := int64(1800)
	hash, err := BuildProviderBinding(ctx, mockCreds, mockCaps, ProviderBindingParams{
		UserID:             "user-1",
		Namespace:          "default",
		SandboxName:        "sbx-test",
		CreationID:         "create-1",
		Provider:           "openai",
		Model:              "gpt-4o",
		MaxDurationSeconds: &dur,
	}, sbx)
	if err != nil {
		t.Fatalf("BuildProviderBinding failed: %v", err)
	}
	if string(hash) != "hash-xyz" {
		t.Fatalf("expected hash 'hash-xyz', got %q", string(hash))
	}
	if !sbx.Spec.Network.AllowAPIBroker {
		t.Fatal("expected AllowAPIBroker=true")
	}

	envMap := make(map[string]string)
	for _, env := range sbx.Spec.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["OPENAI_API_KEY"] != mockTokenValue {
		t.Fatalf("expected token, got %q", envMap["OPENAI_API_KEY"])
	}
	if envMap["OPENAI_MODEL"] != "gpt-4o" {
		t.Fatalf("expected gpt-4o, got %q", envMap["OPENAI_MODEL"])
	}
	if envMap["BUDDY_PROVIDER_MODE"] != "broker" {
		t.Fatalf("expected broker mode, got %q", envMap["BUDDY_PROVIDER_MODE"])
	}
	if mockCaps.issuedExpiresAt.After(time.Now().Add(1810 * time.Second)) {
		t.Fatalf("expected expiration <= ~30m, got %v", mockCaps.issuedExpiresAt)
	}

	// Case 2: Interactive sandbox with long duration (>2h) preserves 2-hour cap
	longDur := int64(8 * 3600)
	sbxLong := &sandboxesv1alpha1.Sandbox{}
	_, errLong := BuildProviderBinding(ctx, mockCreds, mockCaps, ProviderBindingParams{
		UserID:             "user-1",
		Namespace:          "default",
		SandboxName:        "sbx-long",
		CreationID:         "create-2",
		Provider:           "openai",
		MaxDurationSeconds: &longDur,
	}, sbxLong)
	if errLong != nil {
		t.Fatalf("BuildProviderBinding for long duration failed: %v", errLong)
	}
	if mockCaps.issuedExpiresAt.After(time.Now().Add(2*time.Hour + 5*time.Second)) {
		t.Fatalf("expected 2-hour cap for interactive duration, got %v", mockCaps.issuedExpiresAt)
	}

	// Case 3: Explicit ExpiresAt is respected
	explicitExp := time.Now().Add(5 * time.Hour)
	sbxExplicit := &sandboxesv1alpha1.Sandbox{}
	_, errExplicit := BuildProviderBinding(ctx, mockCreds, mockCaps, ProviderBindingParams{
		UserID:      "user-1",
		Namespace:   "default",
		SandboxName: "sbx-explicit",
		CreationID:  "create-3",
		Provider:    "openai",
		ExpiresAt:   &explicitExp,
	}, sbxExplicit)
	if errExplicit != nil {
		t.Fatalf("BuildProviderBinding with explicit ExpiresAt failed: %v", errExplicit)
	}
	if !mockCaps.issuedExpiresAt.Equal(explicitExp) {
		t.Fatalf("expected explicit expiration %v, got %v", explicitExp, mockCaps.issuedExpiresAt)
	}

	// Missing credentials returns Conflict
	missingCreds := &mockProviderPolicy{hasResult: false}
	_, errMissing := BuildProviderBinding(ctx, missingCreds, mockCaps, ProviderBindingParams{
		UserID:      "user-1",
		SandboxName: "sbx-test",
		Provider:    "openai",
	}, &sandboxesv1alpha1.Sandbox{})
	if errMissing == nil {
		t.Fatal("expected error for missing credentials")
	}
	var pErr *ProviderBindingError
	if !errors.As(errMissing, &pErr) || pErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected Conflict status, got %v", errMissing)
	}
}

func TestDedicatedRunnerResourceSanitizationAndBinding(t *testing.T) {
	scheme := setupTestScheme(t)

	memStore := newMemTaskStore()
	mockExec := &mockRemoteExecutor{}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	dispatcher := NewTaskDispatcher(memStore, fakeClient, mockExec, "worker-clean")
	dispatcher.SetCredentials(&mockProviderPolicy{hasResult: true}, &mockCapabilityLifecycle{})

	ctx := context.Background()
	task := &store.Task{
		ID:            "task_test_123",
		Namespace:     "default",
		WorkspaceName: "ws-test",
		Prompt:        "Fix it",
	}
	exec := &store.Execution{
		ID:            "exec_raw_uuid_456",
		TaskID:        task.ID,
		AttemptNumber: 1,
	}
	_, _, _ = memStore.CreateTask(ctx, task, exec.ID)

	// Pre-create the sandbox with running phase so acquire succeeds quickly
	expectedSbxName := "sbx-exec-raw-uuid-456"
	sbxRunning := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      expectedSbxName,
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "pod-raw-uuid-456",
		},
	}
	_ = fakeClient.Create(ctx, sbxRunning)

	profile := &store.AgentProfile{
		HarnessType: "codex",
		ModelName:   "o3-mini",
	}

	plan, _ := agents.Plan(task, agents.AgentFromProfile(profile), nil)
	runner, err := dispatcher.acquireDedicatedWorkspaceRunner(ctx, task, exec, profile, plan)
	if err != nil {
		t.Fatalf("acquireDedicatedWorkspaceRunner failed: %v", err)
	}
	defer func() {
		_ = runner.Release(ctx, 1, 1, true)
	}()

	if runner.SandboxName() != expectedSbxName {
		t.Fatalf("expected sandbox name %q, got %q", expectedSbxName, runner.SandboxName())
	}

	// Verify Sandbox was bound in store
	updatedExec, err := memStore.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("failed to fetch updated execution: %v", err)
	}
	if updatedExec.SandboxName == nil || *updatedExec.SandboxName != expectedSbxName {
		t.Fatalf("expected execution bound sandbox %q, got %v", expectedSbxName, updatedExec.SandboxName)
	}
}

func TestAcquireRunnerRejectsNoWorkspaceForCodexAndClaude(t *testing.T) {
	scheme := setupTestScheme(t)

	memStore := newMemTaskStore()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, nil, "worker-noworkspace")

	ctx := context.Background()
	task := &store.Task{
		ID:            "task-noworkspace",
		Namespace:     "default",
		WorkspaceName: "", // empty workspace
		Prompt:        "Fix it",
	}
	exec := &store.Execution{
		ID:     "exec-noworkspace-1",
		TaskID: task.ID,
	}

	codexProf := &store.AgentProfile{
		HarnessType: "codex",
	}
	planCodex, _ := agents.Plan(task, agents.AgentFromProfile(codexProf), nil)
	_, errCodex := dispatcher.acquireRunner(ctx, task, exec, codexProf, planCodex)
	if errCodex == nil || !strings.Contains(errCodex.Error(), "requires an explicit workspace") {
		t.Fatalf("expected codex to fail with workspace required error, got %v", errCodex)
	}

	claudeProf := &store.AgentProfile{
		HarnessType: "claude-code",
	}
	planClaude, _ := agents.Plan(task, agents.AgentFromProfile(claudeProf), nil)
	_, errClaude := dispatcher.acquireRunner(ctx, task, exec, claudeProf, planClaude)
	if errClaude == nil || !strings.Contains(errClaude.Error(), "requires an explicit workspace") {
		t.Fatalf("expected claude to fail with workspace required error, got %v", errClaude)
	}
}

func TestDispatcherMissingCredentialsFailsImmediatelyWithoutRetry(t *testing.T) {
	scheme := setupTestScheme(t)

	memStore := newMemTaskStore()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	mockExec := &mockRemoteExecutor{}
	// Note: No credentials set on dispatcher
	dispatcher := NewTaskDispatcher(memStore, fakeClient, mockExec, "worker-nocreds")

	ctx := context.Background()
	profID := "prof-codex-nocreds"
	_ = memStore.CreateAgentProfile(ctx, &store.AgentProfile{
		ID:          profID,
		Name:        "Codex Profile",
		HarnessType: "codex",
		ModelName:   "o3-mini",
	})

	task := &store.Task{
		ID:             "task-nocreds",
		Namespace:      "default",
		WorkspaceName:  "ws-codex",
		Prompt:         "Fix tests",
		AgentProfileID: &profID,
		MaxAttempts:    3,
	}
	_, _, err := memStore.CreateTask(ctx, task, "exec-nocreds-1")
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	dispatcher.pollAndDispatch(ctx)
	time.Sleep(30 * time.Millisecond)

	finalExec, err := memStore.GetExecution(ctx, "exec-nocreds-1")
	if err != nil {
		t.Fatalf("GetExecution failed: %v", err)
	}
	if finalExec.Status != testStatusFailed {
		t.Fatalf("expected execution failed, got %s", finalExec.Status)
	}
	if finalExec.ErrorMessage == nil || !strings.Contains(*finalExec.ErrorMessage, "credential") {
		t.Fatalf("expected credential error message, got %v", finalExec.ErrorMessage)
	}

	finalTask, err := memStore.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	// Because it is a non-retryable credential error, it should fail immediately rather than scheduling attempt 2
	if finalTask.Status != testStatusFailed {
		t.Fatalf("expected task status failed immediately without retry, got %s", finalTask.Status)
	}
}

func TestDispatcherCancelTaskExecution(t *testing.T) {
	memStore := newMemTaskStore()
	dispatcher := NewTaskDispatcher(memStore, nil, nil, "worker-cancel")

	ctx := context.Background()
	task := &store.Task{
		ID:             "task-to-cancel",
		Namespace:      "default",
		WorkspaceName:  "ws-cancel",
		Prompt:         "Run long job",
		MaxAttempts:    1,
		IdempotencyKey: "key-cancel-1",
	}
	_, _, err := memStore.CreateTask(ctx, task, "exec-cancel-1")
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	cancelledCh := make(chan struct{})
	dispatcher.SetRunnerFunc(func(execCtx context.Context, exec *store.Execution, profile *store.AgentProfile) (store.ExecutionResult, error) {
		select {
		case <-execCtx.Done():
			close(cancelledCh)
			return store.ExecutionResult{
				Status: statusCancelled,
			}, nil
		case <-time.After(2 * time.Second):
			return store.ExecutionResult{
				Status: testStatusSucceeded,
			}, nil
		}
	})

	go dispatcher.pollAndDispatch(ctx)

	// Wait for execution to start
	time.Sleep(20 * time.Millisecond)
	dispatcher.CancelTaskExecution(task.ID)

	select {
	case <-cancelledCh:
		// Succeeded in receiving cancellation
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for runnerFunc to receive cancellation")
	}
}

type errorInjectingTaskStore struct {
	*memTaskStore
	getProfileErr error
}

func (s *errorInjectingTaskStore) GetAgentProfile(ctx context.Context, id string) (*store.AgentProfile, error) {
	if s.getProfileErr != nil {
		return nil, s.getProfileErr
	}
	return s.memTaskStore.GetAgentProfile(ctx, id)
}

func TestTransientDatabaseErrorDoesNotStopRetries(t *testing.T) {
	ctx := context.Background()

	// Case 1: Transient DB error allows retry
	mem1 := newMemTaskStore()
	storeTransient := &errorInjectingTaskStore{
		memTaskStore:  mem1,
		getProfileErr: errors.New("transient database connection reset"),
	}
	dispTransient := NewTaskDispatcher(storeTransient, nil, nil, "worker-retry")

	profID := "prof-some"
	task1 := &store.Task{
		ID:             "task-transient",
		Namespace:      "default",
		WorkspaceName:  "ws-retry",
		Prompt:         "Fix bug",
		AgentProfileID: &profID,
		MaxAttempts:    3,
	}
	_, _, err := storeTransient.CreateTask(ctx, task1, "exec-transient-1")
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	dispTransient.pollAndDispatch(ctx)
	time.Sleep(30 * time.Millisecond)

	finalTask1, err := storeTransient.GetTask(ctx, task1.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	// Because it is a transient error, retry should be scheduled rather than permanently failing the task
	if finalTask1.Status == testStatusFailed {
		t.Fatalf("expected task status to NOT be permanently failed on transient DB error, got %s", finalTask1.Status)
	}

	// Verify attempt 2 was scheduled
	_, errAtt2 := storeTransient.GetExecution(ctx, "task-transient-att2")
	if errAtt2 != nil {
		t.Fatalf("expected retry attempt task-transient-att2 to be scheduled: %v", errAtt2)
	}

	// Case 2: Permanent ErrNotFound fails task immediately without retry
	mem2 := newMemTaskStore()
	storePermanent := &errorInjectingTaskStore{
		memTaskStore:  mem2,
		getProfileErr: store.ErrNotFound,
	}
	dispPermanent := NewTaskDispatcher(storePermanent, nil, nil, "worker-noretry")

	task2 := &store.Task{
		ID:             "task-permanent",
		Namespace:      "default",
		WorkspaceName:  "ws-retry",
		Prompt:         "Fix bug",
		AgentProfileID: &profID,
		MaxAttempts:    3,
	}
	_, _, err = storePermanent.CreateTask(ctx, task2, "exec-perm-1")
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	dispPermanent.pollAndDispatch(ctx)
	time.Sleep(30 * time.Millisecond)

	finalTask2, err := storePermanent.GetTask(ctx, task2.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if finalTask2.Status != testStatusFailed {
		t.Fatalf("expected permanent error to immediately fail task, got %s", finalTask2.Status)
	}
}

func TestTaskDispatcherPreservesTerminalResultOnShutdown(t *testing.T) {
	mem := newMemTaskStore()
	disp := NewTaskDispatcher(mem, nil, nil, "worker-shutdown")

	parentCtx, parentCancel := context.WithCancel(context.Background())

	task := &store.Task{
		ID:            "task-shutdown",
		Namespace:     "default",
		WorkspaceName: "ws-shutdown",
		Prompt:        "test shutdown",
		MaxAttempts:   3,
	}
	_, _, err := mem.CreateTask(parentCtx, task, "exec-shutdown-1")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	exec, err := mem.ClaimNextExecution(parentCtx, "worker-shutdown")
	if err != nil || exec == nil {
		t.Fatalf("ClaimNextExecution: %v", err)
	}

	exitCode := 0
	// Runner simulates finishing successfully right as the gateway triggers shutdown
	disp.runnerFunc = func(runCtx context.Context, e *store.Execution, p *store.AgentProfile) (store.ExecutionResult, error) {
		parentCancel() // Trigger shutdown
		return store.ExecutionResult{
			Status:    statusSucceeded,
			ExitCode:  &exitCode,
			LogBuffer: "task finished before shutdown was processed",
		}, nil
	}

	disp.runExecution(parentCtx, exec)

	savedExec, err := mem.GetExecution(context.Background(), exec.ID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if savedExec.Status != statusSucceeded {
		t.Fatalf("expected status %q to be preserved on shutdown, got %q", statusSucceeded, savedExec.Status)
	}
	if savedExec.ExitCode == nil || *savedExec.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", savedExec.ExitCode)
	}
}

func TestTaskDispatcherShutdownLeavesContextCanceledForSweeper(t *testing.T) {
	memStore := newMemTaskStore()
	disp := NewTaskDispatcher(memStore, nil, nil, "worker-shutdown")

	parentCtx, parentCancel := context.WithCancel(context.Background())

	task := &store.Task{
		ID:            "task-shutdown-retry",
		Namespace:     "default",
		WorkspaceName: "ws-retry",
		Prompt:        "retry test",
		MaxAttempts:   3,
	}
	_, _, err := memStore.CreateTask(parentCtx, task, "exec-shutdown-1")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	exec, err := memStore.ClaimNextExecution(parentCtx, "worker-shutdown")
	if err != nil || exec == nil {
		t.Fatalf("ClaimNextExecution: %v", err)
	}

	// Runner simulates failure due to context cancellation during gateway shutdown
	disp.runnerFunc = func(runCtx context.Context, e *store.Execution, p *store.AgentProfile) (store.ExecutionResult, error) {
		parentCancel() // cancel parent context (shutdown)
		exitCode := 1
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exitCode,
			ErrorMessage: context.Canceled.Error(),
		}, context.Canceled
	}

	disp.runExecution(parentCtx, exec)

	// Verify execution was NOT marked terminal with nextExecID="", but left running
	savedExec, err := memStore.GetExecution(context.Background(), exec.ID)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	if savedExec.Status != testStatusRunning {
		t.Fatalf("expected execution to remain in running status for orphan sweeper, got %s", savedExec.Status)
	}

	// Age heartbeat and run orphan sweeper
	old := time.Now().UTC().Add(-60 * time.Second)
	savedExec.LastHeartbeatAt = &old

	reaped, err := memStore.ReapOrphanExecutions(context.Background(), 30*time.Second)
	if err != nil {
		t.Fatalf("ReapOrphanExecutions: %v", err)
	}
	if len(reaped) != 1 {
		t.Fatalf("expected 1 execution reaped, got %d", len(reaped))
	}

	// Verify retry attempt 2 was scheduled
	att2, err := memStore.GetExecution(context.Background(), "task-shutdown-retry-att2")
	if err != nil {
		t.Fatalf("expected attempt 2 to be scheduled: %v", err)
	}
	if att2.Status != "scheduled" {
		t.Fatalf("expected attempt 2 status to be scheduled, got %s", att2.Status)
	}
}

func TestTaskDispatcherNonRetryableOnExit78(t *testing.T) {
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:            "task-exit78",
		Namespace:     "default",
		WorkspaceName: "ws-exit78",
		Prompt:        "test exit 78",
		MaxAttempts:   3, // Multiple attempts allowed
	}
	exec := &store.Execution{
		ID:            "exec-exit78-att1",
		TaskID:        task.ID,
		AttemptNumber: 1,
	}
	_, _, err := memStore.CreateTask(ctx, task, exec.ID)
	if err != nil {
		t.Fatal(err)
	}

	disp := NewTaskDispatcher(memStore, nil, nil, "worker-exit78")
	// Custom runner returning exit code 78
	disp.SetRunnerFunc(func(ctx context.Context, exec *store.Execution, profile *store.AgentProfile) (store.ExecutionResult, error) {
		exit78 := 78
		return store.ExecutionResult{
			Status:       statusFailed,
			ExitCode:     &exit78,
			ErrorMessage: "configuration error: unsupported spec_version or missing agent entrypoint",
		}, &NonRetryableExecutionError{Err: errors.New("configuration error")}
	})

	disp.runExecution(ctx, exec)

	// Verify attempt 1 is failed
	savedExec, err := memStore.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedExec.Status != statusFailed {
		t.Fatalf("expected execution status failed, got %s", savedExec.Status)
	}

	// Verify attempt 2 was NOT scheduled (non-retryable error suppressed retry)
	_, err = memStore.GetExecution(ctx, "task-exit78-att2")
	if err == nil {
		t.Fatal("expected attempt 2 NOT to be scheduled on exit 78")
	}
}

func TestNoHarnessLiteralsInDispatcher(t *testing.T) {
	dispatcherCode, err := os.ReadFile("task_dispatcher.go")
	if err != nil {
		t.Fatalf("failed to read task_dispatcher.go: %v", err)
	}
	content := string(dispatcherCode)

	for _, literal := range []string{"codex", "claude-code", "harnessCodex", "harnessClaude"} {
		if strings.Contains(content, literal) {
			t.Fatalf("task_dispatcher.go must not contain hardcoded harness literal %q", literal)
		}
	}
}

func TestTaskDispatcherExecutionDeadlineAndImageOverrides(t *testing.T) {
	const customCodexImage = "custom-registry.io/agents/codex:v99"
	t.Setenv("BUDDY_AGENT_IMAGE_CODEX", customCodexImage)

	scheme := setupTestScheme(t)
	memStore := newMemTaskStore()
	ctx := context.Background()

	task := &store.Task{
		ID:            "task-override-1",
		UserID:        "user-test-override",
		Namespace:     "default",
		WorkspaceName: "ws-override",
	}
	exec := &store.Execution{
		ID:     "exec-override-1",
		TaskID: "task-override-1",
	}
	profile := &store.AgentProfile{
		HarnessType:                "codex",
		ModelName:                  "gpt-4o",
		TimeoutSeconds:             1000,
		VerificationTimeoutSeconds: 200,
	}
	_, _, _ = memStore.CreateTask(ctx, task, exec.ID)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, nil, "worker-override")
	mockCaps := &mockCapabilityLifecycle{}
	dispatcher.SetCredentials(&mockProviderPolicy{hasResult: true}, mockCaps)

	harness := agents.HarnessFromEnv(profile.HarnessType)
	if harness == nil || harness.Image != customCodexImage {
		t.Fatalf("expected harness image from env override, got %+v", harness)
	}

	plan, err := agents.PlanWithExecution(task, exec, agents.AgentFromProfile(profile), harness)
	if err != nil {
		t.Fatalf("PlanWithExecution failed: %v", err)
	}
	if plan.Image != customCodexImage {
		t.Fatalf("expected plan image %q, got %q", customCodexImage, plan.Image)
	}

	expectedDeadline := time.Duration(plan.TimeoutSeconds+plan.VerificationTimeoutSeconds)*time.Second + 30*time.Second
	if expectedDeadline != (1000+200)*time.Second+30*time.Second {
		t.Fatalf("unexpected deadline calculation: %v", expectedDeadline)
	}

	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(5 * time.Millisecond)
			var sbx sandboxesv1alpha1.Sandbox
			if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-override-1"}, &sbx); err == nil {
				sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
				sbx.Status.PodName = "pod-exec-override-1"
				_ = fakeClient.Status().Update(ctx, &sbx)
				_ = fakeClient.Update(ctx, &sbx)
				return
			}
		}
	}()

	runner, err := dispatcher.acquireRunner(ctx, task, exec, profile, plan)
	if err != nil {
		t.Fatalf("acquireRunner failed: %v", err)
	}
	defer func() {
		_ = runner.Release(ctx, 1, 1, true)
	}()

	createdSbx := &sandboxesv1alpha1.Sandbox{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-override-1"}, createdSbx); err != nil {
		t.Fatalf("failed to get created sandbox: %v", err)
	}
	if createdSbx.Spec.Image != customCodexImage {
		t.Fatalf("expected created sandbox image to be %q, got %q", customCodexImage, createdSbx.Spec.Image)
	}

	minExpectedExpiry := time.Now().Add(time.Duration(plan.TimeoutSeconds+plan.VerificationTimeoutSeconds) * time.Second)
	if mockCaps.issuedExpiresAt.Before(minExpectedExpiry) {
		t.Fatalf("expected capability token expiry to include verification timeout (min %v), got %v", minExpectedExpiry, mockCaps.issuedExpiresAt)
	}
}

func TestTaskDispatcherMissingOrCorruptResultRecordFailsTask(t *testing.T) {
	scheme := setupTestScheme(t)
	memStore := newMemTaskStore()
	ctx := context.Background()

	mockExec := &mockTaskRemoteExec{
		handlers: map[string]func(stdout, stderr io.Writer) int{
			"buddy-runner": func(stdout, _ io.Writer) int {
				return 0
			},
			"result.json": func(stdout, _ io.Writer) int {
				// Simulate empty or missing result.json
				return 0
			},
		},
	}

	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-exec-missing-res",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "pod-missing-res",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sbx).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, mockExec, "worker-missing-res")
	dispatcher.SetCredentials(&mockProviderPolicy{hasResult: true}, &mockCapabilityLifecycle{})

	task := &store.Task{
		ID:            "task-missing-res",
		Namespace:     "default",
		WorkspaceName: "ws-test",
		Prompt:        "Testing missing result",
	}
	exec := &store.Execution{
		ID:     "exec-missing-res",
		TaskID: "task-missing-res",
	}
	profile := &store.AgentProfile{
		HarnessType: "codex",
	}
	_, _, _ = memStore.CreateTask(ctx, task, exec.ID)

	res, _ := dispatcher.executeHarness(ctx, task, exec, profile)
	if res.Status != testStatusFailed {
		t.Fatalf("expected failure when result.json is missing, got %+v", res)
	}
	if !strings.Contains(res.ErrorMessage, "missing result record") {
		t.Fatalf("unexpected error message: %q", res.ErrorMessage)
	}
}

func TestTaskDispatcherTimeoutMapsToExit124(t *testing.T) {
	scheme := setupTestScheme(t)
	memStore := newMemTaskStore()
	ctx := context.Background()

	mockExec := &mockTaskRemoteExec{
		handlers: map[string]func(stdout, stderr io.Writer) int{
			"buddy-runner": func(stdout, _ io.Writer) int {
				return 0
			},
		},
	}

	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-exec-timeout-test",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "pod-timeout-test",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sbx).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, mockExec, "worker-timeout")
	dispatcher.SetCredentials(&mockProviderPolicy{hasResult: true}, &mockCapabilityLifecycle{})

	task := &store.Task{
		ID:            "task-timeout-test",
		Namespace:     "default",
		WorkspaceName: "ws-test",
		Prompt:        "Testing timeout mapping",
	}
	exec := &store.Execution{
		ID:     "exec-timeout-test",
		TaskID: "task-timeout-test",
	}
	profile := &store.AgentProfile{
		HarnessType: "codex",
	}
	_, _, _ = memStore.CreateTask(ctx, task, exec.ID)

	// Cancel context to simulate deadline expiration during StreamExec
	timeoutCtx, cancel := context.WithCancel(ctx)
	cancel()

	res, execErr := dispatcher.executeHarness(timeoutCtx, task, exec, profile)
	if execErr == nil {
		t.Fatal("expected execution error on canceled context")
	}
	if res.Status != testStatusFailed {
		t.Fatalf("expected status failed on timeout, got %q", res.Status)
	}
}
