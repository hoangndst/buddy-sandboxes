package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

func setupTestScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add clientgo scheme: %v", err)
	}
	if err := sandboxesv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add sandbox scheme: %v", err)
	}
	return s
}

func TestAuthMiddleware(t *testing.T) {
	tokens := map[string]string{
		"token-valid-a": "tenant-ns-a",
		"token-valid-b": "tenant-ns-b",
	}
	auth := NewStaticTokenAuthenticator(tokens)

	handlerCalled := false
	var capturedNamespace string
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		capturedNamespace, _ = TenantNamespaceFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	wrapped := AuthMiddleware(auth, dummyHandler)

	// Case 1: Missing token
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for missing token, got %d", rr.Code)
	}
	if handlerCalled {
		t.Fatal("handler should not have been called on unauthorized request")
	}

	// Case 2: Invalid token
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for invalid token, got %d", rr.Code)
	}

	// Case 3: Valid bearer header token
	handlerCalled = false
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer token-valid-a")
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for valid token, got %d", rr.Code)
	}
	if !handlerCalled || capturedNamespace != "tenant-ns-a" {
		t.Fatalf("expected namespace 'tenant-ns-a', got '%s'", capturedNamespace)
	}

	// Case 4: Valid query parameter token
	handlerCalled = false
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes?token=token-valid-b", nil)
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for query token, got %d", rr.Code)
	}
	if !handlerCalled || capturedNamespace != "tenant-ns-b" {
		t.Fatalf("expected namespace 'tenant-ns-b', got '%s'", capturedNamespace)
	}
}

func TestSandboxRESTLifecycle(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient)

	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")

	// 1. Test Create - missing image
	badBody, _ := json.Marshal(apiv1.CreateSandboxRequest{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(badBody)).WithContext(tenantCtx)
	rr := httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing image, got %d", rr.Code)
	}

	// 2. Test Create - valid request
	dur := int64(3600)
	createReq := apiv1.CreateSandboxRequest{
		Image:              "golang:1.25",
		Command:            []string{"/bin/sh", "-c"},
		Args:               []string{"sleep 3600"},
		WorkingDir:         "/workspace",
		MaxDurationSeconds: &dur,
		Resources: &apiv1.ResourceRequirements{
			Limits:   map[string]string{"cpu": "1", "memory": "2Gi"},
			Requests: map[string]string{"cpu": "500m", "memory": "1Gi"},
		},
		Env: []apiv1.EnvVar{
			{Name: "ENV", Value: "test"},
		},
	}
	validBody, _ := json.Marshal(createReq)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(validBody)).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rr.Code, rr.Body.String())
	}

	var createdResp apiv1.SandboxResponse
	if err := json.NewDecoder(rr.Body).Decode(&createdResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	if createdResp.ID == "" || createdResp.Namespace != "test-tenant" {
		t.Fatalf("unexpected created sandbox response: %+v", createdResp)
	}
	sandboxID := createdResp.ID

	// 3. Test List
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.ListSandboxesHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on list, got %d", rr.Code)
	}
	var listResp apiv1.ListSandboxesResponse
	if err := json.NewDecoder(rr.Body).Decode(&listResp); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Items) != 1 || listResp.Items[0].ID != sandboxID {
		t.Fatalf("unexpected list response: %+v", listResp)
	}

	// 4. Test Get existing
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/"+sandboxID, nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.GetSandboxHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on get, got %d", rr.Code)
	}

	// 5. Test Logs endpoint
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/"+sandboxID+"/logs", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.LogsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on logs, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), sandboxID) {
		t.Fatalf("expected logs to contain sandbox ID, got: %s", rr.Body.String())
	}

	// 6. Test Get non-existent
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sbx-missing", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.GetSandboxHandler(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found on missing sandbox, got %d", rr.Code)
	}

	// 7. Test Delete
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/"+sandboxID, nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.DeleteSandboxHandler(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content on delete, got %d", rr.Code)
	}

	// 8. Verify deletion via List
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.ListSandboxesHandler(rr, req)
	_ = json.NewDecoder(rr.Body).Decode(&listResp)
	if listResp.Total != 0 {
		t.Fatalf("expected 0 sandboxes after delete, got %d", listResp.Total)
	}
}

func TestSandboxNameAndLabelsValidation(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient)
	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")

	// 1. Valid custom name
	validReq := apiv1.CreateSandboxRequest{
		Name:  "my-custom-sbx-1",
		Image: "alpine:latest",
		Labels: map[string]string{
			"team": "core",
			"env":  "dev",
		},
	}
	body, _ := json.Marshal(validReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(body)).WithContext(tenantCtx)
	rr := httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for valid custom name, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp apiv1.SandboxResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Name != "my-custom-sbx-1" || resp.Labels["team"] != "core" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// 2. Duplicate custom name should return 409 Conflict
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(body)).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate name, got %d", rr.Code)
	}

	// 3. Invalid custom names (uppercase, bad symbols, leading dash)
	invalidNames := []string{"MySbx", "sbx_test", "-leading-dash", "trailing-dash-", strings.Repeat("a", 64)}
	for _, badName := range invalidNames {
		badReq := apiv1.CreateSandboxRequest{
			Name:  badName,
			Image: "alpine:latest",
		}
		b, _ := json.Marshal(badReq)
		req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(b)).WithContext(tenantCtx)
		rr = httptest.NewRecorder()
		handlers.CreateSandboxHandler(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for name %q, got %d", badName, rr.Code)
		}
	}

	// 4. Prohibited labels (starting with buddy.io/, kubernetes.io/, k8s.io/)
	prohibitedLabelKeys := []string{"buddy.io/role", "kubernetes.io/hostname", "k8s.io/custom"}
	for _, forbiddenKey := range prohibitedLabelKeys {
		badReq := apiv1.CreateSandboxRequest{
			Image: "alpine:latest",
			Labels: map[string]string{
				forbiddenKey: "malicious-value",
			},
		}
		b, _ := json.Marshal(badReq)
		req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(b)).WithContext(tenantCtx)
		rr = httptest.NewRecorder()
		handlers.CreateSandboxHandler(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for label key %q, got %d", forbiddenKey, rr.Code)
		}
	}
}

func TestPortForwardingTunnel(t *testing.T) {
	scheme := setupTestScheme(t)

	// Pre-create running sandbox
	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pf-sbx",
			Namespace: "test-tenant",
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image: "nginx:latest",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "test-pf-pod",
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sbx).Build()

	// Spin up a mock TCP echo server
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start mock TCP listener: %v", err)
	}
	defer tcpListener.Close()

	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	mockDialer := func(ctx context.Context, namespace, podName string, targetPort int) (net.Conn, error) {
		return net.Dial("tcp", tcpListener.Addr().String())
	}

	auth := NewStaticTokenAuthenticator(map[string]string{"test-token": "test-tenant"})
	server := NewServer(Config{
		Client: fakeClient,
		Auth:   auth,
	})
	server.SetPortForwardDialer(mockDialer)

	ts := httptest.NewServer(server.httpServer.Handler)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/sandboxes/test-pf-sbx/portforward?port=8080"
	headers := http.Header{"Authorization": []string{"Bearer test-token"}}

	wsConn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("failed to dial portforward websocket: %v (status %d)", err, resp.StatusCode)
	}
	defer wsConn.Close()

	// Send test payload
	testPayload := []byte("hello-port-forwarding-stream")
	if err := wsConn.WriteMessage(websocket.BinaryMessage, testPayload); err != nil {
		t.Fatalf("failed to write binary message: %v", err)
	}

	_ = wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	msgType, data, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read echoed binary message: %v", err)
	}
	if msgType != websocket.BinaryMessage || string(data) != string(testPayload) {
		t.Fatalf("expected binary echo %q, got msgType=%d %q", string(testPayload), msgType, string(data))
	}
}
