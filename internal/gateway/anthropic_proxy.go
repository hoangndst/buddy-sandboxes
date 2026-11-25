package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/egressgateway"
)

const anthropicRequestLimit = 4 << 20
const providerEventStream = "text/event-stream"

type AnthropicProxyConfig struct {
	HMACSecret   []byte
	ProxyAddr    string
	Store        *CredentialSessionStore
	SandboxState SandboxAuthorizer
	Transport    http.RoundTripper
}

type anthropicProxy struct {
	store  *CredentialSessionStore
	secret []byte
	client *http.Client
	state  SandboxAuthorizer
}

func NewAnthropicProxy(cfg AnthropicProxyConfig) (http.Handler, error) {
	if cfg.Store == nil || cfg.SandboxState == nil {
		return nil, errors.New("credential session store and Sandbox state cache are required")
	}
	if len(cfg.HMACSecret) < 32 {
		return nil, errors.New("egress gateway HMAC secret must contain at least 32 bytes")
	}
	if cfg.ProxyAddr == "" {
		cfg.ProxyAddr = "buddy-sandboxes-egress-gateway.buddy-sandboxes-system.svc.cluster.local:8080"
	}
	if cfg.Transport == nil {
		cfg.Transport = &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				token, _ := r.Context().Value(egressProxyTokenContextKey{}).(string)
				if token == "" {
					return nil, errors.New("egress gateway token is missing")
				}
				return egressProxyURL(cfg.ProxyAddr, token), nil
			},
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: providerResponseHeaderLimit,
			DisableCompression:    true,
		}
	}
	return &anthropicProxy{store: cfg.Store, secret: append([]byte(nil), cfg.HMACSecret...), client: &http.Client{Transport: cfg.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, state: cfg.SandboxState}, nil
}

//nolint:gocyclo // Request validation, fixed-origin forwarding, and bounded streaming share one trust boundary.
func (p *anthropicProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, ok := credentialSessionFromContext(r.Context())
	if !ok || !p.store.isActive(session) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	apiKey, ok := providerKeyFromContext(r.Context())
	if !ok {
		http.Error(w, "Provider credential is unavailable", http.StatusUnauthorized)
		return
	}
	if !p.state.Allows(session.key.namespace, session.key.sandbox, session.key.creationID) {
		http.Error(w, "Provider service is unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != providerAnthropicMessagesPath || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.RequestURI != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || contentTypes[0] != "application/json" {
		http.Error(w, "Invalid provider request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, anthropicRequestLimit+1))
	if err != nil || len(body) == 0 || len(body) > anthropicRequestLimit || !json.Valid(body) || validateJSONNoDuplicateKeys(body) != nil {
		http.Error(w, "Invalid provider request", http.StatusBadRequest)
		return
	}
	var input struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
	}
	if json.Unmarshal(body, &input) != nil || input.Model == "" || input.MaxTokens < 1 || input.MaxTokens > maxProviderOutputTokens || containsUnsupportedMedia(body) {
		http.Error(w, "Unsupported provider request", http.StatusBadRequest)
		return
	}
	reserved := int64(len(body)) + input.MaxTokens
	if !p.store.reserve(session, reserved) {
		w.Header().Set("X-Buddy-Rate-Limited", "true")
		http.Error(w, "Provider session is busy or its quota is exhausted", http.StatusTooManyRequests)
		return
	}
	defer p.store.finishRequest(session)
	ctx, cancel := context.WithTimeout(r.Context(), providerRequestTimeout)
	defer cancel()
	proxyToken, err := egressgateway.GenerateToken(egressgateway.Claims{Sub: "buddy-api", Ns: "buddy-sandboxes-system", Domains: []string{"api.anthropic.com"}, Exp: time.Now().Add(providerRequestTimeout + 30*time.Second).Unix()}, p.secret)
	if err != nil {
		http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
		return
	}
	upstream.Header.Set("x-api-key", apiKey)
	upstream.Header.Set("anthropic-version", "2023-06-01")
	upstream.Header.Set("Content-Type", "application/json")
	accept := r.Header.Get("Accept")
	if accept != providerEventStream {
		accept = "application/json"
	}
	upstream.Header.Set("Accept", accept)
	if values := r.Header.Values("anthropic-beta"); len(values) == 1 && len(values[0]) <= 1024 && !strings.ContainsAny(values[0], "\r\n") {
		upstream.Header.Set("anthropic-beta", values[0])
	}
	if values := r.Header.Values("User-Agent"); len(values) == 1 && !strings.ContainsAny(values[0], "\r\n") {
		upstream.Header.Set("User-Agent", values[0])
	}
	upstream.ContentLength = int64(len(body))
	upstream = upstream.WithContext(context.WithValue(upstream.Context(), egressProxyTokenContextKey{}, proxyToken))
	response, err := p.client.Do(upstream)
	if err != nil {
		http.Error(w, "Provider request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		http.Error(w, "Provider redirects are not supported", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "Request-Id"} {
		if v := response.Header.Get(name); v != "" && !strings.ContainsAny(v, "\r\n") {
			w.Header().Set(name, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	remaining := int64(maxProviderResponse)
	buf := make([]byte, 32<<10)
	usageBody := make([]byte, 0, 16<<10)
	isEventStream := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), providerEventStream)
	idle := time.AfterFunc(time.Minute, cancel)
	defer idle.Stop()
	for remaining > 0 {
		size := int64(len(buf))
		if size > remaining {
			size = remaining
		}
		n, readErr := response.Body.Read(buf[:size])
		if n > 0 {
			idle.Reset(time.Minute)
			if len(usageBody)+n <= maxProviderResponse {
				usageBody = append(usageBody, buf[:n]...)
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			remaining -= int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if used, ok := parseAnthropicUsage(usageBody, isEventStream); ok {
					p.store.reconcileReservation(session, reserved, used)
				}
			}
			return
		}
	}
}

func parseAnthropicUsage(body []byte, eventStream bool) (int64, bool) {
	if !eventStream {
		var response struct {
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal(body, &response) != nil || response.Usage == nil {
			return 0, false
		}
		input, inputOK := numericJSONField(response.Usage["input_tokens"])
		output, outputOK := numericJSONField(response.Usage["output_tokens"])
		return input + output, inputOK && outputOK
	}

	var input int64
	var output int64
	var hasInput, hasOutput, messageStopped bool
	// Anthropic reports input usage at message_start and cumulative output usage in message_delta.
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Message struct {
				Usage map[string]any `json:"usage"`
			} `json:"message"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		switch frame.Type {
		case "message_start":
			if value, ok := numericJSONField(frame.Message.Usage["input_tokens"]); ok {
				input, hasInput = value, true
			}
		case "message_delta":
			if value, ok := numericJSONField(frame.Usage["output_tokens"]); ok {
				// Keep the greatest cumulative count instead of summing repeated deltas.
				if !hasOutput || value > output {
					output = value
				}
				hasOutput = true
			}
		case "message_stop":
			messageStopped = true
		}
	}
	if !messageStopped || !hasInput || !hasOutput {
		return 0, false
	}
	return input + output, true
}
