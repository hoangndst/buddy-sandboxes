package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/sessions"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

const sessionPodUIDMetadataKey = "buddyPodUID"

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
	podName := sbx.Status.PodName
	if podName == "" {
		podName = sbx.Name
	}
	metadata := make(map[string]string, len(req.Metadata)+1)
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	if kube := h.server.kubeClient; kube != nil {
		readCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		if pod, err := kube.CoreV1().Pods(ns).Get(readCtx, podName, metav1.GetOptions{}); err == nil {
			metadata[sessionPodUIDMetadataKey] = string(pod.UID)
		}
		cancel()
	}
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
		Metadata:    metadata,
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
		if sess.State == apiv1.SessionStateTerminated {
			h.memMu.Unlock()
			return
		}
		sess.StartedAt = &now
		sess.State = apiv1.SessionStateRunning
		h.memMu.Unlock()

		if h.store != nil {
			if err := h.store.UpdateSessionState(context.Background(), sessionID, apiv1.SessionStateRunning, nil, nil); err != nil {
				return
			}
		}

		ctx := context.Background()
		podUID := types.UID(sess.Metadata[sessionPodUIDMetadataKey])
		var stdoutBuf, stderrBuf bytes.Buffer
		exitCode, execErr := h.server.remoteExecutor.StreamExec(
			ctx,
			ns,
			podName,
			startCmd,
			false,
			nil,
			&stdoutBuf,
			&stderrBuf,
			nil,
		)
		state := sessionExitState(exitCode)
		result := &exitCode
		if execErr != nil {
			state, result = h.reconcileSupervisorExit(ctx, ns, podName, podUID, sessionID)
			if state == "" {
				state = apiv1.SessionStateFailed
			}
		}

		h.memMu.Lock()
		if sess.State == apiv1.SessionStateTerminated {
			h.memMu.Unlock()
			return
		}

		completedTime := time.Now().UTC()
		sess.CompletedAt = &completedTime
		sess.ExitCode = result
		sess.State = state
		h.memMu.Unlock()

		if h.store != nil {
			_ = h.store.UpdateSessionState(ctx, sessionID, state, result, nil)
		}
	}()

	h.memMu.RLock()
	resp := toSessionResponse(sess)
	h.memMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

func sessionExitState(code int) apiv1.SessionState {
	if code == 0 {
		return apiv1.SessionStateCompleted
	}
	return apiv1.SessionStateFailed
}

func (h *SessionHandlers) reconcileSupervisorExit(
	ctx context.Context, namespace, podName string, podUID types.UID, sessionID string,
) (apiv1.SessionState, *int) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	const maxRetries = 5
	for attempts := 0; attempts <= maxRetries && (h.server == nil || !h.server.IsDraining()); attempts++ {
		if h.sessionPodIsGone(ctx, namespace, podName, podUID) {
			return apiv1.SessionStateTerminated, nil
		}
		if code, ok := readSupervisorExit(ctx, h.server.remoteExecutor, namespace, podName, sessionID); ok {
			return sessionExitState(code), &code
		}
		if attempts == maxRetries {
			break
		}
		select {
		case <-ctx.Done():
			return apiv1.SessionStateFailed, nil
		case <-ticker.C:
		}
	}
	return apiv1.SessionStateFailed, nil
}

func (h *SessionHandlers) sessionPodIsGone(ctx context.Context, namespace, name string, uid types.UID) bool {
	kube := h.server.kubeClient
	if kube == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pod, err := kube.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	return apierrors.IsNotFound(err) || err == nil &&
		(uid != "" && pod.UID != uid || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed)
}

// reconcilePersistedSession refreshes an active session from its sandbox pod and
// runner exit record. This also recovers state after buddy-api restarts, when the
// goroutine that originally launched the session no longer exists.
func (h *SessionHandlers) reconcilePersistedSession(ctx context.Context, sess *sessions.Session) {
	if sess == nil || (sess.State != apiv1.SessionStatePending && sess.State != apiv1.SessionStateRunning) ||
		h.server.client == nil || h.server.kubeClient == nil || h.server.remoteExecutor == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var sandbox sandboxesv1alpha1.Sandbox
	key := types.NamespacedName{Namespace: sess.Namespace, Name: sess.SandboxName}
	if err := h.server.client.Get(ctx, key, &sandbox); err != nil {
		if apierrors.IsNotFound(err) {
			h.persistReconciledState(ctx, sess, apiv1.SessionStateTerminated, nil)
		}
		return
	}
	podName := sandbox.Status.PodName
	if podName == "" {
		podName = sandbox.Name
	}
	pod, err := h.server.kubeClient.CoreV1().Pods(sess.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		h.persistReconciledState(ctx, sess, apiv1.SessionStateTerminated, nil)
		return
	}
	if err != nil {
		return
	}
	if uid := sess.Metadata[sessionPodUIDMetadataKey]; uid != "" && string(pod.UID) != uid {
		h.persistReconciledState(ctx, sess, apiv1.SessionStateTerminated, nil)
		return
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		h.persistReconciledState(ctx, sess, apiv1.SessionStateTerminated, nil)
		return
	}
	if code, ok := readSupervisorExit(ctx, h.server.remoteExecutor, sess.Namespace, podName, sess.ID); ok {
		h.persistReconciledState(ctx, sess, sessionExitState(code), &code)
	}
}

func (h *SessionHandlers) persistReconciledState(
	ctx context.Context, sess *sessions.Session, state apiv1.SessionState, exitCode *int,
) {
	completed := time.Now().UTC()
	if h.store != nil {
		if err := h.store.UpdateSessionState(ctx, sess.ID, state, exitCode, nil); err != nil {
			return
		}
		setSessionCompletion(sess, state, exitCode, completed)
		return
	} else {
		h.memMu.Lock()
		defer h.memMu.Unlock()
		stored := h.memSessions[sess.ID]
		if stored == nil || (stored.State != apiv1.SessionStatePending && stored.State != apiv1.SessionStateRunning) {
			return
		}
		setSessionCompletion(stored, state, exitCode, completed)
		if stored != sess {
			setSessionCompletion(sess, state, exitCode, completed)
		}
	}
}

func setSessionCompletion(sess *sessions.Session, state apiv1.SessionState, exitCode *int, completed time.Time) {
	sess.State = state
	sess.ExitCode = exitCode
	sess.CompletedAt = &completed
	sess.UpdatedAt = completed
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
	for i := range sessionList {
		h.reconcilePersistedSession(r.Context(), &sessionList[i])
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
		if stored := h.memSessions[sessionID]; stored != nil {
			snapshot := *stored
			sess = &snapshot
		}
		h.memMu.RUnlock()
	}

	if sess == nil || sess.Namespace != ns || sess.SandboxName != sandboxName {
		http.NotFound(w, r)
		return
	}
	h.reconcilePersistedSession(r.Context(), sess)

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
		if stored := h.memSessions[sessionID]; stored != nil {
			snapshot := *stored
			sess = &snapshot
		}
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
		code, err := h.server.remoteExecutor.StreamExec(
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
		if err != nil || code != 0 {
			http.Error(w, "Unable to confirm session termination", http.StatusBadGateway)
			return
		}
	}

	if h.store != nil {
		if err := h.store.UpdateSessionState(r.Context(), sessionID, apiv1.SessionStateTerminated, &exitCode, nil); err != nil {
			http.Error(w, "Unable to save session termination", http.StatusInternalServerError)
			return
		}
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

	takeLease := r.URL.Query().Get("take_lease") == queryTrue
	readOnly := r.URL.Query().Get("mode") == "ro"
	noFollow := r.URL.Query().Get("follow") == "false"

	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = wsConn.Close() }()

	sessionCtx, sessionCancel := context.WithCancel(r.Context())
	defer sessionCancel()

	if h.server != nil {
		streamID := generateRandomID("stream")
		h.server.registerSession(streamID, wsConn, sessionCancel)
		defer h.server.deregisterSession(streamID)
	}

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
		NoFollow:    noFollow,
	}

	_ = h.hub.Attach(sessionCtx, wsConn, req)
}

func toSessionResponse(s *sessions.Session) apiv1.SessionResponse {
	metadata := make(map[string]string, len(s.Metadata))
	for key, value := range s.Metadata {
		if key != sessionPodUIDMetadataKey {
			metadata[key] = value
		}
	}
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
		Metadata:    metadata,
	}
}
