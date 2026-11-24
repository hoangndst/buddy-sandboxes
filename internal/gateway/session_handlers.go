package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/sessions"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// SessionHandlers manages detached process sessions within sandboxes.
type SessionHandlers struct {
	server      *Server
	store       *sessions.Store
	hub         *SessionHub
	memMu       sync.RWMutex
	memSessions map[string]*sessions.Session
}

// NewSessionHandlers creates a new SessionHandlers instance.
func NewSessionHandlers(server *Server, store *sessions.Store, hub *SessionHub) *SessionHandlers {
	return &SessionHandlers{
		server:      server,
		store:       store,
		hub:         hub,
		memSessions: make(map[string]*sessions.Session),
	}
}

func (h *SessionHandlers) validateRunningSandbox(w http.ResponseWriter, r *http.Request, sandboxName string) (string, *sandboxesv1alpha1.Sandbox, bool) {
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

	var sbx sandboxesv1alpha1.Sandbox
	if err := h.server.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: sandboxName}, &sbx); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("Sandbox %s not found", sandboxName), http.StatusNotFound)
			return "", nil, false
		}
		http.Error(w, fmt.Sprintf("Failed to get sandbox: %v", err), http.StatusInternalServerError)
		return "", nil, false
	}

	if rejectSuspendedPodAccess(w, &sbx) {
		return "", nil, false
	}

	if sbx.Status.Phase != sandboxesv1alpha1.SandboxPhaseRunning {
		http.Error(w, fmt.Sprintf("Sandbox is not in Running phase (current: %s)", sbx.Status.Phase), http.StatusConflict)
		return "", nil, false
	}

	return ns, &sbx, true
}

// CreateSession handles POST /api/v1/sandboxes/:sandboxName/sessions
func (h *SessionHandlers) CreateSession(w http.ResponseWriter, r *http.Request, sandboxName string) {
	ns, sbx, ok := h.validateRunningSandbox(w, r, sandboxName)
	if !ok {
		return
	}

	var req apiv1.CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request payload: %v", err), http.StatusBadRequest)
		return
	}

	if len(req.Command) == 0 {
		http.Error(w, "Command cannot be empty", http.StatusBadRequest)
		return
	}

	workingDir := req.WorkingDir
	if workingDir == "" {
		workingDir = "/workspace"
	}
	useTTY := true
	if req.TTY != nil {
		useTTY = *req.TTY
	}

	sessionID := generateRandomID("sess")
	principal, ok := PrincipalFromContext(r.Context())
	userID := "anonymous"
	if ok && principal.ID != "" {
		userID = principal.ID
	}

	sess := &sessions.Session{
		ID:          sessionID,
		UserID:      userID,
		Namespace:   ns,
		SandboxName: sandboxName,
		Command:     req.Command,
		WorkingDir:  workingDir,
		TTY:         useTTY,
		State:       apiv1.SessionStatePending,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		Metadata:    req.Metadata,
	}

	if h.store != nil {
		if err := h.store.CreateSession(r.Context(), sess); err != nil {
			http.Error(w, fmt.Sprintf("Failed creating session record: %v", err), http.StatusInternalServerError)
			return
		}
	} else {
		h.memMu.Lock()
		h.memSessions[sessionID] = sess
		h.memMu.Unlock()
	}

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	// Prepare supervisor start command
	startCmd := []string{
		"/opt/buddy/bin/buddy-runner", "start", sessionID,
		"--dir", workingDir,
	}
	if useTTY {
		startCmd = append(startCmd, "--tty")
	} else {
		startCmd = append(startCmd, "--tty=false")
	}
	startCmd = append(startCmd, "--")
	startCmd = append(startCmd, req.Command...)

	// Launch supervisor process asynchronously in the target pod
	go func() {
		now := time.Now().UTC()
		h.memMu.Lock()
		sess.StartedAt = &now
		sess.State = apiv1.SessionStateRunning
		h.memMu.Unlock()

		if h.store != nil {
			_ = h.store.UpdateSessionState(context.Background(), sessionID, apiv1.SessionStateRunning, nil, nil)
		}

		var stdoutBuf, stderrBuf bytes.Buffer
		exitCode, execErr := h.server.remoteExecutor.StreamExec(
			context.Background(),
			ns,
			podName,
			startCmd,
			false,
			nil,
			&stdoutBuf,
			&stderrBuf,
			nil,
		)

		h.memMu.Lock()
		if sess.State == apiv1.SessionStateTerminated {
			h.memMu.Unlock()
			return
		}

		completedTime := time.Now().UTC()
		sess.CompletedAt = &completedTime
		sess.ExitCode = &exitCode
		sess.State = apiv1.SessionStateCompleted
		if exitCode != 0 || execErr != nil {
			sess.State = apiv1.SessionStateFailed
		}
		h.memMu.Unlock()

		if h.store != nil {
			_ = h.store.UpdateSessionState(context.Background(), sessionID, sess.State, &exitCode, nil)
		}
	}()

	h.memMu.RLock()
	resp := toSessionResponse(sess)
	h.memMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// ListSessions handles GET /api/v1/sandboxes/:sandboxName/sessions
func (h *SessionHandlers) ListSessions(w http.ResponseWriter, r *http.Request, sandboxName string) {
	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	var sessionList []sessions.Session
	if h.store != nil {
		list, err := h.store.ListSessions(r.Context(), ns, sandboxName)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to list sessions: %v", err), http.StatusInternalServerError)
			return
		}
		sessionList = list
	} else {
		h.memMu.RLock()
		for _, s := range h.memSessions {
			if s.Namespace == ns && s.SandboxName == sandboxName {
				sessionList = append(sessionList, *s)
			}
		}
		h.memMu.RUnlock()
	}

	responses := make([]apiv1.SessionResponse, len(sessionList))
	for i := range sessionList {
		responses[i] = toSessionResponse(&sessionList[i])
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apiv1.ListSessionsResponse{Sessions: responses})
}

// GetSession handles GET /api/v1/sandboxes/:sandboxName/sessions/:sessionID
func (h *SessionHandlers) GetSession(w http.ResponseWriter, r *http.Request, sandboxName, sessionID string) {
	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	var sess *sessions.Session
	if h.store != nil {
		s, err := h.store.GetSession(r.Context(), sessionID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		sess = s
	} else {
		h.memMu.RLock()
		sess = h.memSessions[sessionID]
		h.memMu.RUnlock()
	}

	if sess == nil || sess.Namespace != ns || sess.SandboxName != sandboxName {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(toSessionResponse(sess))
}

// DeleteSession handles DELETE /api/v1/sandboxes/:sandboxName/sessions/:sessionID
func (h *SessionHandlers) DeleteSession(w http.ResponseWriter, r *http.Request, sandboxName, sessionID string) {
	ns, ok := TenantNamespaceFromContext(r.Context())
	if !ok || ns == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}

	var sess *sessions.Session
	if h.store != nil {
		s, err := h.store.GetSession(r.Context(), sessionID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		sess = s
	} else {
		h.memMu.RLock()
		sess = h.memSessions[sessionID]
		h.memMu.RUnlock()
	}

	if sess == nil || sess.Namespace != ns || sess.SandboxName != sandboxName {
		http.NotFound(w, r)
		return
	}

	exitCode := 137 // SIGKILL equivalent

	// Actively terminate supervisor process in the pod before reporting termination
	if sess.State == apiv1.SessionStateRunning || sess.State == apiv1.SessionStatePending {
		var sbx sandboxesv1alpha1.Sandbox
		podName := sandboxName
		if err := h.server.client.Get(r.Context(), types.NamespacedName{Namespace: ns, Name: sandboxName}, &sbx); err == nil && sbx.Status.PodName != "" {
			podName = sbx.Status.PodName
		}

		stopCmd := []string{"/opt/buddy/bin/buddy-runner", "stop", sessionID}
		var stopStdout, stopStderr bytes.Buffer
		_, _ = h.server.remoteExecutor.StreamExec(
			r.Context(),
			ns,
			podName,
			stopCmd,
			false,
			nil,
			&stopStdout,
			&stopStderr,
			nil,
		)
	}

	if h.store != nil {
		_ = h.store.UpdateSessionState(r.Context(), sessionID, apiv1.SessionStateTerminated, &exitCode, nil)
	} else {
		h.memMu.Lock()
		if s, ok := h.memSessions[sessionID]; ok {
			s.State = apiv1.SessionStateTerminated
			s.ExitCode = &exitCode
		}
		h.memMu.Unlock()
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"terminated"}`))
}

// StreamSession handles GET /api/v1/sandboxes/:sandboxName/sessions/:sessionID/stream
func (h *SessionHandlers) StreamSession(w http.ResponseWriter, r *http.Request, sandboxName, sessionID string) {
	ns, sbx, ok := h.validateRunningSandbox(w, r, sandboxName)
	if !ok {
		return
	}

	var sess *sessions.Session
	if h.store != nil {
		s, err := h.store.GetSession(r.Context(), sessionID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		sess = s
	} else {
		h.memMu.RLock()
		sess = h.memSessions[sessionID]
		h.memMu.RUnlock()
	}

	if sess == nil || sess.Namespace != ns || sess.SandboxName != sandboxName {
		http.NotFound(w, r)
		return
	}

	takeLease := r.URL.Query().Get("take_lease") == "true"
	readOnly := r.URL.Query().Get("mode") == "ro"

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = wsConn.Close() }()

	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}

	req := AttachRequest{
		Namespace:   ns,
		SandboxName: sandboxName,
		PodName:     podName,
		SessionID:   sessionID,
		TakeLease:   takeLease,
		ReadOnly:    readOnly,
	}

	_ = h.hub.Attach(r.Context(), wsConn, req)
}

func toSessionResponse(s *sessions.Session) apiv1.SessionResponse {
	return apiv1.SessionResponse{
		ID:          s.ID,
		Namespace:   s.Namespace,
		SandboxName: s.SandboxName,
		Command:     s.Command,
		WorkingDir:  s.WorkingDir,
		TTY:         s.TTY,
		State:       s.State,
		ExitCode:    s.ExitCode,
		PID:         s.PID,
		CreatedAt:   s.CreatedAt,
		StartedAt:   s.StartedAt,
		CompletedAt: s.CompletedAt,
		Metadata:    s.Metadata,
	}
}
