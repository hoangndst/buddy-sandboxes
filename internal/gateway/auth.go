package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type contextKey string

const (
	tenantNamespaceKey contextKey = "tenantNamespace"
)

var (
	ErrUnauthorized = errors.New("unauthorized: missing or invalid authentication token")
)

// Authenticator validates request credentials and returns the assigned tenant namespace.
type Authenticator interface {
	Authenticate(r *http.Request) (string, error)
}

// StaticTokenAuthenticator maps static pre-shared bearer tokens to Kubernetes namespaces.
type StaticTokenAuthenticator struct {
	tokens map[string]string // token -> namespace
}

// NewStaticTokenAuthenticator creates a token authenticator with the provided mapping.
func NewStaticTokenAuthenticator(tokens map[string]string) *StaticTokenAuthenticator {
	return &StaticTokenAuthenticator{tokens: tokens}
}

// Authenticate extracts the token from Authorization header or query parameter and resolves namespace.
func (a *StaticTokenAuthenticator) Authenticate(r *http.Request) (string, error) {
	token := ""

	// Check Authorization header
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimPrefix(authHeader, "Bearer ")
	} else if authHeader != "" {
		token = authHeader
	}

	// Fallback to query parameter (useful for WebSocket handshakes)
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	if token == "" {
		return "", ErrUnauthorized
	}

	namespace, ok := a.tokens[token]
	if !ok {
		return "", ErrUnauthorized
	}

	return namespace, nil
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

// AuthMiddleware wraps an http.Handler with token-to-namespace authentication.
func AuthMiddleware(auth Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		namespace, err := auth.Authenticate(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}

		ctx := WithTenantNamespace(r.Context(), namespace)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
