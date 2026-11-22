package main

import "testing"

func TestLoadRuntimeConfig(t *testing.T) {
	values := map[string]string{
		"BUDDY_API_TOKEN":            "abcdefghijklmnopqrstuvwxyz012345",
		"BUDDY_API_NAMESPACE":        "default",
		"EGRESS_GATEWAY_HMAC_SECRET": "hmac-secret-material-which-is-long-enough",
	}
	getenv := func(key string) string { return values[key] }
	config, err := loadRuntimeConfig(getenv)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if config.token != values["BUDDY_API_TOKEN"] || config.namespace != "default" || string(config.hmac) != values["EGRESS_GATEWAY_HMAC_SECRET"] {
		t.Fatalf("unexpected config: %+v", config)
	}

	for key, invalid := range map[string]string{
		"BUDDY_API_TOKEN":            "dev-token-secret",
		"BUDDY_API_NAMESPACE":        "",
		"EGRESS_GATEWAY_HMAC_SECRET": "short",
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
