package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/egressgateway"
	"github.com/klauspost/compress/zstd"
)

const (
	maxCompressedProviderBody   = 512 << 10
	maxProviderBody             = 4 << 20
	maxProviderResponse         = 16 << 20
	maxProviderOutputTokens     = 8192
	providerRequestTimeout      = 15 * time.Minute
	providerResponseHeaderLimit = 60 * time.Second
	providerJSONContentType     = "application/json"
)

// OpenAIProxyConfig configures the fixed-origin provider broker.
type OpenAIProxyConfig struct {
	HMACSecret   []byte
	ProxyAddr    string
	Store        *CredentialSessionStore
	SandboxState SandboxAuthorizer
	Transport    http.RoundTripper
}

// SandboxAuthorizer binds a credential session to the current Kubernetes Sandbox state.
type SandboxAuthorizer interface {
	Allows(namespace, name, creationID string) bool
	Ready() bool
}

type openAIProxy struct {
	store        *CredentialSessionStore
	secret       []byte
	client       *http.Client
	sandboxState SandboxAuthorizer
}

// NewOpenAIProxy creates a provider handler that can reach only OpenAI's Responses API.
func NewOpenAIProxy(cfg OpenAIProxyConfig) (http.Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("credential session store is required")
	}
	if cfg.SandboxState == nil {
		return nil, errors.New("Sandbox state cache is required")
	}
	if len(cfg.HMACSecret) < 32 {
		return nil, errors.New("egress gateway HMAC secret must contain at least 32 bytes")
	}
	if cfg.ProxyAddr == "" {
		cfg.ProxyAddr = "buddy-sandboxes-egress-gateway.buddy-sandboxes-system.svc.cluster.local:8080"
	}
	if cfg.Transport == nil {
		transport := &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				token, ok := r.Context().Value(egressProxyTokenContextKey{}).(string)
				if !ok || token == "" {
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
		cfg.Transport = transport
	}
	return &openAIProxy{
		store:        cfg.Store,
		secret:       append([]byte(nil), cfg.HMACSecret...),
		client:       &http.Client{Transport: cfg.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		sandboxState: cfg.SandboxState,
	}, nil
}

//nolint:gocyclo // Validation, quota reservation, fixed-origin forwarding, and bounded streaming form one request boundary.
func (p *openAIProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, ok := credentialSessionFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !p.store.isActive(session) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	cred, ok := providerRecordFromContext(r.Context())
	if !ok {
		providerKey, keyOK := providerKeyFromContext(r.Context())
		if !keyOK {
			http.Error(w, "Provider credential is unavailable", http.StatusUnauthorized)
			return
		}
		cred = credentials.Record{Key: providerKey}
	}
	if !p.sandboxState.Allows(session.key.namespace, session.key.sandbox, session.key.creationID) {
		http.Error(w, "Provider session is unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != providerResponsesPath || r.URL.RawQuery != "" || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	body, accept, err := decodeProviderRequest(r)
	if err != nil {
		http.Error(w, "Invalid provider request", http.StatusBadRequest)
		return
	}
	var envelope struct {
		Model           string          `json:"model"`
		Input           json.RawMessage `json:"input"`
		MaxOutputTokens int64           `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || containsUnsupportedMedia(body) {
		http.Error(w, "Unsupported provider request", http.StatusBadRequest)
		return
	}
	if err := validateJSONNoDuplicateKeys(body); err != nil {
		http.Error(w, "Unsupported provider request", http.StatusBadRequest)
		return
	}
	var requestFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &requestFields); err != nil {
		http.Error(w, "Unsupported provider request", http.StatusBadRequest)
		return
	}
	if envelope.Model == "" && cred.DefaultModel != "" {
		envelope.Model = cred.DefaultModel
		requestFields["model"], _ = json.Marshal(envelope.Model)
	}
	if err := credentials.ValidateModelName(envelope.Model); err != nil || envelope.Model == "" {
		http.Error(w, "Unsupported provider request", http.StatusBadRequest)
		return
	}
	if _, exists := requestFields["max_output_tokens"]; !exists {
		envelope.MaxOutputTokens = maxProviderOutputTokens
		requestFields["max_output_tokens"], _ = json.Marshal(maxProviderOutputTokens)
		body, err = json.Marshal(requestFields)
		if err != nil {
			http.Error(w, "Unsupported provider request", http.StatusBadRequest)
			return
		}
	}
	if envelope.MaxOutputTokens < 1 || envelope.MaxOutputTokens > maxProviderOutputTokens {
		http.Error(w, "Unsupported provider request", http.StatusBadRequest)
		return
	}
	reserve := int64(len(body)) + envelope.MaxOutputTokens
	if reserve < envelope.MaxOutputTokens {
		http.Error(w, "Provider request exceeds session budget", http.StatusTooManyRequests)
		return
	}
	if !p.store.reserve(session, reserve) {
		http.Error(w, "Provider session is busy or its quota is exhausted", http.StatusTooManyRequests)
		return
	}
	defer p.store.finishRequest(session)

	targetHost := "api.openai.com"
	upstreamURL := &url.URL{Scheme: "https", Host: targetHost, Path: "/v1/responses"}
	if cred.BaseURL != "" {
		parsed, err := url.Parse(cred.BaseURL)
		if err != nil || parsed.Host == "" {
			http.Error(w, "Invalid provider base URL", http.StatusBadGateway)
			return
		}
		targetHost = parsed.Hostname()
		basePath := strings.TrimSuffix(parsed.Path, "/")
		subpath := "/responses"
		if !strings.HasSuffix(basePath, "/v1") {
			subpath = "/v1/responses"
		}
		upstreamURL.Scheme = parsed.Scheme
		upstreamURL.Host = parsed.Host
		upstreamURL.Path = path.Clean(basePath + subpath)
		upstreamURL.RawQuery = parsed.RawQuery
	}

	ctx, cancel := context.WithTimeout(r.Context(), providerRequestTimeout)
	defer cancel()
	sub := "buddy-api"
	ns := "buddy-sandboxes-system"
	if session.key.sandbox != "" {
		sub = session.key.sandbox
		ns = session.key.namespace
	}
	proxyToken, err := egressgateway.GenerateToken(egressgateway.Claims{
		Sub: sub, Ns: ns, Domains: []string{targetHost},
		Exp: time.Now().Add(providerRequestTimeout + 30*time.Second).Unix(),
	}, p.secret)
	if err != nil {
		http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
		return
	}
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "Provider service unavailable", http.StatusServiceUnavailable)
		return
	}
	upstreamReq.Host = upstreamURL.Host
	isAzure := strings.HasSuffix(targetHost, ".openai.azure.com") ||
		strings.HasSuffix(targetHost, ".cognitiveservices.azure.com") ||
		strings.HasSuffix(targetHost, ".azure.com") ||
		cred.AuthType == "api-key"
	if isAzure {
		upstreamReq.Header.Set("api-key", cred.Key)
	} else {
		upstreamReq.Header.Set("Authorization", "Bearer "+cred.Key)
	}
	upstreamReq.Header.Set("Content-Type", providerJSONContentType)
	upstreamReq.Header.Set("Accept", accept)
	for _, name := range []string{"User-Agent", "originator", "x-client-request-id", "session-id", "thread-id", "x-openai-subagent", "x-codex-beta-features", "x-codex-turn-state", "x-codex-routing-hint"} {
		if value := r.Header.Values(name); len(value) == 1 && !strings.ContainsAny(value[0], "\r\n") {
			upstreamReq.Header.Set(name, value[0])
		}
	}
	upstreamReq.ContentLength = int64(len(body))
	upstreamReq = upstreamReq.WithContext(context.WithValue(upstreamReq.Context(), egressProxyTokenContextKey{}, proxyToken))
	response, err := p.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("Provider request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusBadRequest {
		p.store.reconcileReservation(session, reserve, 0)
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		http.Error(w, "Provider redirects are not supported", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "X-Request-ID"} {
		if value := response.Header.Get(name); value != "" && !strings.ContainsAny(value, "\r\n") {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	remaining := int64(maxProviderResponse)
	buffer := make([]byte, 32<<10)
	usageBody := make([]byte, 0, 16<<10)
	idleTimer := time.AfterFunc(time.Minute, cancel)
	defer idleTimer.Stop()
	for remaining > 0 {
		readSize := int64(len(buffer))
		if readSize > remaining {
			readSize = remaining
		}
		n, readErr := response.Body.Read(buffer[:readSize])
		if n > 0 {
			idleTimer.Reset(time.Minute)
			if len(usageBody)+n <= maxProviderResponse {
				usageBody = append(usageBody, buffer[:n]...)
			}
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
			remaining -= int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return
			}
			mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if mediaType == providerJSONContentType || mediaType == "text/event-stream" {
				if used, ok := parseUsage(usageBody); ok {
					p.store.reconcileReservation(session, reserve, used)
				}
			}
			return
		}
	}
}

func decodeProviderRequest(r *http.Request) ([]byte, string, error) {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 || contentTypes[0] != providerJSONContentType {
		return nil, "", fmt.Errorf("unsupported Content-Type")
	}
	acceptValues := r.Header.Values("Accept")
	if len(acceptValues) > 1 {
		return nil, "", fmt.Errorf("multiple Accept headers")
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		accept = providerJSONContentType
	}
	if accept != providerJSONContentType && accept != "text/event-stream" {
		return nil, "", fmt.Errorf("unsupported Accept header")
	}
	encodings := r.Header.Values("Content-Encoding")
	if len(encodings) > 1 {
		return nil, "", fmt.Errorf("multiple Content-Encoding headers")
	}
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "zstd" {
		return nil, "", fmt.Errorf("unsupported Content-Encoding")
	}
	compressed, err := io.ReadAll(io.LimitReader(r.Body, maxCompressedProviderBody+1))
	if err != nil || len(compressed) > maxCompressedProviderBody {
		return nil, "", fmt.Errorf("provider request too large")
	}
	body := compressed
	if encoding == "zstd" {
		decoder, err := zstd.NewReader(bytes.NewReader(compressed), zstd.WithDecoderMaxMemory(maxProviderBody), zstd.WithDecoderMaxWindow(maxProviderBody))
		if err != nil {
			return nil, "", fmt.Errorf("invalid zstd body")
		}
		decompressed, err := io.ReadAll(io.LimitReader(decoder, maxProviderBody+1))
		decoder.Close()
		if err != nil || len(decompressed) > maxProviderBody {
			return nil, "", fmt.Errorf("provider request too large")
		}
		body = decompressed
	}
	if len(body) == 0 || len(body) > maxProviderBody || !json.Valid(body) {
		return nil, "", fmt.Errorf("invalid provider JSON")
	}
	return body, accept, nil
}

func containsUnsupportedMedia(body []byte) bool {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return true
	}
	var visit func(any) bool
	visit = func(node any) bool {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				switch strings.ToLower(key) {
				case "image", "images", "image_url", "input_image", "file", "files", "file_id", "file_url", "file_data", "input_file", "input_audio", "audio", "video":
					return true
				}
				if key == "type" {
					if typeName, ok := child.(string); ok && (typeName == "input_image" || typeName == "input_audio" || typeName == "input_file") {
						return true
					}
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if visit(child) {
					return true
				}
			}
		case string:
			return strings.Contains(value, "data:image/")
		}
		return false
	}
	return visit(value)
}

func parseUsage(body []byte) (int64, bool) {
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "data:")
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		var value any
		if json.Unmarshal([]byte(line), &value) != nil {
			continue
		}
		if input, output, ok := findUsage(value); ok {
			return input + output, true
		}
	}
	return 0, false
}

func findUsage(value any) (int64, int64, bool) {
	switch node := value.(type) {
	case map[string]any:
		if raw, ok := node["usage"].(map[string]any); ok {
			input, inputOK := numericJSONField(raw["input_tokens"])
			if !inputOK {
				input, inputOK = numericJSONField(raw["prompt_tokens"])
			}
			output, outputOK := numericJSONField(raw["output_tokens"])
			if !outputOK {
				output, outputOK = numericJSONField(raw["completion_tokens"])
			}
			if inputOK && outputOK {
				return input, output, true
			}
			if total, totalOK := numericJSONField(raw["total_tokens"]); totalOK {
				return total, 0, true
			}
		}
		for _, child := range node {
			if input, output, ok := findUsage(child); ok {
				return input, output, true
			}
		}
	case []any:
		for _, child := range node {
			if input, output, ok := findUsage(child); ok {
				return input, output, true
			}
		}
	}
	return 0, 0, false
}

func numericJSONField(value any) (int64, bool) {
	number, ok := value.(float64)
	return int64(number), ok && number >= 0 && float64(int64(number)) == number
}

type egressProxyTokenContextKey struct{}

func egressProxyURL(address, token string) *url.URL {
	return &url.URL{Scheme: "http", Host: address, User: url.UserPassword("buddy-api", token)}
}
