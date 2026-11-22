package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const credentialsPath = "/api/v1/credentials"

type providerCredentialStore interface {
	List(context.Context, string) ([]credentials.Metadata, error)
	Set(context.Context, string, string, string, string, string, ...credentials.Options) error
	Delete(context.Context, string, string, string, string) error
}

type credentialAuditStore interface {
	Audit(context.Context, string, string, string, bool) error
}

type CredentialHandlers struct {
	store  providerCredentialStore
	client client.Client
	users  credentialAuditStore
}

func NewCredentialHandlers(store *credentials.Store, kubeClient client.Client, users *identity.Store) *CredentialHandlers {
	return &CredentialHandlers{store: store, client: kubeClient, users: users}
}

func (h *CredentialHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.ID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == credentialsPath && r.Method == http.MethodGet {
		items, err := h.store.List(r.Context(), principal.ID)
		if err != nil {
			http.Error(w, "Unable to list credentials", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	if !strings.HasPrefix(r.URL.Path, credentialsPath+"/") || strings.Contains(strings.TrimPrefix(r.URL.Path, credentialsPath+"/"), "/") {
		http.NotFound(w, r)
		return
	}
	service := strings.TrimPrefix(r.URL.Path, credentialsPath+"/")
	if service != credentials.ServiceOpenAI && service != credentials.ServiceAnthropic {
		http.NotFound(w, r)
		return
	}
	sandboxName := r.URL.Query().Get("sandbox")
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
	switch r.Method {
	case http.MethodPut:
		if sandboxName != "" {
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
			BaseURL:      request.BaseURL,
			DefaultModel: request.DefaultModel,
			AuthType:     request.AuthType,
		}
		if err := h.store.Set(r.Context(), principal.ID, service, scope, sandboxName, request.Value, opts); err != nil {
			http.Error(w, "Invalid or unavailable credential request", http.StatusBadRequest)
			_ = h.users.Audit(r.Context(), principal.ID, "set_provider_credential", service, false)
			return
		}
		_ = h.users.Audit(r.Context(), principal.ID, "set_provider_credential", service+"/"+sandboxName, true)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if r.ContentLength > 0 {
			http.Error(w, "Remove request must not have a body", http.StatusBadRequest)
			return
		}
		if err := h.store.Delete(r.Context(), principal.ID, service, scope, sandboxName); err != nil {
			http.Error(w, "Unable to remove credential", http.StatusInternalServerError)
			return
		}
		_ = h.users.Audit(r.Context(), principal.ID, "remove_provider_credential", service+"/"+sandboxName, true)
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (h *CredentialHandlers) requireOwnedSandbox(r *http.Request, namespace, name string) error {
	if namespace == "" {
		return errors.New("user has no assigned namespace")
	}
	sandbox := &sandboxesv1alpha1.Sandbox{}
	return h.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, sandbox)
}
