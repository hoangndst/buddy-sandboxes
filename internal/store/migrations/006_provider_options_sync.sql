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
        target_key='',ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,
        options=service_credentials.options || EXCLUDED.options,
        updated_at=EXCLUDED.updated_at;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
