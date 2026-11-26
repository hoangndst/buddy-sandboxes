package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
)

type capabilityAuthenticatorStub struct {
	token      string
	method     string
	path       string
	capability credentials.Capability
	err        error
	calls      int
}

func (s *capabilityAuthenticatorStub) Authenticate(_ context.Context, token, method, path string) (credentials.Capability, error) {
	s.calls++
	s.token, s.method, s.path = token, method, path
	return s.capability, s.err
}

type providerCredentialResolverStub struct {
	userID  string
	service string
	sandbox string
	value   string
	calls   int
}

type sandboxAuthorizerStub struct {
	allowed bool
	calls   int
}

func (s *sandboxAuthorizerStub) Allows(string, string, string) bool {
	s.calls++
	return s.allowed
}

func (s *sandboxAuthorizerStub) Ready() bool { return s.allowed }

func (s *providerCredentialResolverStub) GetForSandbox(_ context.Context, userID, service, sandbox string) (string, error) {
	s.calls++
	s.userID, s.service, s.sandbox = userID, service, sandbox
	return s.value, nil
}

func TestAnthropicCapabilityStillRequiresActiveSandbox(t *testing.T) {
	capabilities := &capabilityAuthenticatorStub{capability: credentials.Capability{
		UserID: "user-a", Namespace: "tenant-a", SandboxName: "work", CreationID: "creation-a",
		Service: credentials.ServiceAnthropic, ExpiresAt: time.Now().Add(time.Hour),
	}}
	providerCredentials := &providerCredentialResolverStub{value: "provider-key"}
	state := &sandboxAuthorizerStub{allowed: false}
	handler := NewCredentialSessionStore().authenticateCapability(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("inactive Sandbox reached provider handler") }),
		capabilities, providerCredentials, state,
	)
	request := httptest.NewRequest(http.MethodPost, providerAnthropicMessagesPath, nil)
	request.Header.Set("x-api-key", "broker-capability")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if capabilities.calls != 1 || state.calls != 1 || providerCredentials.calls != 0 {
		t.Fatalf("validation calls: capability=%d state=%d provider=%d", capabilities.calls, state.calls, providerCredentials.calls)
	}
}

func TestCapabilityAuthenticationUsesProviderSpecificHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		header  string
		service string
	}{
		{name: "Anthropic API key header", path: providerAnthropicMessagesPath, header: "x-api-key", service: credentials.ServiceAnthropic},
		{name: "OpenAI bearer header", path: providerResponsesPath, header: "Authorization", service: credentials.ServiceOpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capabilities := &capabilityAuthenticatorStub{capability: credentials.Capability{
				UserID: "user-a", Namespace: "tenant-a", SandboxName: "work", CreationID: "creation-a",
				Service: tc.service, ExpiresAt: time.Now().Add(time.Hour),
			}}
			providerCredentials := &providerCredentialResolverStub{value: "provider-key"}
			store := NewCredentialSessionStore()
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if value, ok := providerKeyFromContext(r.Context()); !ok || value != "provider-key" {
					t.Errorf("provider credential context = %q, %v", value, ok)
				}
				if _, ok := r.Context().Value(credentialSessionContextKey{}).(*credentialSession); !ok {
					t.Error("credential session was not bound")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			handler := store.authenticateCapability(next, capabilities, providerCredentials, staticSandboxAuthorizer{allowed: true})
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			if tc.header == "Authorization" {
				req.Header.Set(tc.header, "Bearer broker-capability")
			} else {
				req.Header.Set(tc.header, "broker-capability")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", response.Code)
			}
			if capabilities.calls != 1 || capabilities.token != "broker-capability" ||
				capabilities.method != http.MethodPost || capabilities.path != tc.path {
				t.Fatalf("capability validation got token=%q method=%q path=%q calls=%d", capabilities.token, capabilities.method, capabilities.path, capabilities.calls)
			}
			if providerCredentials.calls != 1 || providerCredentials.userID != "user-a" ||
				providerCredentials.service != tc.service || providerCredentials.sandbox != "work" {
				t.Fatalf("provider credential lookup = %+v", providerCredentials)
			}
		})
	}
}

func TestCapabilityAuthenticationRejectsWrongHeaderForRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		header string
		value  string
	}{
		{name: "bearer is not accepted on Anthropic route", path: providerAnthropicMessagesPath, header: "Authorization", value: "Bearer capability"},
		{name: "Anthropic key is not accepted on OpenAI route", path: providerResponsesPath, header: "x-api-key", value: "capability"},
		{name: "Anthropic key is not accepted on a different path", path: providerAnthropicMessagesPath + "/extra", header: "x-api-key", value: "capability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capabilities := &capabilityAuthenticatorStub{}
			providerCredentials := &providerCredentialResolverStub{value: "provider-key"}
			handler := NewCredentialSessionStore().authenticateCapability(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthorized request reached the next handler") }),
				capabilities, providerCredentials, staticSandboxAuthorizer{allowed: true},
			)
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set(tc.header, tc.value)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
			if capabilities.calls != 0 || providerCredentials.calls != 0 {
				t.Fatalf("invalid header reached validation or provider lookup: capability calls=%d provider calls=%d", capabilities.calls, providerCredentials.calls)
			}
		})
	}
}

type providerRecordResolverStub struct {
	rec   credentials.Record
	calls int
}

func (s *providerRecordResolverStub) GetForSandbox(_ context.Context, _, _, _ string) (string, error) {
	return s.rec.Key, nil
}

func (s *providerRecordResolverStub) GetRecordForSandbox(_ context.Context, _, _, _ string) (credentials.Record, error) {
	s.calls++
	return s.rec, nil
}

func TestCapabilityAuthenticationBindsCustomRecordLimits(t *testing.T) {
	capabilities := &capabilityAuthenticatorStub{capability: credentials.Capability{
		UserID: "user-a", Namespace: "tenant-a", SandboxName: "work", CreationID: "creation-a",
		Service: credentials.ServiceOpenAI, ExpiresAt: time.Now().Add(time.Hour),
	}}
	providerRecords := &providerRecordResolverStub{rec: credentials.Record{
		Key:               "sk-test-key",
		RequestsPerMinute: 2000,
		MaxConcurrent:     50,
		TokensPerMinute:   5_000_000,
	}}
	store := NewCredentialSessionStore(CredentialSessionLimits{
		MaxInFlight:         4,
		MaxRequestsPerMin:   10,
		MaxTokensPerSession: 100_000,
	})

	var boundSession *credentialSession
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		boundSession, ok = credentialSessionFromContext(r.Context())
		if !ok || boundSession == nil {
			t.Fatal("expected bound credential session in context")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	handler := store.authenticateCapability(next, capabilities, providerRecords, staticSandboxAuthorizer{allowed: true})
	req := httptest.NewRequest(http.MethodPost, providerResponsesPath, nil)
	req.Header.Set("Authorization", "Bearer broker-capability")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	if boundSession.limits.MaxRequestsPerMin != 2000 {
		t.Errorf("session MaxRequestsPerMin = %d, want 2000", boundSession.limits.MaxRequestsPerMin)
	}
	if boundSession.limits.MaxInFlight != 50 {
		t.Errorf("session MaxInFlight = %d, want 50", boundSession.limits.MaxInFlight)
	}
	if boundSession.limits.MaxTokensPerMin != 5_000_000 {
		t.Errorf("session MaxTokensPerMin = %d, want 5000000", boundSession.limits.MaxTokensPerMin)
	}
}
