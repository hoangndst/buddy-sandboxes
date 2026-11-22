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

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/egressgateway"
	"github.com/klauspost/compress/zstd"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withTestProviderContext(req *http.Request, session *credentialSession, key string) *http.Request {
	ctx := context.WithValue(req.Context(), credentialSessionContextKey{}, session)
	ctx = context.WithValue(ctx, providerKeyContextKey{}, key)
	ctx = context.WithValue(ctx, providerRecordContextKey{}, credentials.Record{Key: key})
	return req.WithContext(ctx)
}

func withTestProviderRecordContext(req *http.Request, session *credentialSession, rec credentials.Record) *http.Request {
	ctx := context.WithValue(req.Context(), credentialSessionContextKey{}, session)
	ctx = context.WithValue(ctx, providerKeyContextKey{}, rec.Key)
	ctx = context.WithValue(ctx, providerRecordContextKey{}, rec)
	return req.WithContext(ctx)
}

type staticSandboxAuthorizer struct{ allowed bool }

func (s staticSandboxAuthorizer) Allows(string, string, string) bool { return s.allowed }
func (s staticSandboxAuthorizer) Ready() bool                        { return s.allowed }

func TestOpenAIProxyRestrictsOriginAndForwardsAllowlistedRequest(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(1)},
		expiresAt: time.Now().Add(time.Hour),
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
	req = withTestProviderContext(req, session, "sk-provider-secret")
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
	if claims.Sub != "work" || claims.Ns != "default" || len(claims.Domains) != 1 || claims.Domains[0] != "api.openai.com" {
		t.Fatalf("unexpected egress claims: %+v", claims)
	}
}

func TestOpenAIProxyRejectsUpstreamRedirect(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(1)},
		expiresAt: time.Now().Add(time.Hour),
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
	req = withTestProviderContext(req, session, "sk-provider-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Header().Get("Location"), "attacker") {
		t.Fatalf("redirect response status=%d headers=%v", response.Code, response.Header())
	}
}

func TestOpenAIProxyDecodesZstdAndRejectsUnsupportedPayloads(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{key: credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(1)}, expiresAt: time.Now().Add(time.Hour)}
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
	req = withTestProviderContext(req, session, "sk-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || forwarded != 1 {
		t.Fatalf("zstd request status=%d forwarded=%d", response.Code, forwarded)
	}

	for _, body := range []string{
		`{"model":"gpt-6.2-sol","input":[{"type":"input_image","image_url":"https://example.invalid/a.png"}]}`,
		`{"model":"gpt-6.2-sol","input":"x","max_output_tokens":8193}`,
		`{"model":"../evil/model","input":"x"}`,
		`{"model":"/root/model","input":"x"}`,
		`{"model":"","input":"x"}`,
	} {
		req = httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = withTestProviderContext(req, session, "sk-key")
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
		key:       credentialSessionKey{namespace: "default", sandbox: "work", creationID: encodedToken(1)},
		expiresAt: time.Now().Add(time.Hour),
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
	req = withTestProviderContext(req, session, "sk-key")
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

const (
	testAzureHost      = "test-account.openai.azure.com"
	testOpenRouterHost = "openrouter.ai"
)

func TestOpenAIProxyCustomAzureEndpointAndAuthHeader(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "test-ns", sandbox: "sbx-azure", creationID: encodedToken(2)},
		expiresAt: time.Now().Add(time.Hour),
	}
	store.byKey[session.key] = session
	secret := bytes.Repeat([]byte("a"), 32)
	var got *http.Request
	handler, err := NewOpenAIProxy(OpenAIProxyConfig{
		HMACSecret:   secret,
		Store:        store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`)),
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	azureRec := credentials.Record{
		Key:          "azure-secret-key-123",
		BaseURL:      "https://" + testAzureHost + "/openai/v1?api-version=2024-02-15-preview",
		DefaultModel: "gpt-6-luna",
	}

	body := `{"model":"gpt-6-luna","input":"test task"}`
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req = withTestProviderRecordContext(req, session, azureRec)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", response.Code, response.Body.String())
	}
	if got == nil {
		t.Fatal("request was not forwarded")
	}
	// Verify URL path normalization and query preservation
	if got.URL.Host != testAzureHost || got.URL.Path != "/openai/v1/responses" {
		t.Fatalf("unexpected URL: %s", got.URL.String())
	}
	if got.URL.Query().Get("api-version") != "2024-02-15-preview" {
		t.Fatalf("missing or corrupted query param: %s", got.URL.RawQuery)
	}
	if got.Host != testAzureHost {
		t.Fatalf("Host header = %q, want %q", got.Host, testAzureHost)
	}
	// Azure requires api-key and NO Authorization header
	if got.Header.Get("api-key") != "azure-secret-key-123" {
		t.Fatalf("api-key header = %q, want %q", got.Header.Get("api-key"), "azure-secret-key-123")
	}
	if got.Header.Get("Authorization") != "" {
		t.Fatalf("Authorization header must be omitted for Azure endpoints, got %q", got.Header.Get("Authorization"))
	}

	// Verify egress token claims
	proxyToken := got.Context().Value(egressProxyTokenContextKey{}).(string)
	claims, err := egressgateway.VerifyToken(proxyToken, secret)
	if err != nil {
		t.Fatalf("verify egress token: %v", err)
	}
	if claims.Sub != "sbx-azure" || claims.Ns != "test-ns" || len(claims.Domains) != 1 || claims.Domains[0] != testAzureHost {
		t.Fatalf("unexpected egress claims: %+v", claims)
	}
}

func TestOpenAIProxyCustomOpenRouterEndpoint(t *testing.T) {
	store := NewCredentialSessionStore()
	session := &credentialSession{
		key:       credentialSessionKey{namespace: "openrouter-ns", sandbox: "sbx-or", creationID: encodedToken(3)},
		expiresAt: time.Now().Add(time.Hour),
	}
	store.byKey[session.key] = session
	secret := bytes.Repeat([]byte("b"), 32)
	var got *http.Request
	handler, err := NewOpenAIProxy(OpenAIProxyConfig{
		HMACSecret:   secret,
		Store:        store,
		SandboxState: staticSandboxAuthorizer{allowed: true},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":20,"completion_tokens":10}}`)),
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	orRec := credentials.Record{
		Key:          "sk-or-test-key",
		BaseURL:      "https://" + testOpenRouterHost + "/api/v1",
		DefaultModel: "deepseek/deepseek-chat",
	}

	body := `{"model":"deepseek/deepseek-chat","input":"code review"}`
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req = withTestProviderRecordContext(req, session, orRec)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", response.Code, response.Body.String())
	}
	if got == nil {
		t.Fatal("request was not forwarded")
	}
	if got.URL.Host != testOpenRouterHost || got.URL.Path != "/api/v1/responses" {
		t.Fatalf("unexpected URL: %s", got.URL.String())
	}
	if got.Host != testOpenRouterHost {
		t.Fatalf("Host header = %q, want %q", got.Host, testOpenRouterHost)
	}
	if got.Header.Get("Authorization") != "Bearer sk-or-test-key" {
		t.Fatalf("Authorization header = %q, want Bearer sk-or-test-key", got.Header.Get("Authorization"))
	}
	if got.Header.Get("api-key") != "" {
		t.Fatalf("api-key header must be omitted for non-Azure, got %q", got.Header.Get("api-key"))
	}

	proxyToken := got.Context().Value(egressProxyTokenContextKey{}).(string)
	claims, err := egressgateway.VerifyToken(proxyToken, secret)
	if err != nil {
		t.Fatalf("verify egress token: %v", err)
	}
	if claims.Sub != "sbx-or" || claims.Ns != "openrouter-ns" || len(claims.Domains) != 1 || claims.Domains[0] != testOpenRouterHost {
		t.Fatalf("unexpected egress claims: %+v", claims)
	}
}
