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
	HTTPClient *http.Client
	WSDialer   *websocket.Dialer
}

// Client provides an API client for Buddy Sandboxes Gateway.
type Client struct {
	endpoint     string
	token        string
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
	if err := validateGatewayEndpoint(endpoint); err != nil {
		return nil, err
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

// SetCredential stores a provider API key without placing it in a sandbox.
func (c *Client) SetCredential(ctx context.Context, service, sandbox, value string) error {
	if err := c.validateCredentialEndpoint(); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return fmt.Errorf("encode provider credential: %w", err)
	}
	path := c.endpoint + "/api/v1/credentials/" + url.PathEscape(service)
	if sandbox != "" {
		path += "?sandbox=" + url.QueryEscape(sandbox)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create provider credential request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req)
	resp, err := clientWithoutRedirects(c.httpClient).Do(req)
	if err != nil {
		return fmt.Errorf("store provider credential: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("store provider credential failed with status %d", resp.StatusCode)
	}
	return nil
}

// ListCredentials returns provider credential metadata, never secret values.
func (c *Client) ListCredentials(ctx context.Context) ([]apiv1.CredentialMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/v1/credentials", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list provider credentials: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list provider credentials failed with status %d", resp.StatusCode)
	}
	var result struct {
		Items []apiv1.CredentialMetadata `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode provider credential metadata: %w", err)
	}
	return result.Items, nil
}

// DeleteCredential removes a user or sandbox scoped provider API key.
func (c *Client) DeleteCredential(ctx context.Context, service, sandbox string) error {
	if err := c.validateCredentialEndpoint(); err != nil {
		return err
	}
	path := c.endpoint + "/api/v1/credentials/" + url.PathEscape(service)
	if sandbox != "" {
		path += "?sandbox=" + url.QueryEscape(sandbox)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := clientWithoutRedirects(c.httpClient).Do(req)
	if err != nil {
		return fmt.Errorf("remove provider credential: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("remove provider credential failed with status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) validateCredentialEndpoint() error {
	return validateGatewayEndpoint(c.endpoint)
}

func validateGatewayEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid gateway endpoint")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("gateway endpoint must be an origin without user information, a query, fragment, or path prefix")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return fmt.Errorf("gateway endpoint must use HTTPS or explicit loopback HTTP")
	}
	host := strings.Trim(u.Hostname(), "[]")
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return fmt.Errorf("gateway endpoint must use HTTPS or explicit loopback HTTP")
	}
	return nil
}

func clientWithoutRedirects(source *http.Client) *http.Client {
	copy := *source
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

// Upload streams a tar archive to the specified sandbox directory.
func (c *Client) Upload(ctx context.Context, id, destPath string, reader io.Reader) error {
	return c.upload(ctx, id, destPath, "", "", reader)
}

// UploadFile streams a single file to the exact destination path, renaming it when needed.
func (c *Client) UploadFile(ctx context.Context, id, destPath, archiveName string, reader io.Reader) error {
	return c.upload(ctx, id, destPath, "file", archiveName, reader)
}

// UploadDirectory streams a directory archive to the exact destination path.
func (c *Client) UploadDirectory(ctx context.Context, id, destPath, archiveName string, reader io.Reader) error {
	return c.upload(ctx, id, destPath, "directory", archiveName, reader)
}

func (c *Client) upload(ctx context.Context, id, destPath, mode, archiveName string, reader io.Reader) error {
	query := url.Values{"path": []string{destPath}}
	if mode != "" && archiveName != "" {
		query.Set(mode, archiveName)
	}
	reqURL := fmt.Sprintf(
		"%s/api/v1/sandboxes/%s/upload?%s",
		c.endpoint, url.PathEscape(id), query.Encode(),
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
	return c.download(ctx, id, srcPath, false)
}

// DownloadWorkspace streams the workspace root using root-relative archive paths.
func (c *Client) DownloadWorkspace(ctx context.Context, id string) (io.ReadCloser, error) {
	return c.download(ctx, id, "/workspace", true)
}

func (c *Client) download(ctx context.Context, id, srcPath string, workspaceRoot bool) (io.ReadCloser, error) {
	query := url.Values{"path": []string{srcPath}}
	if workspaceRoot {
		query.Set("workspaceRoot", "true")
	}
	reqURL := fmt.Sprintf(
		"%s/api/v1/sandboxes/%s/download?%s",
		c.endpoint, url.PathEscape(id), query.Encode(),
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

// ListUsers returns all platform users (admin only).
func (c *Client) ListUsers(ctx context.Context) ([]apiv1.User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/v1/admin/users", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list users failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	var result struct {
		Items []apiv1.User `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode users response: %w", err)
	}
	return result.Items, nil
}

// CreateUser creates a new user with an assigned namespace (admin only).
func (c *Client) CreateUser(ctx context.Context, displayName, namespace string) (*apiv1.User, error) {
	body, err := json.Marshal(map[string]string{
		"displayName": displayName,
		"namespace":   namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("encode create user request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/v1/admin/users", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("create user failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	var user apiv1.User
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decode create user response: %w", err)
	}
	return &user, nil
}

// SetUserDisabled sets a user's disabled status (admin only).
func (c *Client) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	body, err := json.Marshal(map[string]bool{"disabled": disabled})
	if err != nil {
		return fmt.Errorf("encode update user status: %w", err)
	}
	path := c.endpoint + "/api/v1/admin/users/" + url.PathEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update user status failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// SetUserRole updates a user's role (admin only).
func (c *Client) SetUserRole(ctx context.Context, userID, role string) error {
	body, err := json.Marshal(map[string]string{"role": role})
	if err != nil {
		return fmt.Errorf("encode update user role: %w", err)
	}
	path := c.endpoint + "/api/v1/admin/users/" + url.PathEscape(userID) + "/role"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("update user role: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update user role failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// IssueUserToken generates and returns a new login token for a user (admin only).
func (c *Client) IssueUserToken(ctx context.Context, userID string) (*apiv1.IssuedTokenResponse, error) {
	path := c.endpoint + "/api/v1/admin/users/" + url.PathEscape(userID) + "/tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("issue user token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("issue user token failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	var tokenResp apiv1.IssuedTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode issue token response: %w", err)
	}
	return &tokenResp, nil
}

// RevokeUserToken revokes an issued token for a user (admin only).
func (c *Client) RevokeUserToken(ctx context.Context, userID, tokenID string) error {
	path := c.endpoint + "/api/v1/admin/users/" + url.PathEscape(userID) + "/tokens/" + url.PathEscape(tokenID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("revoke user token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("revoke user token failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// ListUserSandboxes lists sandboxes belonging to a specific user (admin only).
func (c *Client) ListUserSandboxes(ctx context.Context, userID string) ([]apiv1.SandboxResponse, error) {
	reqURL := c.endpoint + "/api/v1/admin/users/" + url.PathEscape(userID) + "/sandboxes"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("list user sandboxes request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list user sandboxes failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result apiv1.ListSandboxesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Items, nil
}

// DeleteUserSandbox deletes a sandbox belonging to a specific user (admin only).
func (c *Client) DeleteUserSandbox(ctx context.Context, userID, sandboxName string) error {
	reqURL := c.endpoint + "/api/v1/admin/users/" + url.PathEscape(userID) + "/sandboxes/" + url.PathEscape(sandboxName)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create http request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("delete user sandbox request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete user sandbox failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
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
