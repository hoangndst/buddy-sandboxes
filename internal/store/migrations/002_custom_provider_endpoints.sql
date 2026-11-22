ALTER TABLE provider_credentials ADD COLUMN IF NOT EXISTS base_url text;
ALTER TABLE provider_credentials ADD COLUMN IF NOT EXISTS default_model text;
ALTER TABLE provider_credentials ADD COLUMN IF NOT EXISTS auth_type text;
