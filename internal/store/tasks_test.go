package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func newTasksTestDatabase(t *testing.T) (*Postgres, string) {
	t.Helper()
	dsn := os.Getenv("BUDDY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUDDY_TEST_POSTGRES_DSN is required for PostgreSQL integration tests")
	}
	ctx := t.Context()
	db, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	userID := fmt.Sprintf("usr_task_test_%d", time.Now().UnixNano())
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users(id,display_name,role) VALUES($1,'Task Test','admin')", userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.WithoutCancel(ctx), "DELETE FROM users WHERE id=$1", userID)
	})

	return db, userID
}

func TestPostgresPreserveTaskCancellationOnFailure(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_cancel_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_cancel_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  fmt.Sprintf("ws_%d", time.Now().UnixNano()),
		Prompt:         "Fix bug",
		IdempotencyKey: fmt.Sprintf("key_%d", time.Now().UnixNano()),
		MaxAttempts:    1,
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-1")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v, exec: %v", err, exec)
	}

	// Cancel the task
	if err := db.CancelTask(ctx, taskID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}

	// Complete execution with failed status
	exitCode := 1
	retried, err := db.CompleteExecution(ctx, exec.ID, ExecutionResult{
		Status:       "failed",
		ExitCode:     &exitCode,
		ErrorMessage: "execution error",
	}, "")
	if err != nil {
		t.Fatalf("complete execution: %v", err)
	}
	if retried {
		t.Fatal("expected retried = false")
	}

	// Assert task is still cancelled
	tGot, err := db.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if tGot.Status != statusCancelled {
		t.Fatalf("expected task status to remain cancelled, got %s", tGot.Status)
	}
}

func TestPostgresRejectLateCompletionOfTerminalExecution(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_late_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_late_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  fmt.Sprintf("ws_%d", time.Now().UnixNano()),
		Prompt:         "Fix bug",
		IdempotencyKey: fmt.Sprintf("key_%d", time.Now().UnixNano()),
		MaxAttempts:    2,
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-stale")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v, exec: %v", err, exec)
	}

	// Reap execution by making heartbeat stale
	_, err = db.Pool.Exec(ctx, "UPDATE executions SET last_heartbeat_at = NOW() - INTERVAL '60 seconds' WHERE id = $1", exec.ID)
	if err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}

	reaped, err := db.ReapOrphanExecutions(ctx, 30*time.Second)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("expected 1 reaped execution, got %v, err: %v", reaped, err)
	}

	// Late completion
	exitZero := 0
	retried, err := db.CompleteExecution(ctx, exec.ID, ExecutionResult{
		Status:   "succeeded",
		ExitCode: &exitZero,
		DiffStat: "1 file changed",
	}, "")
	if err != nil {
		t.Fatalf("complete execution error: %v", err)
	}
	if retried {
		t.Fatal("expected retried = false")
	}

	eGot, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if eGot.Status != statusFailed {
		t.Fatalf("expected execution status failed after reap, got %s", eGot.Status)
	}

	tGot, err := db.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if tGot.Status == statusSucceeded {
		t.Fatal("late completion overwrote task status to succeeded")
	}
}

func TestTruncateOutputPreservesUTF8Boundaries(t *testing.T) {
	// maxOutputCap is 64 * 1024 = 65536.
	// Create string with "head" followed by 3-byte rune '€' and then 65535 ASCII bytes.
	suffix := strings.Repeat("a", maxOutputCap-1)
	input := "head" + "€" + suffix

	truncated := truncateOutput(input)
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncateOutput produced invalid UTF-8 string: %q", truncated)
	}

	if !strings.HasPrefix(truncated, "[TRUNCATED]\n") {
		t.Fatalf("expected truncated string to have prefix [TRUNCATED]\\n, got %q", truncated[:min(len(truncated), 30)])
	}
	if len([]byte(truncated)) > maxOutputCap {
		t.Fatalf("truncated string length %d exceeds maxOutputCap", len([]byte(truncated)))
	}
	if !strings.HasSuffix(truncated, "a") {
		t.Fatalf("expected truncated string to preserve tail")
	}
}

func TestTruncateHeadDiffPreservesHeaders(t *testing.T) {
	header := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n"
	hugeBody := strings.Repeat("+ println(\"hello world\")\n", 4000) // ~96 KB
	input := header + hugeBody

	cut := truncateHead(input)
	if !strings.HasPrefix(cut, "diff --git a/main.go") {
		t.Fatalf("expected diff to preserve file headers at head, got %q", cut[:min(len(cut), 40)])
	}
	if !strings.HasSuffix(cut, "\n[TRUNCATED]") {
		t.Fatalf("expected diff truncation marker \\n[TRUNCATED] at end, got %q", cut[max(0, len(cut)-20):])
	}
	if len([]byte(cut)) > maxOutputCap {
		t.Fatalf("expected truncated diff byte length %d <= maxOutputCap %d", len([]byte(cut)), maxOutputCap)
	}
	if !utf8.ValidString(cut) {
		t.Fatal("expected truncated diff to be valid UTF-8")
	}
}

func TestCompleteExecutionPersistsDiff(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_diff_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_diff_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  "ws-diff",
		Prompt:         "prompt",
		IdempotencyKey: fmt.Sprintf("idemp_diff_%d", time.Now().UnixNano()),
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-1")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v", err)
	}

	exitZero := 0
	expectedDiff := "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
	_, err = db.CompleteExecution(ctx, exec.ID, ExecutionResult{
		Status:   "succeeded",
		ExitCode: &exitZero,
		DiffStat: "1 file changed, 1 insertion(+), 1 deletion(-)",
		Diff:     expectedDiff,
	}, "")
	if err != nil {
		t.Fatalf("complete execution: %v", err)
	}

	eGot, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if eGot.Diff == nil || *eGot.Diff != expectedDiff {
		t.Fatalf("expected diff %q, got %v", expectedDiff, eGot.Diff)
	}
}

func TestAppendExecutionLogIncremental(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_log_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_log_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  "ws-log",
		Prompt:         "prompt",
		IdempotencyKey: fmt.Sprintf("idemp_log_%d", time.Now().UnixNano()),
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-1")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v", err)
	}

	// Append first chunk
	if err := db.AppendExecutionLog(ctx, exec.ID, "chunk 1\n"); err != nil {
		t.Fatalf("append chunk 1: %v", err)
	}

	e1, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if e1.LogBuffer == nil || *e1.LogBuffer != "chunk 1\n" {
		t.Fatalf("expected log 'chunk 1\\n', got %v", e1.LogBuffer)
	}

	// Append second chunk
	if err := db.AppendExecutionLog(ctx, exec.ID, "chunk 2\n"); err != nil {
		t.Fatalf("append chunk 2: %v", err)
	}

	e2, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if e2.LogBuffer == nil || *e2.LogBuffer != "chunk 1\nchunk 2\n" {
		t.Fatalf("expected concatenated log, got %v", e2.LogBuffer)
	}
}

func TestAppendExecutionLogTailTruncation(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_tail_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_tail_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  "ws-tail",
		Prompt:         "prompt",
		IdempotencyKey: fmt.Sprintf("idemp_tail_%d", time.Now().UnixNano()),
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-tail")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v", err)
	}

	// Append large chunk > 65536 bytes
	largeData := strings.Repeat("A", 40000)
	if err := db.AppendExecutionLog(ctx, exec.ID, largeData); err != nil {
		t.Fatalf("append largeData 1: %v", err)
	}

	largeData2 := strings.Repeat("B", 40000)
	if err := db.AppendExecutionLog(ctx, exec.ID, largeData2); err != nil {
		t.Fatalf("append largeData 2: %v", err)
	}

	e, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if e.LogBuffer == nil {
		t.Fatal("expected non-nil log buffer")
	}
	log := *e.LogBuffer
	if !strings.HasPrefix(log, "[TRUNCATED]\n") {
		t.Fatalf("expected log to start with [TRUNCATED]\\n, got %q", log[:min(len(log), 50)])
	}
	// The tail must end with all B's
	if !strings.HasSuffix(log, strings.Repeat("B", 40000)) {
		t.Fatal("expected log tail to contain the second chunk")
	}
}

func TestBindExecutionRunner(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_bind_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_bind_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  "ws-bind",
		Prompt:         "prompt",
		IdempotencyKey: fmt.Sprintf("idemp_bind_%d", time.Now().UnixNano()),
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Non-existent execution returns ErrNotFound
	if err := db.BindExecutionRunner(ctx, "non-existent-exec", "", "sbx-test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for non-existent execution, got %v", err)
	}

	// Valid execution binds successfully
	if err := db.BindExecutionRunner(ctx, execID, "", "sbx-test"); err != nil {
		t.Fatalf("expected success binding execution, got %v", err)
	}

	e, err := db.GetExecution(ctx, execID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if e.SandboxName == nil || *e.SandboxName != "sbx-test" {
		t.Fatalf("expected sandboxName 'sbx-test', got %v", e.SandboxName)
	}
}

func TestCompleteExecutionNonASCIITruncation(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_nonascii_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_nonascii_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  "ws-nonascii",
		Prompt:         "prompt",
		IdempotencyKey: fmt.Sprintf("idemp_nonascii_%d", time.Now().UnixNano()),
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-nonascii")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v", err)
	}

	// Payload > 64 KiB with 3-byte and 4-byte runes
	nonASCIIPayload := strings.Repeat("你好世界🚀\n", 8000) // ~136 KB
	exitCode := 0
	passed := true
	res := ExecutionResult{
		Status:             "succeeded",
		ExitCode:           &exitCode,
		VerificationPassed: &passed,
		VerificationOutput: nonASCIIPayload,
		LogBuffer:          nonASCIIPayload,
	}

	retried, err := db.CompleteExecution(ctx, exec.ID, res, "")
	if err != nil {
		t.Fatalf("complete execution: %v", err)
	}
	if retried {
		t.Fatal("expected retried to be false on success")
	}

	e, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if e.LogBuffer == nil || e.VerificationOutput == nil {
		t.Fatal("expected non-nil log buffer and verification output")
	}

	for name, val := range map[string]string{
		"logBuffer":          *e.LogBuffer,
		"verificationOutput": *e.VerificationOutput,
	} {
		if len([]byte(val)) > maxOutputCap {
			t.Fatalf("%s byte length %d exceeded maxOutputCap %d", name, len([]byte(val)), maxOutputCap)
		}
		if !strings.HasPrefix(val, "[TRUNCATED]\n") {
			t.Fatalf("%s missing [TRUNCATED]\\n prefix", name)
		}
		if !utf8.ValidString(val) {
			t.Fatalf("%s contains invalid UTF-8 sequences", name)
		}
		if !strings.HasSuffix(val, "你好世界🚀\n") {
			t.Fatalf("%s tail does not match end of original output", name)
		}
	}
}

func TestAppendExecutionLogConcurrent(t *testing.T) {
	db, userID := newTasksTestDatabase(t)
	ctx := t.Context()

	taskID := fmt.Sprintf("task_concurrent_%d", time.Now().UnixNano())
	execID := fmt.Sprintf("exec_concurrent_%d", time.Now().UnixNano())
	task := &Task{
		ID:             taskID,
		UserID:         userID,
		Namespace:      "default",
		WorkspaceName:  "ws-concurrent",
		Prompt:         "prompt",
		IdempotencyKey: fmt.Sprintf("idemp_concurrent_%d", time.Now().UnixNano()),
	}

	_, _, err := db.CreateTask(ctx, task, execID)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	exec, err := db.ClaimNextExecution(ctx, "worker-concurrent")
	if err != nil || exec == nil {
		t.Fatalf("claim execution: %v", err)
	}

	// 50 concurrent writers pushing chunks through AppendExecutionLog
	const numWriters = 50
	errCh := make(chan error, numWriters)
	var wg sync.WaitGroup

	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			chunk := fmt.Sprintf("writer-%02d\n", writerID)
			if err := db.AppendExecutionLog(ctx, exec.ID, chunk); err != nil {
				errCh <- fmt.Errorf("writer %d failed: %w", writerID, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	e, err := db.GetExecution(ctx, exec.ID)
	if err != nil {
		t.Fatalf("get execution: %v", err)
	}
	if e.LogBuffer == nil {
		t.Fatal("expected non-nil log buffer")
	}
	log := *e.LogBuffer

	// Assert that ALL 50 chunks are present (no dropped chunks from concurrent updates)
	for i := 0; i < numWriters; i++ {
		expectedChunk := fmt.Sprintf("writer-%02d\n", i)
		if !strings.Contains(log, expectedChunk) {
			t.Fatalf("missing expected chunk %q from log buffer", expectedChunk)
		}
	}
}
