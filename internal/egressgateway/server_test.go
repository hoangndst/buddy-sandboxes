package egressgateway

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServerAuthenticationAndValidation(t *testing.T) {
	secret := []byte("test-hmac-secret-1234567890123456")
	server := NewServer(secret)

	claims := Claims{
		Sub:     "sb-123",
		Ns:      "default",
		Domains: []string{"github.com", "*.npmjs.org"},
		Exp:     time.Now().Add(1 * time.Hour).Unix(),
	}
	token, err := GenerateToken(claims, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("sb-123:"+token))

	t.Run("missing proxy authorization header returns 407", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://github.com/test", nil)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusProxyAuthRequired {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusProxyAuthRequired)
		}
		if rec.Header().Get("Proxy-Authenticate") == "" {
			t.Errorf("expected Proxy-Authenticate header")
		}
	})

	t.Run("invalid token returns 407", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://github.com/test", nil)
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sb-123:bad-token")))
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusProxyAuthRequired {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusProxyAuthRequired)
		}
	})

	t.Run("http to unauthorized domain returns 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://evil-c2.com/payload", nil)
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("http to forbidden port returns 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://github.com:8080/test", nil)
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("connect to forbidden port returns 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://github.com:22", nil)
		req.Host = "github.com:22"
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("connect to unauthorized domain returns 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, "http://attacker.com:443", nil)
		req.Host = "attacker.com:443"
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("concurrency limit returns 429 when exhausted", func(t *testing.T) {
		limitedServer := NewServer(secret)
		limitedServer.MaxConcurrentPerSandbox = 1

		// Acquire the single slot manually
		if !limitedServer.acquireSlot("sb-123") {
			t.Fatal("expected slot acquisition to succeed")
		}
		defer limitedServer.releaseSlot("sb-123")

		req := httptest.NewRequest(http.MethodGet, "http://github.com/test", nil)
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		limitedServer.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
	})
}
