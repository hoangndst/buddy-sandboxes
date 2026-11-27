package sessions

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/store"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestSessionDefaults(t *testing.T) {
	s := &Session{
		ID:          "sess_test1",
		UserID:      "usr_123",
		Namespace:   "sbx-default",
		SandboxName: "test-sbx",
		Command:     []string{"echo", "hi"},
	}
	if s.WorkingDir != "" {
		t.Fatalf("expected empty WorkingDir initially")
	}
}

func TestSessionsIntegrationPostgres(t *testing.T) {
	dsn := os.Getenv("BUDDY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUDDY_TEST_POSTGRES_DSN is required for PostgreSQL integration tests")
	}
	ctx := t.Context()
	db, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	userID := "usr_session_test_" + time.Now().Format("150405")
	userNs := "ns_" + userID
	sbxName := "sbx_" + userID
	_, _ = db.Pool.Exec(ctx, "INSERT INTO namespace_registry(name) VALUES($1) ON CONFLICT DO NOTHING", userNs)
	if _, err := db.Pool.Exec(ctx, "INSERT INTO users(id,display_name,role,namespace) VALUES($1,'Session Test','user',$2)", userID, userNs); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.WithoutCancel(ctx), "DELETE FROM sessions WHERE user_id=$1", userID)
		_, _ = db.Pool.Exec(context.WithoutCancel(ctx), "DELETE FROM users WHERE id=$1", userID)
	})

	sessStore := NewStore(db.Pool)

	sessID := "sess_" + time.Now().Format("150405999")
	sess := &Session{
		ID:          sessID,
		UserID:      userID,
		Namespace:   userNs,
		SandboxName: sbxName,
		Command:     []string{"sh", "-c", "echo hello"},
		WorkingDir:  "/workspace",
		TTY:         true,
		State:       apiv1.SessionStatePending,
		Metadata:    map[string]string{"env": "test"},
	}

	if err := sessStore.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	retrieved, err := sessStore.GetSession(ctx, sessID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if retrieved.ID != sessID || retrieved.State != apiv1.SessionStatePending {
		t.Fatalf("unexpected retrieved session: %+v", retrieved)
	}
	if len(retrieved.Command) != 3 || retrieved.Command[2] != "echo hello" {
		t.Fatalf("unexpected retrieved command: %v", retrieved.Command)
	}
	if retrieved.Metadata["env"] != "test" {
		t.Fatalf("unexpected metadata: %v", retrieved.Metadata)
	}

	pid := 4242
	if err := sessStore.UpdateSessionState(ctx, sessID, apiv1.SessionStateRunning, nil, &pid); err != nil {
		t.Fatalf("UpdateSessionState to running failed: %v", err)
	}

	retrieved, err = sessStore.GetSession(ctx, sessID)
	if err != nil || retrieved.State != apiv1.SessionStateRunning || retrieved.PID == nil || *retrieved.PID != 4242 {
		t.Fatalf("unexpected state after running: %+v, err=%v", retrieved, err)
	}

	exitCode := 0
	if err := sessStore.UpdateSessionState(ctx, sessID, apiv1.SessionStateCompleted, &exitCode, nil); err != nil {
		t.Fatalf("UpdateSessionState to completed failed: %v", err)
	}

	retrieved, err = sessStore.GetSession(ctx, sessID)
	if err != nil || retrieved.State != apiv1.SessionStateCompleted || retrieved.ExitCode == nil || *retrieved.ExitCode != 0 {
		t.Fatalf("unexpected state after completed: %+v, err=%v", retrieved, err)
	}

	list, err := sessStore.ListSessions(ctx, userNs, sbxName)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != sessID {
		t.Fatalf("unexpected list result: %+v", list)
	}

	// Test terminate active sessions
	sessID2 := sessID + "_2"
	sess2 := &Session{
		ID:          sessID2,
		UserID:      userID,
		Namespace:   userNs,
		SandboxName: sbxName,
		Command:     []string{"sleep", "10"},
		State:       apiv1.SessionStateRunning,
	}
	if err := sessStore.CreateSession(ctx, sess2); err != nil {
		t.Fatalf("CreateSession 2 failed: %v", err)
	}
	if err := sessStore.TerminateActiveSessionsForSandbox(ctx, userNs, sbxName); err != nil {
		t.Fatalf("TerminateActiveSessionsForSandbox failed: %v", err)
	}
	retrieved2, _ := sessStore.GetSession(ctx, sessID2)
	if retrieved2.State != apiv1.SessionStateTerminated {
		t.Fatalf("expected terminated state, got %s", retrieved2.State)
	}
}
