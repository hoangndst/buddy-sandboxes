package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"github.com/hoangndst/buddy-sandboxes/internal/store"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// TaskStore defines the persistence interface required by TaskHandlers and TaskDispatcher.
type TaskStore interface {
	CreateTask(ctx context.Context, task *store.Task, initialExecutionID string) (*store.Task, bool, error)
	GetTask(ctx context.Context, id string) (*store.Task, error)
	ListTasks(ctx context.Context, filter store.TaskFilter) ([]store.Task, error)
	CancelTask(ctx context.Context, id string) error
	GetExecution(ctx context.Context, id string) (*store.Execution, error)
	ListExecutionsByTask(ctx context.Context, taskID string) ([]store.Execution, error)
	CreateAgentProfile(ctx context.Context, profile *store.AgentProfile) error
	GetAgentProfile(ctx context.Context, id string) (*store.AgentProfile, error)
	ListAgentProfiles(ctx context.Context) ([]store.AgentProfile, error)
	ClaimNextExecution(ctx context.Context, workerID string) (*store.Execution, error)
	UpdateExecutionHeartbeat(ctx context.Context, executionID string) error
	BindExecutionRunner(ctx context.Context, executionID string, leaseName, sandboxName string) error
	TransitionExecutionToVerifying(ctx context.Context, executionID string) error
	CompleteExecution(ctx context.Context, executionID string, res store.ExecutionResult, nextExecutionID string) (bool, error)
	ReapOrphanExecutions(ctx context.Context, orphanTimeout time.Duration) ([]store.Execution, error)
}

const defaultTenantNamespace = "default"

// TaskHandlers handles HTTP endpoints for tasks, executions, and agent profiles.
type TaskHandlers struct {
	server     *Server
	store      TaskStore
	dispatcher *TaskDispatcher
	router     chi.Router
}

// NewTaskHandlers creates a new TaskHandlers instance.
func NewTaskHandlers(server *Server, store TaskStore, dispatcher *TaskDispatcher) *TaskHandlers {
	h := &TaskHandlers{
		server:     server,
		store:      store,
		dispatcher: dispatcher,
	}
	h.router = h.initRouter()
	return h
}

func generateID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b))
}

// TaskRouter returns a Chi router for /tasks operations.
func (h *TaskHandlers) TaskRouter() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.CreateTaskHandler)
	r.Get("/", h.ListTasksHandler)
	r.Route("/{id}", func(task chi.Router) {
		task.Get("/", func(w http.ResponseWriter, r *http.Request) {
			h.GetTaskHandler(w, r, chi.URLParam(r, "id"))
		})
		task.Delete("/", func(w http.ResponseWriter, r *http.Request) {
			h.CancelTaskHandler(w, r, chi.URLParam(r, "id"))
		})
		task.Post("/cancel", func(w http.ResponseWriter, r *http.Request) {
			h.CancelTaskHandler(w, r, chi.URLParam(r, "id"))
		})
		task.Get("/executions", func(w http.ResponseWriter, r *http.Request) {
			h.ListTaskExecutionsHandler(w, r, chi.URLParam(r, "id"))
		})
	})
	return r
}

// ExecutionRouter returns a Chi router for /executions operations.
func (h *TaskHandlers) ExecutionRouter() chi.Router {
	r := chi.NewRouter()
	r.Route("/{id}", func(exec chi.Router) {
		exec.Get("/", func(w http.ResponseWriter, r *http.Request) {
			h.GetExecutionHandler(w, r, chi.URLParam(r, "id"))
		})
		exec.Get("/logs", func(w http.ResponseWriter, r *http.Request) {
			h.GetExecutionLogsHandler(w, r, chi.URLParam(r, "id"))
		})
	})
	return r
}

// AgentProfileRouter returns a Chi router for /agent-profiles operations.
func (h *TaskHandlers) AgentProfileRouter() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.CreateAgentProfileHandler)
	r.Get("/", h.ListAgentProfilesHandler)
	r.Get("/{id}", func(w http.ResponseWriter, r *http.Request) {
		h.GetAgentProfileHandler(w, r, chi.URLParam(r, "id"))
	})
	return r
}

func (h *TaskHandlers) initRouter() chi.Router {
	r := chi.NewRouter()
	r.Mount("/api/v1/tasks", h.TaskRouter())
	r.Mount("/api/v1/executions", h.ExecutionRouter())
	r.Mount("/api/v1/agent-profiles", h.AgentProfileRouter())
	r.Mount("/tasks", h.TaskRouter())
	r.Mount("/executions", h.ExecutionRouter())
	r.Mount("/agent-profiles", h.AgentProfileRouter())
	return r
}

// ServeHTTP routes task, execution, and profile requests.
func (h *TaskHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "Task store not configured", http.StatusServiceUnavailable)
		return
	}
	if h.router == nil {
		h.router = h.initRouter()
	}
	h.router.ServeHTTP(w, r)
}

func (h *TaskHandlers) CreateTaskHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	ns, _ := TenantNamespaceFromContext(r.Context())
	if ns == "" {
		ns = defaultTenantNamespace
	}

	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)
		return
	}
	if len(idempotencyKey) > 128 {
		http.Error(w, "Idempotency-Key exceeds 128 characters", http.StatusBadRequest)
		return
	}

	var req apiv1.CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request payload: %v", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.WorkspaceName) == "" {
		http.Error(w, "workspaceName is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.Namespace != "" && req.Namespace != ns {
		if principal.Role != roleAdmin {
			http.Error(w, "Forbidden: cannot target different namespace", http.StatusForbidden)
			return
		}
		ns = req.Namespace
	}

	maxAttempts := 1
	if req.MaxAttempts != nil && *req.MaxAttempts > 0 {
		maxAttempts = *req.MaxAttempts
	}

	taskID := generateID("task")
	execID := generateID("exec")

	task := &store.Task{
		ID:             taskID,
		UserID:         principal.ID,
		Namespace:      ns,
		AgentProfileID: req.AgentProfileID,
		WorkspaceName:  req.WorkspaceName,
		Prompt:         req.Prompt,
		TargetBranch:   req.TargetBranch,
		IdempotencyKey: idempotencyKey,
		MaxAttempts:    maxAttempts,
	}

	created, isNew, err := h.store.CreateTask(r.Context(), task, execID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create task: %v", err), http.StatusInternalServerError)
		return
	}

	resp := taskToResponse(created)
	w.Header().Set("Content-Type", "application/json")
	if isNew {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func authorizeTask(task *store.Task, principal identity.Principal, ns string) bool {
	if principal.Role == roleAdmin {
		return true
	}
	return task.Namespace == ns && task.UserID == principal.ID
}

func (h *TaskHandlers) GetTaskHandler(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := PrincipalFromContext(r.Context())
	ns, _ := TenantNamespaceFromContext(r.Context())

	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Get task: %v", err), http.StatusInternalServerError)
		return
	}

	if !authorizeTask(task, principal, ns) {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(taskToResponse(task))
}

func (h *TaskHandlers) ListTasksHandler(w http.ResponseWriter, r *http.Request) {
	principal, _ := PrincipalFromContext(r.Context())
	ns, _ := TenantNamespaceFromContext(r.Context())

	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	filter := store.TaskFilter{
		UserID:        principal.ID,
		Namespace:     q.Get("namespace"),
		WorkspaceName: q.Get("workspace"),
		Status:        q.Get("status"),
		Limit:         limit,
		Offset:        offset,
	}
	if filter.Namespace == "" && ns != "" {
		filter.Namespace = ns
	}
	if principal.Role == roleAdmin && q.Get("user") != "" {
		filter.UserID = q.Get("user")
	}

	tasks, err := h.store.ListTasks(r.Context(), filter)
	if err != nil {
		http.Error(w, fmt.Sprintf("List tasks: %v", err), http.StatusInternalServerError)
		return
	}

	items := make([]apiv1.TaskResponse, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, taskToResponse(&t))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiv1.ListTasksResponse{Tasks: items})
}

func (h *TaskHandlers) CancelTaskHandler(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := PrincipalFromContext(r.Context())
	ns, _ := TenantNamespaceFromContext(r.Context())

	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Get task: %v", err), http.StatusInternalServerError)
		return
	}

	if !authorizeTask(task, principal, ns) {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	err = h.store.CancelTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Cancel task: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"id":     id,
		"status": string(apiv1.TaskStateCancelled),
	})
}

func (h *TaskHandlers) ListTaskExecutionsHandler(w http.ResponseWriter, r *http.Request, taskID string) {
	principal, _ := PrincipalFromContext(r.Context())
	ns, _ := TenantNamespaceFromContext(r.Context())

	task, err := h.store.GetTask(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Task not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Get task: %v", err), http.StatusInternalServerError)
		return
	}

	if !authorizeTask(task, principal, ns) {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	execs, err := h.store.ListExecutionsByTask(r.Context(), taskID)
	if err != nil {
		http.Error(w, fmt.Sprintf("List executions: %v", err), http.StatusInternalServerError)
		return
	}

	items := make([]apiv1.ExecutionResponse, 0, len(execs))
	for _, e := range execs {
		items = append(items, executionToResponse(&e))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiv1.ListExecutionsResponse{Executions: items})
}

func (h *TaskHandlers) GetExecutionHandler(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := PrincipalFromContext(r.Context())
	ns, _ := TenantNamespaceFromContext(r.Context())

	exec, err := h.store.GetExecution(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Execution not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Get execution: %v", err), http.StatusInternalServerError)
		return
	}

	task, err := h.store.GetTask(r.Context(), exec.TaskID)
	if err != nil {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}

	if !authorizeTask(task, principal, ns) {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(executionToResponse(exec))
}

func (h *TaskHandlers) GetExecutionLogsHandler(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := PrincipalFromContext(r.Context())
	ns, _ := TenantNamespaceFromContext(r.Context())

	exec, err := h.store.GetExecution(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Execution not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Get execution: %v", err), http.StatusInternalServerError)
		return
	}

	task, err := h.store.GetTask(r.Context(), exec.TaskID)
	if err != nil {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}

	if !authorizeTask(task, principal, ns) {
		http.Error(w, "Execution not found", http.StatusNotFound)
		return
	}

	logContent := ""
	if exec.LogBuffer != nil {
		logContent = *exec.LogBuffer
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(logContent))
}

func (h *TaskHandlers) CreateAgentProfileHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Role != roleAdmin {
		http.Error(w, "Forbidden: admin role required to manage agent profiles", http.StatusForbidden)
		return
	}

	var req apiv1.CreateAgentProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid profile payload: %v", err), http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		req.ID = generateID("profile")
	}
	if req.Name == "" || req.HarnessType == "" || req.ModelName == "" {
		http.Error(w, "name, harnessType, and modelName are required", http.StatusBadRequest)
		return
	}

	maxSteps := 30
	if req.MaxSteps != nil && *req.MaxSteps > 0 {
		maxSteps = *req.MaxSteps
	}
	timeout := 3600
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds > 0 {
		timeout = *req.TimeoutSeconds
	}
	verifyTimeout := 300
	if req.VerificationTimeoutSeconds != nil && *req.VerificationTimeoutSeconds > 0 {
		verifyTimeout = *req.VerificationTimeoutSeconds
	}

	prof := &store.AgentProfile{
		ID:                         req.ID,
		Name:                       req.Name,
		Description:                req.Description,
		HarnessType:                req.HarnessType,
		ModelName:                  req.ModelName,
		SystemPrompt:               req.SystemPrompt,
		VerificationCommand:        req.VerificationCommand,
		MaxSteps:                   maxSteps,
		TimeoutSeconds:             timeout,
		VerificationTimeoutSeconds: verifyTimeout,
	}

	if err := h.store.CreateAgentProfile(r.Context(), prof); err != nil {
		http.Error(w, fmt.Sprintf("Create profile: %v", err), http.StatusInternalServerError)
		return
	}

	created, err := h.store.GetAgentProfile(r.Context(), req.ID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Retrieve profile: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(profileToResponse(created))
}

func (h *TaskHandlers) GetAgentProfileHandler(w http.ResponseWriter, r *http.Request, id string) {
	prof, err := h.store.GetAgentProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "Agent profile not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Get profile: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(profileToResponse(prof))
}

func (h *TaskHandlers) ListAgentProfilesHandler(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.store.ListAgentProfiles(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("List profiles: %v", err), http.StatusInternalServerError)
		return
	}

	items := make([]apiv1.AgentProfileResponse, 0, len(profiles))
	for _, p := range profiles {
		items = append(items, profileToResponse(&p))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiv1.ListAgentProfilesResponse{Profiles: items})
}

func taskToResponse(t *store.Task) apiv1.TaskResponse {
	return apiv1.TaskResponse{
		ID:                t.ID,
		UserID:            t.UserID,
		Namespace:         t.Namespace,
		AgentProfileID:    t.AgentProfileID,
		WorkspaceName:     t.WorkspaceName,
		Prompt:            t.Prompt,
		TargetBranch:      t.TargetBranch,
		IdempotencyKey:    t.IdempotencyKey,
		Status:            apiv1.TaskState(t.Status),
		MaxAttempts:       t.MaxAttempts,
		ActiveExecutionID: t.ActiveExecutionID,
		CreatedAt:         t.CreatedAt,
		UpdatedAt:         t.UpdatedAt,
		CompletedAt:       t.CompletedAt,
	}
}

func executionToResponse(e *store.Execution) apiv1.ExecutionResponse {
	return apiv1.ExecutionResponse{
		ID:                 e.ID,
		TaskID:             e.TaskID,
		Namespace:          e.Namespace,
		WorkspaceName:      e.WorkspaceName,
		AttemptNumber:      e.AttemptNumber,
		WorkerID:           e.WorkerID,
		LeaseName:          e.LeaseName,
		SandboxName:        e.SandboxName,
		Status:             apiv1.ExecutionState(e.Status),
		ExitCode:           e.ExitCode,
		VerificationPassed: e.VerificationPassed,
		VerificationOutput: e.VerificationOutput,
		DiffStat:           e.DiffStat,
		Diff:               e.Diff,
		ErrorMessage:       e.ErrorMessage,
		LogBuffer:          e.LogBuffer,
		StartedAt:          e.StartedAt,
		LastHeartbeatAt:    e.LastHeartbeatAt,
		CompletedAt:        e.CompletedAt,
		CreatedAt:          e.CreatedAt,
	}
}

func profileToResponse(p *store.AgentProfile) apiv1.AgentProfileResponse {
	return apiv1.AgentProfileResponse{
		ID:                         p.ID,
		Name:                       p.Name,
		Description:                p.Description,
		HarnessType:                p.HarnessType,
		ModelName:                  p.ModelName,
		SystemPrompt:               p.SystemPrompt,
		VerificationCommand:        p.VerificationCommand,
		MaxSteps:                   p.MaxSteps,
		TimeoutSeconds:             p.TimeoutSeconds,
		VerificationTimeoutSeconds: p.VerificationTimeoutSeconds,
		CreatedAt:                  p.CreatedAt,
		UpdatedAt:                  p.UpdatedAt,
	}
}
