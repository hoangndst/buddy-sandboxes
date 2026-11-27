package store

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	// Create string with 65535 ASCII bytes followed by 3-byte rune '€' (0xE2, 0x82, 0xAC).
	prefix := strings.Repeat("a", maxOutputCap-1)
	input := prefix + "€" + "tail"

	truncated := truncateOutput(input)
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncateOutput produced invalid UTF-8 string: %q", truncated)
	}

	if !strings.HasSuffix(truncated, "\n[TRUNCATED]") {
		t.Fatalf("expected truncated string to have suffix [TRUNCATED]")
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
