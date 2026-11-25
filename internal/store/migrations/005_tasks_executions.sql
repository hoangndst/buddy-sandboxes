CREATE TABLE IF NOT EXISTS agent_profiles (
    id text PRIMARY KEY,
    name text NOT NULL,
    description text,
    harness_type text NOT NULL CHECK (harness_type IN ('codex', 'claude-code', 'custom')),
    model_name text NOT NULL,
    system_prompt text,
    verification_command text,
    max_steps integer NOT NULL DEFAULT 30,
    timeout_seconds integer NOT NULL DEFAULT 3600,
    verification_timeout_seconds integer NOT NULL DEFAULT 300,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tasks (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    namespace text NOT NULL DEFAULT 'default',
    agent_profile_id text REFERENCES agent_profiles(id),
    workspace_name text NOT NULL,
    prompt text NOT NULL,
    target_branch text,
    idempotency_key text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    max_attempts integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    UNIQUE (user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS tasks_user_idx
    ON tasks(user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS tasks_workspace_status_idx
    ON tasks(namespace, workspace_name, status);

CREATE TABLE IF NOT EXISTS executions (
    id text PRIMARY KEY,
    task_id text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    namespace text NOT NULL DEFAULT 'default',
    workspace_name text NOT NULL,
    attempt_number integer NOT NULL DEFAULT 1,
    worker_id text,
    lease_name text,
    sandbox_name text,
    status text NOT NULL CHECK (status IN ('scheduled', 'running', 'verifying', 'succeeded', 'failed', 'cancelled')),
    exit_code integer,
    verification_passed boolean,
    verification_output text,
    diff_stat text,
    error_message text,
    log_buffer text,
    started_at timestamptz,
    last_heartbeat_at timestamptz,
    completed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, attempt_number)
);

-- Mutual exclusion: Only one active execution per workspace at a time
CREATE UNIQUE INDEX IF NOT EXISTS executions_active_workspace_idx
    ON executions (namespace, workspace_name)
    WHERE status IN ('running', 'verifying');

CREATE INDEX IF NOT EXISTS executions_claim_idx
    ON executions (status, created_at ASC)
    WHERE status = 'scheduled';

CREATE INDEX IF NOT EXISTS executions_heartbeat_idx
    ON executions (status, last_heartbeat_at)
    WHERE status IN ('running', 'verifying');
