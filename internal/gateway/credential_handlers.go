package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const credentialsPath = "/api/v1/credentials"

type providerCredentialStore interface {
	List(context.Context, string) ([]credentials.Metadata, error)
	SetAudited(context.Context, string, string, string, string, string, ...credentials.Options) error
	DeleteAudited(context.Context, string, string, string, string) error
}

type credentialAuditStore interface {
	Audit(context.Context, string, string, string, bool) error
}

type gitCredentialStore interface {
	List(context.Context, string) ([]credentials.GitMetadata, error)
	SetAudited(context.Context, string, string, string, string, string) error
	DeleteAudited(context.Context, string, string, string, string) error
}

type CredentialHandlers struct {
	store  providerCredentialStore
	client client.Client
	users  credentialAuditStore
	git    gitCredentialStore
}

func (h *CredentialHandlers) SetGitStore(store gitCredentialStore) { h.git = store }

func NewCredentialHandlers(store *credentials.Store, kubeClient client.Client, users *identity.Store) *CredentialHandlers {
	return &CredentialHandlers{store: store, client: kubeClient, users: users}
}

// CredentialRouter returns a Chi router for provider credentials.
func (h *CredentialHandlers) CredentialRouter() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.ListCredentialsHandler)
	r.Route("/{service}", func(srv chi.Router) {
		srv.Put("/", h.SetCredentialHandler)
		srv.Delete("/", h.DeleteCredentialHandler)
	})
	return r
}

// GitCredentialRouter returns a Chi router for git credentials.
func (h *CredentialHandlers) GitCredentialRouter() chi.Router {
	r := chi.NewRouter()
	r.Put("/", h.SetGitCredentialHandler)
	r.Delete("/", h.DeleteGitCredentialHandler)
	return r
}

func (h *CredentialHandlers) getRouter() chi.Router {
	r := chi.NewRouter()
	r.Mount("/api/v1/credentials", h.CredentialRouter())
	r.Mount("/api/v1/git-credentials", h.GitCredentialRouter())
	r.Mount("/credentials", h.CredentialRouter())
	r.Mount("/git-credentials", h.GitCredentialRouter())
	return r
}

func (h *CredentialHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.getRouter().ServeHTTP(w, r)
}

func (h *CredentialHandlers) ListCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	h.listCredentials(w, r, principal)
}

func (h *CredentialHandlers) SetCredentialHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	service := chi.URLParam(r, "service")
	if service == "" {
		service = strings.TrimPrefix(r.URL.Path, credentialsPath+"/")
	}
	descriptor, ok := credentials.LookupService(service)
	if !ok || descriptor.Kind != credentials.ServiceKindProvider {
		http.NotFound(w, r)
		return
	}
	sandboxName := r.URL.Query().Get("sandbox")
	if sandboxName != "" && !descriptor.SupportsSandboxScope {
		http.Error(w, "Credential service does not support sandbox scope", http.StatusBadRequest)
		return
	}
	if r.URL.RawQuery != "" {
		if sandboxName == "" || len(r.URL.Query()) != 1 {
			http.Error(w, "Invalid credential scope", http.StatusBadRequest)
			return
		}
		if err := validateSandboxName(sandboxName); err != nil {
			http.Error(w, "Invalid credential scope", http.StatusBadRequest)
			return
		}
	}
	scope := credentials.ScopeUser
	if sandboxName != "" {
		scope = credentials.ScopeSandbox
		if err := h.requireOwnedSandbox(r, principal.Namespace, sandboxName); err != nil {
			http.Error(w, "Sandbox not found", http.StatusNotFound)
			return
		}
	}
	var request apiv1.SetCredentialRequest
	if !decodeIdentityRequest(w, r, &request) {
		return
	}
	opts := credentials.Options{
		BaseURL:           request.BaseURL,
		DefaultModel:      request.DefaultModel,
		AuthType:          request.AuthType,
		RequestsPerMinute: request.RequestsPerMinute,
		MaxConcurrent:     request.MaxConcurrent,
		TokensPerMinute:   request.TokensPerMinute,
	}
	if err := h.store.SetAudited(r.Context(), principal.ID, service, scope, sandboxName, request.Value, opts); err != nil {
		if errors.Is(err, credentials.ErrAuditUnavailable) {
			slog.Error("credential mutation rolled back because its audit event could not be stored", "service", service, "operation", "set")
			http.Error(w, "Credential change was not saved because audit storage is unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "Invalid or unavailable credential request", http.StatusBadRequest)
		h.recordFailedCredentialAttempt(r.Context(), principal.ID, "set_service_credential", service)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *CredentialHandlers) DeleteCredentialHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	service := chi.URLParam(r, "service")
	if service == "" {
		service = strings.TrimPrefix(r.URL.Path, credentialsPath+"/")
	}
	descriptor, ok := credentials.LookupService(service)
	if !ok || descriptor.Kind != credentials.ServiceKindProvider {
		http.NotFound(w, r)
		return
	}
	sandboxName := r.URL.Query().Get("sandbox")
	if sandboxName != "" && !descriptor.SupportsSandboxScope {
		http.Error(w, "Credential service does not support sandbox scope", http.StatusBadRequest)
		return
	}
	if r.URL.RawQuery != "" {
		if sandboxName == "" || len(r.URL.Query()) != 1 {
			http.Error(w, "Invalid credential scope", http.StatusBadRequest)
			return
		}
		if err := validateSandboxName(sandboxName); err != nil {
			http.Error(w, "Invalid credential scope", http.StatusBadRequest)
			return
		}
	}
	scope := credentials.ScopeUser
	if sandboxName != "" {
		scope = credentials.ScopeSandbox
	}
	if r.ContentLength > 0 {
		http.Error(w, "Remove request must not have a body", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteAudited(r.Context(), principal.ID, service, scope, sandboxName); err != nil {
		if errors.Is(err, credentials.ErrAuditUnavailable) {
			slog.Error("credential removal rolled back because its audit event could not be stored", "service", service, "operation", "remove")
			http.Error(w, "Credential change was not saved because audit storage is unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "Unable to remove credential", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *CredentialHandlers) SetGitCredentialHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	h.serveGitCredentials(w, r, principal)
}

func (h *CredentialHandlers) DeleteGitCredentialHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	h.serveGitCredentials(w, r, principal)
}

func (h *CredentialHandlers) listCredentials(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
	items, err := h.store.List(r.Context(), principal.ID)
	if err != nil {
		http.Error(w, "Unable to list credentials", http.StatusInternalServerError)
		return
	}
	combined := make([]apiv1.CredentialMetadata, 0, len(items))
	for _, item := range items {
		descriptor, ok := credentials.LookupService(item.Service)
		if ok && descriptor.Kind == credentials.ServiceKindProvider {
			combined = append(combined, apiv1.CredentialMetadata{
				Service:           item.Service,
				Scope:             item.Scope,
				SandboxName:       item.SandboxName,
				BaseURL:           item.BaseURL,
				DefaultModel:      item.DefaultModel,
				AuthType:          item.AuthType,
				RequestsPerMinute: item.RequestsPerMinute,
				MaxConcurrent:     item.MaxConcurrent,
				TokensPerMinute:   item.TokensPerMinute,
				UpdatedAt:         item.UpdatedAt,
			})
		}
	}
	if h.git != nil {
		gitItems, err := h.git.List(r.Context(), principal.ID)
		if err != nil {
			http.Error(w, "Unable to list credentials", http.StatusInternalServerError)
			return
		}
		for _, item := range gitItems {
			descriptor, ok := credentials.LookupService(item.Service)
			if ok && descriptor.Kind == credentials.ServiceKindGitRepository {
				combined = append(combined, apiv1.CredentialMetadata{Service: item.Service, Scope: item.Scope,
					SandboxName: item.SandboxName, Repository: item.Repository, UpdatedAt: item.UpdatedAt})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": combined})
}

func (h *CredentialHandlers) serveGitCredentials(w http.ResponseWriter, r *http.Request, principal identity.Principal) {
	descriptor, registered := credentials.LookupService(credentials.ServiceGitHub)
	if !registered || descriptor.Kind != credentials.ServiceKindGitRepository || h.git == nil {
		http.Error(w, "Git credential store is unavailable", http.StatusServiceUnavailable)
		return
	}
	query := r.URL.Query()
	repository := query.Get("repository")
	sandboxName := query.Get("sandbox")
	repositoryValues, hasSandbox := query["sandbox"]
	if repository == "" || len(query["repository"]) != 1 ||
		(len(query) == 1 && hasSandbox) ||
		(len(query) == 2 && (!hasSandbox || len(repositoryValues) != 1 || sandboxName == "")) || len(query) > 2 {
		http.Error(w, "Repository and optional sandbox scope are required", http.StatusBadRequest)
		return
	}
	if sandboxName != "" {
		if err := validateSandboxName(sandboxName); err != nil {
			http.Error(w, "Invalid credential scope", http.StatusBadRequest)
			return
		}
	}
	scope := credentials.ScopeUser
	if sandboxName != "" {
		scope = credentials.ScopeSandbox
	}
	switch r.Method {
	case http.MethodPut:
		if r.ContentLength > credentials.MaxCredentialLen+1024 {
			http.Error(w, "Credential request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		var request apiv1.SetGitCredentialRequest
		if !decodeIdentityRequest(w, r, &request) {
			return
		}
		if err := h.git.SetAudited(r.Context(), principal.ID, repository, scope, sandboxName, request.Value); err != nil {
			if errors.Is(err, credentials.ErrAuditUnavailable) {
				slog.Error("credential mutation rolled back because its audit event could not be stored", "service", credentials.ServiceGitHub, "operation", "set")
				http.Error(w, "Credential change was not saved because audit storage is unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "Invalid GitHub credential request", http.StatusBadRequest)
			h.recordFailedCredentialAttempt(r.Context(), principal.ID, "set_service_credential", credentials.ServiceGitHub)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if r.ContentLength > 0 {
			http.Error(w, "Remove request must not have a body", http.StatusBadRequest)
			return
		}
		if err := h.git.DeleteAudited(r.Context(), principal.ID, repository, scope, sandboxName); err != nil {
			if errors.Is(err, credentials.ErrAuditUnavailable) {
				slog.Error("credential removal rolled back because its audit event could not be stored", "service", credentials.ServiceGitHub, "operation", "remove")
				http.Error(w, "Credential change was not saved because audit storage is unavailable", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "Unable to remove GitHub credential", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (h *CredentialHandlers) recordFailedCredentialAttempt(ctx context.Context, actorID, action, service string) {
	if h.users == nil {
		return
	}
	if err := h.users.Audit(ctx, actorID, action, service, false); err != nil {
		slog.Error("failed to record rejected credential mutation audit event", "service", service, "operation", action)
	}
}

func (h *CredentialHandlers) requireOwnedSandbox(r *http.Request, namespace, name string) error {
	if namespace == "" {
		return errors.New("user has no assigned namespace")
	}
	sandbox := &sandboxesv1alpha1.Sandbox{}
	return h.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, sandbox)
}
