package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

type dummyMockRemoteExecutor struct {
	lastCmd []string
}

func (m *dummyMockRemoteExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	m.lastCmd = cmd
	if stdout != nil {
		_, _ = stdout.Write([]byte("mock output\n"))
	}
	return 0, nil
}

func TestSessionRESTEndpoints(t *testing.T) {
	s := setupTestScheme(t)
	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-box",
			Namespace: "tenant",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "test-box-pod",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(sbx).Build()
	mockExec := &dummyMockRemoteExecutor{}

	server := NewServer(Config{
		Client:         fakeClient,
		RemoteExecutor: mockExec,
		Auth:           NewStaticTokenAuthenticator(map[string]string{"test-token": "tenant"}),
	})

	tenantCtx := WithTenantNamespace(context.Background(), "tenant")

	// 1. POST /api/v1/sandboxes/test-box/sessions (Create)
	createReq := apiv1.CreateSessionRequest{
		Command:    []string{"echo", "hello"},
		WorkingDir: "/app",
	}
	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/test-box/sessions", bytes.NewReader(body)).WithContext(tenantCtx)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rr.Code, rr.Body.String())
	}

	var sessionResp apiv1.SessionResponse
	if err := json.NewDecoder(rr.Body).Decode(&sessionResp); err != nil {
		t.Fatalf("failed decoding session response: %v", err)
	}
	if sessionResp.ID == "" || sessionResp.SandboxName != "test-box" {
		t.Fatalf("unexpected session response: %+v", sessionResp)
	}

	// 2. GET /api/v1/sandboxes/test-box/sessions (List)
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/test-box/sessions", nil).WithContext(tenantCtx)
	listReq.Header.Set("Authorization", "Bearer test-token")
	listRR := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(listRR, listReq)

	if listRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", listRR.Code, listRR.Body.String())
	}
	var listResp apiv1.ListSessionsResponse
	if err := json.NewDecoder(listRR.Body).Decode(&listResp); err != nil {
		t.Fatalf("failed decoding list response: %v", err)
	}
	if len(listResp.Sessions) != 1 || listResp.Sessions[0].ID != sessionResp.ID {
		t.Fatalf("unexpected list items: %+v", listResp.Sessions)
	}

	// 3. GET /api/v1/sandboxes/test-box/sessions/:id (Get)
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/test-box/sessions/"+sessionResp.ID, nil).WithContext(tenantCtx)
	getReq.Header.Set("Authorization", "Bearer test-token")
	getRR := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", getRR.Code, getRR.Body.String())
	}

	// 4. DELETE /api/v1/sandboxes/test-box/sessions/:id (Delete)
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/test-box/sessions/"+sessionResp.ID, nil).WithContext(tenantCtx)
	delReq.Header.Set("Authorization", "Bearer test-token")
	delRR := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(delRR, delReq)

	if delRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", delRR.Code, delRR.Body.String())
	}
}
