package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hoangndst/buddy-sandboxes/internal/identity"
)

type contextKey string

const (
	tenantNamespaceKey contextKey = "tenantNamespace"
	principalKey       contextKey = "principal"
	roleAdmin                     = identity.RoleAdmin
)

var (
	ErrUnauthorized = errors.New("unauthorized: missing or invalid authentication token")
)

// Authenticator validates request credentials and returns the assigned tenant namespace.
type Authenticator interface {
	Authenticate(r *http.Request) (string, error)
}

type PrincipalAuthenticator interface {
	AuthenticatePrincipal(r *http.Request) (identity.Principal, error)
}

type DatabaseTokenAuthenticator struct {
	store *identity.Store
}

func NewDatabaseTokenAuthenticator(store *identity.Store) *DatabaseTokenAuthenticator {
	return &DatabaseTokenAuthenticator{store: store}
}

func (a *DatabaseTokenAuthenticator) Authenticate(r *http.Request) (string, error) {
	principal, err := a.AuthenticatePrincipal(r)
	return principal.Namespace, err
}

func (a *DatabaseTokenAuthenticator) AuthenticatePrincipal(r *http.Request) (identity.Principal, error) {
	token := bearerToken(r)
	return a.store.Authenticate(r.Context(), token)
}

// StaticTokenAuthenticator maps static pre-shared bearer tokens to Kubernetes namespaces.
type StaticTokenAuthenticator struct {
	tokens map[string]string // token -> namespace
}

// NewStaticTokenAuthenticator creates a token authenticator with the provided mapping.
func NewStaticTokenAuthenticator(tokens map[string]string) *StaticTokenAuthenticator {
	return &StaticTokenAuthenticator{tokens: tokens}
}

// Authenticate extracts the token from Authorization header and resolves namespace.
func (a *StaticTokenAuthenticator) Authenticate(r *http.Request) (string, error) {
	token := bearerToken(r)

	if token == "" {
		return "", ErrUnauthorized
	}

	namespace, ok := a.tokens[token]
	if !ok {
		return "", ErrUnauthorized
	}

	return namespace, nil
}

func bearerToken(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if strings.HasPrefix(value, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	}
	return ""
}

// WithTenantNamespace injects the resolved tenant namespace into the context.
func WithTenantNamespace(ctx context.Context, namespace string) context.Context {
	return context.WithValue(ctx, tenantNamespaceKey, namespace)
}

// TenantNamespaceFromContext extracts the tenant namespace from the context.
func TenantNamespaceFromContext(ctx context.Context) (string, bool) {
	ns, ok := ctx.Value(tenantNamespaceKey).(string)
	return ns, ok
}

func WithPrincipal(ctx context.Context, principal identity.Principal) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

func PrincipalFromContext(ctx context.Context) (identity.Principal, bool) {
	principal, ok := ctx.Value(principalKey).(identity.Principal)
	return principal, ok
}

// AuthMiddleware wraps an http.Handler with token-to-namespace authentication.
func AuthMiddleware(auth Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if principalAuth, ok := auth.(PrincipalAuthenticator); ok {
			principal, err := principalAuth.AuthenticatePrincipal(r)
			if err != nil {
				http.Error(w, ErrUnauthorized.Error(), http.StatusUnauthorized)
				return
			}
			ctx = WithPrincipal(ctx, principal)
			ctx = WithTenantNamespace(ctx, principal.Namespace)
		} else {
			namespace, err := auth.Authenticate(r)
			if err != nil {
				http.Error(w, ErrUnauthorized.Error(), http.StatusUnauthorized)
				return
			}
			ctx = WithTenantNamespace(ctx, namespace)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
