package gateway

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func encodedToken(size int, start byte) string {
	data := make([]byte, size)
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

func TestCredentialSessionRoutesUseSeparateAuthentication(t *testing.T) {
	const cliToken = "cli-token-for-test"
	creationID := encodedToken(16, 1)
	sessionToken := encodedToken(32, 20)
	store := NewCredentialSessionStore()
	var sawSession bool
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := credentialSessionFromContext(r.Context())
		sawSession = ok && session.openAIKey == "sk-test"
		w.WriteHeader(http.StatusNoContent)
	})
	srv := NewServer(Config{
		Auth:            NewStaticTokenAuthenticator(map[string]string{cliToken: "tenant-a"}),
		CredentialStore: store,
		ProviderHandler: provider,
	})
	handler := srv.httpServer.Handler

	registration := fmt.Sprintf(`{"sandboxName":"work-a","sessionToken":%q,"creationID":%q,"openAIKey":"sk-test"}`, sessionToken, creationID)
	req := httptest.NewRequest(http.MethodPost, credentialSessionPath, strings.NewReader(registration))
	req.Header.Set("Authorization", "Bearer "+cliToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), sessionToken) || strings.Contains(response.Body.String(), "sk-test") {
		t.Fatalf("registration response exposed secret: %s", response.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, providerResponsesPath, bytes.NewBufferString(`{"model":"gpt-6.2-sol"}`))
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent || !sawSession {
		t.Fatalf("provider status=%d, session-context=%v", response.Code, sawSession)
	}

	req = httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+cliToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("CLI token provider status = %d, want 401", response.Code)
	}

	req = httptest.NewRequest(http.MethodPost, credentialSessionPath, strings.NewReader(registration))
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("session token management status = %d, want 401", response.Code)
	}

	req = httptest.NewRequest(http.MethodPost, providerResponsesPath+"?x=1", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("provider query status = %d, want 404", response.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, credentialSessionPath+"/work-a/"+creationID, nil)
	req.Header.Set("Authorization", "Bearer "+cliToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body=%s", response.Code, response.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d, want 401", response.Code)
	}
}

func TestCredentialSessionRegistrationRejectsMalformedRequests(t *testing.T) {
	store := NewCredentialSessionStore()
	namespaceCtx := WithTenantNamespace(httptest.NewRequest(http.MethodPost, credentialSessionPath, nil).Context(), "tenant-a")
	for _, body := range []string{
		`{"sandboxName":"x","sessionToken":"bad","creationID":"bad","openAIKey":"sk-test"}`,
		`{"sandboxName":"x","sandboxName":"y","sessionToken":"` + encodedToken(32, 1) + `","creationID":"` + encodedToken(16, 2) + `","openAIKey":"sk-test"}`,
		`{"sandboxName":"x","sessionToken":"` + encodedToken(32, 1) + `","creationID":"` + encodedToken(16, 2) + `","openAIKey":"sk-test","namespace":"other"}`,
		`{"sandboxName":"x","sessionToken":"` + encodedToken(32, 1) + `","creationID":"` + encodedToken(16, 2) + `","openAIKey":" sk-test"}`,
		`{"sandboxName":"x","sessionToken":"` + encodedToken(32, 1) + `","creationID":"` + encodedToken(16, 2) + `","openAIKey":"sk-test"} {}`,
	} {
		req := httptest.NewRequest(http.MethodPost, credentialSessionPath, strings.NewReader(body)).WithContext(namespaceCtx)
		response := httptest.NewRecorder()
		store.CredentialSessionHandler(response, req)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %q: status=%d want 400", body, response.Code)
		}
	}
}
