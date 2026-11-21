package egressgateway

import (
	"encoding/base64"
	"net"
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

func TestPipeConnectionsBidirectionalActivity(t *testing.T) {
	clientConn, clientRemote := net.Pipe()
	defer func() {
		_ = clientConn.Close()
		_ = clientRemote.Close()
	}()

	upstreamConn, upstreamRemote := net.Pipe()
	defer func() {
		_ = upstreamConn.Close()
		_ = upstreamRemote.Close()
	}()

	idleTimeout := 100 * time.Millisecond
	go pipeConnections(clientConn, upstreamConn, idleTimeout)

	// Stream 5 chunks from upstream to client with 40ms intervals (total 200ms > 100ms idleTimeout).
	// With bidirectional activity tracking, upstream reads refresh both deadlines, preventing timeout.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		for i := 0; i < 5; i++ {
			if _, err := clientRemote.Read(buf); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(40 * time.Millisecond)
			if _, err := upstreamRemote.Write([]byte("chunk-data-payload")); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected read error during stream: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("pipe transfer timed out")
	}
}
