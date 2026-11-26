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

func TestOpenAPIContractDetails(t *testing.T) {
	specJSON, err := GenerateOpenAPISpec()
	if err != nil {
		t.Fatalf("GenerateOpenAPISpec failed: %v", err)
	}

	var parsed struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
				Schema   struct {
					MaxLength int    `json:"maxLength"`
					Type      string `json:"type"`
				} `json:"schema"`
			} `json:"parameters"`
			Responses map[string]struct {
				Description string `json:"description"`
				Content     map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(specJSON, &parsed); err != nil {
		t.Fatalf("failed to parse spec: %v", err)
	}

	// 1. Verify CreateTask Idempotency-Key header
	createTaskPath, ok := parsed.Paths["/api/v1/tasks"]["post"]
	if !ok {
		t.Fatal("expected POST /api/v1/tasks operation")
	}
	var foundIdempotencyKey bool
	for _, p := range createTaskPath.Parameters {
		if p.In == "header" && p.Name == "Idempotency-Key" {
			foundIdempotencyKey = true
			if !p.Required {
				t.Errorf("expected Idempotency-Key to be required")
			}
			if p.Schema.MaxLength != 128 {
				t.Errorf("expected Idempotency-Key maxLength 128, got %d", p.Schema.MaxLength)
			}
			break
		}
	}
	if !foundIdempotencyKey {
		t.Errorf("missing Idempotency-Key header parameter in createTask operation")
	}

	// 2. Verify Suspend and Resume declare 202 and SandboxResponse
	for _, path := range []string{"/api/v1/sandboxes/{name}/suspend", "/api/v1/sandboxes/{name}/resume"} {
		op, ok := parsed.Paths[path]["post"]
		if !ok {
			t.Fatalf("expected POST %s operation", path)
		}
		resp202, ok := op.Responses["202"]
		if !ok {
			t.Errorf("expected 202 response for %s, available responses: %+v", path, op.Responses)
			continue
		}
		contentJSON, ok := resp202.Content["application/json"]
		if !ok {
			t.Errorf("expected application/json content in 202 response for %s", path)
			continue
		}
		if !strings.HasSuffix(contentJSON.Schema.Ref, "SandboxResponse") {
			t.Errorf("expected SandboxResponse schema reference in %s 202 response, got %s", path, contentJSON.Schema.Ref)
		}
	}
}
