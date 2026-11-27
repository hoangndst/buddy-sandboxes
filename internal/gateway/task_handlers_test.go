package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

const (
	testStatusPending   = "pending"
	testStatusScheduled = "scheduled"
	testStatusRunning   = "running"
	testStatusVerifying = "verifying"
	testStatusSucceeded = "succeeded"
	testStatusFailed    = "failed"
	testStatusCancelled = "cancelled"
)

// memTaskStore provides an in-memory implementation of TaskStore for testing.
type memTaskStore struct {
	mu         sync.RWMutex
	tasks      map[string]*store.Task
	executions map[string]*store.Execution
	profiles   map[string]*store.AgentProfile
}

func newMemTaskStore() *memTaskStore {
	return &memTaskStore{
		tasks:      make(map[string]*store.Task),
		executions: make(map[string]*store.Execution),
		profiles:   make(map[string]*store.AgentProfile),
	}
}

func (m *memTaskStore) CreateTask(ctx context.Context, task *store.Task, initialExecutionID string) (*store.Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, t := range m.tasks {
		if t.UserID == task.UserID && t.IdempotencyKey == task.IdempotencyKey {
			return t, false, nil
		}
	}
	now := time.Now().UTC()
	task.CreatedAt = now
	task.UpdatedAt = now
	task.Status = "pending"
	task.ActiveExecutionID = &initialExecutionID
	m.tasks[task.ID] = task

	exec := &store.Execution{
		ID:            initialExecutionID,
		TaskID:        task.ID,
		Namespace:     task.Namespace,
		WorkspaceName: task.WorkspaceName,
		AttemptNumber: 1,
		Status:        "scheduled",
		CreatedAt:     now,
	}
	m.executions[initialExecutionID] = exec
	return task, true, nil
}

func (m *memTaskStore) GetTask(ctx context.Context, id string) (*store.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, ok := m.tasks[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return t, nil
}

func (m *memTaskStore) ListTasks(ctx context.Context, filter store.TaskFilter) ([]store.Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]store.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if filter.UserID != "" && t.UserID != filter.UserID {
			continue
		}
		if filter.Namespace != "" && t.Namespace != filter.Namespace {
			continue
		}
		if filter.WorkspaceName != "" && t.WorkspaceName != filter.WorkspaceName {
			continue
		}
		if filter.Status != "" && t.Status != filter.Status {
			continue
		}
		list = append(list, *t)
	}
	if filter.Offset > 0 {
		if filter.Offset >= len(list) {
			return []store.Task{}, nil
		}
		list = list[filter.Offset:]
	}
	if filter.Limit > 0 && filter.Limit < len(list) {
		list = list[:filter.Limit]
	}
	return list, nil
}

func (m *memTaskStore) CancelTask(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	t.Status = testStatusCancelled
	now := time.Now().UTC()
	t.CompletedAt = &now
	for _, e := range m.executions {
		if e.TaskID == id && (e.Status == testStatusScheduled || e.Status == testStatusRunning || e.Status == testStatusVerifying) {
			e.Status = testStatusCancelled
			e.CompletedAt = &now
		}
	}
	return nil
}

func (m *memTaskStore) GetExecution(ctx context.Context, id string) (*store.Execution, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	e, ok := m.executions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e, nil
}

func (m *memTaskStore) ListExecutionsByTask(ctx context.Context, taskID string) ([]store.Execution, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var list []store.Execution
	for _, e := range m.executions {
		if e.TaskID == taskID {
			list = append(list, *e)
		}
	}
	return list, nil
}

func (m *memTaskStore) CreateAgentProfile(ctx context.Context, profile *store.AgentProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	profile.CreatedAt = now
	profile.UpdatedAt = now
	m.profiles[profile.ID] = profile
	return nil
}

func (m *memTaskStore) GetAgentProfile(ctx context.Context, id string) (*store.AgentProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.profiles[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (m *memTaskStore) ListAgentProfiles(ctx context.Context) ([]store.AgentProfile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]store.AgentProfile, 0, len(m.profiles))
	for _, p := range m.profiles {
		list = append(list, *p)
	}
	return list, nil
}

func (m *memTaskStore) ClaimNextExecution(ctx context.Context, workerID string) (*store.Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var candidates []*store.Execution
	for _, e := range m.executions {
		if e.Status == "scheduled" {
			candidates = append(candidates, e)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})

	for _, e := range candidates {
		// Check if another execution is active on same workspace
		active := false
		for _, other := range m.executions {
			if other.ID != e.ID && other.Namespace == e.Namespace && other.WorkspaceName == e.WorkspaceName &&
				(other.Status == testStatusRunning || other.Status == testStatusVerifying) {
				active = true
				break
			}
		}
		if active {
			continue
		}
		now := time.Now().UTC()
		e.Status = testStatusRunning
		e.WorkerID = &workerID
		e.StartedAt = &now
		e.LastHeartbeatAt = &now
		if t, ok := m.tasks[e.TaskID]; ok && t.Status == "pending" {
			t.Status = testStatusRunning
		}
		return e, nil
	}
	return nil, nil
}

func (m *memTaskStore) UpdateExecutionHeartbeat(ctx context.Context, executionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.executions[executionID]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	e.LastHeartbeatAt = &now
	return nil
}

func (m *memTaskStore) BindExecutionRunner(ctx context.Context, executionID string, leaseName, sandboxName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.executions[executionID]
	if !ok {
		return store.ErrNotFound
	}
	e.LeaseName = &leaseName
	e.SandboxName = &sandboxName
	return nil
}

func (m *memTaskStore) TransitionExecutionToVerifying(ctx context.Context, executionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.executions[executionID]
	if !ok {
		return store.ErrNotFound
	}
	e.Status = "verifying"
	now := time.Now().UTC()
	e.LastHeartbeatAt = &now
	return nil
}

func (m *memTaskStore) CompleteExecution(ctx context.Context, executionID string, res store.ExecutionResult, nextExecutionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.executions[executionID]
	if !ok {
		return false, store.ErrNotFound
	}
	if e.Status == testStatusSucceeded || e.Status == testStatusFailed || e.Status == testStatusCancelled {
		return false, nil
	}
	now := time.Now().UTC()
	e.Status = res.Status
	e.ExitCode = res.ExitCode
	e.VerificationPassed = res.VerificationPassed
	e.VerificationOutput = &res.VerificationOutput
	e.DiffStat = &res.DiffStat
	e.Diff = &res.Diff
	e.ErrorMessage = &res.ErrorMessage
	e.LogBuffer = &res.LogBuffer
	e.CompletedAt = &now

	t := m.tasks[e.TaskID]
	if res.Status == testStatusSucceeded {
		if t.Status != testStatusCancelled {
			t.Status = testStatusSucceeded
			t.CompletedAt = &now
		}
		return false, nil
	} else if res.Status == testStatusFailed {
		if t.Status != testStatusCancelled {
			if e.AttemptNumber < t.MaxAttempts {
				retry := &store.Execution{
					ID:            nextExecutionID,
					TaskID:        t.ID,
					Namespace:     e.Namespace,
					WorkspaceName: e.WorkspaceName,
					AttemptNumber: e.AttemptNumber + 1,
					Status:        testStatusScheduled,
					CreatedAt:     now,
				}
				m.executions[nextExecutionID] = retry
				t.ActiveExecutionID = &nextExecutionID
				return true, nil
			}
			t.Status = testStatusFailed
			t.CompletedAt = &now
		}
		return false, nil
	} else if res.Status == testStatusCancelled {
		t.Status = testStatusCancelled
		t.CompletedAt = &now
	}
	return false, nil
}

func (m *memTaskStore) ReapOrphanExecutions(ctx context.Context, orphanTimeout time.Duration) ([]store.Execution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var reaped []store.Execution
	now := time.Now().UTC()
	for _, e := range m.executions {
		if (e.Status == "running" || e.Status == "verifying") && e.LastHeartbeatAt != nil && now.Sub(*e.LastHeartbeatAt) > orphanTimeout {
			e.Status = "failed"
			errMsg := "worker heartbeat timeout; execution orphaned"
			e.ErrorMessage = &errMsg
			e.CompletedAt = &now
			reaped = append(reaped, *e)
			if t, ok := m.tasks[e.TaskID]; ok {
				t.Status = "failed"
				t.CompletedAt = &now
			}
		}
	}
	return reaped, nil
}

func TestTaskHandlersLifecycle(t *testing.T) {
	memStore := newMemTaskStore()
	handlers := NewTaskHandlers(nil, memStore, nil)

	ctx := context.Background()
	ctx = WithPrincipal(ctx, identity.Principal{ID: "usr_alice", Role: "member"})
	ctx = WithTenantNamespace(ctx, "tenant-dev")

	// 1. Submit task without idempotency key -> 400
	body, _ := json.Marshal(apiv1.CreateTaskRequest{
		WorkspaceName: "repo-auth",
		Prompt:        "Fix race condition",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without Idempotency-Key, got %d", w.Code)
	}

	// 2. Submit task successfully -> 201 Created
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "idemp-001")
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	var created apiv1.TaskResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Status != apiv1.TaskStatePending {
		t.Fatalf("expected status pending, got %s", created.Status)
	}
	if created.ActiveExecutionID == nil || *created.ActiveExecutionID == "" {
		t.Fatal("expected activeExecutionId to be populated")
	}

	// 3. Resubmit same task with same idempotency key -> 200 OK (idempotent replay)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "idemp-001")
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for duplicate idempotency key, got %d", w.Code)
	}
	var replayed apiv1.TaskResponse
	_ = json.NewDecoder(w.Body).Decode(&replayed)
	if replayed.ID != created.ID {
		t.Fatalf("expected replayed task ID %s, got %s", created.ID, replayed.ID)
	}

	// 4. Get task
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+created.ID, nil).WithContext(ctx)
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	// 5. List tasks
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var listResp apiv1.ListTasksResponse
	_ = json.NewDecoder(w.Body).Decode(&listResp)
	if len(listResp.Tasks) != 1 {
		t.Fatalf("expected 1 task in list, got %d", len(listResp.Tasks))
	}

	// 6. Get execution and executions list
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+created.ID+"/executions", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var execList apiv1.ListExecutionsResponse
	_ = json.NewDecoder(w.Body).Decode(&execList)
	if len(execList.Executions) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(execList.Executions))
	}

	// 7. Cancel task
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil).WithContext(ctx)
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	task, _ := memStore.GetTask(ctx, created.ID)
	if task.Status != testStatusCancelled {
		t.Fatalf("expected cancelled status, got %s", task.Status)
	}
}

func TestAgentProfileHandlers(t *testing.T) {
	memStore := newMemTaskStore()
	handlers := NewTaskHandlers(nil, memStore, nil)
	// 1. Non-admin cannot create profile
	memberCtx := WithPrincipal(context.Background(), identity.Principal{ID: "usr_member", Role: "member"})
	body, _ := json.Marshal(apiv1.CreateAgentProfileRequest{
		ID:                  "profile-golang",
		Name:                "Go Test Agent",
		HarnessType:         "codex",
		ModelName:           "claude-3-7-sonnet",
		VerificationCommand: "go test ./...",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-profiles", bytes.NewReader(body)).WithContext(memberCtx)
	w := httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-admin, got %d: %s", w.Code, w.Body.String())
	}

	// 2. Admin can create profile
	adminCtx := WithPrincipal(context.Background(), identity.Principal{ID: "usr_admin", Role: "admin"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agent-profiles", bytes.NewReader(body)).WithContext(adminCtx)
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	// 3. Get profile (any authenticated user)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/agent-profiles/profile-golang", nil).WithContext(memberCtx)
	w = httptest.NewRecorder()
	handlers.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var prof apiv1.AgentProfileResponse
	_ = json.NewDecoder(w.Body).Decode(&prof)
	if prof.VerificationCommand != "go test ./..." {
		t.Fatalf("unexpected verification command: %s", prof.VerificationCommand)
	}
}
