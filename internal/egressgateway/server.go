package egressgateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server implements an HTTP CONNECT tunneling and plain HTTP forward proxy.
type Server struct {
	HMACSecret              []byte
	Resolver                *net.Resolver
	IdleTimeout             time.Duration
	MaxConcurrentPerSandbox int

	mu          sync.Mutex
	activeConns map[string]int
}

// NewServer initializes a new forward proxy server.
func NewServer(hmacSecret []byte) *Server {
	return &Server{
		HMACSecret:              hmacSecret,
		IdleTimeout:             120 * time.Second,
		MaxConcurrentPerSandbox: 64,
		activeConns:             make(map[string]int),
	}
}

func (s *Server) authenticate(req *http.Request) (*Claims, error) {
	authHeader := req.Header.Get("Proxy-Authorization")
	if authHeader == "" {
		return nil, errors.New("missing Proxy-Authorization header")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "basic") {
		return nil, errors.New("unsupported authorization scheme")
	}

	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed basic auth payload")
	}

	credParts := strings.SplitN(string(decoded), ":", 2)
	if len(credParts) != 2 {
		return nil, errors.New("invalid credentials format")
	}

	token := credParts[1]
	claims, err := VerifyToken(token, s.HMACSecret)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}

	return claims, nil
}

func (s *Server) acquireSlot(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	max := s.MaxConcurrentPerSandbox
	if max <= 0 {
		max = 64
	}

	current := s.activeConns[sub]
	if current >= max {
		return false
	}
	s.activeConns[sub] = current + 1
	return true
}

func (s *Server) releaseSlot(sub string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.activeConns[sub]--
	if s.activeConns[sub] <= 0 {
		delete(s.activeConns, sub)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	claims, err := s.authenticate(req)
	if err != nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="buddy-egress-gateway"`)
		http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
		return
	}

	if !s.acquireSlot(claims.Sub) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	defer s.releaseSlot(claims.Sub)

	if req.Method == http.MethodConnect {
		s.handleConnect(w, req, claims)
		return
	}

	s.handleHTTP(w, req, claims)
}

func (s *Server) handleConnect(w http.ResponseWriter, req *http.Request, claims *Claims) {
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil {
		http.Error(w, "Bad Request: invalid host:port format", http.StatusBadRequest)
		return
	}

	if port != "443" {
		http.Error(w, "Forbidden: only port 443 permitted for CONNECT", http.StatusForbidden)
		return
	}

	if !MatchDomain(host, claims.Domains) {
		http.Error(w, "Forbidden: domain not allowed", http.StatusForbidden)
		return
	}

	upstream, err := DialPinnedIP(req.Context(), host, port, s.Resolver)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad Gateway: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, rw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = clientConn.Close() }()

	if buffered := rw.Reader.Buffered(); buffered > 0 {
		if _, err := io.CopyN(upstream, rw.Reader, int64(buffered)); err != nil {
			return
		}
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	idle := s.IdleTimeout
	if idle <= 0 {
		idle = 120 * time.Second
	}
	pipeConnections(clientConn, upstream, idle)
}

func (s *Server) handleHTTP(w http.ResponseWriter, req *http.Request, claims *Claims) {
	host := req.URL.Hostname()
	if host == "" {
		host = req.Host
	}

	port := req.URL.Port()
	if port == "" {
		if _, p, err := net.SplitHostPort(req.Host); err == nil {
			port = p
		} else {
			port = "80"
		}
	}

	if port != "80" {
		http.Error(w, "Forbidden: only port 80 permitted for plain HTTP", http.StatusForbidden)
		return
	}

	if !MatchDomain(host, claims.Domains) {
		http.Error(w, "Forbidden: domain not allowed", http.StatusForbidden)
		return
	}

	req.RequestURI = ""
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Keep-Alive")

	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, p, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return DialPinnedIP(ctx, h, p, s.Resolver)
		},
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad Gateway: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func pipeConnections(c1, c2 net.Conn, idleTimeout time.Duration) {
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}

	refresh := func() {
		deadline := time.Now().Add(idleTimeout)
		_ = c1.SetDeadline(deadline)
		_ = c2.SetDeadline(deadline)
	}

	refresh()

	var wg sync.WaitGroup
	wg.Add(2)

	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				refresh()
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
				refresh()
			}
			if rerr != nil {
				break
			}
		}
		_ = dst.SetDeadline(time.Now())
		_ = src.SetDeadline(time.Now())
	}

	go copyHalf(c1, c2)
	go copyHalf(c2, c1)

	wg.Wait()
}
