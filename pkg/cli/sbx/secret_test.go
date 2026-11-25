package sbx

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func TestSecretSetAndListFlags(t *testing.T) {
	var capturedSetReq apiv1.SetCredentialRequest
	var capturedService string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/credentials/") {
			capturedService = strings.TrimPrefix(r.URL.Path, "/api/v1/credentials/")
			_ = json.NewDecoder(r.Body).Decode(&capturedSetReq)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/credentials" {
			res := map[string]any{
				"items": []apiv1.CredentialMetadata{
					{
						Service:      "openai",
						Scope:        "user",
						BaseURL:      "https://test.openai.azure.com/openai/v1",
						DefaultModel: "gpt-6-luna",
						UpdatedAt:    time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC),
					},
				},
			}
			_ = json.NewEncoder(w).Encode(res)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	prevEndpoint := flagEndpoint
	prevToken := flagToken
	flagEndpoint = ts.URL
	flagToken = "test-user-token"
	defer func() {
		flagEndpoint = prevEndpoint
		flagToken = prevToken
	}()

	t.Setenv("OPENAI_API_KEY", "sk-env-token-12345")

	// Test create secret with --url, --model, --rpm, --max-concurrent, --tpm
	setCmd := newCreateSecretCmd()
	setCmd.SetArgs([]string{
		"openai",
		"--url", "https://test.openai.azure.com/openai/v1",
		"--model", "gpt-6-luna",
		"--rpm", "1000",
		"--max-concurrent", "16",
		"--tpm", "5000000",
	})
	setOut := &bytes.Buffer{}
	setCmd.SetOut(setOut)
	setCmd.SetErr(setOut)

	if err := setCmd.Execute(); err != nil {
		t.Fatalf("create secret failed: %v", err)
	}

	if capturedService != "openai" {
		t.Fatalf("service = %q, want openai", capturedService)
	}
	if capturedSetReq.Value != "sk-env-token-12345" {
		t.Fatalf("key value = %q, want sk-env-token-12345", capturedSetReq.Value)
	}
	if capturedSetReq.BaseURL != "https://test.openai.azure.com/openai/v1" {
		t.Fatalf("base URL = %q", capturedSetReq.BaseURL)
	}
	if capturedSetReq.DefaultModel != "gpt-6-luna" {
		t.Fatalf("default model = %q", capturedSetReq.DefaultModel)
	}
	if capturedSetReq.RequestsPerMinute != 1000 {
		t.Fatalf("requests per minute = %d, want 1000", capturedSetReq.RequestsPerMinute)
	}
	if capturedSetReq.MaxConcurrent != 16 {
		t.Fatalf("max concurrent = %d, want 16", capturedSetReq.MaxConcurrent)
	}
	if capturedSetReq.TokensPerMinute != 5000000 {
		t.Fatalf("tokens per minute = %d, want 5000000", capturedSetReq.TokensPerMinute)
	}

	// Test get secrets
	lsCmd := newGetCmd()
	lsCmd.SetArgs([]string{"secrets"})
	lsOut := &bytes.Buffer{}
	lsCmd.SetOut(lsOut)
	lsCmd.SetErr(lsOut)

	if err := lsCmd.Execute(); err != nil {
		t.Fatalf("get secrets failed: %v", err)
	}

	outStr := lsOut.String()
	if !strings.Contains(outStr, "BASE_URL") || !strings.Contains(outStr, "MODEL") {
		t.Fatalf("get secrets missing headers: %s", outStr)
	}
	if !strings.Contains(outStr, "https://test.openai.azure.com/openai/v1") || !strings.Contains(outStr, "gpt-6-luna") {
		t.Fatalf("get secrets output missing record details: %s", outStr)
	}

	// Test get secrets -o wide
	lsWideCmd := newGetCmd()
	lsWideCmd.SetArgs([]string{"secrets", "-o", "wide"})
	lsWideOut := &bytes.Buffer{}
	lsWideCmd.SetOut(lsWideOut)
	lsWideCmd.SetErr(lsWideOut)
	if err := lsWideCmd.Execute(); err != nil {
		t.Fatalf("get secrets -o wide failed: %v", err)
	}
	wideStr := lsWideOut.String()
	if !strings.Contains(wideStr, "RPM") ||
		!strings.Contains(wideStr, "CONCURRENCY") ||
		!strings.Contains(wideStr, "TPM") {
		t.Fatalf("get secrets -o wide missing rate limit headers: %s", wideStr)
	}
}

func TestRunCommandSupportsModelFlag(t *testing.T) {
	cmd := newRunCmd()
	flag := cmd.Flags().Lookup("model")
	if flag == nil {
		t.Fatal("expected --model flag on run command")
	}
	if flag.Shorthand != "m" {
		t.Fatalf("expected -m shorthand on --model flag, got %q", flag.Shorthand)
	}
}
