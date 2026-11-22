package main

import "testing"

func TestLoadRuntimeConfig(t *testing.T) {
	values := map[string]string{
		"BUDDY_DATABASE_URL":                         "postgres://user:pass@localhost:5432/buddy",
		"BUDDY_CREDENTIAL_ENCRYPTION_KEYS_FILE":      "/etc/buddy/keys.json",
		"BUDDY_CREDENTIAL_ENCRYPTION_ACTIVE_VERSION": "1",
		"EGRESS_GATEWAY_HMAC_SECRET":                 "hmac-secret-material-which-is-long-enough",
	}
	getenv := func(key string) string { return values[key] }
	config, err := loadRuntimeConfig(getenv)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if config.databaseURL != values["BUDDY_DATABASE_URL"] || config.encryptionKeys != values["BUDDY_CREDENTIAL_ENCRYPTION_KEYS_FILE"] ||
		config.activeKeyVersion != 1 || string(config.hmac) != values["EGRESS_GATEWAY_HMAC_SECRET"] {
		t.Fatalf("unexpected config: %+v", config)
	}

	for key, invalid := range map[string]string{
		"BUDDY_DATABASE_URL":                         "",
		"BUDDY_CREDENTIAL_ENCRYPTION_KEYS_FILE":      "",
		"BUDDY_CREDENTIAL_ENCRYPTION_ACTIVE_VERSION": "bad",
		"EGRESS_GATEWAY_HMAC_SECRET":                 "short",
	} {
		copyValues := map[string]string{}
		for k, v := range values {
			copyValues[k] = v
		}
		copyValues[key] = invalid
		if _, err := loadRuntimeConfig(func(name string) string { return copyValues[name] }); err == nil {
			t.Errorf("invalid %s accepted", key)
		}
	}
}
