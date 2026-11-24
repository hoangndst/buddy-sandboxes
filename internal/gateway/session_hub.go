package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hoangndst/buddy-sandboxes/internal/sessions"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// SessionHub multiplexes WebSocket client connections to in-guest supervisor sessions.
type SessionHub struct {
	mu           sync.RWMutex
	sessions     map[string]*hubSession
	executor     RemoteStreamExecutor
	store        *sessions.Store
	drainTimeout time.Duration
}

type hubSession struct {
	hub          *SessionHub
	key          string
	sessionID    string
	namespace    string
	sandboxName  string
	podName      string
	mu           sync.Mutex
	clients      map[*sessionClient]struct{}
	leaseholder  *sessionClient
	scrollback   []byte
	stdinWriter  io.WriteCloser
	resizeChan   chan apiv1.ResizeMessage
	cancel       context.CancelFunc
	done         chan struct{}
	exitReceived bool
	exitCode     int
}

type sessionClient struct {
	conn       *websocket.Conn
	writeMu    sync.Mutex
	sendMu     sync.Mutex
	closed     bool
	readOnly   bool
	takeLease  bool
	sendChan   chan []byte
	done       chan struct{}
	cancelFunc context.CancelFunc
}

func (c *sessionClient) close() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.sendChan)
	}
}

// NewSessionHub creates a new SessionHub.
func NewSessionHub(exec RemoteStreamExecutor, store *sessions.Store) *SessionHub {
	return &SessionHub{
		sessions:     make(map[string]*hubSession),
		executor:     exec,
		store:        store,
		drainTimeout: 60 * time.Second,
	}
}

// AttachRequest carries parameters for attaching a WebSocket client to a session.
type AttachRequest struct {
	Namespace   string
	SandboxName string
	PodName     string
	SessionID   string
	TakeLease   bool
	ReadOnly    bool
}

// Attach upgrades and multiplexes a WebSocket connection into a session.
func (h *SessionHub) Attach(ctx context.Context, wsConn *websocket.Conn, req AttachRequest) error {
	key := fmt.Sprintf("%s/%s/%s", req.Namespace, req.SandboxName, req.SessionID)

	h.mu.Lock()
	hs, exists := h.sessions[key]
	if !exists {
		hs = h.newHubSession(key, req)
		h.sessions[key] = hs
		go hs.runUpstream()
	}
	h.mu.Unlock()

	clientCtx, clientCancel := context.WithCancel(ctx)
	defer clientCancel()

	client := &sessionClient{
		conn:       wsConn,
		readOnly:   req.ReadOnly,
		takeLease:  req.TakeLease,
		sendChan:   make(chan []byte, 128),
		done:       make(chan struct{}),
		cancelFunc: clientCancel,
	}

	hs.registerClient(client)
	defer hs.unregisterClient(client)

	// Writer goroutine to client
	go func() {
		defer func() {
			close(client.done)
			client.writeMu.Lock()
			_ = wsConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			_ = wsConn.Close()
			client.writeMu.Unlock()
			client.cancelFunc()
		}()
		for {
			select {
			case <-clientCtx.Done():
				return
			case msg, ok := <-client.sendChan:
				if !ok {
					return
				}
				client.writeMu.Lock()
				_ = wsConn.WriteMessage(websocket.BinaryMessage, msg)
				client.writeMu.Unlock()
			}
		}
	}()

	// Reader goroutine from client
	for {
		msgType, payload, err := wsConn.ReadMessage()
		if err != nil {
			break
		}
		if msgType != websocket.BinaryMessage || len(payload) == 0 {
			continue
		}

		channel := payload[0]
		data := payload[1:]

		switch channel {
		case apiv1.ChannelStdin:
			hs.handleStdin(client, data)
		case apiv1.ChannelResize:
			hs.handleResize(client, data)
		}
	}

	return nil
}

func (h *SessionHub) newHubSession(key string, req AttachRequest) *hubSession {
	return &hubSession{
		hub:         h,
		key:         key,
		sessionID:   req.SessionID,
		namespace:   req.Namespace,
		sandboxName: req.SandboxName,
		podName:     req.PodName,
		clients:     make(map[*sessionClient]struct{}),
		resizeChan:  make(chan apiv1.ResizeMessage, 8),
		done:        make(chan struct{}),
	}
}

func (hs *hubSession) runUpstream() {
	defer func() {
		hs.hub.mu.Lock()
		delete(hs.hub.sessions, hs.key)
		hs.hub.mu.Unlock()
		close(hs.done)
	}()

	upstreamCtx, cancel := context.WithCancel(context.Background())
	hs.mu.Lock()
	hs.cancel = cancel
	stdinReader, stdinWriter := io.Pipe()
	hs.stdinWriter = stdinWriter
	hs.mu.Unlock()

	defer func() {
		_ = stdinWriter.Close()
		_ = stdinReader.Close()
	}()

	cmd := []string{"/opt/buddy/bin/buddy-runner", "attach", "--raw", hs.sessionID}

	stdoutPipeR, stdoutPipeW := io.Pipe()
	stderrPipeR, stderrPipeW := io.Pipe()

	var ioWg sync.WaitGroup
	ioWg.Add(2)

	go func() {
		defer ioWg.Done()
		buf := make([]byte, 16384)
		for {
			n, err := stdoutPipeR.Read(buf)
			if n > 0 {
				hs.broadcast(apiv1.ChannelStdout, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer ioWg.Done()
		buf := make([]byte, 16384)
		for {
			n, err := stderrPipeR.Read(buf)
			if n > 0 {
				hs.broadcast(apiv1.ChannelStderr, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	exitCode, execErr := hs.hub.executor.StreamExec(
		upstreamCtx,
		hs.namespace,
		hs.podName,
		cmd,
		true,
		stdinReader,
		stdoutPipeW,
		stderrPipeW,
		hs.resizeChan,
	)

	_ = stdoutPipeW.Close()
	_ = stderrPipeW.Close()
	ioWg.Wait()
	_ = stdoutPipeR.Close()
	_ = stderrPipeR.Close()

	if execErr != nil {
		hs.mu.Lock()
		received := hs.exitReceived
		hs.mu.Unlock()
		if !received {
			// Upstream attach transport failure or unexpected EOF.
			// The supervised workload may still be running in the guest.
			// Preserve running state so DeleteSession can still stop it or future Attach can reconnect.
			hs.closeClients()
			return
		}
	}

	hs.mu.Lock()
	hs.exitReceived = true
	hs.exitCode = exitCode
	hs.mu.Unlock()

	// Update store state
	if hs.hub.store != nil {
		terminalState := apiv1.SessionStateCompleted
		if exitCode != 0 {
			terminalState = apiv1.SessionStateFailed
		}
		_ = hs.hub.store.UpdateSessionState(context.Background(), hs.sessionID, terminalState, &exitCode, nil)
	}

	exitMsg := apiv1.ExitMessage{ExitCode: exitCode}
	exitData, _ := json.Marshal(exitMsg)
	hs.broadcast(apiv1.ChannelExit, exitData)
	hs.closeClients()
}

func (hs *hubSession) closeClients() {
	hs.mu.Lock()
	clients := make([]*sessionClient, 0, len(hs.clients))
	for c := range hs.clients {
		clients = append(clients, c)
	}
	hs.mu.Unlock()

	for _, c := range clients {
		c.close()
	}
}

func (hs *hubSession) registerClient(c *sessionClient) {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	hs.clients[c] = struct{}{}

	// Lease arbitration
	if !c.readOnly {
		if hs.leaseholder == nil {
			hs.leaseholder = c
		} else if c.takeLease {
			prev := hs.leaseholder
			hs.leaseholder = c
			// Notify previous leaseholder
			if prev != nil {
				notice := []byte("[Notice: write lease transferred to another client]\r\n")
				prev.enqueue(append([]byte{apiv1.ChannelStderr}, notice...))
			}
		}
	}

	// Replay scrollback if present
	if len(hs.scrollback) > 0 {
		c.enqueue(append([]byte{apiv1.ChannelStdout}, hs.scrollback...))
	}

	// If session already exited, emit exit frame immediately and close client stream
	if hs.exitReceived {
		exitPayload, _ := json.Marshal(apiv1.ExitMessage{ExitCode: hs.exitCode})
		c.enqueue(append([]byte{apiv1.ChannelExit}, exitPayload...))
		c.close()
	}
}

func (hs *hubSession) unregisterClient(c *sessionClient) {
	hs.mu.Lock()
	delete(hs.clients, c)
	if hs.leaseholder == c {
		hs.leaseholder = nil
		// Assign next non-read-only client
		for other := range hs.clients {
			if !other.readOnly {
				hs.leaseholder = other
				notice := []byte("[Notice: acquired write lease]\r\n")
				other.enqueue(append([]byte{apiv1.ChannelStderr}, notice...))
				break
			}
		}
	}
	hs.mu.Unlock()

	c.close()
}

func (hs *hubSession) handleStdin(c *sessionClient, data []byte) {
	hs.mu.Lock()
	isLeaseholder := (hs.leaseholder == c)
	writer := hs.stdinWriter
	hs.mu.Unlock()

	if !isLeaseholder {
		notice := []byte("[Notice: stdin ignored in read-only observer mode]\r\n")
		c.enqueue(append([]byte{apiv1.ChannelStderr}, notice...))
		return
	}

	if writer != nil && len(data) > 0 {
		_, _ = writer.Write(data)
	}
}

func (hs *hubSession) handleResize(c *sessionClient, data []byte) {
	hs.mu.Lock()
	isLeaseholder := (hs.leaseholder == c)
	ch := hs.resizeChan
	hs.mu.Unlock()

	if !isLeaseholder || ch == nil || len(data) == 0 {
		return
	}

	var res apiv1.ResizeMessage
	if err := json.Unmarshal(data, &res); err == nil {
		select {
		case ch <- res:
		default:
		}
	}
}

func (hs *hubSession) broadcast(channel byte, data []byte) {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	if channel == apiv1.ChannelStdout {
		// Append to in-memory scrollback (capped at 1MB)
		hs.scrollback = append(hs.scrollback, data...)
		if len(hs.scrollback) > 1024*1024 {
			hs.scrollback = hs.scrollback[len(hs.scrollback)-1024*1024:]
		}
	}

	payload := append([]byte{channel}, data...)
	for c := range hs.clients {
		c.enqueue(payload)
	}
}

func (c *sessionClient) enqueue(payload []byte) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.sendChan <- payload:
	default:
	}
}
