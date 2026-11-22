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
	store := NewCredentialSessionStore()
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
