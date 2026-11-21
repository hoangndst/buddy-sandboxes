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
	DialContext             func(ctx context.Context, network, addr string) (net.Conn, error)

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

func (s *Server) acquireSlot(ns, sub string) bool {
	key := ns + "/" + sub
	s.mu.Lock()
	defer s.mu.Unlock()

	max := s.MaxConcurrentPerSandbox
	if max <= 0 {
		max = 64
	}

	current := s.activeConns[key]
	if current >= max {
		return false
	}
	s.activeConns[key] = current + 1
	return true
}

func (s *Server) releaseSlot(ns, sub string) {
	key := ns + "/" + sub
	s.mu.Lock()
	defer s.mu.Unlock()

	s.activeConns[key]--
	if s.activeConns[key] <= 0 {
		delete(s.activeConns, key)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	claims, err := s.authenticate(req)
	if err != nil {
		w.Header().Set("Proxy-Authenticate", `Basic realm="buddy-egress-gateway"`)
		http.Error(w, "Proxy Authentication Required", http.StatusProxyAuthRequired)
		return
	}

	if !s.acquireSlot(claims.Ns, claims.Sub) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	defer s.releaseSlot(claims.Ns, claims.Sub)

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

	var upstream net.Conn
	if s.DialContext != nil {
		upstream, err = s.DialContext(req.Context(), "tcp", net.JoinHostPort(host, port))
	} else {
		upstream, err = DialPinnedIP(req.Context(), host, port, s.Resolver)
	}
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

	idleTimeout := s.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}

	transport := &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: idleTimeout,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var conn net.Conn
			var err error
			if s.DialContext != nil {
				conn, err = s.DialContext(ctx, network, addr)
			} else {
				h, p, errSplit := net.SplitHostPort(addr)
				if errSplit != nil {
					return nil, errSplit
				}
				conn, err = DialPinnedIP(ctx, h, p, s.Resolver)
			}
			if err != nil {
				return nil, err
			}
			_ = conn.SetDeadline(time.Now().Add(idleTimeout))
			return &idleConn{Conn: conn, idleTimeout: idleTimeout}, nil
		},
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		var netErr net.Error
		if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, fmt.Sprintf("Gateway Timeout: %v", err), http.StatusGatewayTimeout)
			return
		}
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
	var dst io.Writer = w
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
		dst = &flushWriter{w: w, f: f}
	}
	if _, copyErr := io.Copy(dst, resp.Body); copyErr != nil && !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, context.Canceled) {
		panic(http.ErrAbortHandler)
	}
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if n > 0 && fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

type idleConn struct {
	net.Conn
	idleTimeout time.Duration
}

func (c *idleConn) Read(b []byte) (n int, err error) {
	if c.idleTimeout > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
	}
	n, err = c.Conn.Read(b)
	if n > 0 && c.idleTimeout > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (n int, err error) {
	if c.idleTimeout > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
	}
	n, err = c.Conn.Write(b)
	if n > 0 && c.idleTimeout > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
	}
	return n, err
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
