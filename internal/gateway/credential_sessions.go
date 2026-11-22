package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
)

const (
	providerResponsesPath         = "/api/v1/provider/openai/v1/responses"
	providerAnthropicMessagesPath = "/api/v1/provider/anthropic/v1/messages"
)

type credentialSessionKey struct {
	namespace  string
	sandbox    string
	creationID string
	service    string
}

type SandboxUIDAuthorizer interface {
	AllowsSandbox(namespace, name, uid, creationID string) bool
}

type brokerCapabilityAuthenticator interface {
	Authenticate(context.Context, string, string, string) (credentials.Capability, error)
}

type providerCredentialResolver interface {
	GetForSandbox(context.Context, string, string, string) (string, error)
}

type credentialSession struct {
	key            credentialSessionKey
	tokenHash      [sha256.Size]byte
	expiresAt      time.Time
	reservedTokens int64
	inFlight       int
	requestTimes   []time.Time
}

// CredentialSessionStore keeps short-lived request budgets in process memory, keyed by capability hash.
type CredentialSessionStore struct {
	mu      sync.Mutex
	byKey   map[credentialSessionKey]*credentialSession
	byToken map[[sha256.Size]byte]*credentialSession
	now     func() time.Time
}

// NewCredentialSessionStore creates an empty in-memory credential store.
func NewCredentialSessionStore() *CredentialSessionStore {
	return &CredentialSessionStore{
		byKey:   make(map[credentialSessionKey]*credentialSession),
		byToken: make(map[[sha256.Size]byte]*credentialSession),
		now:     time.Now,
	}
}

func (s *CredentialSessionStore) authenticateCapability(
	next http.Handler,
	capabilities brokerCapabilityAuthenticator,
	providerCredentials providerCredentialResolver,
	state SandboxAuthorizer,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.RawPath != "" || r.RequestURI != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		token, ok := capabilityToken(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		capability, err := capabilities.Authenticate(r.Context(), token, r.Method, r.URL.Path)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		allowed := state.Allows(capability.Namespace, capability.SandboxName, capability.CreationID)
		if uidState, ok := state.(SandboxUIDAuthorizer); ok {
			allowed = uidState.AllowsSandbox(capability.Namespace, capability.SandboxName, capability.SandboxUID, capability.CreationID)
		}
		if !allowed {
			http.Error(w, "Provider service is unavailable", http.StatusServiceUnavailable)
			return
		}
		providerKey, err := providerCredentials.GetForSandbox(r.Context(), capability.UserID, capability.Service, capability.SandboxName)
		if err != nil {
			http.Error(w, "Provider credential is unavailable", http.StatusUnauthorized)
			return
		}
		session := s.bindCapability(token, capability)
		ctx := context.WithValue(r.Context(), providerKeyContextKey{}, providerKey)
		next.ServeHTTP(w, r.WithContext(contextWithCredentialSession(r.WithContext(ctx), session)))
	})
}

func capabilityToken(r *http.Request) (string, bool) {
	switch r.URL.Path {
	case providerAnthropicMessagesPath:
		if len(r.Header.Values("Authorization")) != 0 {
			return "", false
		}
		values := r.Header.Values("x-api-key")
		if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] {
			return "", false
		}
		return values[0], true
	case providerResponsesPath:
		if len(r.Header.Values("x-api-key")) != 0 {
			return "", false
		}
		values := r.Header.Values("Authorization")
		if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) <= len("Bearer ") {
			return "", false
		}
		token := strings.TrimPrefix(values[0], "Bearer ")
		if strings.TrimSpace(token) != token {
			return "", false
		}
		return token, true
	default:
		return "", false
	}
}

func (s *CredentialSessionStore) bindCapability(token string, capability credentials.Capability) *credentialSession {
	tokenHash := sha256.Sum256([]byte(token))
	key := credentialSessionKey{namespace: capability.Namespace, sandbox: capability.SandboxName, creationID: capability.CreationID, service: capability.Service}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.byToken[tokenHash]; existing != nil {
		existing.expiresAt = capability.ExpiresAt
		return existing
	}
	session := &credentialSession{key: key, tokenHash: tokenHash, expiresAt: capability.ExpiresAt}
	s.byToken[tokenHash] = session
	s.byKey[key] = session
	return session
}

type providerKeyContextKey struct{}

func providerKeyFromContext(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(providerKeyContextKey{}).(string)
	return key, ok && key != ""
}

func (s *CredentialSessionStore) reserve(session *credentialSession, tokens int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey[session.key] != session || !s.now().Before(session.expiresAt) || session.inFlight >= 4 || session.reservedTokens+tokens > 1_000_000 {
		return false
	}
	now := s.now()
	cutoff := now.Add(-time.Minute)
	kept := session.requestTimes[:0]
	for _, at := range session.requestTimes {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= 10 {
		session.requestTimes = kept
		return false
	}
	session.requestTimes = append(kept, now)
	session.reservedTokens += tokens
	session.inFlight++
	return true
}

func (s *CredentialSessionStore) isActive(session *credentialSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey[session.key] != session || !s.now().Before(session.expiresAt) {
		return false
	}
	return true
}

// Revoke removes one namespace and creation-scoped session if it exists.
func (s *CredentialSessionStore) Revoke(namespace, sandboxName, creationID string) {
	s.mu.Lock()
	for key, session := range s.byKey {
		if key.namespace != namespace || key.sandbox != sandboxName || key.creationID != creationID {
			continue
		}
		delete(s.byToken, session.tokenHash)
		delete(s.byKey, key)
	}
	s.mu.Unlock()
}

func (s *CredentialSessionStore) finishRequest(session *credentialSession) {
	s.mu.Lock()
	if session.inFlight > 0 {
		session.inFlight--
	}
	s.mu.Unlock()
}

func (s *CredentialSessionStore) reconcileReservation(session *credentialSession, reserved, used int64) {
	if used < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if used < reserved {
		delta := reserved - used
		if delta > session.reservedTokens {
			session.reservedTokens = 0
		} else {
			session.reservedTokens -= delta
		}
		return
	}
	session.reservedTokens += used - reserved
}

type credentialSessionContextKey struct{}

func contextWithCredentialSession(r *http.Request, session *credentialSession) context.Context {
	return context.WithValue(r.Context(), credentialSessionContextKey{}, session)
}

func credentialSessionFromContext(ctx context.Context) (*credentialSession, bool) {
	session, ok := ctx.Value(credentialSessionContextKey{}).(*credentialSession)
	return session, ok
}

// validateJSONNoDuplicateKeys rejects duplicate object keys before decoding into a typed request.
func validateJSONNoDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := readJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

func readJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("invalid JSON object key")
			}
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := readJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := readJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
}
