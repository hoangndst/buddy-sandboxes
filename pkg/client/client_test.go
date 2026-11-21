package client

import (
	"bytes"
	"context"
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
		Name:  "test-cli-sbx",
		Image: "golang:1.25",
	})
	if err != nil {
		t.Fatalf("CreateSandbox failed: %v", err)
	}
	if created.Name != "test-cli-sbx" || created.Phase != "Running" {
		t.Fatalf("unexpected sandbox: %+v", created)
	}

	// 2. Get sandbox
	got, err := c.GetSandbox(ctx, "test-cli-sbx")
	if err != nil {
		t.Fatalf("GetSandbox failed: %v", err)
	}
	if got.ID != "test-cli-sbx" {
		t.Fatalf("unexpected got ID: %s", got.ID)
	}

	// 3. List sandboxes
	list, err := c.ListSandboxes(ctx)
	if err != nil {
		t.Fatalf("ListSandboxes failed: %v", err)
	}
	if list.Total != 1 || list.Items[0].Name != "test-cli-sbx" {
		t.Fatalf("unexpected list: %+v", list)
	}

	// 4. Stream logs
	var logBuf bytes.Buffer
	err = c.StreamLogs(ctx, "test-cli-sbx", LogOptions{Follow: false}, &logBuf)
	if err != nil {
		t.Fatalf("StreamLogs failed: %v", err)
	}
	if !strings.Contains(logBuf.String(), "mock container log line") {
		t.Fatalf("unexpected logs: %s", logBuf.String())
	}

	// 5. Delete sandbox
	if err := c.DeleteSandbox(ctx, "test-cli-sbx"); err != nil {
		t.Fatalf("DeleteSandbox failed: %v", err)
	}

	// 6. Verify deleted
	_, err = c.GetSandbox(ctx, "test-cli-sbx")
	if err == nil {
		t.Fatal("expected 404 after delete, got nil error")
	}
}

func TestClientExecAndResize(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sandboxes/test-exec/exec", func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

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
}

func TestClientPortForward(t *testing.T) {
	// 1. Mock TCP echo server (simulating pod process)
	tcpBackend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start backend tcp listener: %v", err)
	}
	defer tcpBackend.Close()

	go func() {
		for {
			conn, err := tcpBackend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
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
		defer wsConn.Close()

		backendConn, err := net.Dial("tcp", tcpBackend.Addr().String())
		if err != nil {
			return
		}
		defer backendConn.Close()

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
	defer localConn.Close()

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
