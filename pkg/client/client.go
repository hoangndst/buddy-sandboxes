package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// Config configures the SDK Client.
type Config struct {
	Endpoint   string
	Token      string
	Namespace  string
	HTTPClient *http.Client
}

// Client provides an API client for Buddy Sandboxes Gateway.
type Client struct {
	endpoint   string
	token      string
	namespace  string
	httpClient *http.Client
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

	return &Client{
		endpoint:   endpoint,
		token:      cfg.Token,
		namespace:  cfg.Namespace,
		httpClient: httpClient,
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
