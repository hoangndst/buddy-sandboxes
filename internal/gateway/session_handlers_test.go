package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/sessions"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

type dummyMockRemoteExecutor struct {
	mu       sync.Mutex
	lastCmd  []string
	commands [][]string
	unblock  chan struct{}
	stopErr  error
	stopCode int
}

func (m *dummyMockRemoteExecutor) getCommands() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([][]string, len(m.commands))
	copy(res, m.commands)
	return res
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
	m.mu.Lock()
	m.lastCmd = cmd
	m.commands = append(m.commands, cmd)
	m.mu.Unlock()
	if stdout != nil {
		_, _ = stdout.Write([]byte("mock output\n"))
	}
	if len(cmd) > 1 && cmd[1] == "start" && m.unblock != nil {
		select {
		case <-ctx.Done():
			return 137, ctx.Err()
		case <-m.unblock:
			return 0, nil
		}
	}
	if len(cmd) > 1 && cmd[1] == "stop" {
		if m.stopErr != nil || m.stopCode != 0 {
			return m.stopCode, m.stopErr
		}
		if m.unblock != nil {
			select {
			case m.unblock <- struct{}{}:
			default:
			}
		}
	}
	return 0, nil
}

func TestDeleteSessionReportsStopFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{name: "transport", err: errors.New("disconnected")},
		{name: "runner", code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockExec := &dummyMockRemoteExecutor{stopCode: tc.code, stopErr: tc.err}
			server := NewServer(Config{
				Client:         fake.NewClientBuilder().WithScheme(setupTestScheme(t)).Build(),
				RemoteExecutor: mockExec,
			})
			handlers := NewSessionHandlers(server, nil, nil)
			handlers.memSessions["session"] = &sessions.Session{
				Namespace: "tenant", SandboxName: "sandbox", State: apiv1.SessionStateRunning,
			}
			req := httptest.NewRequest(http.MethodDelete, "/", nil)
			req = req.WithContext(WithTenantNamespace(req.Context(), "tenant"))
			resp := httptest.NewRecorder()
			handlers.DeleteSession(resp, req, "sandbox", "session")
			if resp.Code != http.StatusBadGateway || handlers.memSessions["session"].State != apiv1.SessionStateRunning {
				t.Fatalf("stop failure returned %d, stored state %s", resp.Code, handlers.memSessions["session"].State)
			}
		})
	}
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
	mockExec := &dummyMockRemoteExecutor{
		unblock: make(chan struct{}, 1),
	}

	server := NewServer(Config{
		Client:         fakeClient,
		RemoteExecutor: mockExec,
		Auth:           NewStaticTokenAuthenticator(map[string]string{"test-token": "tenant", "other-token": "other-tenant"}),
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

	// 4. Ownership validation on DELETE: mismatched sandbox name
	delMismatchReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/other-box/sessions/"+sessionResp.ID, nil).WithContext(tenantCtx)
	delMismatchReq.Header.Set("Authorization", "Bearer test-token")
	delMismatchRR := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(delMismatchRR, delMismatchReq)

	if delMismatchRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for sandbox mismatch on DELETE, got %d", delMismatchRR.Code)
	}

	// 5. Ownership validation on DELETE: mismatched tenant namespace
	otherTenantCtx := WithTenantNamespace(context.Background(), "other-tenant")
	delOtherTenantReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/test-box/sessions/"+sessionResp.ID, nil).WithContext(otherTenantCtx)
	delOtherTenantReq.Header.Set("Authorization", "Bearer other-token")
	delOtherTenantRR := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(delOtherTenantRR, delOtherTenantReq)

	if delOtherTenantRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for tenant mismatch on DELETE, got %d", delOtherTenantRR.Code)
	}

	// 6. DELETE /api/v1/sandboxes/test-box/sessions/:id (Delete)
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sandboxes/test-box/sessions/"+sessionResp.ID, nil).WithContext(tenantCtx)
	delReq.Header.Set("Authorization", "Bearer test-token")
	delRR := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(delRR, delReq)

	if delRR.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", delRR.Code, delRR.Body.String())
	}

	// Verify remoteExecutor was called to stop the process
	if len(mockExec.lastCmd) < 3 || mockExec.lastCmd[0] != "/opt/buddy/bin/buddy-runner" || mockExec.lastCmd[1] != "stop" || mockExec.lastCmd[2] != sessionResp.ID {
		t.Fatalf("expected lastCmd to be buddy-runner stop, got: %v", mockExec.lastCmd)
	}

	// 7. Create session with TTY: false explicitly passes --tty=false
	falseVal := false
	createNoTTYReq := apiv1.CreateSessionRequest{
		Command: []string{"echo", "notty"},
		TTY:     &falseVal,
	}
	bodyNoTTY, _ := json.Marshal(createNoTTYReq)
	reqNoTTY := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/test-box/sessions", bytes.NewReader(bodyNoTTY)).WithContext(tenantCtx)
	reqNoTTY.Header.Set("Authorization", "Bearer test-token")
	rrNoTTY := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(rrNoTTY, reqNoTTY)

	if rrNoTTY.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for non-tty session, got %d: %s", rrNoTTY.Code, rrNoTTY.Body.String())
	}

	// Check commands for --tty=false with brief poll for goroutine
	foundNoTTY := false
	for i := 0; i < 50; i++ {
		for _, cmd := range mockExec.getCommands() {
			for _, arg := range cmd {
				if arg == "--tty=false" {
					foundNoTTY = true
					break
				}
			}
			if foundNoTTY {
				break
			}
		}
		if foundNoTTY {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !foundNoTTY {
		t.Fatalf("expected to find --tty=false in start command, commands: %v", mockExec.getCommands())
	}
}
