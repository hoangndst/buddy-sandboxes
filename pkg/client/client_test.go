package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

var testUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func TestCredentialRequestsRequireSecureEndpointAndDoNotFollowRedirects(t *testing.T) {
	var firstCalls, redirectCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "redirect-target") {
			redirectCalls++
			return
		}
		firstCalls++
		w.Header().Set("Location", "/redirect-target")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, Token: "cli-token"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SetCredential(context.Background(), "openai", "", "sk-test")
	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Fatalf("registration redirect should return status error, got %v", err)
	}
	if firstCalls != 1 || redirectCalls != 0 {
		t.Fatalf("redirect calls: first=%d target=%d", firstCalls, redirectCalls)
	}

	_, err = NewClient(Config{Endpoint: "http://gateway.example.test", Token: "cli-token"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain HTTP non-loopback endpoint should be rejected at client creation, got %v", err)
	}
}

func TestNewClientRequiresHTTPSForRemoteEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"http://gateway.example.test",
		"http://localhost.example.test",
		"ftp://gateway.example.test",
		"https://gateway.example.test/api",
	} {
		if _, err := NewClient(Config{Endpoint: endpoint}); err == nil {
			t.Errorf("NewClient(%q) unexpectedly succeeded", endpoint)
		}
	}
	allowed := []string{
		"https://gateway.example.test",
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	}
	for _, endpoint := range allowed {
		if _, err := NewClient(Config{Endpoint: endpoint}); err != nil {
			t.Errorf("NewClient(%q) failed: %v", endpoint, err)
		}
	}
}

const testSandboxID = "test-cli-sbx"

func TestClientRESTLifecycle(t *testing.T) {
	// Mock REST server
	mux := http.NewServeMux()
	sandboxes := make(map[string]apiv1.SandboxResponse)

	mux.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req apiv1.CreateSandboxRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			name := req.Name
			if name == "" {
				name = "sbx-auto-1"
			}
			resp := apiv1.SandboxResponse{
				ID:        name,
				Name:      name,
				Namespace: "default",
				Image:     req.Image,
				Phase:     "Running",
				CreatedAt: time.Now(),
			}
			sandboxes[name] = resp
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(resp)
		case http.MethodGet:
			items := make([]apiv1.SandboxResponse, 0, len(sandboxes))
			for _, s := range sandboxes {
				items = append(items, s)
			}
			_ = json.NewEncoder(w).Encode(apiv1.ListSandboxesResponse{Items: items, Total: len(items)})
		}
	})

	mux.HandleFunc("/api/v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/sandboxes/")
		if strings.HasSuffix(id, "/logs") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("mock container log line 1\nline 2\n"))
			return
		}

		switch r.Method {
		case http.MethodGet:
			s, ok := sandboxes[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(s)
		case http.MethodDelete:
			delete(sandboxes, id)
			w.WriteHeader(http.StatusNoContent)
		}
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	c, err := NewClient(Config{
		Endpoint: ts.URL,
		Token:    "test-token",
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Create sandbox
	created, err := c.CreateSandbox(ctx, &apiv1.CreateSandboxRequest{
		Name:  testSandboxID,
		Image: "golang:1.25",
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}
	if created.Name != testSandboxID || created.Phase != "Running" {
		t.Fatalf("unexpected sandbox: %+v", created)
	}

	// 2. Get sandbox
	got, err := c.GetSandbox(ctx, testSandboxID)
	if err != nil {
		t.Fatalf("GetSandbox failed: %v", err)
	}
	if got.ID != testSandboxID {
		t.Fatalf("unexpected got ID: %s", got.ID)
	}

	// 3. List sandboxes
	list, err := c.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes failed: %v", err)
	}
	if list.Total != 1 || list.Items[0].Name != testSandboxID {
		t.Fatalf("unexpected list: %+v", list)
	}

	// 4. Stream logs
	var logBuf bytes.Buffer
	err = c.StreamLogs(ctx, testSandboxID, LogOptions{Follow: false}, &logBuf)
	if err != nil {
		t.Fatalf("StreamLogs failed: %v", err)
	}
	if !strings.Contains(logBuf.String(), "mock container log line") {
		t.Fatalf("unexpected logs: %s", logBuf.String())
	}

	// 5. Delete sandbox
	if err := c.DeleteSandbox(ctx, testSandboxID); err != nil {
		t.Fatalf("DeleteSandbox failed: %v", err)
	}

	// 6. Verify deleted
	_, err = c.GetSandbox(ctx, testSandboxID)
	if err == nil {
		t.Fatal("expected 404 after delete, got nil error")
	}
}

func TestClientSuspendAndResume(t *testing.T) {
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes/workspace/suspend", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "workspace", Suspend: true, SuspensionPhase: "Suspending"})
	})
	mux.HandleFunc("/api/v1/sandboxes/workspace/resume", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(apiv1.SandboxResponse{ID: "workspace", Suspend: false, SuspensionPhase: "Resuming"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c, err := NewClient(Config{Endpoint: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SuspendSandbox(context.Background(), "workspace"); err != nil {
		t.Fatalf("SuspendSandbox failed: %v", err)
	}
	if _, err := c.ResumeSandbox(context.Background(), "workspace"); err != nil {
		t.Fatalf("ResumeSandbox failed: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/api/v1/sandboxes/workspace/suspend" ||
		paths[1] != "/api/v1/sandboxes/workspace/resume" {
		t.Fatalf("unexpected operation paths: %v", paths)
	}
}

func TestClientExecAndResize(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes/test-exec/exec", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage && len(data) > 0 {
				channel := data[0]
				payload := data[1:]

				switch channel {
				case apiv1.ChannelStdin:
					// Echo back on Channel 1 (Stdout)
					resp := append([]byte{apiv1.ChannelStdout}, payload...)
					_ = conn.WriteMessage(websocket.BinaryMessage, resp)

					// Send Channel 3 exit code 0
					exitData, _ := json.Marshal(apiv1.ExitMessage{ExitCode: 0})
					_ = conn.WriteMessage(websocket.BinaryMessage, append([]byte{apiv1.ChannelExit}, exitData...))
					time.Sleep(20 * time.Millisecond)
					return
				case apiv1.ChannelResize:
					// Acknowledged
				}
			}
		}
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	c, err := NewClient(Config{Endpoint: ts.URL})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	stdin := strings.NewReader("echo hello\n")
	var stdout bytes.Buffer
	resizeChan := make(chan apiv1.ResizeMessage, 1)
	resizeChan <- apiv1.ResizeMessage{Cols: 80, Rows: 24}
	close(resizeChan)

	exitCode, err := c.Exec(context.Background(), "test-exec", ExecOptions{
		Cmd:        []string{"bash"},
		TTY:        true,
		Stdin:      stdin,
		Stdout:     &stdout,
		ResizeChan: resizeChan,
	})

	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if !strings.Contains(stdout.String(), "echo hello") {
		t.Fatalf("expected echoed stdout, got: %q", stdout.String())
	}

	// Test Nil Stdin sending EOF
	var nilStdout bytes.Buffer
	exitCode, err = c.Exec(context.Background(), "test-exec", ExecOptions{
		Cmd:    []string{"bash"},
		Stdout: &nilStdout,
	})
	if err != nil {
		t.Fatalf("Exec with nil stdin failed: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0 for nil stdin, got %d", exitCode)
	}

	// Test Failing Writer propagates error
	_, err = c.Exec(context.Background(), "test-exec", ExecOptions{
		Cmd:    []string{"bash"},
		Stdin:  strings.NewReader("hello\n"),
		Stdout: &failingWriter{},
	})
	if err == nil || !strings.Contains(err.Error(), "simulated disk full") {
		t.Fatalf("expected error containing 'simulated disk full', got: %v", err)
	}
}

type failingWriter struct{}

func (f *failingWriter) Write(p []byte) (n int, err error) {
	return 0, fmt.Errorf("simulated disk full")
}

func TestClientPortForward(t *testing.T) {
	// 1. Mock TCP echo server (simulating pod process)
	tcpBackend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend tcp listener: %v", err)
	}
	defer func() { _ = tcpBackend.Close() }()

	go func() {
		for {
			conn, err := tcpBackend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	// 2. Mock Gateway Server WebSocket /portforward endpoint
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes/test-pf/portforward", func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		backendConn, err := net.Dial("tcp", tcpBackend.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = backendConn.Close() }()

		done := make(chan struct{})

		// WebSocket -> TCP
		go func() {
			for {
				msgType, data, err := wsConn.ReadMessage()
				if err != nil {
					break
				}
				if msgType == websocket.BinaryMessage {
					_, _ = backendConn.Write(data)
				}
			}
			close(done)
		}()

		// TCP -> WebSocket
		buf := make([]byte, 1024)
		for {
			n, err := backendConn.Read(buf)
			if n > 0 {
				_ = wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
		<-done
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	c, err := NewClient(Config{Endpoint: ts.URL})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 3. Find free local port
	testLocalListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind port: %v", err)
	}
	localPort := testLocalListener.Addr().(*net.TCPAddr).Port
	_ = testLocalListener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyChan := make(chan struct{})
	go func() {
		_ = c.PortForward(ctx, "test-pf", PortForwardOptions{
			LocalPort:  localPort,
			RemotePort: 8080,
			ReadyChan:  readyChan,
		})
	}()

	select {
	case <-readyChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for port forward listener")
	}

	// 4. Connect to local port and assert echo round-trip
	localConn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("failed to dial local port forward: %v", err)
	}
	defer func() { _ = localConn.Close() }()

	payload := []byte("ping-pong-roundtrip")
	if _, err := localConn.Write(payload); err != nil {
		t.Fatalf("failed to write to local port: %v", err)
	}

	recvBuf := make([]byte, len(payload))
	_ = localConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(localConn, recvBuf); err != nil {
		t.Fatalf("failed to read from local port: %v", err)
	}

	if string(recvBuf) != string(payload) {
		t.Fatalf("expected echoed %q, got %q", string(payload), string(recvBuf))
	}
}

func TestClientWSDialerTLSInheritance(t *testing.T) {
	customTLS := &tls.Config{InsecureSkipVerify: true}
	transport := &http.Transport{
		TLSClientConfig: customTLS,
	}
	httpClient := &http.Client{Transport: transport}

	c, err := NewClient(Config{
		Endpoint:   "https://127.0.0.1:8443",
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	if c.wsDialer == nil {
		t.Fatal("expected non-nil wsDialer")
	}
	if c.wsDialer.TLSClientConfig != customTLS {
		t.Fatalf("expected wsDialer to inherit custom TLS config, got %+v", c.wsDialer.TLSClientConfig)
	}
}

func TestAdminClientMethods(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/v1/admin/users" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []apiv1.User{
					{ID: "usr_alice", DisplayName: "Alice", Namespace: "ns-alice", Role: "user", Disabled: false},
				},
			})
		case path == "/api/v1/admin/users" && r.Method == http.MethodPost:
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(apiv1.User{
				ID: "usr_bob", DisplayName: req["displayName"], Namespace: req["namespace"], Role: "user",
			})
		case path == "/api/v1/admin/users/usr_bob" && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/v1/admin/users/usr_bob/role" && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		case path == "/api/v1/admin/users/usr_bob/tokens" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "tok_123", "token": "bb_sec_secret", "expiresAt": time.Now().Add(24 * time.Hour),
			})
		case path == "/api/v1/admin/users/usr_bob/tokens/tok_123" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c, err := NewClient(Config{Endpoint: server.URL, Token: "admin-token"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	users, err := c.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers failed: %v", err)
	}
	if len(users) != 1 || users[0].ID != "usr_alice" {
		t.Fatalf("unexpected users: %+v", users)
	}

	newUser, err := c.CreateUser(context.Background(), "Bob", "ns-bob")
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if newUser.ID != "usr_bob" || newUser.DisplayName != "Bob" {
		t.Fatalf("unexpected created user: %+v", newUser)
	}

	if err := c.SetUserDisabled(context.Background(), "usr_bob", true); err != nil {
		t.Fatalf("SetUserDisabled failed: %v", err)
	}

	if err := c.SetUserRole(context.Background(), "usr_bob", "admin"); err != nil {
		t.Fatalf("SetUserRole failed: %v", err)
	}

	tokenResp, err := c.IssueUserToken(context.Background(), "usr_bob")
	if err != nil {
		t.Fatalf("IssueUserToken failed: %v", err)
	}
	if tokenResp.ID != "tok_123" || tokenResp.Token != "bb_sec_secret" {
		t.Fatalf("unexpected issued token: %+v", tokenResp)
	}

	if err := c.RevokeUserToken(context.Background(), "usr_bob", "tok_123"); err != nil {
		t.Fatalf("RevokeUserToken failed: %v", err)
	}
}

func TestAdminUserSandboxes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/admin/users/usr_bob/sandboxes" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []apiv1.SandboxResponse{
					{Name: "sbx-bob-1", Phase: "Running", Image: "alpine", Namespace: "ns-bob", CreatedAt: time.Now()},
				},
				"total": 1,
			})
		case r.URL.Path == "/api/v1/admin/users/usr_bob/sandboxes/sbx-bob-1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c, err := NewClient(Config{Endpoint: server.URL, Token: "admin-token"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	sandboxes, err := c.ListUserSandboxes(context.Background(), "usr_bob")
	if err != nil {
		t.Fatalf("ListUserSandboxes failed: %v", err)
	}
	if len(sandboxes) != 1 || sandboxes[0].Name != "sbx-bob-1" {
		t.Fatalf("unexpected user sandboxes: %+v", sandboxes)
	}

	if err := c.DeleteUserSandbox(context.Background(), "usr_bob", "sbx-bob-1"); err != nil {
		t.Fatalf("DeleteUserSandbox failed: %v", err)
	}
}

func TestAdminUserSandboxesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/admin/users/usr_err/sandboxes":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal gateway error"))
		case "/api/v1/admin/users/usr_invalid_json/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not-json"))
		case "/api/v1/admin/users/usr_err/sandboxes/sbx-1":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("sandbox not found"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c, err := NewClient(Config{Endpoint: server.URL, Token: "admin-token"})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	if _, err := c.ListUserSandboxes(context.Background(), "usr_err"); err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}

	if _, err := c.ListUserSandboxes(context.Background(), "usr_invalid_json"); err == nil {
		t.Fatal("expected error for malformed json, got nil")
	}

	if err := c.DeleteUserSandbox(context.Background(), "usr_err", "sbx-1"); err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
}
