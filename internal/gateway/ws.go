package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for API clients and local developer tools
	},
	ReadBufferSize:  32768,
	WriteBufferSize: 32768,
}

// RemoteStreamExecutor abstracts container exec streaming to allow testing.
type RemoteStreamExecutor interface {
	StreamExec(ctx context.Context, namespace, podName string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer, resizeChan <-chan apiv1.ResizeMessage) (int, error)
}

// PortForwardDialer defines a function signature for dialing TCP ports on sandbox pods.
type PortForwardDialer func(ctx context.Context, namespace, podName string, targetPort int) (net.Conn, error)

// WSHandler manages WebSocket connections for interactive terminal execution, port forwarding, and log streaming.
type WSHandler struct {
	client            client.Client
	server            *Server
	executor          RemoteStreamExecutor
	portForwardDialer PortForwardDialer
	drainTimeout      time.Duration
}

// NewWSHandler creates a new WebSocket handler instance.
func NewWSHandler(c client.Client, s *Server, exec RemoteStreamExecutor) *WSHandler {
	return &WSHandler{
		client:       c,
		server:       s,
		executor:     exec,
		drainTimeout: 60 * time.Second,
	}
}

// SetPortForwardDialer sets a custom dialer for port forwarding (used in tests or custom networking).
func (h *WSHandler) SetPortForwardDialer(d PortForwardDialer) {
	h.portForwardDialer = d
}

// ExecHandler upgrades HTTP connection to WebSocket and multiplexes interactive terminal I/O.
func (h *WSHandler) ExecHandler(w http.ResponseWriter, r *http.Request) {
	ns, sbx, ok := h.validateRunningSandbox(w, r)
	if !ok {
		return
	}

	cmdParams := r.URL.Query()["cmd"]
	if len(cmdParams) == 0 {
		cmdParams = []string{"/bin/sh"}
	}
	tty := r.URL.Query().Get("tty") != "false"

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = wsConn.Close() }()

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()

	sessionID := generateRandomID("sess")
	h.server.registerSession(sessionID, wsConn, sessionCancel)
	defer h.server.deregisterSession(sessionID)

	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinReader.Close() }()
	resizeChan := make(chan apiv1.ResizeMessage, 8)
	stdinChan := make(chan []byte, 32)

	go func() {
		<-sessionCtx.Done()
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
	}()

	go func() {
		defer func() { _ = stdinWriter.Close() }()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case chunk, ok := <-stdinChan:
				if !ok || len(chunk) == 0 {
					return
				}
				if _, err := stdinWriter.Write(chunk); err != nil {
					return
				}
			}
		}
	}()

	var writeMu sync.Mutex
	sendChannelFrame := func(channel byte, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		payload := append([]byte{channel}, data...)
		return wsConn.WriteMessage(websocket.BinaryMessage, payload)
	}

	stdoutWriter := &channelWriter{channel: apiv1.ChannelStdout, send: sendChannelFrame}
	stderrWriter := &channelWriter{channel: apiv1.ChannelStderr, send: sendChannelFrame}

	go h.pumpWSFrames(sessionCtx, sessionCancel, wsConn, stdinChan, resizeChan, stdinWriter)

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	exitCode, execErr := h.executor.StreamExec(sessionCtx, ns, podName, cmdParams, tty, stdinReader, stdoutWriter, stderrWriter, resizeChan)

	exitMsg := apiv1.ExitMessage{ExitCode: exitCode}
	if execErr != nil {
		exitMsg.Error = execErr.Error()
	}
	exitData, _ := json.Marshal(exitMsg)
	_ = sendChannelFrame(apiv1.ChannelExit, exitData)
}

func (h *WSHandler) validateRunningSandbox(w http.ResponseWriter, r *http.Request) (string, *sandboxesv1alpha1.Sandbox, bool) {
	if h.server.IsDraining() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Gateway is shutting down", http.StatusServiceUnavailable)
		return "", nil, false
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return "", nil, false
	}

	id := extractIDFromPath(r.URL.Path)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return "", nil, false
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return "", nil, false
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return "", nil, false
	}

	if sbx.Status.Phase != sandboxesv1alpha1.SandboxPhaseRunning {
		http.Error(w, fmt.Sprintf("Sandbox is not in Running phase (current: %s)", sbx.Status.Phase), http.StatusConflict)
		return "", nil, false
	}

	return ns, &sbx, true
}

func (h *WSHandler) pumpWSFrames(
	sessionCtx context.Context,
	sessionCancel context.CancelFunc,
	wsConn *websocket.Conn,
	stdinChan chan<- []byte,
	resizeChan chan<- apiv1.ResizeMessage,
	stdinWriter *io.PipeWriter,
) {
	defer func() { _ = stdinWriter.Close() }()
	defer sessionCancel()
	var stdinClosed bool

	for {
		msgType, payload, err := wsConn.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.BinaryMessage || len(payload) == 0 {
			continue
		}

		channel := payload[0]
		data := payload[1:]

		switch channel {
		case apiv1.ChannelStdin:
			if stdinClosed {
				continue
			}
			if len(data) > 0 {
				chunk := make([]byte, len(data))
				copy(chunk, data)
				select {
				case stdinChan <- chunk:
				case <-sessionCtx.Done():
					return
				}
			} else {
				stdinClosed = true
				close(stdinChan)
			}
		case apiv1.ChannelResize:
			var resize apiv1.ResizeMessage
			if err := json.Unmarshal(data, &resize); err == nil {
				if resize.Cols == 0 && resize.Width > 0 {
					resize.Cols = int(resize.Width)
				}
				if resize.Rows == 0 && resize.Height > 0 {
					resize.Rows = int(resize.Height)
				}
				select {
				case resizeChan <- resize:
				default:
				}
			}
		}
	}
}

// PortForwardHandler handles WebSocket binary TCP tunneling to sandbox pod ports.
func (h *WSHandler) PortForwardHandler(w http.ResponseWriter, r *http.Request) {
	if h.server.IsDraining() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Gateway is shutting down", http.StatusServiceUnavailable)
		return
	}

	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	id := extractIDFromPath(r.URL.Path)
	if id == "" {
		http.Error(w, "Missing sandbox ID", http.StatusBadRequest)
		return
	}

	portStr := r.URL.Query().Get("port")
	if portStr == "" {
		http.Error(w, "Missing 'port' query parameter", http.StatusBadRequest)
		return
	}

	targetPort, err := strconv.Atoi(portStr)
	if err != nil || targetPort < 1 || targetPort > 65535 {
		http.Error(w, "Invalid port parameter: must be between 1 and 65535", http.StatusBadRequest)
		return
	}

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: id}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", id), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return
	}

	if sbx.Status.Phase != sandboxesv1alpha1.SandboxPhaseRunning {
		http.Error(w, fmt.Sprintf("Sandbox is not in Running phase (current: %s)", sbx.Status.Phase), http.StatusConflict)
		return
	}

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	// Dial the backend pod TCP socket via Kubernetes port-forward dialer
	if h.portForwardDialer == nil {
		http.Error(w, "Port-forward dialer is not configured on gateway", http.StatusNotImplemented)
		return
	}
	tcpConn, err := h.portForwardDialer(r.Context(), ns, podName, targetPort)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to connect to pod port %d: %v", targetPort, err), http.StatusBadGateway)
		return
	}
	defer func() { _ = tcpConn.Close() }()

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = wsConn.Close() }()

	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	defer sessionCancel()

	sessionID := generateRandomID("pf")
	h.server.registerSession(sessionID, wsConn, sessionCancel)
	defer h.server.deregisterSession(sessionID)

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: WebSocket Binary -> TCP Socket
	go func() {
		defer wg.Done()
		defer sessionCancel()
		for {
			msgType, data, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage && len(data) > 0 {
				if _, err := tcpConn.Write(data); err != nil {
					return
				}
			}
		}
	}()

	// Goroutine 2: TCP Socket -> WebSocket Binary
	var writeMu sync.Mutex
	go func() {
		defer wg.Done()
		defer sessionCancel()
		buf := make([]byte, 32768)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				writeMu.Lock()
				err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
				writeMu.Unlock()
				if err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	<-sessionCtx.Done()
	_ = tcpConn.Close()
	_ = wsConn.Close()
	wg.Wait()
}

type channelWriter struct {
	channel byte
	send    func(channel byte, data []byte) error
}

func (w *channelWriter) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.send(w.channel, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
