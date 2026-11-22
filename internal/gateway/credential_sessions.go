package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	credentialSessionPath = "/api/v1/credential-sessions"
	providerResponsesPath = "/api/v1/provider/openai/v1/responses"
	maxRegistrationBytes  = 8 << 10
	maxSessionLifetime    = 2 * time.Hour
)

type credentialSessionKey struct {
	namespace  string
	sandbox    string
	creationID string
}

type credentialSession struct {
	key            credentialSessionKey
	tokenHash      [sha256.Size]byte
	openAIKey      string
	expiresAt      time.Time
	reservedTokens int64
	inFlight       int
	requestTimes   []time.Time
}

// CredentialSessionStore keeps provider keys in process memory and indexes session tokens by hash.
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

type registerCredentialSessionRequest struct {
	SandboxName        string `json:"sandboxName"`
	SessionToken       string `json:"sessionToken"`
	CreationID         string `json:"creationID"`
	OpenAIKey          string `json:"openAIKey"`
	MaxLifetimeSeconds int64  `json:"maxLifetimeSeconds,omitempty"`
}

// CredentialSessionHandler serves authenticated session registration and revocation routes.
func (s *CredentialSessionStore) CredentialSessionHandler(w http.ResponseWriter, r *http.Request) {
	namespace, ok := TenantNamespaceFromContext(r.Context())
	if !ok || namespace == "" {
		http.Error(w, "Tenant namespace not resolved", http.StatusUnauthorized)
		return
	}
	if r.URL.RawQuery != "" || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.URL.Path == credentialSessionPath && r.Method == http.MethodPost:
		s.register(w, r, namespace)
	case strings.HasPrefix(r.URL.Path, credentialSessionPath+"/") && r.Method == http.MethodDelete:
		s.revoke(w, r, namespace)
	default:
		http.NotFound(w, r)
	}
}

func (s *CredentialSessionStore) register(w http.ResponseWriter, r *http.Request, namespace string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRegistrationBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Invalid registration request", http.StatusBadRequest)
		return
	}
	if err := validateJSONNoDuplicateKeys(body); err != nil {
		http.Error(w, "Invalid registration request", http.StatusBadRequest)
		return
	}
	var req registerCredentialSessionRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "Invalid registration request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid registration request", http.StatusBadRequest)
		return
	}
	if err := validateSandboxName(req.SandboxName); err != nil || !validOpaqueID(req.CreationID, 16) || !validOpaqueID(req.SessionToken, 32) || !validProviderKey(req.OpenAIKey) || req.MaxLifetimeSeconds < 0 {
		http.Error(w, "Invalid registration request", http.StatusBadRequest)
		return
	}
	key := credentialSessionKey{namespace: namespace, sandbox: req.SandboxName, creationID: req.CreationID}
	tokenHash := sha256.Sum256([]byte(req.SessionToken))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byKey[key]; exists {
		http.Error(w, "Credential session already exists", http.StatusConflict)
		return
	}
	now := s.now()
	lifetime := maxSessionLifetime
	if req.MaxLifetimeSeconds > 0 && time.Duration(req.MaxLifetimeSeconds)*time.Second < lifetime {
		lifetime = time.Duration(req.MaxLifetimeSeconds) * time.Second
	}
	if lifetime < time.Second {
		http.Error(w, "Invalid registration request", http.StatusBadRequest)
		return
	}
	session := &credentialSession{key: key, tokenHash: tokenHash, openAIKey: req.OpenAIKey, expiresAt: now.Add(lifetime)}
	s.byKey[key] = session
	s.byToken[tokenHash] = session
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(struct {
		SandboxName string `json:"sandboxName"`
	}{SandboxName: req.SandboxName})
}

func (s *CredentialSessionStore) revoke(w http.ResponseWriter, r *http.Request, namespace string) {
	if r.ContentLength > 0 {
		http.Error(w, "Revocation request must not have a body", http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, credentialSessionPath+"/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !validOpaqueID(parts[1], 16) {
		http.NotFound(w, r)
		return
	}
	key := credentialSessionKey{namespace: namespace, sandbox: parts[0], creationID: parts[1]}
	s.mu.Lock()
	if session := s.byKey[key]; session != nil {
		delete(s.byToken, session.tokenHash)
		delete(s.byKey, key)
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *CredentialSessionStore) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != providerResponsesPath || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.RequestURI != providerResponsesPath {
			http.NotFound(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || len(header) <= len("Bearer ") {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		tokenHash := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
		s.mu.Lock()
		session := s.byToken[tokenHash]
		if session != nil && !s.now().Before(session.expiresAt) {
			delete(s.byToken, session.tokenHash)
			delete(s.byKey, session.key)
			session = nil
		}
		s.mu.Unlock()
		if session == nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(contextWithCredentialSession(r, session)))
	})
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
	key := credentialSessionKey{namespace: namespace, sandbox: sandboxName, creationID: creationID}
	s.mu.Lock()
	if session := s.byKey[key]; session != nil {
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

func validOpaqueID(value string, bytes int) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == bytes && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validProviderKey(key string) bool {
	if len(key) == 0 || len(key) > 4096 || strings.TrimSpace(key) != key {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
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
