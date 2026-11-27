package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
					MaxLength int `json:"maxLength"`
					Type      any `json:"type"`
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

	// 3. Verify CancelTask declares 200 with dedicated cancellation body (not TaskResponse)
	cancelOp, ok := parsed.Paths["/api/v1/tasks/{id}/cancel"]["post"]
	if !ok {
		t.Fatal("expected POST /api/v1/tasks/{id}/cancel operation")
	}
	respCancel, ok := cancelOp.Responses["200"]
	if !ok {
		t.Fatalf("expected 200 response for cancelTask, got %+v", cancelOp.Responses)
	}
	if content, ok := respCancel.Content["application/json"]; !ok || strings.HasSuffix(content.Schema.Ref, "TaskResponse") {
		t.Errorf("expected dedicated cancellation response, got %s", content.Schema.Ref)
	}

	// 4. Verify DeleteSession declares 200 with termination response (not 204)
	deleteSessOp, ok := parsed.Paths["/api/v1/sandboxes/{name}/sessions/{sessionID}"]["delete"]
	if !ok {
		t.Fatal("expected DELETE /api/v1/sandboxes/{name}/sessions/{sessionID} operation")
	}
	if _, ok := deleteSessOp.Responses["200"]; !ok {
		t.Errorf("expected 200 response for deleteSession, got %+v", deleteSessOp.Responses)
	}

	// 5. Verify ListSandboxes has no unsupported query parameters
	listSbxOp, ok := parsed.Paths["/api/v1/sandboxes"]["get"]
	if !ok {
		t.Fatal("expected GET /api/v1/sandboxes operation")
	}
	if len(listSbxOp.Parameters) != 0 {
		t.Errorf("expected no parameters for ListSandboxes, got %+v", listSbxOp.Parameters)
	}

	// 6. Verify GetAgentProfile uses /api/v1/agent-profiles/{id}
	if _, ok := parsed.Paths["/api/v1/agent-profiles/{id}"]["get"]; !ok {
		t.Errorf("expected GET /api/v1/agent-profiles/{id} operation")
	}

	// 7. Verify default error responses use text/plain
	if defaultResp, ok := listSbxOp.Responses["default"]; !ok {
		t.Errorf("expected default response on listSandboxes")
	} else if _, ok := defaultResp.Content["text/plain"]; !ok {
		t.Errorf("expected text/plain content in default error response, got %+v", defaultResp.Content)
	}

	// 8. Verify absence of ErrorModel schema and phantom $schema property
	if strings.Contains(string(specJSON), "ErrorModel") {
		t.Errorf("expected no ErrorModel schema in OpenAPI spec")
	}
	if strings.Contains(string(specJSON), `"$schema"`) {
		t.Errorf("expected no phantom $schema property in OpenAPI spec")
	}
}

type testUserGetter struct{}

func (testUserGetter) GetUser(ctx context.Context, userID string) (identity.User, error) {
	return identity.User{
		Principal: identity.Principal{
			ID:        userID,
			Namespace: "default",
			Role:      identity.RoleUser,
		},
	}, nil
}

type testAuthenticator struct{}

func (testAuthenticator) Authenticate(r *http.Request) (string, error) {
	return "default", nil
}

func (testAuthenticator) AuthenticatePrincipal(r *http.Request) (identity.Principal, error) {
	return identity.Principal{
		ID:        "usr_admin_0123456789abcdef",
		Namespace: "default",
		Role:      identity.RoleAdmin,
	}, nil
}

func TestOpenAPIRouteCoverageContract(t *testing.T) {
	specJSON, err := GenerateOpenAPISpec()
	if err != nil {
		t.Fatalf("GenerateOpenAPISpec failed: %v", err)
	}

	var parsed struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(specJSON, &parsed); err != nil {
		t.Fatalf("failed to parse spec: %v", err)
	}

	scheme := runtime.NewScheme()
	_ = sandboxesv1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	_ = k8sClient.Create(context.Background(), &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-test",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase: sandboxesv1alpha1.SandboxPhaseRunning,
		},
	})
	memStore := newMemTaskStore()
	credHandler := NewCredentialHandlers(nil, k8sClient, nil)
	credHandler.SetGitStore(&memoryGitCredentialStore{})

	server := NewServer(Config{
		Auth:          testAuthenticator{},
		Client:        k8sClient,
		TaskStore:     memStore,
		Credentials:   credHandler,
		IdentityStore: identity.NewStore(nil),
		UserGetter:    testUserGetter{},
	})

	adminCtx := WithPrincipal(context.Background(), identity.Principal{
		ID:        "usr_admin_0123456789abcdef",
		Namespace: "default",
		Role:      identity.RoleAdmin,
	})
	adminCtx = WithTenantNamespace(adminCtx, "default")

	replacer := strings.NewReplacer(
		"{name}", "sbx-test",
		"{id}", "usr_0123456789abcdef",
		"{sessionID}", "sess-test",
		"{service}", "openai",
		"{tokenID}", "tok_0123456789abcdef",
	)

	for path, methods := range parsed.Paths {
		for method, op := range methods {
			if method == "parameters" {
				continue
			}
			t.Run(op.OperationID, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						// A panic in a backend store (e.g. nil database pool) confirms the router successfully reached the handler.
					}
				}()

				resolvedPath := replacer.Replace(path)
				if resolvedPath == "/api/v1/git-credentials" {
					resolvedPath += "?repository=https%3A%2F%2Fgithub.com%2Ftest%2Frepo.git"
				} else if resolvedPath == "/api/v1/sandboxes/sbx-test/portforward" {
					resolvedPath += "?port=8080"
				}

				req := httptest.NewRequest(strings.ToUpper(method), resolvedPath, nil).WithContext(adminCtx)
				if resolvedPath == "/api/v1/tasks" && strings.ToUpper(method) == http.MethodPost {
					req.Header.Set("Idempotency-Key", "test-key-1")
				}
				rr := httptest.NewRecorder()

				server.router.ServeHTTP(rr, req)

				// Contract invariant: No documented operation should return 405 (Method not allowed).
				// 404 is only permitted if returned intentionally by application business logic, never from router miss.
				if rr.Code == http.StatusMethodNotAllowed {
					t.Errorf("documented operation %s (%s %s) returned 405 Method Not Allowed on server router", op.OperationID, method, path)
				}
				if rr.Code == http.StatusNotFound && strings.TrimSpace(rr.Body.String()) == "gateway route not found" {
					t.Errorf("documented operation %s (%s %s) returned 404 router miss on server router", op.OperationID, method, path)
				}
			})
		}
	}
}
