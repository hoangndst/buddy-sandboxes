package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
)

// AttachOptions specifies parameters for attaching to a detached supervisor session.
type AttachOptions struct {
	TakeLease  bool
	ReadOnly   bool
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	ResizeChan <-chan apiv1.ResizeMessage
}

// CreateSession launches a new detached session inside the sandbox.
func (c *Client) CreateSession(
	ctx context.Context,
	sandboxID string,
	req *apiv1.CreateSessionRequest,
) (*apiv1.SessionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal create session request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/sandboxes/%s/sessions", c.endpoint, url.PathEscape(sandboxID))
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
		return nil, fmt.Errorf("create session failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var sessionResp apiv1.SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sessionResp); err != nil {
		return nil, fmt.Errorf("decode create session response: %w", err)
	}

	return &sessionResp, nil
}

// GetSession retrieves the current status and metadata of a session.
func (c *Client) GetSession(ctx context.Context, sandboxID, sessionID string) (*apiv1.SessionResponse, error) {
	escapedSbx := url.PathEscape(sandboxID)
	escapedSess := url.PathEscape(sessionID)
	endpoint := fmt.Sprintf("%s/api/v1/sandboxes/%s/sessions/%s", c.endpoint, escapedSbx, escapedSess)
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
		return nil, fmt.Errorf("get session failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var sessionResp apiv1.SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sessionResp); err != nil {
		return nil, fmt.Errorf("decode get session response: %w", err)
	}

	return &sessionResp, nil
}

// ListSessions lists all sessions associated with a sandbox.
func (c *Client) ListSessions(ctx context.Context, sandboxID string) (*apiv1.ListSessionsResponse, error) {
	endpoint := fmt.Sprintf("%s/api/v1/sandboxes/%s/sessions", c.endpoint, url.PathEscape(sandboxID))
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
		return nil, fmt.Errorf("list sessions failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var listResp apiv1.ListSessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("decode list sessions response: %w", err)
	}

	return &listResp, nil
}

// DeleteSession terminates a session.
func (c *Client) DeleteSession(ctx context.Context, sandboxID, sessionID string) error {
	escapedSbx := url.PathEscape(sandboxID)
	escapedSess := url.PathEscape(sessionID)
	endpoint := fmt.Sprintf("%s/api/v1/sandboxes/%s/sessions/%s", c.endpoint, escapedSbx, escapedSess)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
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
		return fmt.Errorf("delete session failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// AttachSession connects to an interactive session stream via WebSocket.
func (c *Client) AttachSession(ctx context.Context, sandboxID, sessionID string, opts AttachOptions) (int, error) {
	q := url.Values{}
	if opts.TakeLease {
		q.Set("take_lease", "true")
	}
	if opts.ReadOnly {
		q.Set("mode", "ro")
	}

	escapedSbx := url.PathEscape(sandboxID)
	escapedSess := url.PathEscape(sessionID)
	path := fmt.Sprintf("/api/v1/sandboxes/%s/sessions/%s/stream?%s", escapedSbx, escapedSess, q.Encode())
	wsURL := c.WSURL(path)

	headers := http.Header{}
	if c.token != "" {
		headers.Set("Authorization", "Bearer "+c.token)
	}

	dialer := c.wsDialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			return 1, fmt.Errorf("attach websocket dial failed with status %d: %w", resp.StatusCode, err)
		}
		return 1, fmt.Errorf("attach websocket dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var writeMu sync.Mutex
	sendFrame := func(channel byte, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		payload := append([]byte{channel}, data...)
		return conn.WriteMessage(websocket.BinaryMessage, payload)
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	go func() {
		<-sessionCtx.Done()
		_ = conn.Close()
	}()

	if opts.Stdin != nil && !opts.ReadOnly {
		go c.pumpAttachStdin(sessionCtx, sessionCancel, opts.Stdin, sendFrame)
	}

	if opts.ResizeChan != nil && !opts.ReadOnly {
		go c.pumpResize(sessionCtx, opts.ResizeChan, sendFrame)
	}

	return c.readFrames(sessionCtx, conn, ExecOptions{
		Stdout: opts.Stdout,
		Stderr: opts.Stderr,
	})
}

func (c *Client) pumpAttachStdin(ctx context.Context, cancel context.CancelFunc, stdin io.Reader, send frameSender) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			n, err := stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				// Check for escape hotkey Ctrl+] (ASCII 0x1D)
				for i, b := range chunk {
					if b == 0x1D {
						// Detach requested by user
						if i > 0 {
							_ = send(apiv1.ChannelStdin, chunk[:i])
						}
						cancel()
						return
					}
				}
				if err := send(apiv1.ChannelStdin, chunk); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
}
