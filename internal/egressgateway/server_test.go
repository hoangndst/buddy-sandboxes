package egressgateway

import (
	"context"
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
		if !limitedServer.acquireSlot("default", "sb-123") {
			t.Fatal("expected slot acquisition to succeed")
		}
		defer limitedServer.releaseSlot("default", "sb-123")

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

func TestPipeConnectionsWriteDeadlineExceeded(t *testing.T) {
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

	idleTimeout := 80 * time.Millisecond
	pipeDone := make(chan struct{})
	go func() {
		pipeConnections(clientConn, upstreamConn, idleTimeout)
		close(pipeDone)
	}()

	// Feed data so the pipe attempts to write to the peers while peers do not read.
	go func() {
		_, _ = clientRemote.Write([]byte("data-for-upstream"))
	}()
	go func() {
		_, _ = upstreamRemote.Write([]byte("data-for-client"))
	}()

	select {
	case <-pipeDone:
		// Success: write deadline expired and unblocked pipeConnections cleanly
	case <-time.After(1 * time.Second):
		t.Fatal("pipeConnections hung indefinitely on write deadline without reading peers")
	}
}

func TestServerConcurrencyLimitTenantIsolation(t *testing.T) {
	secret := []byte("test-hmac-secret-1234567890123456")
	server := NewServer(secret)
	server.MaxConcurrentPerSandbox = 1

	claimsA := Claims{
		Sub:     "worker",
		Ns:      "tenant-a",
		Domains: []string{"github.com"},
		Exp:     time.Now().Add(1 * time.Hour).Unix(),
	}
	tokenA, err := GenerateToken(claimsA, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed for tenant-a: %v", err)
	}
	authHeaderA := "Basic " + base64.StdEncoding.EncodeToString([]byte("worker:"+tokenA))

	claimsB := Claims{
		Sub:     "worker",
		Ns:      "tenant-b",
		Domains: []string{"github.com"},
		Exp:     time.Now().Add(1 * time.Hour).Unix(),
	}
	tokenB, err := GenerateToken(claimsB, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed for tenant-b: %v", err)
	}
	authHeaderB := "Basic " + base64.StdEncoding.EncodeToString([]byte("worker:"+tokenB))

	// Acquire single slot for tenant-a to exhaust its concurrency limit
	if !server.acquireSlot("tenant-a", "worker") {
		t.Fatal("expected slot acquisition for tenant-a to succeed")
	}
	defer server.releaseSlot("tenant-a", "worker")

	// Tenant-a request should be rejected with 429 Too Many Requests
	reqA := httptest.NewRequest(http.MethodGet, "http://github.com:8080/test", nil)
	reqA.Header.Set("Proxy-Authorization", authHeaderA)
	recA := httptest.NewRecorder()
	server.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusTooManyRequests {
		t.Errorf("tenant-a got status %d, want %d", recA.Code, http.StatusTooManyRequests)
	}

	// Tenant-b request with identical sandbox name must not be blocked by tenant-a exhaustion.
	// It acquires its slot and proceeds past concurrency check (failing on forbidden port 8080).
	reqB := httptest.NewRequest(http.MethodGet, "http://github.com:8080/test", nil)
	reqB.Header.Set("Proxy-Authorization", authHeaderB)
	recB := httptest.NewRecorder()
	server.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusForbidden {
		t.Errorf("tenant-b got status %d, want %d (expected request to bypass 429 and hit port validation)", recB.Code, http.StatusForbidden)
	}

	// Direct slot acquisition check: tenant-a cannot acquire another slot, tenant-b can acquire its slot
	if server.acquireSlot("tenant-a", "worker") {
		t.Error("tenant-a should not be able to acquire another slot when limit is exhausted")
	}
	if !server.acquireSlot("tenant-b", "worker") {
		t.Error("tenant-b should be able to acquire slot independently of tenant-a")
	} else {
		server.releaseSlot("tenant-b", "worker")
	}
}

func TestServerHTTPInactivityTimeout(t *testing.T) {
	secret := []byte("test-hmac-secret-1234567890123456")
	claims := Claims{
		Sub:     "sb-test",
		Ns:      "default",
		Domains: []string{"github.com"},
		Exp:     time.Now().Add(1 * time.Hour).Unix(),
	}
	token, err := GenerateToken(claims, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("sb-test:"+token))

	t.Run("upstream stalls on headers returns gateway timeout or bad gateway error", func(t *testing.T) {
		hangServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(1 * time.Second):
			}
		}))
		defer hangServer.Close()

		server := NewServer(secret)
		server.IdleTimeout = 50 * time.Millisecond
		server.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, hangServer.Listener.Addr().String())
		}

		req := httptest.NewRequest(http.MethodGet, "http://github.com/hang", nil)
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		start := time.Now()
		server.ServeHTTP(rec, req)
		elapsed := time.Since(start)

		if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusBadGateway {
			t.Errorf("got status %d, want %d or %d", rec.Code, http.StatusGatewayTimeout, http.StatusBadGateway)
		}
		if elapsed >= 250*time.Millisecond {
			t.Errorf("request took %v, expected timeout around %v", elapsed, server.IdleTimeout)
		}

		if !server.acquireSlot("default", "sb-test") {
			t.Error("expected sandbox concurrency slot to be released after header timeout")
		} else {
			server.releaseSlot("default", "sb-test")
		}
	})

	t.Run("upstream stalls on body chunks terminates without hanging indefinitely", func(t *testing.T) {
		stallServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				_, _ = w.Write([]byte("chunk-before-stall"))
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
			case <-time.After(1 * time.Second):
			}
		}))
		defer stallServer.Close()

		server := NewServer(secret)
		server.IdleTimeout = 50 * time.Millisecond
		server.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, stallServer.Listener.Addr().String())
		}

		req := httptest.NewRequest(http.MethodGet, "http://github.com/stall", nil)
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		start := time.Now()
		server.ServeHTTP(rec, req)
		elapsed := time.Since(start)

		if elapsed >= 250*time.Millisecond {
			t.Errorf("request took %v, expected termination around %v", elapsed, server.IdleTimeout)
		}

		if !server.acquireSlot("default", "sb-test") {
			t.Error("expected sandbox concurrency slot to be released after body stall")
		} else {
			server.releaseSlot("default", "sb-test")
		}
	})

	t.Run("streaming body chunks refreshes deadline and completes successfully", func(t *testing.T) {
		streamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			for i := 0; i < 4; i++ {
				time.Sleep(20 * time.Millisecond)
				_, _ = w.Write([]byte("chunk"))
				if ok {
					flusher.Flush()
				}
			}
		}))
		defer streamServer.Close()

		server := NewServer(secret)
		server.IdleTimeout = 50 * time.Millisecond
		server.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, streamServer.Listener.Addr().String())
		}

		req := httptest.NewRequest(http.MethodGet, "http://github.com/stream", nil)
		req.Header.Set("Proxy-Authorization", authHeader)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("got status %d, want %d", rec.Code, http.StatusOK)
		}
		if rec.Body.String() != "chunkchunkchunkchunk" {
			t.Errorf("got body %q, want %q", rec.Body.String(), "chunkchunkchunkchunk")
		}

		if !server.acquireSlot("default", "sb-test") {
			t.Error("expected sandbox concurrency slot to be released after stream completion")
		} else {
			server.releaseSlot("default", "sb-test")
		}
	})
}
