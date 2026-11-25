package gateway

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func encodedToken(start byte) string {
	data := make([]byte, 16)
	for i := range data {
		data[i] = start + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func TestCredentialSessionReservationsAreAtomic(t *testing.T) {
	store := NewCredentialSessionStore(CredentialSessionLimits{
		MaxInFlight:         4,
		MaxRequestsPerMin:   10,
		MaxTokensPerSession: 1_000_000,
	})
	session := &credentialSession{key: credentialSessionKey{namespace: "n", sandbox: "s", creationID: "c"}, expiresAt: time.Now().Add(time.Hour)}
	store.byKey[session.key] = session
	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if store.reserve(session, 100) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted != 4 || session.inFlight != 4 || session.reservedTokens != 400 {
		t.Fatalf("accepted=%d inflight=%d reserved=%d", accepted, session.inFlight, session.reservedTokens)
	}
	for range 4 {
		store.finishRequest(session)
	}
	session.reservedTokens = 1_000_000
	if store.reserve(session, 1) {
		t.Fatal("reservation exceeded cumulative budget")
	}
}

func TestCredentialSessionCustomLimits(t *testing.T) {
	store := NewCredentialSessionStore(CredentialSessionLimits{
		MaxInFlight:         2,
		MaxRequestsPerMin:   5,
		MaxTokensPerSession: 100,
	})

	// Session with custom higher limits
	customSession := &credentialSession{
		key:       credentialSessionKey{namespace: "n", sandbox: "s1", creationID: "c1"},
		expiresAt: time.Now().Add(time.Hour),
		limits: CredentialSessionLimits{
			MaxInFlight:         10,
			MaxRequestsPerMin:   20,
			MaxTokensPerSession: 10_000,
		},
	}
	store.byKey[customSession.key] = customSession

	// Should allow up to custom limit (10), exceeding store limit (2)
	for i := 0; i < 10; i++ {
		if !store.reserve(customSession, 100) {
			t.Fatalf("failed to reserve request %d for custom session", i)
		}
	}
	// 11th request should be rejected by session MaxInFlight (10)
	if store.reserve(customSession, 100) {
		t.Fatal("expected request 11 to be rejected by custom MaxInFlight")
	}

	// Session without custom limits should fallback to store limits (2)
	defaultSession := &credentialSession{
		key:       credentialSessionKey{namespace: "n", sandbox: "s2", creationID: "c2"},
		expiresAt: time.Now().Add(time.Hour),
	}
	store.byKey[defaultSession.key] = defaultSession

	firstOK := store.reserve(defaultSession, 10)
	secondOK := store.reserve(defaultSession, 10)
	if !firstOK || !secondOK {
		t.Fatal("expected 2 requests to succeed on default session")
	}
	if store.reserve(defaultSession, 10) {
		t.Fatal("expected 3rd request to be rejected by store default MaxInFlight (2)")
	}
}

func TestLegacyCredentialSessionRoutesAreRemoved(t *testing.T) {
	const cliToken = "cli-token-for-test"
	srv := NewServer(Config{Auth: NewStaticTokenAuthenticator(map[string]string{cliToken: "tenant-a"})})
	handler := srv.httpServer.Handler

	req := httptest.NewRequest(http.MethodPost, "/api/v1/credential-sessions", strings.NewReader(`{"openAIKey":"sk-test"}`))
	req.Header.Set("Authorization", "Bearer "+cliToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("legacy registration status = %d, want 404", response.Code)
	}

	req = httptest.NewRequest(http.MethodPost, providerResponsesPath, bytes.NewBufferString(`{"model":"gpt-6.2-sol"}`))
	req.Header.Set("Authorization", "Bearer "+cliToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("provider route without configured broker status = %d, want 404", response.Code)
	}
}
