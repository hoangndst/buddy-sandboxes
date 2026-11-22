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
	store.mu.Lock()
	defer store.mu.Unlock()
	if session.reservedTokens != 5 || session.inFlight != 0 {
		t.Fatalf("non-stream usage was not reconciled: reserved=%d inFlight=%d", session.reservedTokens, session.inFlight)
	}
}

func TestAnthropicProxyReconcilesCumulativeStreamingUsage(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{key: credentialSessionKey{namespace: "tenant-a", sandbox: "work", creationID: encodedToken(1)}, expiresAt: time.Now().Add(time.Hour)}
	store.byKey[session.key] = session
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":17,"output_tokens":1}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hello"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":4}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	handler, err := NewAnthropicProxy(AnthropicProxyConfig{
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"), Store: store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
				Body:       io.NopCloser(strings.NewReader(stream)),
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, providerAnthropicMessagesPath,
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":512,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", providerEventStream)
	request = withTestProviderContext(request, session, "capability-value")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != stream {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if session.reservedTokens != 26 || session.inFlight != 0 {
		t.Fatalf("stream usage = reserved %d inFlight %d, want 26 tokens and no in-flight request", session.reservedTokens, session.inFlight)
	}
}

func TestAnthropicProxyKeepsReservationForIncompleteUsageStream(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{key: credentialSessionKey{namespace: "tenant-a", sandbox: "work", creationID: encodedToken(1)}, expiresAt: time.Now().Add(time.Hour)}
	store.byKey[session.key] = session
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":17,"output_tokens":1}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
		"",
	}, "\n")
	const requestBody = `{"model":"claude-sonnet-4-5","max_tokens":512,"messages":[{"role":"user","content":"hello"}]}`
	handler, err := NewAnthropicProxy(AnthropicProxyConfig{
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"), Store: store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{providerEventStream}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, providerAnthropicMessagesPath, strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", providerEventStream)
	request = withTestProviderContext(request, session, "capability-value")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	wantReservation := int64(len(requestBody)) + 512
	if session.reservedTokens != wantReservation || session.inFlight != 0 {
		t.Fatalf("incomplete stream reservation = %d inFlight %d, want %d and no in-flight request", session.reservedTokens, session.inFlight, wantReservation)
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
