package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
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

	// Case 4: Query parameter token is rejected (hardened)
	handlerCalled = false
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes?token=token-valid-b", nil)
	rr = httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for query token, got %d", rr.Code)
	}
	if handlerCalled {
		t.Fatal("handler should not have been called on query token request")
	}
}

func TestSandboxRESTLifecycle(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)

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
	const testRuntimeClass = "gvisor"
	dur := int64(3600)
	runtimeClass := testRuntimeClass
	createReq := apiv1.CreateSandboxRequest{
		Image:                 "golang:1.25",
		Command:               []string{"/bin/sh", "-c"},
		Args:                  []string{"sleep 3600"},
		WorkingDir:            "/workspace",
		RuntimeClassName:      &runtimeClass,
		MaxDurationSeconds:    &dur,
		ShareProcessNamespace: func() *bool { value := true; return &value }(),
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
	if createdResp.RuntimeClassName == nil || *createdResp.RuntimeClassName != testRuntimeClass {
		t.Fatalf("expected RuntimeClassName '%s', got %v", testRuntimeClass, createdResp.RuntimeClassName)
	}
	if !createdResp.ShareProcessNamespace {
		t.Fatal("expected opt-in process namespace policy in create response")
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
	if !listResp.Items[0].ShareProcessNamespace {
		t.Fatal("expected opt-in process namespace policy in list response")
	}

	// 4. Test Get existing
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/"+sandboxID, nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.GetSandboxHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on get, got %d", rr.Code)
	}
	var getResp apiv1.SandboxResponse
	if err := json.NewDecoder(rr.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode get response: %v", err)
	}
	if getResp.RuntimeClassName == nil || *getResp.RuntimeClassName != testRuntimeClass {
		t.Fatalf("expected get response RuntimeClassName '%s', got %v", testRuntimeClass, getResp.RuntimeClassName)
	}
	if !getResp.ShareProcessNamespace {
		t.Fatal("expected opt-in process namespace policy in get response")
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

func TestSandboxRESTRejectsInvalidDurationAndReservedEnvironment(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)
	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")

	zero := int64(0)
	negative := int64(-1)
	tooLarge := sandboxesv1alpha1.MaxActiveDeadlineSeconds + 1
	requests := []apiv1.CreateSandboxRequest{
		{Image: "alpine:latest", MaxDurationSeconds: &zero},
		{Image: "alpine:latest", MaxDurationSeconds: &negative},
		{Image: "alpine:latest", MaxDurationSeconds: &tooLarge},
		{Image: "alpine:latest", Env: []apiv1.EnvVar{{Name: sandboxesv1alpha1.AgentEnvPodName, Value: "spoof"}}},
	}
	for _, payload := range requests {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(body)).WithContext(tenantCtx)
		rr := httptest.NewRecorder()
		handlers.CreateSandboxHandler(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid request %+v, got %d: %s", payload, rr.Code, rr.Body.String())
		}
	}
	var sandboxes sandboxesv1alpha1.SandboxList
	if err := fakeClient.List(context.Background(), &sandboxes); err != nil {
		t.Fatal(err)
	}
	if len(sandboxes.Items) != 0 {
		t.Fatalf("invalid requests created Sandbox objects: %d", len(sandboxes.Items))
	}
}

func TestSandboxNameAndLabelsValidation(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)
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
	if resp.ShareProcessNamespace {
		t.Fatal("omitted process namespace setting should default to false")
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
	defer func() { _ = tcpListener.Close() }()

	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
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
	defer func() { _ = wsConn.Close() }()

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

func TestKubeTerminalSizeQueue(t *testing.T) {
	resizeChan := make(chan apiv1.ResizeMessage, 2)
	q := &kubeTerminalSizeQueue{resizeChan: resizeChan}

	// 1. Cols/Rows
	resizeChan <- apiv1.ResizeMessage{Cols: 80, Rows: 24}
	size := q.Next()
	if size == nil || size.Width != 80 || size.Height != 24 {
		t.Fatalf("expected 80x24, got %+v", size)
	}

	// 2. Width/Height
	resizeChan <- apiv1.ResizeMessage{Width: 120, Height: 40}
	size = q.Next()
	if size == nil || size.Width != 120 || size.Height != 40 {
		t.Fatalf("expected 120x40, got %+v", size)
	}

	// 3. Closed channel
	close(resizeChan)
	if size = q.Next(); size != nil {
		t.Fatalf("expected nil size on closed channel, got %+v", size)
	}
}

func TestWrappedPortForwardConn(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()

	stopChan := make(chan struct{})
	wrapped := &wrappedPortForwardConn{
		Conn:     c1,
		stopChan: stopChan,
	}

	if err := wrapped.Close(); err != nil {
		t.Fatalf("wrapped.Close() failed: %v", err)
	}

	select {
	case <-stopChan:
	default:
		t.Fatal("stopChan was not closed by wrapped.Close()")
	}

	// Idempotent second close
	if err := wrapped.Close(); err != nil {
		t.Fatalf("second wrapped.Close() failed: %v", err)
	}
}

func TestHandlersErrorBranches(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)
	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")
	noTenantCtx := context.Background()

	// 1. Method Not Allowed checks
	req := httptest.NewRequest(http.MethodPut, "/api/v1/sandboxes", nil).WithContext(tenantCtx)
	rr := httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.ListSandboxesHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/test-1", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.GetSandboxHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/test-1", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.DeleteSandboxHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/test-1/logs", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.LogsHandler(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}

	// 2. Missing tenant context checks
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", nil).WithContext(noTenantCtx)
	rr = httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil).WithContext(noTenantCtx)
	rr = httptest.NewRecorder()
	handlers.ListSandboxesHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/test-1", nil).WithContext(noTenantCtx)
	rr = httptest.NewRecorder()
	handlers.GetSandboxHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/test-1", nil).WithContext(noTenantCtx)
	rr = httptest.NewRecorder()
	handlers.DeleteSandboxHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/test-1/logs", nil).WithContext(noTenantCtx)
	rr = httptest.NewRecorder()
	handlers.LogsHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}

	// 3. Delete missing sandbox
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/missing-delete-id", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.DeleteSandboxHandler(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestPortForwardValidationBranches(t *testing.T) {
	scheme := setupTestScheme(t)
	pendingSbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-sbx", Namespace: "test-tenant"},
		Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine"},
		Status:     sandboxesv1alpha1.SandboxStatus{Phase: sandboxesv1alpha1.SandboxPhasePending},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pendingSbx).Build()

	server := NewServer(Config{Client: fakeClient})
	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")
	noTenantCtx := context.Background()

	// Missing tenant
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/pending-sbx/portforward?port=8080", nil).WithContext(noTenantCtx)
	rr := httptest.NewRecorder()
	server.wsHandler.PortForwardHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}

	// Missing port query
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/pending-sbx/portforward", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	server.wsHandler.PortForwardHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}

	// Invalid port query
	badPorts := []string{"abc", "0", "70000", "-5"}
	for _, bp := range badPorts {
		req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/pending-sbx/portforward?port="+bp, nil).WithContext(tenantCtx)
		rr = httptest.NewRecorder()
		server.wsHandler.PortForwardHandler(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for port %q, got %d", bp, rr.Code)
		}
	}

	// Missing sandbox
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/missing-sbx/portforward?port=8080", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	server.wsHandler.PortForwardHandler(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}

	// Sandbox not in Running phase (pending)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/pending-sbx/portforward?port=8080", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	server.wsHandler.PortForwardHandler(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict, got %d", rr.Code)
	}
}

func TestChannelWriter(t *testing.T) {
	var capturedChannel byte
	var capturedData []byte
	sendFunc := func(channel byte, data []byte) error {
		capturedChannel = channel
		capturedData = data
		return nil
	}

	w := &channelWriter{channel: apiv1.ChannelStdout, send: sendFunc}

	// 1. Empty write
	n, err := w.Write([]byte{})
	if n != 0 || err != nil {
		t.Fatalf("unexpected result on empty write: n=%d, err=%v", n, err)
	}

	// 2. Data write
	n, err = w.Write([]byte("hello stdout"))
	if n != len("hello stdout") || err != nil || capturedChannel != apiv1.ChannelStdout || string(capturedData) != "hello stdout" {
		t.Fatalf("unexpected write result: n=%d, channel=%d, data=%q", n, capturedChannel, string(capturedData))
	}
}

func TestServerHealthEndpoints(t *testing.T) {
	auth := NewStaticTokenAuthenticator(map[string]string{"test-token": "test-tenant"})
	server := NewServer(Config{Auth: auth})

	// 1. livez
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rr := httptest.NewRecorder()
	server.livezHandler(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("unexpected livez response: code=%d body=%q", rr.Code, rr.Body.String())
	}

	// 2. readyz healthy
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr = httptest.NewRecorder()
	server.readyzHandler(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Fatalf("unexpected readyz response: code=%d body=%q", rr.Code, rr.Body.String())
	}

	// 3. extractIDFromPath
	if id := extractIDFromPath("/api/v1/sandboxes/my-sbx-id"); id != "my-sbx-id" {
		t.Fatalf("expected 'my-sbx-id', got %q", id)
	}
	if id := extractIDFromPath("/api/v1/sandboxes/my-sbx-id/exec"); id != "my-sbx-id" {
		t.Fatalf("expected 'my-sbx-id', got %q", id)
	}
	if id := extractIDFromPath(""); id != "" {
		t.Fatalf("expected empty id, got %q", id)
	}

	// 4. Server router status codes
	mux := server.httpServer.Handler

	// Method not allowed on /api/v1/sandboxes
	r1 := httptest.NewRequest(http.MethodPut, "/api/v1/sandboxes", nil)
	r1.Header.Set("Authorization", "Bearer test-token")
	w1 := httptest.NewRecorder()
	mux.ServeHTTP(w1, r1)
	if w1.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w1.Code)
	}

	// Method not allowed on /api/v1/sandboxes/id
	r2 := httptest.NewRequest(http.MethodPut, "/api/v1/sandboxes/test-id", nil)
	r2.Header.Set("Authorization", "Bearer test-token")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w2.Code)
	}

	// 404 on unrecognized subpath
	r3 := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/test-id/unknown", nil)
	r3.Header.Set("Authorization", "Bearer test-token")
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, r3)
	if w3.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w3.Code)
	}

	// 404 on unrecognized api route
	r4 := httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	r4.Header.Set("Authorization", "Bearer test-token")
	w4 := httptest.NewRecorder()
	mux.ServeHTTP(w4, r4)
	if w4.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w4.Code)
	}
}

type mockStreamExecutor struct {
	exitCode int
	err      error
	cmd      []string
	stdin    []byte
}

func (m *mockStreamExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	m.cmd = cmd
	if stdin != nil {
		m.stdin, _ = io.ReadAll(stdin)
	}
	if stdout != nil {
		_, _ = stdout.Write([]byte("mock-tar-archive-bytes"))
	}
	return m.exitCode, m.err
}

func TestWhoAmIHandler(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)

	// Unauthorized (no tenant namespace)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/whoami", nil)
	rr := httptest.NewRecorder()
	handlers.WhoAmIHandler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rr.Code)
	}

	// Authorized
	tenantCtx := WithTenantNamespace(context.Background(), "my-tenant")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/whoami", nil).WithContext(tenantCtx)
	rr = httptest.NewRecorder()
	handlers.WhoAmIHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}

	var resp apiv1.WhoAmIResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode WhoAmIResponse: %v", err)
	}
	if resp.Namespace != "my-tenant" {
		t.Errorf("expected namespace 'my-tenant', got %q", resp.Namespace)
	}
	if resp.User != "developer" {
		t.Errorf("expected user 'developer', got %q", resp.User)
	}
}

func TestUploadAndDownloadHandlers(t *testing.T) {
	scheme := setupTestScheme(t)
	mockExec := &mockStreamExecutor{}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, mockExec)

	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")

	// Create running sandbox custom resource
	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-sync-1",
			Namespace: "test-tenant",
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image: "alpine:latest",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "sbx-sync-1-pod",
		},
	}
	if err := fakeClient.Create(tenantCtx, sbx); err != nil {
		t.Fatalf("failed to create sandbox: %v", err)
	}

	// 1. Upload test
	uploadBody := bytes.NewReader([]byte("tar-payload"))
	uploadReq := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/sbx-sync-1/upload?path=/workspace/project", uploadBody).WithContext(tenantCtx)
	uploadRR := httptest.NewRecorder()
	handlers.UploadHandler(uploadRR, uploadReq)
	if uploadRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on upload, got %d: %s", uploadRR.Code, uploadRR.Body.String())
	}
	if string(mockExec.stdin) != "tar-payload" {
		t.Errorf("expected stdin 'tar-payload', got %q", string(mockExec.stdin))
	}
	fileUploadReq := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/sbx-sync-1/upload?path=/workspace/renamed.json&file=renamed.json", bytes.NewReader([]byte("tar-file"))).WithContext(tenantCtx)
	fileUploadRR := httptest.NewRecorder()
	handlers.UploadHandler(fileUploadRR, fileUploadReq)
	if fileUploadRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on file upload, got %d: %s", fileUploadRR.Code, fileUploadRR.Body.String())
	}
	if !strings.Contains(strings.Join(mockExec.cmd, " "), `mv "$TMP/$NAME" "$DEST"`) {
		t.Errorf("expected file upload command to place archive entry at exact destination, got %v", mockExec.cmd)
	}
	if len(mockExec.cmd) < 3 || !strings.HasPrefix(mockExec.cmd[2], "set -e\n") {
		t.Errorf("expected file upload extraction script to stop on tar errors, got %v", mockExec.cmd)
	}
	directoryUploadReq := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/sbx-sync-1/upload?path=/workspace/project&directory=project", bytes.NewReader([]byte("tar-directory"))).WithContext(tenantCtx)
	directoryUploadRR := httptest.NewRecorder()
	handlers.UploadHandler(directoryUploadRR, directoryUploadReq)
	if directoryUploadRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on directory upload, got %d: %s", directoryUploadRR.Code, directoryUploadRR.Body.String())
	}
	if !strings.Contains(strings.Join(mockExec.cmd, " "), `mv "$TMP/$NAME" "$DEST"`) {
		t.Errorf("expected directory upload command to place archive root at exact destination, got %v", mockExec.cmd)
	}

	// 2. Download test
	downloadReq := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sbx-sync-1/download?path=/workspace/dist.tar.gz", nil).WithContext(tenantCtx)
	downloadRR := httptest.NewRecorder()
	handlers.DownloadHandler(downloadRR, downloadReq)
	if downloadRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on download, got %d", downloadRR.Code)
	}
	if downloadRR.Body.String() != "mock-tar-archive-bytes" {
		t.Errorf("expected download content 'mock-tar-archive-bytes', got %q", downloadRR.Body.String())
	}
	trailingSlashReq := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sbx-sync-1/download?path=%2Fworkspace%2Fproject%2F", nil).WithContext(tenantCtx)
	trailingSlashRR := httptest.NewRecorder()
	handlers.DownloadHandler(trailingSlashRR, trailingSlashReq)
	if trailingSlashRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for trailing-slash directory download, got %d", trailingSlashRR.Code)
	}
	if !strings.Contains(strings.Join(mockExec.cmd, " "), "-C /workspace project") {
		t.Errorf("expected normalized directory download command, got %v", mockExec.cmd)
	}

	mockExec.err = errors.New("remote pod exec failed")
	failingReq := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sbx-sync-1/download?path=/workspace/missing", nil).WithContext(tenantCtx)
	failingRR := httptest.NewRecorder()
	handlers.DownloadHandler(failingRR, failingReq)
	if got := failingRR.Result().Trailer.Get("X-Buddy-Stream-Error"); got == "" {
		t.Fatal("expected download failure to be reported in the response trailer")
	}
}

func TestCreateSandboxWithWorkspaceAndGit(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)
	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")

	createReq := apiv1.CreateSandboxRequest{
		Name:  "sbx-workspace-test",
		Image: "alpine:latest",
		Workspace: &apiv1.WorkspaceSpec{
			Ephemeral: false,
			MountPath: "/workspace",
			Git: &apiv1.GitRepositorySpec{
				URL:       "https://github.com/example/repo.git",
				Revision:  "feat/new-ui",
				Depth:     1,
				SecretRef: "my-git-creds",
			},
			PersistentVolumeClaim: &apiv1.PersistentVolumeClaimWorkspace{
				ClaimName: "custom-pvc",
				ReadOnly:  true,
			},
		},
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(body)).WithContext(tenantCtx)
	rr := httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp apiv1.SandboxResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode SandboxResponse: %v", err)
	}
	if resp.Workspace == nil {
		t.Fatalf("expected Workspace in response")
	}
	if resp.Workspace.Git == nil || resp.Workspace.Git.URL != "https://github.com/example/repo.git" {
		t.Errorf("expected Git URL in response")
	}
	if resp.Workspace.Git.SecretRef != "my-git-creds" {
		t.Errorf("expected SecretRef 'my-git-creds', got %q", resp.Workspace.Git.SecretRef)
	}
	if resp.Workspace.PersistentVolumeClaim == nil || resp.Workspace.PersistentVolumeClaim.ClaimName != "custom-pvc" {
		t.Errorf("expected ClaimName 'custom-pvc'")
	}
	if !resp.Workspace.PersistentVolumeClaim.ReadOnly {
		t.Errorf("expected ReadOnly true")
	}
}

func TestCreateSandboxWithMCPServers(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handlers := NewHandlers(fakeClient, nil, nil)

	tenantCtx := WithTenantNamespace(context.Background(), "test-tenant")

	createReq := apiv1.CreateSandboxRequest{
		Name:  "sbx-mcp",
		Image: "golang:1.24",
		MCPServers: []apiv1.MCPServerSpec{
			{
				Name:                   "filesystem",
				Image:                  "mcp/filesystem:v1",
				Port:                   8080,
				MountWorkspace:         true,
				MountWorkspaceReadOnly: true,
				Env: []apiv1.EnvVar{
					{Name: "DEBUG", Value: "1"},
				},
				Resources: &apiv1.ResourceRequirements{
					Limits:   map[string]string{"cpu": "500m", "memory": "256Mi"},
					Requests: map[string]string{"cpu": "100m", "memory": "64Mi"},
				},
			},
			{
				Name:  "db",
				Image: "redis:7-alpine",
				Port:  6379,
			},
		},
	}

	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", bytes.NewReader(body)).WithContext(tenantCtx)
	rr := httptest.NewRecorder()
	handlers.CreateSandboxHandler(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp apiv1.SandboxResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode SandboxResponse: %v", err)
	}

	if len(resp.MCPServers) != 2 {
		t.Fatalf("expected 2 MCPServers in response, got %d", len(resp.MCPServers))
	}
	if resp.MCPServers[0].Name != "filesystem" || resp.MCPServers[0].Port != 8080 || !resp.MCPServers[0].MountWorkspace || !resp.MCPServers[0].MountWorkspaceReadOnly {
		t.Errorf("unexpected MCPServer[0]: %+v", resp.MCPServers[0])
	}
	if resp.MCPServers[1].Name != "db" || resp.MCPServers[1].Port != 6379 {
		t.Errorf("unexpected MCPServer[1]: %+v", resp.MCPServers[1])
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "test-tenant", Name: "sbx-mcp"}, &sbx); err != nil {
		t.Fatalf("failed to get created Sandbox from client: %v", err)
	}
	if len(sbx.Spec.MCPServers) != 2 {
		t.Fatalf("expected 2 MCPServers in CRD, got %d", len(sbx.Spec.MCPServers))
	}
	if sbx.Spec.MCPServers[0].Name != "filesystem" || sbx.Spec.MCPServers[0].Port != 8080 {
		t.Errorf("unexpected CRD MCPServer[0]: %+v", sbx.Spec.MCPServers[0])
	}
	if len(sbx.Spec.MCPServers[0].Env) != 1 || sbx.Spec.MCPServers[0].Env[0].Name != "DEBUG" {
		t.Errorf("expected Env DEBUG=1 in CRD")
	}
	if sbx.Spec.MCPServers[0].Resources.Limits.Cpu().String() != "500m" {
		t.Errorf("expected CPU limit 500m, got %s", sbx.Spec.MCPServers[0].Resources.Limits.Cpu().String())
	}
}
