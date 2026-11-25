package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

type hubMockExecutor struct {
	lastCmd       []string
	stdinReceived []byte
	resizeCount   int
}

func (m *hubMockExecutor) StreamExec(
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
		_, _ = stdout.Write([]byte("hub initial stdout\n"))
	}

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				m.stdinReceived = append(m.stdinReceived, buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for range resizeChan {
			m.resizeCount++
		}
	}()

	select {
	case <-ctx.Done():
		return 0, nil
	case <-time.After(300 * time.Millisecond):
		if stdout != nil {
			_, _ = stdout.Write([]byte("hub later stdout\n"))
		}
		return 0, nil
	}
}

func TestSessionHubMultiplexingAndLease(t *testing.T) {
	mockExec := &hubMockExecutor{}
	hub := NewSessionHub(mockExec, nil)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		takeLease := r.URL.Query().Get("take_lease") == "true"
		readOnly := r.URL.Query().Get("mode") == "ro"

		req := AttachRequest{
			Namespace:   "tenant",
			SandboxName: "test-sbx",
			PodName:     "test-pod",
			SessionID:   "sess-123",
			TakeLease:   takeLease,
			ReadOnly:    readOnly,
		}
		_ = hub.Attach(r.Context(), wsConn, req)
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")

	// Client 1 connects (default: becomes write leaseholder)
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("client 1 dial failed: %v", err)
	}
	defer func() { _ = conn1.Close() }()

	// Verify client 1 receives initial scrollback
	_, msg1, err := conn1.ReadMessage()
	if err != nil || len(msg1) == 0 || msg1[0] != apiv1.ChannelStdout {
		t.Fatalf("client 1 did not receive ChannelStdout: %v", err)
	}
	if !strings.Contains(string(msg1[1:]), "hub initial stdout") {
		t.Fatalf("unexpected client 1 initial output: %q", string(msg1[1:]))
	}

	// Client 2 connects as observer
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL+"?mode=ro", nil)
	if err != nil {
		t.Fatalf("client 2 dial failed: %v", err)
	}
	defer func() { _ = conn2.Close() }()

	// Client 2 must immediately receive the scrollback history
	_, msg2, err := conn2.ReadMessage()
	if err != nil || len(msg2) == 0 || msg2[0] != apiv1.ChannelStdout {
		t.Fatalf("client 2 did not receive scrollback: %v", err)
	}
	if !strings.Contains(string(msg2[1:]), "hub initial stdout") {
		t.Fatalf("unexpected client 2 scrollback: %q", string(msg2[1:]))
	}

	// Client 1 (leaseholder) sends stdin
	stdinFrame := append([]byte{apiv1.ChannelStdin}, []byte("ls -la\n")...)
	_ = conn1.WriteMessage(websocket.BinaryMessage, stdinFrame)

	// Client 1 sends resize
	resPayload, _ := json.Marshal(apiv1.ResizeMessage{Cols: 120, Rows: 40})
	resizeFrame := append([]byte{apiv1.ChannelResize}, resPayload...)
	_ = conn1.WriteMessage(websocket.BinaryMessage, resizeFrame)

	// Wait for later stdout and exit frame
	exitReceived := false
	for i := 0; i < 5; i++ {
		_, msg, err := conn1.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) > 0 && msg[0] == apiv1.ChannelExit {
			exitReceived = true
			break
		}
	}
	if !exitReceived {
		t.Fatalf("client 1 did not receive ChannelExit frame")
	}

	// Verify client 1 stream was closed by server after exit frame
	_, _, err = conn1.ReadMessage()
	if err == nil {
		t.Fatalf("expected client 1 connection to be closed after ChannelExit frame")
	}

	// Verify upstream command passed --raw
	expectedCmd := []string{"/opt/buddy/bin/buddy-runner", "attach", "--raw", "sess-123"}
	if len(mockExec.lastCmd) != len(expectedCmd) {
		t.Fatalf("expected cmd %v, got %v", expectedCmd, mockExec.lastCmd)
	}
	for i := range expectedCmd {
		if mockExec.lastCmd[i] != expectedCmd[i] {
			t.Fatalf("expected cmd[%d] = %s, got %s", i, expectedCmd[i], mockExec.lastCmd[i])
		}
	}
}

func TestSessionClientSlowConsumerDisconnectedOnOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &sessionClient{
		sendChan:   make(chan []byte, 2),
		cancelFunc: cancel,
	}

	// First 2 enqueue calls fit in buffer
	client.enqueue([]byte("frame1"))
	client.enqueue([]byte("frame2"))

	if client.closed {
		t.Fatal("client should not be marked closed while buffer has room")
	}

	// 3rd enqueue overflows buffer -> must disconnect client
	client.enqueue([]byte("frame3"))

	if !client.closed {
		t.Fatal("client should be marked closed after buffer overflow")
	}

	// Context should be cancelled
	select {
	case <-ctx.Done():
	default:
		t.Fatal("client context should be cancelled upon buffer overflow")
	}

	// sendChan should be closed
	<-client.sendChan
	<-client.sendChan
	_, ok := <-client.sendChan
	if ok {
		t.Fatal("sendChan should be closed upon buffer overflow")
	}

	// Subsequent enqueues should be safe no-ops
	client.enqueue([]byte("frame4"))
}

func TestSessionClientConcurrentEnqueueAndClose(t *testing.T) {
	client := &sessionClient{
		sendChan: make(chan []byte, 16),
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				client.enqueue([]byte("payload"))
			}
		}()
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.close()
		}()
	}

	wg.Wait()
}

func TestHubSessionConcurrentBroadcastAndUnregister(t *testing.T) {
	hs := &hubSession{
		clients: make(map[*sessionClient]struct{}),
	}

	clients := make([]*sessionClient, 20)
	for i := range clients {
		c := &sessionClient{
			sendChan: make(chan []byte, 32),
		}
		clients[i] = c
		hs.clients[c] = struct{}{}
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				hs.broadcast(apiv1.ChannelStdout, []byte("data"))
			}
		}()
	}

	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hs.unregisterClient(c)
		}()
	}

	wg.Wait()
}

type failingMockExecutor struct {
	err      error
	exitJSON string
}

func (m *failingMockExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	if len(cmd) > 1 && cmd[1] == "status" {
		if m.exitJSON == "" {
			return 1, nil
		}
		_, _ = io.WriteString(stdout, m.exitJSON)
		return 0, nil
	}
	if stdout != nil {
		_, _ = stdout.Write([]byte("partial stdout before transport drop\n"))
	}
	return 1, m.err
}

func TestSessionHubUsesWorkloadExitRecordAfterAttachFailure(t *testing.T) {
	hub := NewSessionHub(&failingMockExecutor{exitJSON: `{"exitCode":23}`}, nil)
	hs := hub.newHubSession("session", AttachRequest{SessionID: "session"})
	hs.runUpstream()
	if !hs.exitReceived || hs.exitCode != 23 {
		t.Fatalf("expected workload exit status 23, received=%t code=%d", hs.exitReceived, hs.exitCode)
	}
}

func TestSessionHubUpstreamTransportFailurePreservesRunningState(t *testing.T) {
	mockExec := &failingMockExecutor{err: errors.New("upstream connection reset by peer")}
	hub := NewSessionHub(mockExec, nil)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		req := AttachRequest{
			Namespace:   "tenant",
			SandboxName: "test-sbx",
			PodName:     "test-pod",
			SessionID:   "sess-failing",
		}
		_ = hub.Attach(r.Context(), wsConn, req)
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	receivedExit := false
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) > 0 && msg[0] == apiv1.ChannelExit {
			receivedExit = true
			break
		}
	}

	if receivedExit {
		t.Fatalf("client unexpectedly received ChannelExit frame on attach transport drop")
	}

	// Verify session hub removed the dead upstream session from its active map
	key := "tenant/test-sbx/sess-failing"
	hub.mu.RLock()
	_, exists := hub.sessions[key]
	hub.mu.RUnlock()
	if exists {
		t.Fatalf("expected hub session %q to be cleared from active sessions map", key)
	}
}

type drainingMockExecutor struct {
	finalStdout string
}

func (m *drainingMockExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	if stdout != nil {
		_, _ = stdout.Write([]byte(m.finalStdout))
	}
	return 0, nil
}

func TestSessionHubDrainsOutputBeforeClosingClients(t *testing.T) {
	mockExec := &drainingMockExecutor{finalStdout: "critical final stdout before exit"}
	hub := NewSessionHub(mockExec, nil)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		req := AttachRequest{
			Namespace:   "tenant",
			SandboxName: "test-sbx",
			PodName:     "test-pod",
			SessionID:   "sess-drain",
		}
		_ = hub.Attach(r.Context(), wsConn, req)
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var receivedChunks []string
	var receivedExit bool
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.Fatalf("expected normal stream closure after exit, got %v", err)
			}
			break
		}
		if len(msg) > 0 {
			switch msg[0] {
			case apiv1.ChannelStdout:
				receivedChunks = append(receivedChunks, string(msg[1:]))
			case apiv1.ChannelExit:
				receivedExit = true
			}
		}
	}

	if !receivedExit {
		t.Errorf("expected ChannelExit frame")
	}
	joinedOutput := strings.Join(receivedChunks, "")
	if !strings.Contains(joinedOutput, "critical final stdout before exit") {
		t.Errorf("expected final stdout to be drained before close, got %q", joinedOutput)
	}
}

type delayedStdinMockExecutor struct {
	mu            sync.Mutex
	stdinReceived []byte
}

func (m *delayedStdinMockExecutor) StreamExec(
	ctx context.Context,
	namespace, podName string,
	cmd []string,
	tty bool,
	stdin io.Reader,
	stdout, stderr io.Writer,
	resizeChan <-chan apiv1.ResizeMessage,
) (int, error) {
	// Delay reading stdin to simulate async upstream startup
	time.Sleep(50 * time.Millisecond)

	buf := make([]byte, 1024)
	n, err := stdin.Read(buf)
	if n > 0 {
		m.mu.Lock()
		m.stdinReceived = append(m.stdinReceived, buf[:n]...)
		m.mu.Unlock()
	}
	if stdout != nil {
		_, _ = stdout.Write([]byte("delayed-exec-done\n"))
	}
	return 0, err
}

func TestSessionHubInitialStdinBufferedBeforeUpstreamExec(t *testing.T) {
	mockExec := &delayedStdinMockExecutor{}
	hub := NewSessionHub(mockExec, nil)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = wsConn.Close() }()

		req := AttachRequest{
			Namespace:   "tenant",
			SandboxName: "test-sbx",
			PodName:     "test-pod",
			SessionID:   "sess-immediate-stdin",
		}
		_ = hub.Attach(r.Context(), wsConn, req)
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("client dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Immediately send stdin without waiting for any output
	immediateData := []byte("early_command\n")
	stdinFrame := append([]byte{apiv1.ChannelStdin}, immediateData...)
	if err := conn.WriteMessage(websocket.BinaryMessage, stdinFrame); err != nil {
		t.Fatalf("failed to send immediate stdin: %v", err)
	}

	// Drain frames until connection closes
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}

	mockExec.mu.Lock()
	received := string(mockExec.stdinReceived)
	mockExec.mu.Unlock()

	if !strings.Contains(received, "early_command") {
		t.Fatalf("expected early stdin to be preserved, got %q", received)
	}
}

func TestSessionHubReplaysStderrScrollback(t *testing.T) {
	hs := &hubSession{
		clients: make(map[*sessionClient]struct{}),
	}

	hs.broadcast(apiv1.ChannelStdout, []byte("stdout history\n"))
	hs.broadcast(apiv1.ChannelStderr, []byte("stderr diagnostics\n"))

	client := &sessionClient{
		sendChan: make(chan []byte, 10),
	}
	hs.registerClient(client)

	var receivedStdout, receivedStderr bool
	close(client.sendChan)
	for msg := range client.sendChan {
		if len(msg) > 0 {
			switch msg[0] {
			case apiv1.ChannelStdout:
				if string(msg[1:]) == "stdout history\n" {
					receivedStdout = true
				}
			case apiv1.ChannelStderr:
				if string(msg[1:]) == "stderr diagnostics\n" {
					receivedStderr = true
				}
			}
		}
	}

	if !receivedStdout {
		t.Errorf("expected client to receive stdout scrollback")
	}
	if !receivedStderr {
		t.Errorf("expected client to receive stderr scrollback")
	}
}

func TestSessionHubDecoupledStdinNonBlocking(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() {
		_ = pr.Close()
		_ = pw.Close()
	}()

	hs := &hubSession{
		clients:     make(map[*sessionClient]struct{}),
		stdinReader: pr,
		stdinWriter: pw,
		stdinChan:   make(chan []byte, 128),
	}

	client := &sessionClient{
		sendChan: make(chan []byte, 10),
	}
	hs.clients[client] = struct{}{}
	hs.leaseholder = client

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Write more than the channel buffer capacity to verify it does not block
		for i := 0; i < 200; i++ {
			hs.handleStdin(client, []byte("test input"))
		}
	}()

	select {
	case <-done:
		// Succeeded immediately without blocking on child stdin consumption
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("handleStdin blocked on unread stdin pipe")
	}
}
