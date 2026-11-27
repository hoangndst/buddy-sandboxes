package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/gitbroker"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"github.com/hoangndst/buddy-sandboxes/internal/sessions"
	"golang.org/x/time/rate"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type activeSession struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

// IdentityUserGetter retrieves user identity records by ID.
type IdentityUserGetter interface {
	GetUser(ctx context.Context, userID string) (identity.User, error)
}

// IdentityDataStore provides storage operations for identity and user administration.
type IdentityDataStore interface {
	IdentityUserGetter
	ListUsers(ctx context.Context) ([]identity.User, error)
	NamespaceEnabled(ctx context.Context, namespace string) (bool, error)
	CreateUser(ctx context.Context, displayName, namespace string) (identity.User, error)
	Audit(ctx context.Context, actorID, action, target string, succeeded bool) error
	SetDisabled(ctx context.Context, userID string, disabled bool) error
	SetRole(ctx context.Context, userID string, role identity.Role) error
	IssueToken(ctx context.Context, userID string) (identity.IssuedToken, error)
	RevokeToken(ctx context.Context, userID, tokenID string) error
}

// Server represents the buddy-api gateway HTTP server.
type Server struct {
	httpServer          *http.Server
	router              chi.Router
	sandboxRouter       chi.Router
	humaAPI             huma.API
	client              client.Client
	kubeClient          kubernetes.Interface
	remoteExecutor      RemoteStreamExecutor
	auth                Authenticator
	handlers            *Handlers
	wsHandler           *WSHandler
	sessionHandlers     *SessionHandlers
	sessionHub          *SessionHub
	sessionStore        *sessions.Store
	isDraining          atomic.Bool
	drainTimeout        time.Duration
	propagationDelay    time.Duration
	sessionsMu          sync.Mutex
	sessions            map[string]*activeSession
	sessionWg           sync.WaitGroup
	credentialStore     *CredentialSessionStore
	providerHandler     http.Handler
	anthropicHandler    http.Handler
	brokerState         SandboxAuthorizer
	identityStore       IdentityDataStore
	userGetter          IdentityUserGetter
	identityHandlers    *IdentityHandlers
	credentialHandlers  http.Handler
	providerCredentials *credentials.Store
	gitCredentials      *credentials.GitStore
	gitBrokerHandler    http.Handler
	brokerCapabilities  *credentials.Capabilities
	storageReady        func(context.Context) error
	rateLimiter         *RateLimiter
	taskStore           TaskStore
	taskHandlers        *TaskHandlers
	taskDispatcher      *TaskDispatcher
	dispatcherMu        sync.Mutex
	dispatcherCancel    context.CancelFunc
}

// Config configures the Server.
type Config struct {
	ListenAddr          string
	DrainTimeout        time.Duration
	PropagationDelay    time.Duration
	Auth                Authenticator
	Client              client.Client
	KubeClient          kubernetes.Interface
	RemoteExecutor      RemoteStreamExecutor
	PortForwardDialer   PortForwardDialer
	CredentialStore     *CredentialSessionStore
	ProviderHandler     http.Handler
	AnthropicHandler    http.Handler
	BrokerState         SandboxAuthorizer
	IdentityStore       IdentityDataStore
	UserGetter          IdentityUserGetter
	SessionStore        *sessions.Store
	SessionHub          *SessionHub
	Credentials         http.Handler
	ProviderCredentials *credentials.Store
	GitCredentials      *credentials.GitStore
	GitBrokerHandler    http.Handler
	BrokerCapabilities  *credentials.Capabilities
	StorageReady        func(context.Context) error
	RateLimiter         *RateLimiter
	TaskStore           TaskStore
	TaskDispatcher      *TaskDispatcher
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
		client:              cfg.Client,
		kubeClient:          cfg.KubeClient,
		remoteExecutor:      cfg.RemoteExecutor,
		auth:                cfg.Auth,
		sessionStore:        cfg.SessionStore,
		drainTimeout:        cfg.DrainTimeout,
		propagationDelay:    cfg.PropagationDelay,
		sessions:            make(map[string]*activeSession),
		credentialStore:     cfg.CredentialStore,
		providerHandler:     cfg.ProviderHandler,
		anthropicHandler:    cfg.AnthropicHandler,
		brokerState:         cfg.BrokerState,
		identityStore:       cfg.IdentityStore,
		userGetter:          cfg.UserGetter,
		credentialHandlers:  cfg.Credentials,
		providerCredentials: cfg.ProviderCredentials,
		gitCredentials:      cfg.GitCredentials,
		gitBrokerHandler:    cfg.GitBrokerHandler,
		brokerCapabilities:  cfg.BrokerCapabilities,
		storageReady:        cfg.StorageReady,
		rateLimiter:         cfg.RateLimiter,
	}

	if s.userGetter == nil && cfg.IdentityStore != nil {
		s.userGetter = cfg.IdentityStore
	}

	if s.rateLimiter == nil {
		s.rateLimiter = NewRateLimiter(rate.Limit(1.0), 10) // 60 RPM, burst 10
	}

	s.handlers = NewHandlers(cfg.Client, cfg.KubeClient, cfg.RemoteExecutor)
	if cfg.ProviderCredentials != nil || cfg.BrokerCapabilities != nil {
		s.handlers.SetProviderServices(cfg.ProviderCredentials, cfg.BrokerCapabilities)
	}
	if cfg.GitCredentials != nil {
		s.handlers.SetGitCredentials(cfg.GitCredentials)
	}
	if cfg.IdentityStore != nil {
		s.identityHandlers = NewIdentityHandlers(cfg.IdentityStore)
	}
	s.wsHandler = NewWSHandler(cfg.Client, s, cfg.RemoteExecutor)
	if cfg.PortForwardDialer != nil {
		s.wsHandler.SetPortForwardDialer(cfg.PortForwardDialer)
	}
	if cfg.SessionHub != nil {
		s.sessionHub = cfg.SessionHub
	} else {
		s.sessionHub = NewSessionHub(cfg.RemoteExecutor, cfg.SessionStore)
	}
	s.sessionHandlers = NewSessionHandlers(s, cfg.SessionStore, s.sessionHub)

	s.taskStore = cfg.TaskStore
	if s.taskStore != nil {
		if cfg.TaskDispatcher != nil {
			s.taskDispatcher = cfg.TaskDispatcher
		} else {
			s.taskDispatcher = NewTaskDispatcher(s.taskStore, s.client, "")
		}
		s.taskHandlers = NewTaskHandlers(s, s.taskStore, s.taskDispatcher)
	}

	r := chi.NewRouter()
	r.Use(middleware.CleanPath)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway route not found", http.StatusNotFound)
	})

	// Unauthenticated health endpoints
	r.HandleFunc("/readyz", s.readyzHandler)
	r.HandleFunc("/livez", s.livezHandler)
	r.HandleFunc("/readyz/broker", s.brokerReadyHandler)
	if s.gitBrokerHandler != nil {
		r.Handle(gitbroker.RedeemPath, s.gitBrokerHandler)
		r.Handle(gitbroker.CompletePath, s.gitBrokerHandler)
	}

	// Model provider proxy routes
	s.registerProviderRoutes(r)

	// Configure and register OpenAPI 3.1 & interactive docs
	humaConfig := NewHumaConfig()
	docRouter := chi.NewRouter()
	humaAPI := humachi.New(docRouter, humaConfig)
	RegisterOpenAPIOperations(humaAPI)
	s.humaAPI = humaAPI
	s.router = r

	// Mount OpenAPI and interactive documentation endpoints
	r.Handle("/openapi.json", docRouter)
	r.Handle("/openapi.yaml", docRouter)
	r.Handle("/docs", docRouter)
	r.Handle("/schemas/*", docRouter)

	// Sandbox subrouter
	sandboxRouter := chi.NewRouter()
	sandboxRouter.Post("/", s.handlers.CreateSandboxHandler)
	sandboxRouter.Get("/", s.handlers.ListSandboxesHandler)
	sandboxRouter.Route("/{name}", func(sbx chi.Router) {
		sbx.Get("/", s.handlers.GetSandboxHandler)
		sbx.Delete("/", s.handlers.DeleteSandboxHandler)
		sbx.Post("/suspend", func(w http.ResponseWriter, req *http.Request) {
			s.handlers.SetSandboxSuspensionHandler(w, req, true)
		})
		sbx.Post("/resume", func(w http.ResponseWriter, req *http.Request) {
			s.handlers.SetSandboxSuspensionHandler(w, req, false)
		})
		sbx.Get("/logs", s.handlers.LogsHandler)
		sbx.Post("/upload", s.handlers.UploadHandler)
		sbx.Get("/download", s.handlers.DownloadHandler)
		sbx.Get("/exec", s.wsHandler.ExecHandler)
		sbx.Get("/portforward", s.wsHandler.PortForwardHandler)

		// Sessions under sandbox
		sbx.Route("/sessions", func(sess chi.Router) {
			sess.Post("/", func(w http.ResponseWriter, req *http.Request) {
				s.sessionHandlers.CreateSession(w, req, chi.URLParam(req, "name"))
			})
			sess.Get("/", func(w http.ResponseWriter, req *http.Request) {
				s.sessionHandlers.ListSessions(w, req, chi.URLParam(req, "name"))
			})
			sess.Route("/{sessionID}", func(singleSess chi.Router) {
				singleSess.Get("/", func(w http.ResponseWriter, req *http.Request) {
					s.sessionHandlers.GetSession(w, req, chi.URLParam(req, "name"), chi.URLParam(req, "sessionID"))
				})
				singleSess.Delete("/", func(w http.ResponseWriter, req *http.Request) {
					s.sessionHandlers.DeleteSession(w, req, chi.URLParam(req, "name"), chi.URLParam(req, "sessionID"))
				})
				singleSess.Get("/stream", func(w http.ResponseWriter, req *http.Request) {
					s.sessionHandlers.StreamSession(w, req, chi.URLParam(req, "name"), chi.URLParam(req, "sessionID"))
				})
			})
		})
	})
	s.sandboxRouter = sandboxRouter

	// API v1 routes wrapped in Auth and RateLimiter
	r.Route("/api/v1", func(sub chi.Router) {
		sub.Use(func(next http.Handler) http.Handler {
			return AuthMiddleware(cfg.Auth, RateLimitMiddleware(s.rateLimiter, next))
		})

		// 1. Identity / Whoami
		sub.Get("/auth/whoami", s.handlers.WhoAmIHandler)

		// 2. Admin users
		sub.Route("/admin/users", func(users chi.Router) {
			users.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					actor, ok := PrincipalFromContext(r.Context())
					if !ok || actor.Role != roleAdmin {
						http.Error(w, "Forbidden", http.StatusForbidden)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			users.Get("/", func(w http.ResponseWriter, r *http.Request) {
				if s.identityHandlers == nil {
					http.NotFound(w, r)
					return
				}
				s.identityHandlers.ListUsers(w, r)
			})
			users.Post("/", func(w http.ResponseWriter, r *http.Request) {
				if s.identityHandlers == nil {
					http.NotFound(w, r)
					return
				}
				actor, _ := PrincipalFromContext(r.Context())
				s.identityHandlers.CreateUser(w, r, actor.ID)
			})
			users.Route("/{id}", func(user chi.Router) {
				user.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						id := chi.URLParam(r, "id")
						if !userIDPattern.MatchString(id) {
							http.NotFound(w, r)
							return
						}
						next.ServeHTTP(w, r)
					})
				})
				user.Patch("/", func(w http.ResponseWriter, r *http.Request) {
					if s.identityHandlers == nil {
						http.NotFound(w, r)
						return
					}
					actor, _ := PrincipalFromContext(r.Context())
					s.identityHandlers.UpdateStatus(w, r, actor.ID, chi.URLParam(r, "id"))
				})
				user.Patch("/role", func(w http.ResponseWriter, r *http.Request) {
					if s.identityHandlers == nil {
						http.NotFound(w, r)
						return
					}
					actor, _ := PrincipalFromContext(r.Context())
					s.identityHandlers.UpdateRole(w, r, actor.ID, chi.URLParam(r, "id"))
				})
				user.Post("/tokens", func(w http.ResponseWriter, r *http.Request) {
					if s.identityHandlers == nil {
						http.NotFound(w, r)
						return
					}
					actor, _ := PrincipalFromContext(r.Context())
					s.identityHandlers.IssueToken(w, r, actor.ID, chi.URLParam(r, "id"))
				})
				user.Delete("/tokens/{tokenID}", func(w http.ResponseWriter, r *http.Request) {
					if s.identityHandlers == nil {
						http.NotFound(w, r)
						return
					}
					actor, _ := PrincipalFromContext(r.Context())
					s.identityHandlers.RevokeToken(w, r, actor.ID, chi.URLParam(r, "id"), chi.URLParam(r, "tokenID"))
				})
				user.HandleFunc("/sandboxes", s.adminUserSandboxesHandler)
				user.HandleFunc("/sandboxes/*", s.adminUserSandboxesHandler)
			})
		})

		// 3. Credentials
		if s.credentialHandlers != nil {
			if ch, ok := s.credentialHandlers.(*CredentialHandlers); ok {
				sub.Mount("/credentials", ch.CredentialRouter())
				sub.Mount("/git-credentials", ch.GitCredentialRouter())
			} else {
				sub.Mount("/credentials", s.credentialHandlers)
				sub.Mount("/git-credentials", s.credentialHandlers)
			}
		}

		// 4. Tasks, Executions, Agent Profiles
		if s.taskHandlers != nil {
			sub.Mount("/tasks", s.taskHandlers.TaskRouter())
			sub.Mount("/executions", s.taskHandlers.ExecutionRouter())
			sub.Mount("/agent-profiles", s.taskHandlers.AgentProfileRouter())
		}

		// 5. Sandboxes
		sub.Mount("/sandboxes", sandboxRouter)
	})

	s.httpServer = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: r,
	}

	return s
}

func (s *Server) adminUserSandboxesHandler(w http.ResponseWriter, r *http.Request) {
	if s.userGetter == nil && s.identityStore == nil {
		http.NotFound(w, r)
		return
	}
	userID := chi.URLParam(r, "id")
	if !userIDPattern.MatchString(userID) {
		http.NotFound(w, r)
		return
	}
	target, err := s.getUser(r.Context(), userID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	subPath := "/" + chi.URLParam(r, "*")
	rctx := chi.NewRouteContext()
	targetCtx := context.WithValue(WithTenantNamespace(WithPrincipal(r.Context(), target.Principal), target.Namespace), chi.RouteCtxKey, rctx)
	copyReq := r.Clone(targetCtx)
	copyReq.URL.Path = subPath
	copyReq.URL.RawPath = subPath
	s.sandboxRouter.ServeHTTP(w, copyReq)
}

func (s *Server) getUser(ctx context.Context, userID string) (identity.User, error) {
	if s.userGetter != nil {
		return s.userGetter.GetUser(ctx, userID)
	}
	if s.identityStore != nil {
		return s.identityStore.GetUser(ctx, userID)
	}
	return identity.User{}, errors.New("identity store unavailable")
}

func (s *Server) registerProviderRoutes(r chi.Router) {
	if s.credentialStore != nil && s.providerCredentials != nil && s.brokerCapabilities != nil && s.brokerState != nil {
		provider := s.providerHandler
		if provider == nil {
			provider = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
			})
		}
		r.Handle(providerResponsesPath, s.credentialStore.authenticateCapability(provider, s.brokerCapabilities, s.providerCredentials, s.brokerState))
		anthropic := s.anthropicHandler
		if anthropic == nil {
			anthropic = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
			})
		}
		r.Handle(providerAnthropicMessagesPath, s.credentialStore.authenticateCapability(anthropic, s.brokerCapabilities, s.providerCredentials, s.brokerState))
	}
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
	if s.taskDispatcher != nil {
		s.dispatcherMu.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		s.dispatcherCancel = cancel
		s.dispatcherMu.Unlock()
		s.taskDispatcher.Start(ctx)
	}
	err := s.httpServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed && s.taskDispatcher != nil {
		_ = s.taskDispatcher.Stop(context.Background())
		s.dispatcherMu.Lock()
		if s.dispatcherCancel != nil {
			s.dispatcherCancel()
			s.dispatcherCancel = nil
		}
		s.dispatcherMu.Unlock()
	}
	return err
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

	if s.taskDispatcher != nil {
		_ = s.taskDispatcher.Stop(ctx)
		s.dispatcherMu.Lock()
		if s.dispatcherCancel != nil {
			s.dispatcherCancel()
			s.dispatcherCancel = nil
		}
		s.dispatcherMu.Unlock()
	}

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
	if s.storageReady != nil {
		if err := s.storageReady(r.Context()); err != nil {
			http.Error(w, "Identity storage is unavailable", http.StatusServiceUnavailable)
			return
		}
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
