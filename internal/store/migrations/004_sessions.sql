CREATE TABLE IF NOT EXISTS sandbox_sessions (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    namespace text NOT NULL,
    sandbox_name text NOT NULL,
    command jsonb NOT NULL,
    working_dir text NOT NULL DEFAULT '/workspace',
    tty boolean NOT NULL DEFAULT true,
    state text NOT NULL CHECK (state IN ('pending', 'running', 'completed', 'failed', 'terminated')),
    exit_code integer,
    pid integer,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS sandbox_sessions_sandbox_idx
    ON sandbox_sessions(namespace, sandbox_name, state);

CREATE INDEX IF NOT EXISTS sandbox_sessions_user_idx
    ON sandbox_sessions(user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS sandbox_sessions_active_idx
    ON sandbox_sessions(state) WHERE state IN ('pending', 'running');
