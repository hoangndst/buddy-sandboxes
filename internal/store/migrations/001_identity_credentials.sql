CREATE TABLE IF NOT EXISTS namespace_registry (
    name text PRIMARY KEY,
    enabled_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id text PRIMARY KEY,
    display_name text NOT NULL,
    role text NOT NULL CHECK (role IN ('admin', 'user')),
    namespace text REFERENCES namespace_registry(name),
    disabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((role = 'user' AND namespace IS NOT NULL) OR role = 'admin')
);
CREATE UNIQUE INDEX IF NOT EXISTS users_namespace_unique ON users(namespace) WHERE namespace IS NOT NULL;

CREATE TABLE IF NOT EXISTS auth_tokens (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS auth_tokens_user_id_idx ON auth_tokens(user_id);

CREATE TABLE IF NOT EXISTS provider_credentials (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service text NOT NULL CHECK (service IN ('openai', 'anthropic')),
    scope text NOT NULL CHECK (scope IN ('user', 'sandbox')),
    sandbox_name text,
    ciphertext bytea NOT NULL,
    key_version integer NOT NULL CHECK (key_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'user' AND sandbox_name IS NULL) OR (scope = 'sandbox' AND sandbox_name IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS provider_credentials_user_service_idx
    ON provider_credentials(user_id, service) WHERE scope = 'user';
CREATE UNIQUE INDEX IF NOT EXISTS provider_credentials_sandbox_service_idx
    ON provider_credentials(user_id, service, sandbox_name) WHERE scope = 'sandbox';

CREATE TABLE IF NOT EXISTS broker_capabilities (
    token_hash bytea PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    namespace text NOT NULL,
    sandbox_name text NOT NULL,
    sandbox_uid text NOT NULL DEFAULT '',
    creation_id text NOT NULL,
    service text NOT NULL CHECK (service IN ('openai', 'anthropic')),
    allowed_routes text[] NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'active', 'revoked')),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS broker_capabilities_sandbox_idx
    ON broker_capabilities(namespace, sandbox_name, creation_id);

CREATE TABLE IF NOT EXISTS bootstrap_state (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    token_hash bytea,
    consumed_at timestamptz
);

CREATE TABLE IF NOT EXISTS audit_events (
    id bigserial PRIMARY KEY,
    actor_user_id text,
    action text NOT NULL,
    target text NOT NULL DEFAULT '',
    succeeded boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
