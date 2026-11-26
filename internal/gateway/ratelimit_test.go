package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"golang.org/x/time/rate"
)

func TestRateLimiter_AllowAndBlock(t *testing.T) {
	// 2 requests per second with burst of 2
	limiter := NewRateLimiter(rate.Limit(2), 2)

	// First two should succeed immediately
	if !limiter.Allow("user-1") {
		t.Fatal("expected first request to be allowed")
	}
	if !limiter.Allow("user-1") {
		t.Fatal("expected second request to be allowed")
	}

	// Third request immediately should be blocked
	if limiter.Allow("user-1") {
		t.Fatal("expected third request to be blocked")
	}

	// Different user should still be allowed
	if !limiter.Allow("user-2") {
		t.Fatal("expected user-2 request to be allowed independently")
	}
}

func TestRateLimiter_Cleanup(t *testing.T) {
	limiter := NewRateLimiter(rate.Limit(10), 10)
	limiter.ttl = 10 * time.Millisecond

	limiter.Allow("stale-user")
	time.Sleep(20 * time.Millisecond)

	limiter.Cleanup()

	limiter.mu.Lock()
	_, exists := limiter.visitors["stale-user"]
	limiter.mu.Unlock()

	if exists {
		t.Fatal("expected stale-user to be cleaned up after ttl expiration")
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	limiter := NewRateLimiter(rate.Limit(1), 1)

	called := 0
	handler := RateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	// Request 1: allowed
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec1.Code)
	}

	// Request 2: blocked (429)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") != "60" {
		t.Fatalf("expected Retry-After header 60, got %s", rec2.Header().Get("Retry-After"))
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("expected text/plain Content-Type, got %s", ct)
	}
}

func TestRateLimitMiddleware_PrincipalKeying(t *testing.T) {
	limiter := NewRateLimiter(rate.Limit(1), 1)

	handler := RateLimitMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	ctx1 := context.WithValue(req1.Context(), principalKey, identity.Principal{ID: "user-1", DisplayName: "alice"})
	req1 = req1.WithContext(ctx1)

	// User 1 - first request allowed
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec1.Code)
	}

	// User 1 - second request blocked
	rec1Blocked := httptest.NewRecorder()
	handler.ServeHTTP(rec1Blocked, req1)
	if rec1Blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status 429 for user 1, got %d", rec1Blocked.Code)
	}

	// User 2 - request allowed despite user 1 being blocked
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil)
	ctx2 := context.WithValue(req2.Context(), principalKey, identity.Principal{ID: "user-2", DisplayName: "bob"})
	req2 = req2.WithContext(ctx2)

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected status 200 for user 2, got %d", rec2.Code)
	}
}

func TestRateLimiter_OpportunisticCleanup(t *testing.T) {
	limiter := NewRateLimiter(rate.Limit(10), 10)
	limiter.ttl = 10 * time.Millisecond

	limiter.Allow("stale-user")
	time.Sleep(20 * time.Millisecond)

	// Simulate 1 minute having passed since last cleanup
	limiter.mu.Lock()
	limiter.lastCleanup = time.Now().Add(-2 * time.Minute)
	limiter.mu.Unlock()

	// Calling Allow for another user triggers opportunistic cleanup of stale-user
	limiter.Allow("active-user")

	limiter.mu.Lock()
	_, existsStale := limiter.visitors["stale-user"]
	_, existsActive := limiter.visitors["active-user"]
	limiter.mu.Unlock()

	if existsStale {
		t.Fatal("expected stale-user to be opportunistically cleaned up on Allow call")
	}
	if !existsActive {
		t.Fatal("expected active-user to be registered")
	}
}
