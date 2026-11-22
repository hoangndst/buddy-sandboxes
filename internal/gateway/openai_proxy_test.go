package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/egressgateway"
	"github.com/klauspost/compress/zstd"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type staticSandboxAuthorizer struct{ allowed bool }

func (s staticSandboxAuthorizer) Allows(string, string, string) bool { return s.allowed }
func (s staticSandboxAuthorizer) Ready() bool                        { return s.allowed }

func TestOpenAIProxyRestrictsOriginAndForwardsAllowlistedRequest(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(16, 1)},
		openAIKey: "sk-provider-secret", expiresAt: time.Now().Add(time.Hour),
	}
	store.byKey[session.key] = session
	secret := []byte("0123456789abcdef0123456789abcdef")
	var got *http.Request
	handler, err := NewOpenAIProxy(OpenAIProxyConfig{
		HMACSecret:   secret,
		Store:        store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r
			return &http.Response{StatusCode: http.StatusTeapot, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"controlled"}`))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-6.2-sol","input":"review this","max_output_tokens":512}`
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-test")
	req.Header.Set("Cookie", "secret-cookie")
	req.Header.Set("X-Other", "must-not-forward")
	req = req.WithContext(context.WithValue(req.Context(), credentialSessionContextKey{}, session))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusTeapot || response.Body.String() != `{"error":"controlled"}` {
		t.Fatalf("response status=%d body=%s", response.Code, response.Body.String())
	}
	if got == nil || got.URL.String() != "https://api.openai.com/v1/responses" || got.Header.Get("Authorization") != "Bearer sk-provider-secret" {
		t.Fatalf("unexpected upstream request: %#v", got)
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Other") != "" || got.Header.Get("User-Agent") != "codex-test" {
		t.Fatalf("header allowlist failure: %v", got.Header)
	}
	proxyToken, ok := got.Context().Value(egressProxyTokenContextKey{}).(string)
	if !ok || got.Header.Get("Proxy-Authorization") != "" {
		t.Fatalf("proxy credentials were not isolated from origin headers: %v", got.Header)
	}
	proxyURL := egressProxyURL("egress-gateway:8080", proxyToken)
	proxyUser, proxyPassword := proxyURL.User.Username(), ""
	proxyPassword, _ = proxyURL.User.Password()
	claims, err := egressgateway.VerifyToken(proxyPassword, secret)
	if err != nil {
		t.Fatal(err)
	}
	if proxyUser != "buddy-api" {
		t.Fatalf("unexpected proxy user: %q", proxyUser)
	}
	if claims.Sub != "buddy-api" || claims.Ns != "buddy-sandboxes-system" || len(claims.Domains) != 1 || claims.Domains[0] != "api.openai.com" {
		t.Fatalf("unexpected egress claims: %+v", claims)
	}
}

func TestOpenAIProxyRejectsUpstreamRedirect(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(16, 1)},
		openAIKey: "sk-provider-secret", expiresAt: time.Now().Add(time.Hour),
	}
	store.byKey[session.key] = session
	handler, err := NewOpenAIProxy(OpenAIProxyConfig{
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"), Store: store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.invalid/"}}, Body: io.NopCloser(strings.NewReader("redirect"))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(`{"model":"gpt-6.2-sol","input":"ok"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), credentialSessionContextKey{}, session))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Header().Get("Location"), "attacker") {
		t.Fatalf("redirect response status=%d headers=%v", response.Code, response.Header())
	}
}

func TestOpenAIProxyDecodesZstdAndRejectsUnsupportedPayloads(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{key: credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(16, 1)}, openAIKey: "sk-key", expiresAt: time.Now().Add(time.Hour)}
	store.byKey[session.key] = session
	var forwarded int
	handler, err := NewOpenAIProxy(OpenAIProxyConfig{
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"), Store: store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			forwarded++
			if r.Header.Get("Content-Encoding") != "" {
				t.Error("compressed encoding forwarded upstream")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":10,"output_tokens":20}}`))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := []byte(`{"model":"gpt-6.2-sol","input":"ok"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(valid, nil)
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, bytes.NewReader(compressed))
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), credentialSessionContextKey{}, session))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || forwarded != 1 {
		t.Fatalf("zstd request status=%d forwarded=%d", response.Code, forwarded)
	}

	for _, body := range []string{
		`{"model":"gpt-6.2-sol","input":[{"type":"input_image","image_url":"https://example.invalid/a.png"}]}`,
		`{"model":"gpt-6.2-sol","input":"x","max_output_tokens":8193}`,
		`{"model":"other","input":"x"}`,
	} {
		req = httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), credentialSessionContextKey{}, session))
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s status=%d want 400", body, response.Code)
		}
	}
	if forwarded != 1 {
		t.Fatalf("unsupported bodies reached upstream %d times", forwarded-1)
	}
}

func TestOpenAIProxyRejectsMissingSandboxAuthorization(t *testing.T) {
	_, err := NewOpenAIProxy(OpenAIProxyConfig{Store: NewCredentialSessionStore(), HMACSecret: bytes.Repeat([]byte{'x'}, 32)})
	if err == nil {
		t.Fatal("proxy initialized without Sandbox state authorization")
	}
	if _, ok := parseUsage([]byte(`{"usage":{"input_tokens":4,"output_tokens":5}}`)); !ok {
		t.Fatal("usage parser rejected valid usage")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(`{"model":"gpt-6.2-sol"}`), &value); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIProxyInjectsOutputCapAndReconcilesSSEUsage(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(16, 1)},
		openAIKey: "sk-key", expiresAt: time.Now().Add(time.Hour),
	}
	store.byKey[session.key] = session
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":11}}}\n\n"
	var forwarded *http.Request
	handler, err := NewOpenAIProxy(OpenAIProxyConfig{
		HMACSecret:   []byte("0123456789abcdef0123456789abcdef"),
		Store:        store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			forwarded = r
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
	requestBody := `{"model":"gpt-6.2-sol","input":"task","stream":true}`
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(requestBody))
	req.Header.Set("Content-Type", providerJSONContentType)
	req.Header.Set("Accept", "text/event-stream")
	req = req.WithContext(context.WithValue(req.Context(), credentialSessionContextKey{}, session))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Body.String() != stream {
		t.Fatalf("response status=%d body=%q", response.Code, response.Body.String())
	}
	if forwarded == nil {
		t.Fatal("provider request was not forwarded")
	}
	var forwardedBody map[string]json.RawMessage
	if err := json.NewDecoder(forwarded.Body).Decode(&forwardedBody); err != nil {
		t.Fatalf("decode forwarded request: %v", err)
	}
	var outputCap int64
	if err := json.Unmarshal(forwardedBody["max_output_tokens"], &outputCap); err != nil || outputCap != maxProviderOutputTokens {
		t.Fatalf("forwarded max_output_tokens=%d err=%v", outputCap, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if session.reservedTokens != 18 || session.inFlight != 0 {
		t.Fatalf("session quota after SSE response: reserved=%d inFlight=%d", session.reservedTokens, session.inFlight)
	}
}
