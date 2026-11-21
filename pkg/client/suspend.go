package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// SuspendSandbox requests cold suspension of a Sandbox.
func (c *Client) SuspendSandbox(ctx context.Context, id string) (*apiv1.SandboxResponse, error) {
	return c.setSuspension(ctx, id, "suspend")
}

// ResumeSandbox requests a new execution Pod for a suspended Sandbox.
func (c *Client) ResumeSandbox(ctx context.Context, id string) (*apiv1.SandboxResponse, error) {
	return c.setSuspension(ctx, id, "resume")
}

func (c *Client) setSuspension(ctx context.Context, id, operation string) (*apiv1.SandboxResponse, error) {
	endpoint := c.endpoint + "/api/v1/sandboxes/" + url.PathEscape(id) + "/" + operation
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return nil, fmt.Errorf("failed to create %s request: %w", operation, err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s sandbox request failed: %w", operation, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s sandbox failed with status %d: %s", operation, resp.StatusCode, string(body))
	}
	var result apiv1.SandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode %s response: %w", operation, err)
	}
	return &result, nil
}
