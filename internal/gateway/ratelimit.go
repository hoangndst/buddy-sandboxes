package gateway

import (
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter manages token-bucket rate limiters per client key.
type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	limit    rate.Limit
	burst    int
	ttl      time.Duration
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter creates a new per-client token-bucket rate limiter.
// r: sustained requests per second (e.g. rate.Every(time.Second) or rate.Limit(1.0) for 60 RPM).
// burst: maximum burst token allowance.
func NewRateLimiter(r rate.Limit, burst int) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*visitor),
		limit:    r,
		burst:    burst,
		ttl:      5 * time.Minute,
	}
}

// Allow reports whether an event may happen now for key.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.visitors[key]
	if !exists {
		limiter := rate.NewLimiter(rl.limit, rl.burst)
		rl.visitors[key] = &visitor{limiter: limiter, lastSeen: time.Now()}
		return limiter.Allow()
	}

	v.lastSeen = time.Now()
	return v.limiter.Allow()
}

// Cleanup removes stale visitor records whose last access exceeds ttl.
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.ttl)
	for k, v := range rl.visitors {
		if v.lastSeen.Before(cutoff) {
			delete(rl.visitors, k)
		}
	}
}

// RateLimitMiddleware returns an HTTP middleware enforcing per-principal token-bucket limits.
func RateLimitMiddleware(limiter *RateLimiter, next http.Handler) http.Handler {
	if limiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := resolveClientKey(r)
		if !limiter.Allow(key) {
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "rate limit exceeded: please slow down your requests",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func resolveClientKey(r *http.Request) string {
	if principal, ok := PrincipalFromContext(r.Context()); ok && principal.ID != "" {
		return "user:" + principal.ID
	}
	// Fall back to client IP address
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}
	return "ip:" + host
}
