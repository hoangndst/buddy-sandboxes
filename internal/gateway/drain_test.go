package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

type mockRemoteExecutor struct {
	mu           sync.Mutex
	executed     bool
	receivedCmd  []string
	simulatedErr error
	delay        time.Duration
}

func (m *mockRemoteExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	m.mu.Lock()
	m.executed = true
	m.receivedCmd = cmd
	delay := m.delay
	simErr := m.simulatedErr
	m.mu.Unlock()

	// Simulate command writing stdout
	_, _ = stdout.Write([]byte("mock output\n"))

	select {
	case <-ctx.Done():
		return 130, ctx.Err()
	case <-time.After(delay):
	}

	if simErr != nil {
		return 1, simErr
	}
	return 0, nil
}

func TestServerGracefulDrainProtocol(t *testing.T) {
	scheme := setupTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	tokens := map[string]string{"secret-token": "default"}
	auth := NewStaticTokenAuthenticator(tokens)

	cfg := Config{
		ListenAddr:       ":0",
		DrainTimeout:     100 * time.Millisecond,
		PropagationDelay: 10 * time.Millisecond,
		Auth:             auth,
		Client:           fakeClient,
	}

	server := NewServer(cfg)

	// Step 1: Assert /readyz is OK initially
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	server.readyzHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected /readyz 200 OK initially, got %d", rr.Code)
	}

	// Step 2: Assert /livez is OK
	req = httptest.NewRequest(http.MethodGet, "/livez", nil)
	rr = httptest.NewRecorder()
	server.livezHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected /livez 200 OK, got %d", rr.Code)
	}

	// Step 3: Trigger shutdown in background
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- server.Shutdown(context.Background())
	}()

	// Wait for Phase 1 to activate
	time.Sleep(5 * time.Millisecond)

	// Step 4: Assert /readyz immediately transitions to 503 during drain (DEC-004)
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr = httptest.NewRecorder()
	server.readyzHandler(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected /readyz 503 Service Unavailable during drain, got %d", rr.Code)
	}
	if retryAfter := rr.Header().Get("Retry-After"); retryAfter != "5" {
		t.Fatalf("expected Retry-After: 5 header on draining /readyz, got '%s'", retryAfter)
	}

	// Step 5: Assert /livez remains 200 during drain
	req = httptest.NewRequest(http.MethodGet, "/livez", nil)
	rr = httptest.NewRecorder()
	server.livezHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected /livez 200 OK during drain, got %d", rr.Code)
	}

	// Step 6: Wait for shutdown to complete
	err := <-shutdownDone
	if err != nil {
		t.Fatalf("unexpected shutdown error: %v", err)
	}
}

func TestWebSocketExecMultiplexing(t *testing.T) {
	scheme := setupTestScheme(t)

	// Pre-seed running sandbox
	runningSbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-running",
			Namespace: "default",
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image: "golang:1.25",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase:   sandboxesv1alpha1.SandboxPhaseRunning,
			PodName: "sbx-running-pod",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(runningSbx).Build()
	mockExec := &mockRemoteExecutor{delay: 10 * time.Millisecond}

	tokens := map[string]string{"valid-token": "default"}
	auth := NewStaticTokenAuthenticator(tokens)

	cfg := Config{
		ListenAddr:       ":0",
		DrainTimeout:     time.Second,
		PropagationDelay: 10 * time.Millisecond,
		Auth:             auth,
		Client:           fakeClient,
		RemoteExecutor:   mockExec,
	}

	server := NewServer(cfg)
	ts := httptest.NewServer(server.httpServer.Handler)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/sandboxes/sbx-running/exec?token=valid-token&cmd=ls"

	// Connect to WebSocket exec endpoint
	wsConn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial websocket exec: %v (status: %v)", err, resp.StatusCode)
	}
	defer wsConn.Close()

	// Send stdin data on Channel 0
	stdinPayload := append([]byte{apiv1.ChannelStdin}, []byte("echo hello\n")...)
	if err := wsConn.WriteMessage(websocket.BinaryMessage, stdinPayload); err != nil {
		t.Fatalf("failed to write stdin message: %v", err)
	}

	// Send resize data on Channel 4
	resizeJSON, _ := json.Marshal(apiv1.ResizeMessage{Cols: 120, Rows: 40})
	resizePayload := append([]byte{apiv1.ChannelResize}, resizeJSON...)
	if err := wsConn.WriteMessage(websocket.BinaryMessage, resizePayload); err != nil {
		t.Fatalf("failed to write resize message: %v", err)
	}

	// Read responses from server
	receivedStdout := false
	receivedExit := false
	var exitCode int

	for {
		_ = wsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		msgType, data, err := wsConn.ReadMessage()
		if err != nil {
			break
		}
		if msgType != websocket.BinaryMessage || len(data) == 0 {
			continue
		}

		channel := data[0]
		payload := data[1:]

		switch channel {
		case apiv1.ChannelStdout:
			if string(payload) == "mock output\n" {
				receivedStdout = true
			}
		case apiv1.ChannelExit:
			var exitMsg apiv1.ExitMessage
			if err := json.Unmarshal(payload, &exitMsg); err == nil {
				receivedExit = true
				exitCode = exitMsg.ExitCode
			}
		}
	}

	if !receivedStdout {
		t.Fatal("expected stdout message on Channel 1")
	}
	if !receivedExit || exitCode != 0 {
		t.Fatalf("expected exit code 0 on Channel 3, got exit=%v, code=%d", receivedExit, exitCode)
	}

	mockExec.mu.Lock()
	defer mockExec.mu.Unlock()
	if !mockExec.executed {
		t.Fatal("expected remote executor to be called")
	}
	if len(mockExec.receivedCmd) != 1 || mockExec.receivedCmd[0] != "ls" {
		t.Fatalf("unexpected cmd passed to executor: %v", mockExec.receivedCmd)
	}
}

func TestWebSocketExecRejectionDuringDrain(t *testing.T) {
	scheme := setupTestScheme(t)

	runningSbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-test",
			Namespace: "default",
		},
		Status: sandboxesv1alpha1.SandboxStatus{
			Phase: sandboxesv1alpha1.SandboxPhaseRunning,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(runningSbx).Build()
	tokens := map[string]string{"token": "default"}
	auth := NewStaticTokenAuthenticator(tokens)

	cfg := Config{
		ListenAddr:       ":0",
		DrainTimeout:     time.Second,
		PropagationDelay: 10 * time.Millisecond,
		Auth:             auth,
		Client:           fakeClient,
	}

	server := NewServer(cfg)
	ts := httptest.NewServer(server.httpServer.Handler)
	defer ts.Close()

	// Put server in draining mode
	server.isDraining.Store(true)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/sandboxes/sbx-test/exec?token=token"

	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("expected dial to fail when server is draining")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 Service Unavailable during drain, got: %v", resp)
	}
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "5" {
		t.Fatalf("expected Retry-After: 5 header on draining exec rejection, got '%s'", retryAfter)
	}
}
