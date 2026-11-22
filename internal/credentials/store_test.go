package credentials

import (
	"strings"
	"testing"
)

func TestValidateBaseURL(t *testing.T) {
	valid := []string{
		"",
		"https://api.openai.com",
		"https://api.openai.com/v1",
		"https://account.openai.azure.com/openai/v1",
		"https://account.openai.azure.com/openai/v1?api-version=2024-02-15-preview",
		"https://openrouter.ai/api/v1",
		"https://my-gateway.example.com:8443/v1",
		"http://127.0.0.1:8080/v1",
		"http://localhost:11434/v1",
	}
	for _, url := range valid {
		if err := ValidateBaseURL(url); err != nil {
			t.Errorf("ValidateBaseURL(%q) unexpected error: %v", url, err)
		}
	}

	invalid := []struct {
		url string
		err string
	}{
		{"ftp://api.openai.com", "HTTPS"},
		{"http://api.openai.com", "HTTPS"},
		{"not-a-url", "invalid provider URL"},
		{"https://", "valid host"},
		{"https://10.0.0.1:8000/v1", "FQDN, not a raw IP literal"},
		{"https://192.168.1.1:8000", "FQDN, not a raw IP literal"},
		{"https://172.16.0.5:8000", "FQDN, not a raw IP literal"},
		{"https://buddy-postgres.buddy-sandboxes-system.svc.cluster.local:5432", "not permitted"},
		{"https://kubernetes.default.svc:443", "not permitted"},
		{"https://metadata.google.internal", "not permitted"},
		{"https://secret.local", "not permitted"},
	}
	for _, tc := range invalid {
		err := ValidateBaseURL(tc.url)
		if err == nil {
			t.Errorf("ValidateBaseURL(%q) expected error containing %q, got nil", tc.url, tc.err)
		} else if !strings.Contains(err.Error(), tc.err) {
			t.Errorf("ValidateBaseURL(%q) error = %v, want substring %q", tc.url, err, tc.err)
		}
	}
}

func TestValidateModelName(t *testing.T) {
	valid := []string{
		"",
		"gpt-4o",
		"gpt-6-luna",
		"gpt-6.2-sol",
		"o3-mini",
		"o1",
		"deepseek/deepseek-chat",
		"deepseek/deepseek-r1:70b",
		"meta-llama/llama-3.3-70b-instruct",
		"claude-3-7-sonnet-20250219",
	}
	for _, m := range valid {
		if err := ValidateModelName(m); err != nil {
			t.Errorf("ValidateModelName(%q) unexpected error: %v", m, err)
		}
	}

	invalid := []string{
		"../traversal",
		"/root/model",
		"model/",
		"model with spaces",
		"model\nnewline",
		strings.Repeat("a", 129),
	}
	for _, m := range invalid {
		if err := ValidateModelName(m); err == nil {
			t.Errorf("ValidateModelName(%q) expected error, got nil", m)
		}
	}
}
