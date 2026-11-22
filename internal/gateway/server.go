package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type activeSession struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

// Server represents the buddy-api gateway HTTP server.
type Server struct {
	httpServer       *http.Server
	client           client.Client
	kubeClient       kubernetes.Interface
	auth             Authenticator
	handlers         *Handlers
	wsHandler        *WSHandler
	isDraining       atomic.Bool
	drainTimeout     time.Duration
	propagationDelay time.Duration
	sessionsMu       sync.Mutex
	sessions         map[string]*activeSession
	sessionWg        sync.WaitGroup
	credentialStore  *CredentialSessionStore
	providerHandler  http.Handler
	brokerState      SandboxAuthorizer
}

// Config configures the Server.
type Config struct {
	ListenAddr        string
	DrainTimeout      time.Duration
	PropagationDelay  time.Duration
	Auth              Authenticator
	Client            client.Client
	KubeClient        kubernetes.Interface
	RemoteExecutor    RemoteStreamExecutor
	PortForwardDialer PortForwardDialer
	CredentialStore   *CredentialSessionStore
	ProviderHandler   http.Handler
	BrokerState       SandboxAuthorizer
}

// NewServer creates a new Server instance.
func NewServer(cfg Config) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 60 * time.Second
	}
	if cfg.PropagationDelay <= 0 {
		cfg.PropagationDelay = 5 * time.Second
	}

	s := &Server{
		client:           cfg.Client,
		kubeClient:       cfg.KubeClient,
		auth:             cfg.Auth,
		drainTimeout:     cfg.DrainTimeout,
		propagationDelay: cfg.PropagationDelay,
		sessions:         make(map[string]*activeSession),
		credentialStore:  cfg.CredentialStore,
		providerHandler:  cfg.ProviderHandler,
		brokerState:      cfg.BrokerState,
	}

	s.handlers = NewHandlers(cfg.Client, cfg.KubeClient, cfg.RemoteExecutor)
	s.wsHandler = NewWSHandler(cfg.Client, s, cfg.RemoteExecutor)
	if cfg.PortForwardDialer != nil {
		s.wsHandler.SetPortForwardDialer(cfg.PortForwardDialer)
	}

	mux := http.NewServeMux()

	// Unauthenticated health endpoints
	mux.HandleFunc("/readyz", s.readyzHandler)
	mux.HandleFunc("/livez", s.livezHandler)
	mux.HandleFunc("/readyz/broker", s.brokerReadyHandler)

	// API endpoints wrapped with authentication
	apiMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/api/v1/auth/whoami" {
			if r.Method == http.MethodGet {
				s.handlers.WhoAmIHandler(w, r)
			} else {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if path == credentialSessionPath || strings.HasPrefix(path, credentialSessionPath+"/") {
			if s.credentialStore == nil {
				http.NotFound(w, r)
				return
			}
			s.credentialStore.CredentialSessionHandler(w, r)
			return
		}

		if path == "/api/v1/sandboxes" {
			switch r.Method {
			case http.MethodPost:
				s.handlers.CreateSandboxHandler(w, r)
			case http.MethodGet:
				s.handlers.ListSandboxesHandler(w, r)
			default:
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		if strings.HasPrefix(path, "/api/v1/sandboxes/") {
			subPath := strings.TrimPrefix(path, "/api/v1/sandboxes/")
			parts := strings.Split(strings.Trim(subPath, "/"), "/")
			if len(parts) == 1 && parts[0] != "" {
				switch r.Method {
				case http.MethodGet:
					s.handlers.GetSandboxHandler(w, r)
				case http.MethodDelete:
					s.handlers.DeleteSandboxHandler(w, r)
				default:
					http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				}
				return
			}
			if len(parts) == 2 && parts[0] != "" {
				switch parts[1] {
				case "suspend":
					s.handlers.SetSandboxSuspensionHandler(w, r, true)
				case "resume":
					s.handlers.SetSandboxSuspensionHandler(w, r, false)
				case "exec":
					s.wsHandler.ExecHandler(w, r)
				case "portforward":
					s.wsHandler.PortForwardHandler(w, r)
				case "logs":
					s.handlers.LogsHandler(w, r)
				case "upload":
					s.handlers.UploadHandler(w, r)
				case "download":
					s.handlers.DownloadHandler(w, r)
				default:
					http.NotFound(w, r)
				}
				return
			}
		}

		http.NotFound(w, r)
	})

	mux.Handle("/api/v1/", AuthMiddleware(cfg.Auth, apiMux))
	if s.credentialStore != nil {
		provider := s.providerHandler
		if provider == nil {
			provider = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
			})
		}
		mux.Handle(providerResponsesPath, s.credentialStore.authenticate(provider))
	}

	s.httpServer = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	return s
}

// SetPortForwardDialer sets a custom dialer on the WebSocket handler.
func (s *Server) SetPortForwardDialer(d PortForwardDialer) {
	s.wsHandler.SetPortForwardDialer(d)
}

// IsDraining returns true if the server is in graceful shutdown mode.
func (s *Server) IsDraining() bool {
	return s.isDraining.Load()
}

// Start initiates listening.
func (s *Server) Start() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown executes the two-phase coordinated drain sequence (DEC-004).
func (s *Server) Shutdown(ctx context.Context) error {
	// Phase 1: Mark draining and delay to allow routing propagation
	s.isDraining.Store(true)

	select {
	case <-time.After(s.propagationDelay):
	case <-ctx.Done():
		return ctx.Err()
	}

	// Phase 2: Close HTTP listener so no new TCP handshakes occur
	_ = s.httpServer.Close()

	// Phase 3: Wait for active sessions up to DrainTimeout
	drainDone := make(chan struct{})
	go func() {
		s.sessionWg.Wait()
		close(drainDone)
	}()

	select {
	case <-drainDone:
		return nil
	case <-time.After(s.drainTimeout):
		// Drain timeout expired: force close lingering WebSocket sessions with code 1001 (Going Away)
		s.sessionsMu.Lock()
		for _, sess := range s.sessions {
			_ = sess.conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseGoingAway, "gateway_draining"),
				time.Now().Add(time.Second),
			)
			sess.cancel()
		}
		s.sessionsMu.Unlock()
		return nil
	}
}

func (s *Server) registerSession(id string, conn *websocket.Conn, cancel context.CancelFunc) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.sessions[id] = &activeSession{conn: conn, cancel: cancel}
	s.sessionWg.Add(1)
}

func (s *Server) deregisterSession(id string) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if _, ok := s.sessions[id]; ok {
		delete(s.sessions, id)
		s.sessionWg.Done()
	}
}

func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if s.IsDraining() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Server is draining", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) livezHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) brokerReadyHandler(w http.ResponseWriter, r *http.Request) {
	if s.IsDraining() || s.credentialStore == nil || s.brokerState == nil || !s.brokerState.Ready() {
		http.Error(w, "Provider broker is not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
