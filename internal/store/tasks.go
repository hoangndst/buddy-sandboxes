package store

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNotFound      = errors.New("resource not found")
	ErrConflict      = errors.New("resource conflict")
	ErrInvalidStatus = errors.New("invalid status transition")
	ErrWorkspaceBusy = errors.New("workspace is currently executing another task")
)

const (
	statusPending   = "pending"
	statusScheduled = "scheduled"
	statusRunning   = "running"
	statusVerifying = "verifying"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusCancelled = "cancelled"
)

type AgentProfile struct {
	ID                         string    `json:"id"`
	Name                       string    `json:"name"`
	Description                string    `json:"description,omitempty"`
	HarnessType                string    `json:"harnessType"`
	ModelName                  string    `json:"modelName"`
	SystemPrompt               string    `json:"systemPrompt,omitempty"`
	VerificationCommand        string    `json:"verificationCommand,omitempty"`
	MaxSteps                   int       `json:"maxSteps"`
	TimeoutSeconds             int       `json:"timeoutSeconds"`
	VerificationTimeoutSeconds int       `json:"verificationTimeoutSeconds"`
	CreatedAt                  time.Time `json:"createdAt"`
	UpdatedAt                  time.Time `json:"updatedAt"`
}

type Task struct {
	ID                string     `json:"id"`
	UserID            string     `json:"userId"`
	Namespace         string     `json:"namespace"`
	AgentProfileID    *string    `json:"agentProfileId,omitempty"`
	WorkspaceName     string     `json:"workspaceName"`
	Prompt            string     `json:"prompt"`
	TargetBranch      *string    `json:"targetBranch,omitempty"`
	IdempotencyKey    string     `json:"idempotencyKey"`
	Status            string     `json:"status"` // pending, running, succeeded, failed, cancelled
	MaxAttempts       int        `json:"maxAttempts"`
	ActiveExecutionID *string    `json:"activeExecutionId,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	CompletedAt       *time.Time `json:"completedAt,omitempty"`
}

type TaskFilter struct {
	UserID        string
	Namespace     string
	WorkspaceName string
	Status        string
	Limit         int
	Offset        int
}

type Execution struct {
	ID                 string     `json:"id"`
	TaskID             string     `json:"taskId"`
	Namespace          string     `json:"namespace"`
	WorkspaceName      string     `json:"workspaceName"`
	AttemptNumber      int        `json:"attemptNumber"`
	WorkerID           *string    `json:"workerId,omitempty"`
	LeaseName          *string    `json:"leaseName,omitempty"`
	SandboxName        *string    `json:"sandboxName,omitempty"`
	Status             string     `json:"status"` // scheduled, running, verifying, succeeded, failed, cancelled
	ExitCode           *int       `json:"exitCode,omitempty"`
	VerificationPassed *bool      `json:"verificationPassed,omitempty"`
	VerificationOutput *string    `json:"verificationOutput,omitempty"`
	DiffStat           *string    `json:"diffStat,omitempty"`
	Diff               *string    `json:"diff,omitempty"`
	ErrorMessage       *string    `json:"errorMessage,omitempty"`
	LogBuffer          *string    `json:"logBuffer,omitempty"`
	StartedAt          *time.Time `json:"startedAt,omitempty"`
	LastHeartbeatAt    *time.Time `json:"lastHeartbeatAt,omitempty"`
	CompletedAt        *time.Time `json:"completedAt,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
}

const maxOutputCap = 64 * 1024 // 64 KB truncation cap

func truncateOutput(s string) string {
	if len(s) > maxOutputCap {
		cut := s[:maxOutputCap]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		return cut + "\n[TRUNCATED]"
	}
	return s
}

func (p *Postgres) CreateAgentProfile(ctx context.Context, profile *AgentProfile) error {
	if profile.ID == "" || profile.Name == "" || profile.HarnessType == "" || profile.ModelName == "" {
		return fmt.Errorf("required profile fields missing")
	}
	if profile.MaxSteps <= 0 {
		profile.MaxSteps = 30
	}
	if profile.TimeoutSeconds <= 0 {
		profile.TimeoutSeconds = 3600
	}
	if profile.VerificationTimeoutSeconds <= 0 {
		profile.VerificationTimeoutSeconds = 300
	}

	query := `
		INSERT INTO agent_profiles (
			id, name, description, harness_type, model_name, system_prompt,
			verification_command, max_steps, timeout_seconds, verification_timeout_seconds,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			harness_type = EXCLUDED.harness_type,
			model_name = EXCLUDED.model_name,
			system_prompt = EXCLUDED.system_prompt,
			verification_command = EXCLUDED.verification_command,
			max_steps = EXCLUDED.max_steps,
			timeout_seconds = EXCLUDED.timeout_seconds,
			verification_timeout_seconds = EXCLUDED.verification_timeout_seconds,
			updated_at = NOW()
	`
	_, err := p.Pool.Exec(ctx, query,
		profile.ID, profile.Name, profile.Description, profile.HarnessType, profile.ModelName,
		profile.SystemPrompt, profile.VerificationCommand, profile.MaxSteps,
		profile.TimeoutSeconds, profile.VerificationTimeoutSeconds,
	)
	if err != nil {
		return fmt.Errorf("insert agent profile: %w", err)
	}
	return nil
}

func (p *Postgres) GetAgentProfile(ctx context.Context, id string) (*AgentProfile, error) {
	query := `
		SELECT id, name, description, harness_type, model_name, system_prompt,
		       verification_command, max_steps, timeout_seconds, verification_timeout_seconds,
		       created_at, updated_at
		FROM agent_profiles
		WHERE id = $1
	`
	var prof AgentProfile
	var desc, sysPrompt, verifyCmd *string
	err := p.Pool.QueryRow(ctx, query, id).Scan(
		&prof.ID, &prof.Name, &desc, &prof.HarnessType, &prof.ModelName, &sysPrompt,
		&verifyCmd, &prof.MaxSteps, &prof.TimeoutSeconds, &prof.VerificationTimeoutSeconds,
		&prof.CreatedAt, &prof.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get agent profile: %w", err)
	}
	if desc != nil {
		prof.Description = *desc
	}
	if sysPrompt != nil {
		prof.SystemPrompt = *sysPrompt
	}
	if verifyCmd != nil {
		prof.VerificationCommand = *verifyCmd
	}
	return &prof, nil
}

func (p *Postgres) ListAgentProfiles(ctx context.Context) ([]AgentProfile, error) {
	query := `
		SELECT id, name, description, harness_type, model_name, system_prompt,
		       verification_command, max_steps, timeout_seconds, verification_timeout_seconds,
		       created_at, updated_at
		FROM agent_profiles
		ORDER BY created_at ASC
	`
	rows, err := p.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list agent profiles: %w", err)
	}
	defer rows.Close()

	var profiles []AgentProfile
	for rows.Next() {
		var prof AgentProfile
		var desc, sysPrompt, verifyCmd *string
		if err := rows.Scan(
			&prof.ID, &prof.Name, &desc, &prof.HarnessType, &prof.ModelName, &sysPrompt,
			&verifyCmd, &prof.MaxSteps, &prof.TimeoutSeconds, &prof.VerificationTimeoutSeconds,
			&prof.CreatedAt, &prof.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent profile: %w", err)
		}
		if desc != nil {
			prof.Description = *desc
		}
		if sysPrompt != nil {
			prof.SystemPrompt = *sysPrompt
		}
		if verifyCmd != nil {
			prof.VerificationCommand = *verifyCmd
		}
		profiles = append(profiles, prof)
	}
	return profiles, nil
}

// CreateTask creates a task and its initial execution attempt atomically.
// If a task with (user_id, idempotency_key) already exists, it returns the existing task with isNew=false.
func (p *Postgres) CreateTask(ctx context.Context, task *Task, initialExecutionID string) (*Task, bool, error) {
	if task.ID == "" || task.UserID == "" || task.WorkspaceName == "" || task.Prompt == "" || task.IdempotencyKey == "" {
		return nil, false, fmt.Errorf("required task fields missing")
	}
	if task.Namespace == "" {
		task.Namespace = "default"
	}
	if task.MaxAttempts <= 0 {
		task.MaxAttempts = 1
	}
	task.Status = "pending"

	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Check if already exists for this idempotency key
	var existing Task
	var activeExecID *string
	err = tx.QueryRow(ctx, `
		SELECT t.id, t.user_id, t.namespace, t.agent_profile_id, t.workspace_name,
		       t.prompt, t.target_branch, t.idempotency_key, t.status, t.max_attempts,
		       t.created_at, t.updated_at, t.completed_at,
		       (SELECT id FROM executions WHERE task_id = t.id ORDER BY attempt_number DESC LIMIT 1)
		FROM tasks t
		WHERE t.user_id = $1 AND t.idempotency_key = $2
	`, task.UserID, task.IdempotencyKey).Scan(
		&existing.ID, &existing.UserID, &existing.Namespace, &existing.AgentProfileID, &existing.WorkspaceName,
		&existing.Prompt, &existing.TargetBranch, &existing.IdempotencyKey, &existing.Status, &existing.MaxAttempts,
		&existing.CreatedAt, &existing.UpdatedAt, &existing.CompletedAt, &activeExecID,
	)
	if err == nil {
		existing.ActiveExecutionID = activeExecID
		_ = tx.Commit(ctx)
		return &existing, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("query existing task: %w", err)
	}

	// Insert task with ON CONFLICT to protect against concurrent key insertion race
	insertTaskQuery := `
		INSERT INTO tasks (
			id, user_id, namespace, agent_profile_id, workspace_name, prompt,
			target_branch, idempotency_key, status, max_attempts, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING created_at, updated_at
	`
	err = tx.QueryRow(ctx, insertTaskQuery,
		task.ID, task.UserID, task.Namespace, task.AgentProfileID, task.WorkspaceName,
		task.Prompt, task.TargetBranch, task.IdempotencyKey, task.Status, task.MaxAttempts,
	).Scan(&task.CreatedAt, &task.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row was inserted concurrently by another request with the same idempotency key
			err = tx.QueryRow(ctx, `
				SELECT t.id, t.user_id, t.namespace, t.agent_profile_id, t.workspace_name,
				       t.prompt, t.target_branch, t.idempotency_key, t.status, t.max_attempts,
				       t.created_at, t.updated_at, t.completed_at,
				       (SELECT id FROM executions WHERE task_id = t.id ORDER BY attempt_number DESC LIMIT 1)
				FROM tasks t
				WHERE t.user_id = $1 AND t.idempotency_key = $2
			`, task.UserID, task.IdempotencyKey).Scan(
				&existing.ID, &existing.UserID, &existing.Namespace, &existing.AgentProfileID, &existing.WorkspaceName,
				&existing.Prompt, &existing.TargetBranch, &existing.IdempotencyKey, &existing.Status, &existing.MaxAttempts,
				&existing.CreatedAt, &existing.UpdatedAt, &existing.CompletedAt, &activeExecID,
			)
			if err != nil {
				return nil, false, fmt.Errorf("query concurrent task: %w", err)
			}
			existing.ActiveExecutionID = activeExecID
			_ = tx.Commit(ctx)
			return &existing, false, nil
		}
		return nil, false, fmt.Errorf("insert task: %w", err)
	}

	// Insert initial execution attempt
	if initialExecutionID == "" {
		initialExecutionID = "exec_" + task.ID
	}
	insertExecQuery := `
		INSERT INTO executions (
			id, task_id, namespace, workspace_name, attempt_number, status, created_at
		) VALUES ($1, $2, $3, $4, 1, 'scheduled', NOW())
	`
	_, err = tx.Exec(ctx, insertExecQuery, initialExecutionID, task.ID, task.Namespace, task.WorkspaceName)
	if err != nil {
		return nil, false, fmt.Errorf("insert initial execution: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit task creation: %w", err)
	}

	task.ActiveExecutionID = &initialExecutionID
	return task, true, nil
}

func (p *Postgres) GetTask(ctx context.Context, id string) (*Task, error) {
	query := `
		SELECT t.id, t.user_id, t.namespace, t.agent_profile_id, t.workspace_name,
		       t.prompt, t.target_branch, t.idempotency_key, t.status, t.max_attempts,
		       t.created_at, t.updated_at, t.completed_at,
		       (SELECT id FROM executions WHERE task_id = t.id ORDER BY attempt_number DESC LIMIT 1)
		FROM tasks t
		WHERE t.id = $1
	`
	var t Task
	var activeExecID *string
	err := p.Pool.QueryRow(ctx, query, id).Scan(
		&t.ID, &t.UserID, &t.Namespace, &t.AgentProfileID, &t.WorkspaceName,
		&t.Prompt, &t.TargetBranch, &t.IdempotencyKey, &t.Status, &t.MaxAttempts,
		&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &activeExecID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get task: %w", err)
	}
	t.ActiveExecutionID = activeExecID
	return &t, nil
}

func (p *Postgres) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	query := `
		SELECT t.id, t.user_id, t.namespace, t.agent_profile_id, t.workspace_name,
		       t.prompt, t.target_branch, t.idempotency_key, t.status, t.max_attempts,
		       t.created_at, t.updated_at, t.completed_at,
		       (SELECT id FROM executions WHERE task_id = t.id ORDER BY attempt_number DESC LIMIT 1)
		FROM tasks t
		WHERE ($1 = '' OR t.user_id = $1)
		  AND ($2 = '' OR t.namespace = $2)
		  AND ($3 = '' OR t.workspace_name = $3)
		  AND ($4 = '' OR t.status = $4)
		ORDER BY t.created_at DESC
		LIMIT $5 OFFSET $6
	`
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := p.Pool.Query(ctx, query, filter.UserID, filter.Namespace, filter.WorkspaceName, filter.Status, limit, filter.Offset)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		var activeExecID *string
		if err := rows.Scan(
			&t.ID, &t.UserID, &t.Namespace, &t.AgentProfileID, &t.WorkspaceName,
			&t.Prompt, &t.TargetBranch, &t.IdempotencyKey, &t.Status, &t.MaxAttempts,
			&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt, &activeExecID,
		); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t.ActiveExecutionID = activeExecID
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (p *Postgres) CancelTask(ctx context.Context, id string) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus string
	err = tx.QueryRow(ctx, "SELECT status FROM tasks WHERE id = $1 FOR UPDATE", id).Scan(&currentStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("get task status: %w", err)
	}
	if currentStatus == statusSucceeded || currentStatus == statusFailed || currentStatus == statusCancelled {
		_ = tx.Commit(ctx)
		return nil
	}

	_, err = tx.Exec(ctx, `
		UPDATE tasks
		SET status = 'cancelled', updated_at = NOW(), completed_at = NOW()
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("update task cancelled: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE executions
		SET status = 'cancelled', completed_at = NOW()
		WHERE task_id = $1 AND status IN ('scheduled', 'running', 'verifying')
	`, id)
	if err != nil {
		return fmt.Errorf("cancel executions: %w", err)
	}

	return tx.Commit(ctx)
}

func (p *Postgres) GetExecution(ctx context.Context, id string) (*Execution, error) {
	query := `
		SELECT id, task_id, namespace, workspace_name, attempt_number, worker_id,
		       lease_name, sandbox_name, status, exit_code, verification_passed,
		       verification_output, diff_stat, diff, error_message, log_buffer,
		       started_at, last_heartbeat_at, completed_at, created_at
		FROM executions
		WHERE id = $1
	`
	var e Execution
	err := p.Pool.QueryRow(ctx, query, id).Scan(
		&e.ID, &e.TaskID, &e.Namespace, &e.WorkspaceName, &e.AttemptNumber, &e.WorkerID,
		&e.LeaseName, &e.SandboxName, &e.Status, &e.ExitCode, &e.VerificationPassed,
		&e.VerificationOutput, &e.DiffStat, &e.Diff, &e.ErrorMessage, &e.LogBuffer,
		&e.StartedAt, &e.LastHeartbeatAt, &e.CompletedAt, &e.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get execution: %w", err)
	}
	return &e, nil
}

func (p *Postgres) ListExecutionsByTask(ctx context.Context, taskID string) ([]Execution, error) {
	query := `
		SELECT id, task_id, namespace, workspace_name, attempt_number, worker_id,
		       lease_name, sandbox_name, status, exit_code, verification_passed,
		       verification_output, diff_stat, diff, error_message, log_buffer,
		       started_at, last_heartbeat_at, completed_at, created_at
		FROM executions
		WHERE task_id = $1
		ORDER BY attempt_number ASC
	`
	rows, err := p.Pool.Query(ctx, query, taskID)
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	defer rows.Close()

	var execs []Execution
	for rows.Next() {
		var e Execution
		if err := rows.Scan(
			&e.ID, &e.TaskID, &e.Namespace, &e.WorkspaceName, &e.AttemptNumber, &e.WorkerID,
			&e.LeaseName, &e.SandboxName, &e.Status, &e.ExitCode, &e.VerificationPassed,
			&e.VerificationOutput, &e.DiffStat, &e.Diff, &e.ErrorMessage, &e.LogBuffer,
			&e.StartedAt, &e.LastHeartbeatAt, &e.CompletedAt, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan execution: %w", err)
		}
		execs = append(execs, e)
	}
	return execs, nil
}

// ClaimNextExecution claims the next scheduled execution using FOR UPDATE SKIP LOCKED
// while ensuring no active execution exists on the same (namespace, workspace_name).
func (p *Postgres) ClaimNextExecution(ctx context.Context, workerID string) (*Execution, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := `
		WITH eligible_execution AS (
			SELECT e.id
			FROM executions e
			WHERE e.status = 'scheduled'
			  AND NOT EXISTS (
				  SELECT 1 FROM executions active
				  WHERE active.namespace = e.namespace
				    AND active.workspace_name = e.workspace_name
				    AND active.status IN ('running', 'verifying')
			  )
			ORDER BY e.created_at ASC
			FOR UPDATE OF e SKIP LOCKED
			LIMIT 1
		)
		UPDATE executions
		SET status = 'running',
		    worker_id = $1,
		    started_at = NOW(),
		    last_heartbeat_at = NOW()
		FROM eligible_execution
		WHERE executions.id = eligible_execution.id
		RETURNING executions.id, executions.task_id, executions.namespace, executions.workspace_name,
		          executions.attempt_number, executions.worker_id, executions.status,
		          executions.started_at, executions.last_heartbeat_at, executions.created_at
	`
	var e Execution
	err = tx.QueryRow(ctx, query, workerID).Scan(
		&e.ID, &e.TaskID, &e.Namespace, &e.WorkspaceName,
		&e.AttemptNumber, &e.WorkerID, &e.Status,
		&e.StartedAt, &e.LastHeartbeatAt, &e.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // No scheduled execution available
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, nil // Concurrent worker claimed another execution on same workspace
		}
		return nil, fmt.Errorf("claim next execution: %w", err)
	}

	// Update task status from pending to running
	_, err = tx.Exec(ctx, `
		UPDATE tasks
		SET status = 'running', updated_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, e.TaskID)
	if err != nil {
		return nil, fmt.Errorf("update task status to running: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit execution claim: %w", err)
	}
	return &e, nil
}

func (p *Postgres) UpdateExecutionHeartbeat(ctx context.Context, executionID string) error {
	query := `
		UPDATE executions
		SET last_heartbeat_at = NOW()
		WHERE id = $1 AND status IN ('running', 'verifying')
	`
	tag, err := p.Pool.Exec(ctx, query, executionID)
	if err != nil {
		return fmt.Errorf("update execution heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendExecutionLog appends incremental output to an active execution log buffer and updates heartbeat.
func (p *Postgres) AppendExecutionLog(ctx context.Context, executionID string, chunk string) error {
	if chunk == "" {
		return nil
	}
	query := `
		UPDATE executions
		SET log_buffer = CASE
			WHEN LENGTH(COALESCE(log_buffer, '')) >= 65536 THEN log_buffer
			WHEN LENGTH(COALESCE(log_buffer, '') || $2) > 65536
			THEN SUBSTRING(COALESCE(log_buffer, '') || $2, 1, 65536) || E'\n[TRUNCATED]'
			ELSE COALESCE(log_buffer, '') || $2
		END,
		last_heartbeat_at = NOW()
		WHERE id = $1 AND status IN ('running', 'verifying')
	`
	tag, err := p.Pool.Exec(ctx, query, executionID, chunk)
	if err != nil {
		return fmt.Errorf("append execution log: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) BindExecutionRunner(ctx context.Context, executionID string, leaseName, sandboxName string) error {
	query := `
		UPDATE executions
		SET lease_name = $2, sandbox_name = $3
		WHERE id = $1
	`
	_, err := p.Pool.Exec(ctx, query, executionID, leaseName, sandboxName)
	if err != nil {
		return fmt.Errorf("bind execution runner: %w", err)
	}
	return nil
}

func (p *Postgres) TransitionExecutionToVerifying(ctx context.Context, executionID string) error {
	query := `
		UPDATE executions
		SET status = 'verifying', last_heartbeat_at = NOW()
		WHERE id = $1 AND status = 'running'
	`
	tag, err := p.Pool.Exec(ctx, query, executionID)
	if err != nil {
		return fmt.Errorf("transition to verifying: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type ExecutionResult struct {
	Status             string // succeeded, failed, cancelled
	ExitCode           *int
	VerificationPassed *bool
	VerificationOutput string
	DiffStat           string
	Diff               string
	ErrorMessage       string
	LogBuffer          string
}

// CompleteExecution records terminal results for an execution and evaluates retry budget.
// If retry is triggered, creates a new execution attempt with status 'scheduled'.
func (p *Postgres) CompleteExecution(ctx context.Context, executionID string, res ExecutionResult, nextExecutionID string) (bool, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Fetch execution and task details
	var taskID, namespace, workspaceName, execStatus string
	var attemptNumber, maxAttempts int
	var taskStatus string
	query := `
		SELECT e.task_id, e.namespace, e.workspace_name, e.attempt_number, e.status,
		       t.max_attempts, t.status
		FROM executions e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.id = $1
		FOR UPDATE OF e, t
	`
	err = tx.QueryRow(ctx, query, executionID).Scan(
		&taskID, &namespace, &workspaceName, &attemptNumber, &execStatus,
		&maxAttempts, &taskStatus,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("fetch execution for completion: %w", err)
	}

	if execStatus == statusSucceeded || execStatus == statusFailed || execStatus == statusCancelled {
		return false, nil
	}

	verificationOutput := truncateOutput(res.VerificationOutput)
	diffStat := truncateOutput(res.DiffStat)
	diff := truncateOutput(res.Diff)
	logBuffer := truncateOutput(res.LogBuffer)

	updateExecQuery := `
		UPDATE executions
		SET status = CASE WHEN status = 'cancelled' THEN status ELSE $2 END,
		    exit_code = $3,
		    verification_passed = $4,
		    verification_output = $5,
		    diff_stat = $6,
		    diff = $7,
		    error_message = $8,
		    log_buffer = $9,
		    completed_at = NOW()
		WHERE id = $1
	`
	_, err = tx.Exec(ctx, updateExecQuery,
		executionID, res.Status, res.ExitCode, res.VerificationPassed,
		verificationOutput, diffStat, diff, res.ErrorMessage, logBuffer,
	)
	if err != nil {
		return false, fmt.Errorf("update execution terminal state: %w", err)
	}

	retried := false
	if res.Status == statusSucceeded && taskStatus != statusCancelled {
		_, err = tx.Exec(ctx, `
			UPDATE tasks
			SET status = 'succeeded', completed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status != 'cancelled'
		`, taskID)
		if err != nil {
			return false, fmt.Errorf("update task succeeded: %w", err)
		}
	} else if res.Status == statusFailed {
		if taskStatus != statusCancelled {
			if attemptNumber < maxAttempts && nextExecutionID != "" {
				// Trigger retry attempt
				insertRetry := `
					INSERT INTO executions (
						id, task_id, namespace, workspace_name, attempt_number, status, created_at
					) VALUES ($1, $2, $3, $4, $5, 'scheduled', NOW())
				`
				_, err = tx.Exec(ctx, insertRetry, nextExecutionID, taskID, namespace, workspaceName, attemptNumber+1)
				if err != nil {
					return false, fmt.Errorf("insert retry execution: %w", err)
				}
				retried = true
			} else {
				_, err = tx.Exec(ctx, `
					UPDATE tasks
					SET status = 'failed', completed_at = NOW(), updated_at = NOW()
					WHERE id = $1 AND status != 'cancelled'
				`, taskID)
				if err != nil {
					return false, fmt.Errorf("update task failed: %w", err)
				}
			}
		}
	} else if res.Status == statusCancelled {
		_, err = tx.Exec(ctx, `
			UPDATE tasks
			SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, taskID)
		if err != nil {
			return false, fmt.Errorf("update task cancelled: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit complete execution: %w", err)
	}
	return retried, nil
}

// ReapOrphanExecutions identifies executions with stale heartbeats, marks them failed,
// and schedules retry if retry budget remains.
func (p *Postgres) ReapOrphanExecutions(ctx context.Context, orphanTimeout time.Duration) ([]Execution, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := `
		SELECT id, task_id, namespace, workspace_name, attempt_number, lease_name, sandbox_name
		FROM executions
		WHERE status IN ('running', 'verifying')
		  AND last_heartbeat_at < NOW() - $1::interval
		FOR UPDATE SKIP LOCKED
	`
	intervalStr := fmt.Sprintf("%d seconds", int(orphanTimeout.Seconds()))
	rows, err := tx.Query(ctx, query, intervalStr)
	if err != nil {
		return nil, fmt.Errorf("query orphan executions: %w", err)
	}
	defer rows.Close()

	var orphans []Execution
	for rows.Next() {
		var e Execution
		if err := rows.Scan(
			&e.ID, &e.TaskID, &e.Namespace, &e.WorkspaceName, &e.AttemptNumber,
			&e.LeaseName, &e.SandboxName,
		); err != nil {
			return nil, fmt.Errorf("scan orphan: %w", err)
		}
		orphans = append(orphans, e)
	}
	rows.Close()

	for _, orphan := range orphans {
		res := ExecutionResult{
			Status:       statusFailed,
			ErrorMessage: "worker heartbeat timeout; execution orphaned",
		}
		nextExecID := fmt.Sprintf("%s-att%d", orphan.TaskID, orphan.AttemptNumber+1)
		var maxAttempts int
		var taskStatus string
		err = tx.QueryRow(ctx, "SELECT max_attempts, status FROM tasks WHERE id = $1 FOR UPDATE", orphan.TaskID).Scan(&maxAttempts, &taskStatus)
		if err != nil {
			continue
		}

		_, _ = tx.Exec(ctx, `
			UPDATE executions
			SET status = CASE WHEN status = 'cancelled' THEN status ELSE 'failed' END,
			    error_message = $2,
			    completed_at = NOW()
			WHERE id = $1
		`, orphan.ID, res.ErrorMessage)

		if orphan.AttemptNumber < maxAttempts && taskStatus != statusCancelled {
			_, _ = tx.Exec(ctx, `
				INSERT INTO executions (
					id, task_id, namespace, workspace_name, attempt_number, status, created_at
				) VALUES ($1, $2, $3, $4, $5, 'scheduled', NOW())
			`, nextExecID, orphan.TaskID, orphan.Namespace, orphan.WorkspaceName, orphan.AttemptNumber+1)
		} else if taskStatus != statusCancelled {
			_, _ = tx.Exec(ctx, `
				UPDATE tasks
				SET status = 'failed', completed_at = NOW(), updated_at = NOW()
				WHERE id = $1 AND status != 'cancelled'
			`, orphan.TaskID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit orphan reap: %w", err)
	}
	return orphans, nil
}
