package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/hoangndst/buddy-sandboxes/internal/identity"
)

const maxIdentityRequestBytes = 8 << 10

var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

type IdentityHandlers struct {
	store IdentityDataStore
}

func NewIdentityHandlers(store IdentityDataStore) *IdentityHandlers {
	return &IdentityHandlers{store: store}
}

func (h *IdentityHandlers) AdminUsersHandler(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Role != identity.RoleAdmin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/users")
	if path == "" || path == "/" {
		switch r.Method {
		case http.MethodGet:
			h.ListUsers(w, r)
		case http.MethodPost:
			h.CreateUser(w, r, principal.ID)
		default:
			methodNotAllowed(w)
		}
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || !userIDPattern.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	userID := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodPatch {
			methodNotAllowed(w)
			return
		}
		h.UpdateStatus(w, r, principal.ID, userID)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "role" && r.Method == http.MethodPatch:
		h.UpdateRole(w, r, principal.ID, userID)
	case len(parts) == 2 && parts[1] == "tokens" && r.Method == http.MethodPost:
		h.IssueToken(w, r, principal.ID, userID)
	case len(parts) == 3 && parts[1] == "tokens" && r.Method == http.MethodDelete:
		h.RevokeToken(w, r, principal.ID, userID, parts[2])
	default:
		http.NotFound(w, r)
	}
}

// ListUsers retrieves all registered users.
func (h *IdentityHandlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "Unable to list users", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": users})
}

// CreateUser provisions a new tenant user.
func (h *IdentityHandlers) CreateUser(w http.ResponseWriter, r *http.Request, actorID string) {
	var req struct {
		DisplayName string `json:"displayName"`
		Namespace   string `json:"namespace"`
	}
	if !decodeIdentityRequest(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" || len(req.DisplayName) > 80 || !validNamespace(req.Namespace) {
		http.Error(w, "Invalid user request", http.StatusBadRequest)
		return
	}
	enabled, err := h.store.NamespaceEnabled(r.Context(), req.Namespace)
	if err != nil {
		http.Error(w, "Unable to validate namespace", http.StatusInternalServerError)
		return
	}
	if !enabled {
		http.Error(w, "Namespace is not enabled", http.StatusConflict)
		return
	}
	user, err := h.store.CreateUser(r.Context(), strings.TrimSpace(req.DisplayName), req.Namespace)
	if err != nil {
		http.Error(w, "Unable to create user", http.StatusConflict)
		return
	}
	_ = h.store.Audit(r.Context(), actorID, "create_user", user.ID, true)
	writeJSON(w, http.StatusCreated, user)
}

// UpdateStatus sets disabled status for a user.
func (h *IdentityHandlers) UpdateStatus(w http.ResponseWriter, r *http.Request, actorID, userID string) {
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if !decodeIdentityRequest(w, r, &req) {
		return
	}
	err := h.store.SetDisabled(r.Context(), userID, req.Disabled)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = h.store.Audit(r.Context(), actorID, "set_user_disabled", userID, true)
	w.WriteHeader(http.StatusNoContent)
}

// UpdateRole updates the role for a user.
func (h *IdentityHandlers) UpdateRole(w http.ResponseWriter, r *http.Request, actorID, userID string) {
	var req struct {
		Role identity.Role `json:"role"`
	}
	if !decodeIdentityRequest(w, r, &req) {
		return
	}
	if req.Role != identity.RoleAdmin && req.Role != identity.RoleUser {
		http.Error(w, "Invalid user role", http.StatusBadRequest)
		return
	}
	if err := h.store.SetRole(r.Context(), userID, req.Role); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = h.store.Audit(r.Context(), actorID, "set_user_role", userID, true)
	w.WriteHeader(http.StatusNoContent)
}

// IssueToken generates a new token for a user.
func (h *IdentityHandlers) IssueToken(w http.ResponseWriter, r *http.Request, actorID, userID string) {
	token, err := h.store.IssueToken(r.Context(), userID)
	if err != nil {
		http.Error(w, "Unable to issue token", http.StatusConflict)
		return
	}
	_ = h.store.Audit(r.Context(), actorID, "issue_user_token", userID, true)
	writeJSON(w, http.StatusCreated, map[string]any{"id": token.ID, "token": token.Value, "expiresAt": token.ExpiresAt})
}

// RevokeToken invalidates a user token.
func (h *IdentityHandlers) RevokeToken(w http.ResponseWriter, r *http.Request, actorID, userID, tokenID string) {
	if !userIDPattern.MatchString(tokenID) {
		http.NotFound(w, r)
		return
	}
	if err := h.store.RevokeToken(r.Context(), userID, tokenID); err != nil {
		http.Error(w, "Unable to revoke token", http.StatusInternalServerError)
		return
	}
	_ = h.store.Audit(r.Context(), actorID, "revoke_user_token", userID+"/"+tokenID, true)
	w.WriteHeader(http.StatusNoContent)
}

func decodeIdentityRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxIdentityRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func validNamespace(namespace string) bool {
	return len(namespace) > 0 && len(namespace) <= 63 && dnsSubdomainRegex.MatchString(namespace)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}
