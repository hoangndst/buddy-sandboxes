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

func getJSONResource[T any](ctx context.Context, c *Client, endpoint, actionName string) (*T, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s failed (status %d): %s", actionName, resp.StatusCode, string(respBody))
	}

	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", actionName, err)
	}
	return &result, nil
}

// CreateTask submits an autonomous task with client idempotency key.
func (c *Client) CreateTask(
	ctx context.Context,
	idempotencyKey string,
	req *apiv1.CreateTaskRequest,
) (*apiv1.TaskResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal create task request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/tasks", c.endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create task failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var taskResp apiv1.TaskResponse
	if err := json.NewDecoder(resp.Body).Decode(&taskResp); err != nil {
		return nil, fmt.Errorf("decode create task response: %w", err)
	}
	return &taskResp, nil
}

// GetTask retrieves task details by ID.
func (c *Client) GetTask(ctx context.Context, taskID string) (*apiv1.TaskResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tasks/%s", c.endpoint, url.PathEscape(taskID))
	return getJSONResource[apiv1.TaskResponse](ctx, c, endpoint, "get task")
}

// ListTasks lists tasks with optional query filters.
func (c *Client) ListTasks(ctx context.Context, namespace, workspace, status string) (*apiv1.ListTasksResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tasks", c.endpoint)
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}
	if workspace != "" {
		params.Set("workspace", workspace)
	}
	if status != "" {
		params.Set("status", status)
	}
	if len(params) > 0 {
		endpoint = fmt.Sprintf("%s?%s", endpoint, params.Encode())
	}
	return getJSONResource[apiv1.ListTasksResponse](ctx, c, endpoint, "list tasks")
}

// CancelTask cancels an active task.
func (c *Client) CancelTask(ctx context.Context, taskID string) error {
	endpoint := fmt.Sprintf("%s/api/v1/tasks/%s/cancel", c.endpoint, url.PathEscape(taskID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel task failed (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// GetExecution retrieves execution attempt details.
func (c *Client) GetExecution(ctx context.Context, executionID string) (*apiv1.ExecutionResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/executions/%s", c.endpoint, url.PathEscape(executionID))
	return getJSONResource[apiv1.ExecutionResponse](ctx, c, endpoint, "get execution")
}

// ListExecutions lists execution attempts for a task.
func (c *Client) ListExecutions(ctx context.Context, taskID string) (*apiv1.ListExecutionsResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/tasks/%s/executions", c.endpoint, url.PathEscape(taskID))
	return getJSONResource[apiv1.ListExecutionsResponse](ctx, c, endpoint, "list executions")
}

// GetExecutionLogs retrieves the execution logs as text.
func (c *Client) GetExecutionLogs(ctx context.Context, executionID string) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/executions/%s/logs", c.endpoint, url.PathEscape(executionID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get execution logs failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read execution logs: %w", err)
	}
	return string(body), nil
}

// CreateAgentProfile registers an agent profile.
func (c *Client) CreateAgentProfile(
	ctx context.Context,
	req *apiv1.CreateAgentProfileRequest,
) (*apiv1.AgentProfileResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal create agent profile request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/agent-profiles", c.endpoint)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create agent profile failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var profResp apiv1.AgentProfileResponse
	if err := json.NewDecoder(resp.Body).Decode(&profResp); err != nil {
		return nil, fmt.Errorf("decode create agent profile response: %w", err)
	}
	return &profResp, nil
}

// GetAgentProfile retrieves an agent profile by ID.
func (c *Client) GetAgentProfile(ctx context.Context, profileID string) (*apiv1.AgentProfileResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agent-profiles/%s", c.endpoint, url.PathEscape(profileID))
	return getJSONResource[apiv1.AgentProfileResponse](ctx, c, endpoint, "get agent profile")
}

// ListAgentProfiles retrieves all agent profiles.
func (c *Client) ListAgentProfiles(ctx context.Context) (*apiv1.ListAgentProfilesResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agent-profiles", c.endpoint)
	return getJSONResource[apiv1.ListAgentProfilesResponse](ctx, c, endpoint, "list agent profiles")
}
