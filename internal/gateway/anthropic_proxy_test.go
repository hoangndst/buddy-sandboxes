package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAnthropicProxyUsesFixedOriginAndAPIKeyHeader(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{key: credentialSessionKey{namespace: "tenant-a", sandbox: "work", creationID: encodedToken(1)}, expiresAt: time.Now().Add(time.Hour)}
	store.byKey[session.key] = session
	secret := []byte("0123456789abcdef0123456789abcdef")
	var upstream *http.Request
	handler, err := NewAnthropicProxy(AnthropicProxyConfig{HMACSecret: secret, Store: store, SandboxState: staticSandboxAuthorizer{allowed: true}, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstream = r
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[],"usage":{"input_tokens":2,"output_tokens":3}}`))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude-sonnet-4-5","max_tokens":512,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, providerAnthropicMessagesPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req = withTestProviderContext(req, session, "capability-value")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if upstream == nil || upstream.URL.String() != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("upstream destination=%v", upstream)
	}
	if upstream.Header.Get("x-api-key") != "capability-value" || upstream.Header.Get("Authorization") != "" || upstream.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("unexpected provider auth headers: %v", upstream.Header)
	}
}

func TestAnthropicProxyRejectsArbitraryPathsAndQueries(t *testing.T) {
	store := NewCredentialSessionStore()
	handler, err := NewAnthropicProxy(AnthropicProxyConfig{HMACSecret: []byte("0123456789abcdef0123456789abcdef"), Store: store, SandboxState: staticSandboxAuthorizer{allowed: true}, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected upstream request"); return nil, nil })})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{providerAnthropicMessagesPath + "?url=https://example.com", "/api/v1/provider/anthropic/v1/models"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status=%d, want unauthenticated request denied", target, response.Code)
		}
	}
}
