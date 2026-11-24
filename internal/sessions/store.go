package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

var ErrSessionNotFound = errors.New("session not found")

// Session represents an in-guest execution session persisted in PostgreSQL.
type Session struct {
	ID          string             `json:"id"`
	UserID      string             `json:"userId"`
	Namespace   string             `json:"namespace"`
	SandboxName string             `json:"sandboxName"`
	Command     []string           `json:"command"`
	WorkingDir  string             `json:"workingDir"`
	TTY         bool               `json:"tty"`
	State       apiv1.SessionState `json:"state"`
	ExitCode    *int               `json:"exitCode,omitempty"`
	PID         *int               `json:"pid,omitempty"`
	CreatedAt   time.Time          `json:"createdAt"`
	StartedAt   *time.Time         `json:"startedAt,omitempty"`
	CompletedAt *time.Time         `json:"completedAt,omitempty"`
	UpdatedAt   time.Time          `json:"updatedAt"`
	Metadata    map[string]string  `json:"metadata,omitempty"`
}

// Store handles persistence and state transitions for sessions.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore initializes a session store backed by PostgreSQL.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateSession inserts a new session record with initial state 'pending' or 'running'.
func (s *Store) CreateSession(ctx context.Context, session *Session) error {
	if session == nil {
		return fmt.Errorf("session cannot be nil")
	}
	if session.ID == "" {
		return fmt.Errorf("session ID is required")
	}
	if session.WorkingDir == "" {
		session.WorkingDir = "/workspace"
	}
	if session.State == "" {
		session.State = apiv1.SessionStatePending
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.CreatedAt
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}

	cmdJSON, err := json.Marshal(session.Command)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	metaJSON, err := json.Marshal(session.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	query := `
		INSERT INTO sandbox_sessions (
			id, user_id, namespace, sandbox_name, command, working_dir, tty,
			state, exit_code, pid, created_at, started_at, completed_at, updated_at, metadata
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		)
	`
	_, err = s.pool.Exec(ctx, query,
		session.ID,
		session.UserID,
		session.Namespace,
		session.SandboxName,
		cmdJSON,
		session.WorkingDir,
		session.TTY,
		string(session.State),
		session.ExitCode,
		session.PID,
		session.CreatedAt,
		session.StartedAt,
		session.CompletedAt,
		session.UpdatedAt,
		metaJSON,
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSession retrieves a single session by its unique ID.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	query := `
		SELECT
			id, user_id, namespace, sandbox_name, command, working_dir, tty,
			state, exit_code, pid, created_at, started_at, completed_at, updated_at, metadata
		FROM sandbox_sessions
		WHERE id = $1
	`
	var (
		sess     Session
		cmdRaw   []byte
		metaRaw  []byte
		stateStr string
	)

	err := s.pool.QueryRow(ctx, query, id).Scan(
		&sess.ID,
		&sess.UserID,
		&sess.Namespace,
		&sess.SandboxName,
		&cmdRaw,
		&sess.WorkingDir,
		&sess.TTY,
		&stateStr,
		&sess.ExitCode,
		&sess.PID,
		&sess.CreatedAt,
		&sess.StartedAt,
		&sess.CompletedAt,
		&sess.UpdatedAt,
		&metaRaw,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("query session %s: %w", id, err)
	}

	sess.State = apiv1.SessionState(stateStr)
	if err := json.Unmarshal(cmdRaw, &sess.Command); err != nil {
		return nil, fmt.Errorf("unmarshal session command: %w", err)
	}
	if err := json.Unmarshal(metaRaw, &sess.Metadata); err != nil {
		sess.Metadata = make(map[string]string)
	}

	return &sess, nil
}

// ListSessions lists all sessions associated with a specific sandbox ordered by creation time descending.
func (s *Store) ListSessions(ctx context.Context, namespace, sandboxName string) ([]Session, error) {
	query := `
		SELECT
			id, user_id, namespace, sandbox_name, command, working_dir, tty,
			state, exit_code, pid, created_at, started_at, completed_at, updated_at, metadata
		FROM sandbox_sessions
		WHERE namespace = $1 AND sandbox_name = $2
		ORDER BY created_at DESC
	`
	rows, err := s.pool.Query(ctx, query, namespace, sandboxName)
	if err != nil {
		return nil, fmt.Errorf("list sessions for %s/%s: %w", namespace, sandboxName, err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var (
			sess     Session
			cmdRaw   []byte
			metaRaw  []byte
			stateStr string
		)
		if err := rows.Scan(
			&sess.ID,
			&sess.UserID,
			&sess.Namespace,
			&sess.SandboxName,
			&cmdRaw,
			&sess.WorkingDir,
			&sess.TTY,
			&stateStr,
			&sess.ExitCode,
			&sess.PID,
			&sess.CreatedAt,
			&sess.StartedAt,
			&sess.CompletedAt,
			&sess.UpdatedAt,
			&metaRaw,
		); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		sess.State = apiv1.SessionState(stateStr)
		if err := json.Unmarshal(cmdRaw, &sess.Command); err != nil {
			sess.Command = []string{}
		}
		if err := json.Unmarshal(metaRaw, &sess.Metadata); err != nil {
			sess.Metadata = make(map[string]string)
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error listing sessions: %w", err)
	}

	return sessions, nil
}

// UpdateSessionState transitions the state of a session and updates completion timestamps if terminal.
func (s *Store) UpdateSessionState(ctx context.Context, id string, state apiv1.SessionState, exitCode *int, pid *int) error {
	now := time.Now().UTC()
	var query string
	var args []interface{}

	switch state {
	case apiv1.SessionStateRunning:
		query = `
			UPDATE sandbox_sessions
			SET state = $2, pid = COALESCE($3, pid), started_at = COALESCE(started_at, $4), updated_at = $4
			WHERE id = $1
		`
		args = []interface{}{id, string(state), pid, now}
	case apiv1.SessionStateCompleted, apiv1.SessionStateFailed, apiv1.SessionStateTerminated:
		query = `
			UPDATE sandbox_sessions
			SET state = $2, exit_code = COALESCE($3, exit_code), completed_at = COALESCE(completed_at, $4), updated_at = $4
			WHERE id = $1
		`
		args = []interface{}{id, string(state), exitCode, now}
	default:
		query = `
			UPDATE sandbox_sessions
			SET state = $2, updated_at = $3
			WHERE id = $1
		`
		args = []interface{}{id, string(state), now}
	}

	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update session state %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// TerminateActiveSessionsForSandbox marks all non-terminal sessions for a sandbox as 'terminated'.
func (s *Store) TerminateActiveSessionsForSandbox(ctx context.Context, namespace, sandboxName string) error {
	now := time.Now().UTC()
	query := `
		UPDATE sandbox_sessions
		SET state = 'terminated', completed_at = COALESCE(completed_at, $3), updated_at = $3
		WHERE namespace = $1 AND sandbox_name = $2 AND state IN ('pending', 'running')
	`
	_, err := s.pool.Exec(ctx, query, namespace, sandboxName, now)
	if err != nil {
		return fmt.Errorf("terminate sessions for %s/%s: %w", namespace, sandboxName, err)
	}
	return nil
}

// SweepZombieSessions transitions active sessions to 'terminated' if their sandbox is not in the active set.
func (s *Store) SweepZombieSessions(ctx context.Context, namespace string, activeSandboxNames []string) (int64, error) {
	now := time.Now().UTC()
	query := `
		UPDATE sandbox_sessions
		SET state = 'terminated', completed_at = COALESCE(completed_at, $3), updated_at = $3
		WHERE namespace = $1 AND state IN ('pending', 'running') AND NOT (sandbox_name = ANY($2))
	`
	tag, err := s.pool.Exec(ctx, query, namespace, activeSandboxNames, now)
	if err != nil {
		return 0, fmt.Errorf("sweep zombie sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
