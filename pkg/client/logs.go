package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// LogOptions configures log retrieval.
type LogOptions struct {
	Follow     bool
	TailLines  int
	Timestamps bool
}

// StreamLogs streams or fetches logs from the sandbox container.
func (c *Client) StreamLogs(ctx context.Context, id string, opts LogOptions, out io.Writer) error {
	q := url.Values{}
	q.Set("follow", strconv.FormatBool(opts.Follow))
	if opts.TailLines > 0 {
		q.Set("tailLines", strconv.Itoa(opts.TailLines))
	}
	if opts.Timestamps {
		q.Set("timestamps", "true")
	}

	logPath := fmt.Sprintf("/api/v1/sandboxes/%s/logs?%s", url.PathEscape(id), q.Encode())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+logPath, nil)
	if err != nil {
		return fmt.Errorf("failed to create logs request: %w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("logs request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("logs request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	_, err = io.Copy(out, resp.Body)
	return err
}
