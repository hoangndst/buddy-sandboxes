package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// Config configures the SDK Client.
type Config struct {
	Endpoint   string
	Token      string
	Namespace  string
	HTTPClient *http.Client
	WSDialer   *websocket.Dialer
}

// Client provides an API client for Buddy Sandboxes Gateway.
type Client struct {
	endpoint     string
	token        string
	namespace    string
	httpClient   *http.Client
	streamClient *http.Client
	wsDialer     *websocket.Dialer
}

// NewClient creates a new Client instance.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}

	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	streamClient := &http.Client{
		Transport: httpClient.Transport,
		Timeout:   0,
	}

	wsDialer := cfg.WSDialer
	if wsDialer == nil {
		var tlsConfig *tls.Config
		if transport, ok := httpClient.Transport.(*http.Transport); ok {
			tlsConfig = transport.TLSClientConfig
		}
		wsDialer = &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 45 * time.Second,
			TLSClientConfig:  tlsConfig,
		}
	}

	return &Client{
		endpoint:     endpoint,
		token:        cfg.Token,
		namespace:    cfg.Namespace,
		httpClient:   httpClient,
		streamClient: streamClient,
		wsDialer:     wsDialer,
	}, nil
}

// CreateSandbox provisions a new sandbox.
func (c *Client) CreateSandbox(ctx context.Context, req *apiv1.CreateSandboxRequest) (*apiv1.SandboxResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/v1/sandboxes", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create sandbox request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create sandbox failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var sbxResp apiv1.SandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbxResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &sbxResp, nil
}

// GetSandbox retrieves sandbox status and metadata.
func (c *Client) GetSandbox(ctx context.Context, id string) (*apiv1.SandboxResponse, error) {
	reqURL := c.endpoint + "/api/v1/sandboxes/" + url.PathEscape(id)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("get sandbox request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get sandbox failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var sbxResp apiv1.SandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbxResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &sbxResp, nil
}

// ListSandboxes retrieves a list of active sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) (*apiv1.ListSandboxesResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/v1/sandboxes", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list sandboxes failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var listResp apiv1.ListSandboxesResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &listResp, nil
}

// DeleteSandbox terminates and removes a sandbox.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	reqURL := c.endpoint + "/api/v1/sandboxes/" + url.PathEscape(id)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create http request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("delete sandbox request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete sandbox failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// WaitForRunning polls until the sandbox is in Running phase or reaches a terminal phase.
func (c *Client) WaitForRunning(
	ctx context.Context,
	id string,
	pollInterval time.Duration,
) (*apiv1.SandboxResponse, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			sbx, err := c.GetSandbox(ctx, id)
			if err != nil {
				return nil, err
			}
			if sbx.Phase == "Running" {
				return sbx, nil
			}
			if sbx.Phase == "Failed" || sbx.Phase == "Succeeded" {
				return sbx, fmt.Errorf("sandbox reached terminal phase %q before Running", sbx.Phase)
			}
		}
	}
}

// WhoAmI retrieves the authenticated caller identity and tenant namespace.
func (c *Client) WhoAmI(ctx context.Context) (*apiv1.WhoAmIResponse, error) {
	reqURL := c.endpoint + "/api/v1/auth/whoami"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create whoami request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("whoami request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whoami failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var whoamiResp apiv1.WhoAmIResponse
	if err := json.NewDecoder(resp.Body).Decode(&whoamiResp); err != nil {
		return nil, fmt.Errorf("failed to decode whoami response: %w", err)
	}
	return &whoamiResp, nil
}

// Upload streams a tar archive to the specified sandbox directory.
func (c *Client) Upload(ctx context.Context, id, destPath string, reader io.Reader) error {
	reqURL := fmt.Sprintf(
		"%s/api/v1/sandboxes/%s/upload?path=%s",
		c.endpoint, url.PathEscape(id), url.QueryEscape(destPath),
	)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, reader)
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-tar")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.streamClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("upload request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// Download initiates streaming download of a tar archive from the specified sandbox directory.
func (c *Client) Download(ctx context.Context, id, srcPath string) (io.ReadCloser, error) {
	reqURL := fmt.Sprintf(
		"%s/api/v1/sandboxes/%s/download?path=%s",
		c.endpoint, url.PathEscape(id), url.QueryEscape(srcPath),
	)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create download request: %w", err)
	}
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("download request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return &downloadBody{ReadCloser: resp.Body, trailer: resp.Trailer}, nil
}

type downloadBody struct {
	io.ReadCloser
	trailer http.Header
	checked bool
}

func (b *downloadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.checked {
		b.checked = true
		if streamErr := b.trailer.Get("X-Buddy-Stream-Error"); streamErr != "" {
			return n, fmt.Errorf("remote download stream failed: %s", streamErr)
		}
	}
	return n, err
}

func (c *Client) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// WSURL returns the WebSocket URL corresponding to an HTTP endpoint path.
func (c *Client) WSURL(path string) string {
	u := c.endpoint + path
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	return "ws://" + strings.TrimPrefix(u, "http://")
}
