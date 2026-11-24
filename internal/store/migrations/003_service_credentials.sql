CREATE TABLE IF NOT EXISTS service_credentials (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    service text NOT NULL CHECK (service ~ '^[a-z][a-z0-9-]{0,62}$'),
    scope text NOT NULL CHECK (scope IN ('user', 'sandbox')),
    sandbox_name text,
    target_key text NOT NULL DEFAULT '',
    ciphertext bytea NOT NULL,
    key_version integer NOT NULL CHECK (key_version > 0),
    options jsonb NOT NULL DEFAULT '{}'::jsonb,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'user' AND sandbox_name IS NULL) OR (scope = 'sandbox' AND sandbox_name IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS service_credentials_user_target_idx
    ON service_credentials(user_id, service, target_key) WHERE scope = 'user';
CREATE UNIQUE INDEX IF NOT EXISTS service_credentials_sandbox_target_idx
    ON service_credentials(user_id, service, target_key, sandbox_name) WHERE scope = 'sandbox';
CREATE INDEX IF NOT EXISTS service_credentials_expiry_idx
    ON service_credentials(expires_at) WHERE expires_at IS NOT NULL;

INSERT INTO service_credentials(id,user_id,service,scope,sandbox_name,target_key,ciphertext,key_version,options,created_at,updated_at)
SELECT id,user_id,service,scope,sandbox_name,'',ciphertext,key_version,
       jsonb_strip_nulls(jsonb_build_object('baseUrl',base_url,'defaultModel',default_model,'authType',auth_type)),
       created_at,updated_at
FROM provider_credentials
ON CONFLICT (id) DO NOTHING;

CREATE OR REPLACE FUNCTION sync_legacy_provider_credential() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM service_credentials WHERE id = OLD.id AND target_key = '';
        RETURN OLD;
    END IF;

    INSERT INTO service_credentials(id,user_id,service,scope,sandbox_name,target_key,ciphertext,key_version,options,created_at,updated_at)
    VALUES(NEW.id,NEW.user_id,NEW.service,NEW.scope,NEW.sandbox_name,'',NEW.ciphertext,NEW.key_version,
           jsonb_strip_nulls(jsonb_build_object('baseUrl',NEW.base_url,'defaultModel',NEW.default_model,'authType',NEW.auth_type)),
           NEW.created_at,NEW.updated_at)
    ON CONFLICT(id) DO UPDATE SET
        user_id=EXCLUDED.user_id,service=EXCLUDED.service,scope=EXCLUDED.scope,sandbox_name=EXCLUDED.sandbox_name,
        target_key='',ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,options=EXCLUDED.options,
        updated_at=EXCLUDED.updated_at;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_credentials_to_service_credentials
AFTER INSERT OR UPDATE OR DELETE ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION sync_legacy_provider_credential();

CREATE TABLE IF NOT EXISTS git_sandbox_bindings (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id text NOT NULL REFERENCES service_credentials(id) ON DELETE CASCADE,
    namespace text NOT NULL,
    sandbox_name text NOT NULL,
    sandbox_uid text NOT NULL DEFAULT '',
    creation_id text NOT NULL,
    host text NOT NULL CHECK (host = 'github.com'),
    repository_path text NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'bound', 'revoked')),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(namespace, sandbox_name, creation_id)
);
CREATE INDEX IF NOT EXISTS git_sandbox_bindings_sandbox_idx
    ON git_sandbox_bindings(namespace, sandbox_name, sandbox_uid);
CREATE INDEX IF NOT EXISTS git_sandbox_bindings_expiry_idx
    ON git_sandbox_bindings(expires_at) WHERE state = 'pending';

CREATE TABLE IF NOT EXISTS git_clone_claims (
    binding_id text PRIMARY KEY REFERENCES git_sandbox_bindings(id) ON DELETE CASCADE,
    sandbox_uid text NOT NULL,
    creation_id text NOT NULL,
    pod_name text NOT NULL,
    pod_uid text NOT NULL,
    attempts integer NOT NULL CHECK (attempts BETWEEN 1 AND 3),
    deadline timestamptz NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('claimed', 'failed', 'succeeded')),
    updated_at timestamptz NOT NULL DEFAULT now()
);
