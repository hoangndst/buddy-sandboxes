package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGenerateOpenAPISpec(t *testing.T) {
	specJSON, err := GenerateOpenAPISpec()
	if err != nil {
		t.Fatalf("GenerateOpenAPISpec failed: %v", err)
	}

	if len(specJSON) == 0 {
		t.Fatal("expected non-empty OpenAPI spec JSON")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(specJSON, &parsed); err != nil {
		t.Fatalf("failed to parse generated spec as JSON: %v", err)
	}

	specStr := string(specJSON)

	// Verify Core Tags
	requiredTags := []string{
		"Sandboxes",
		"Sessions",
		"Tasks",
		"AgentProfiles",
		"Identity",
		"Credentials",
		"Admin",
	}
	for _, tag := range requiredTags {
		if !strings.Contains(specStr, tag) {
			t.Errorf("expected OpenAPI spec to contain tag %q", tag)
		}
	}

	// Verify Key Operation IDs
	requiredOperations := []string{
		"createSandbox",
		"listSandboxes",
		"getSandbox",
		"deleteSandbox",
		"createSession",
		"createTask",
		"listTasks",
		"getTask",
		"whoAmI",
	}
	for _, op := range requiredOperations {
		if !strings.Contains(specStr, op) {
			t.Errorf("expected OpenAPI spec to contain operation %q", op)
		}
	}

	// Verify OpenAPI Version
	if !strings.Contains(specStr, `"openapi": "3.1`) {
		t.Errorf("expected OpenAPI 3.1 specification header")
	}
}

func TestOpenAPIServerEndpoints(t *testing.T) {
	server := NewServer(Config{Auth: NewStaticTokenAuthenticator(map[string]string{"test-token": "default"})})

	// 1. GET /openapi.json
	reqJSON := httptest.NewRequest("GET", "/openapi.json", nil)
	rrJSON := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rrJSON, reqJSON)
	if rrJSON.Code != 200 {
		t.Fatalf("expected 200 for /openapi.json, got %d: %s", rrJSON.Code, rrJSON.Body.String())
	}
	if !strings.Contains(rrJSON.Body.String(), "Buddy Sandboxes API") {
		t.Fatalf("expected title in /openapi.json, got: %s", rrJSON.Body.String()[:100])
	}

	// 2. GET /docs
	reqDocs := httptest.NewRequest("GET", "/docs", nil)
	rrDocs := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rrDocs, reqDocs)
	if rrDocs.Code != 200 {
		t.Fatalf("expected 200 for /docs, got %d: %s", rrDocs.Code, rrDocs.Body.String())
	}

	// 3. GET /readyz (health endpoint)
	reqReady := httptest.NewRequest("GET", "/readyz", nil)
	rrReady := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rrReady, reqReady)
	if rrReady.Code != 200 {
		t.Fatalf("expected 200 for /readyz, got %d", rrReady.Code)
	}
}
