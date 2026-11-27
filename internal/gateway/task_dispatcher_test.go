package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
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
	handlers map[string]func(stdout, stderr io.Writer) int
}

func (m *mockTaskRemoteExec) StreamExec(ctx context.Context, namespace, podName string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer, resizeChan <-chan apiv1.ResizeMessage) (int, error) {
	m.mu.Lock()
	m.commands = append(m.commands, cmd)
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

func TestResolveAgentImage(t *testing.T) {
	if img := resolveAgentImage(nil); img != defaultAgentImageBase {
		t.Fatalf("expected default base image %q, got %q", defaultAgentImageBase, img)
	}

	codexProf := &store.AgentProfile{HarnessType: "codex"}
	if img := resolveAgentImage(codexProf); img != defaultAgentImageCodex {
		t.Fatalf("expected default codex image %q, got %q", defaultAgentImageCodex, img)
	}

	claudeProf := &store.AgentProfile{HarnessType: "claude-code"}
	if img := resolveAgentImage(claudeProf); img != defaultAgentImageClaude {
		t.Fatalf("expected default claude image %q, got %q", defaultAgentImageClaude, img)
	}

	claudeAliasProf := &store.AgentProfile{HarnessType: "claude"}
	if img := resolveAgentImage(claudeAliasProf); img != defaultAgentImageClaude {
		t.Fatalf("expected claude alias image %q, got %q", defaultAgentImageClaude, img)
	}

	customProf := &store.AgentProfile{HarnessType: "custom"}
	if img := resolveAgentImage(customProf); img != defaultAgentImageBase {
		t.Fatalf("expected fallback base image %q, got %q", defaultAgentImageBase, img)
	}

	// Test environment overrides
	t.Setenv("BUDDY_AGENT_IMAGE_CODEX", "ghcr.io/hoangndst/buddy-sandboxes-codex:latest")
	if img := resolveAgentImage(codexProf); img != "ghcr.io/hoangndst/buddy-sandboxes-codex:latest" {
		t.Fatalf("expected overridden codex image, got %q", img)
	}

	t.Setenv("BUDDY_AGENT_IMAGE_CLAUDE", "ghcr.io/hoangndst/buddy-sandboxes-claude:latest")
	if img := resolveAgentImage(claudeProf); img != "ghcr.io/hoangndst/buddy-sandboxes-claude:latest" {
		t.Fatalf("expected overridden claude image, got %q", img)
	}

	t.Setenv("BUDDY_AGENT_IMAGE_BASE", "custom-base:latest")
	if img := resolveAgentImage(nil); img != "custom-base:latest" {
		t.Fatalf("expected overridden base image, got %q", img)
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
	runner, err := dispatcher.acquireRunner(ctx, task, exec, profile)
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

	emptyClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher2 := NewTaskDispatcher(memStore, emptyClient, nil, "worker-2")

	// Call acquireRunner with a timeout since emptyClient won't update Status.Phase to Running
	ctxTimeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, _ = dispatcher2.acquireRunner(ctxTimeout, task2, exec2, profile2)

	// Verify the sandbox was created with the resolved claude image
	createdSbx := &sandboxesv1alpha1.Sandbox{}
	if err := emptyClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sbx-exec-img-2"}, createdSbx); err != nil {
		t.Fatalf("failed to get created sandbox: %v", err)
	}
	if createdSbx.Spec.Image != defaultAgentImageClaude {
		t.Fatalf("expected image %q, got %q", defaultAgentImageClaude, createdSbx.Spec.Image)
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
	issuedToken   string
	issuedHash    []byte
	activatedUID  string
	revokedHashes [][]byte
}

const mockTokenValue = "mock-token-xyz"

func (m *mockCapabilityLifecycle) IssuePending(ctx context.Context, userID, namespace, sandbox, creationID, service string, expiresAt time.Time, routes []string) (string, []byte, error) {
	m.issuedToken = mockTokenValue
	m.issuedHash = []byte("hash-xyz")
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

	mockCreds := &mockProviderPolicy{hasResult: true}
	mockCaps := &mockCapabilityLifecycle{}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher := NewTaskDispatcher(memStore, fakeClient, nil, "worker-cred")
	dispatcher.SetCredentials(mockCreds, mockCaps)

	// Since fakeClient doesn't update Status to Running automatically, run acquireRunner with timeout
	ctxTimeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, _ = dispatcher.acquireRunner(ctxTimeout, task, exec, profile)

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
	resNil := dispatcher.executeHarness(ctx, task, exec, nil)
	if resNil.Status != testStatusFailed || !strings.Contains(resNil.ErrorMessage, "agent profile is required") {
		t.Fatalf("expected failure for nil profile, got %+v", resNil)
	}

	// 3. Direct executeHarness with unsupported harness
	invalidProf := &store.AgentProfile{
		HarnessType: "unsupported-tool",
	}
	resInvalid := dispatcher.executeHarness(ctx, task, exec, invalidProf)
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

	// Case A: Codex harness with model
	codexProf := &store.AgentProfile{
		HarnessType: "codex",
		ModelName:   "o3-mini",
	}
	resCodex := dispatcher.executeHarness(ctx, task, execCodex, codexProf)
	if resCodex.Status != "succeeded" {
		t.Fatalf("expected success, got %+v", resCodex)
	}

	mockExec.mu.Lock()
	if len(mockExec.commands) == 0 {
		t.Fatal("expected commands to be recorded")
	}
	cmd1 := mockExec.commands[0]
	mockExec.commands = nil
	mockExec.mu.Unlock()

	joined1 := strings.Join(cmd1, " ")
	if !strings.Contains(joined1, "codex --no-daemon --dangerously-bypass-approvals-and-sandbox --model o3-mini exec --skip-git-repo-check Fix all unit tests") {
		t.Fatalf("unexpected codex command: %q", joined1)
	}

	// Case B: Claude harness with "claude" alias
	execClaude := &store.Execution{
		ID:     "exec-fmt-claude",
		TaskID: "task-fmt",
	}
	claudeProf := &store.AgentProfile{
		HarnessType: "claude",
		ModelName:   "claude-3-7-sonnet",
	}
	resClaude := dispatcher.executeHarness(ctx, task, execClaude, claudeProf)
	if resClaude.Status != "succeeded" {
		t.Fatalf("expected success, got %+v", resClaude)
	}

	mockExec.mu.Lock()
	cmd2 := mockExec.commands[0]
	mockExec.commands = nil
	mockExec.mu.Unlock()

	joined2 := strings.Join(cmd2, " ")
	if !strings.Contains(joined2, "claude --dangerously-skip-permissions --model claude-3-7-sonnet -p Fix all unit tests") {
		t.Fatalf("unexpected claude command: %q", joined2)
	}
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
